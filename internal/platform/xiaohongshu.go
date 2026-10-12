package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// Xiaohongshu 小红书解析器：笔记页 → window.__INITIAL_STATE__ → 原图 / 视频流。
// 注意：分享链接中的 xsec_token 一次性、短时有效，请用刚分享的新链接。
type Xiaohongshu struct{}

func (Xiaohongshu) Name() string  { return "xiaohongshu" }
func (Xiaohongshu) Label() string { return "小红书" }
func (Xiaohongshu) MatchHost(host string) bool {
	return strings.Contains(host, "xiaohongshu.com") || strings.Contains(host, "xhslink.com")
}

const xhsStateMarker = "window.__INITIAL_STATE__="

func (p Xiaohongshu) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	finalURL, body, err := httpx.Get(ctx, rawURL, map[string]string{
		"User-Agent":      httpx.DesktopUA,
		"Accept-Language": "zh-CN,zh;q=0.9",
	})
	if err != nil {
		return nil, fmt.Errorf("获取笔记页失败: %w", err)
	}
	return parseXiaohongshuPage(string(body), finalURL, p.Name())
}

// parseXiaohongshuPage 解析笔记页 HTML（纯函数，便于单测）。
func parseXiaohongshuPage(s, pageURL, platformName string) (*model.Result, error) {
	idx := strings.Index(s, xhsStateMarker)
	if idx < 0 {
		return nil, fmt.Errorf("页面未携带笔记数据（链接可能已过期，请重新分享）")
	}
	start := idx + len(xhsStateMarker)
	for start < len(s) && (s[start] == ' ' || s[start] == '=') {
		start++
	}
	raw := ExtractJSON(s, start)
	if raw == "" {
		return nil, fmt.Errorf("提取页面数据失败")
	}
	var data any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("解析页面数据失败: %w", err)
	}
	detailMap, ok := httpx.J(data, "note", "noteDetailMap").(map[string]any)
	if !ok || len(detailMap) == 0 {
		return nil, fmt.Errorf("页面数据中未找到笔记")
	}
	var note any
	for _, v := range detailMap {
		note = v
		break
	}
	note = httpx.J(note, "note")
	title := httpx.S(note, "title")
	desc := httpx.S(note, "desc")
	if title == "" {
		title = TruncateRunes(desc, 60)
	}
	res := &model.Result{
		Platform: platformName,
		Title:    title,
		Author:   httpx.S(note, "user", "nickname"),
		PageURL:  pageURL,
	}
	noteType := httpx.S(note, "type") // "video" | "normal"
	if noteType == "video" {
		master := ""
		for _, stream := range httpx.Arr(note, "video", "media", "stream", "h265") {
			if u := httpx.S(stream, "masterUrl"); u != "" {
				master = u
				break
			}
		}
		if master == "" {
			for _, stream := range httpx.Arr(note, "video", "media", "stream", "h264") {
				if u := httpx.S(stream, "masterUrl"); u != "" {
					master = u
					break
				}
			}
		}
		if master == "" {
			master = httpx.S(note, "video", "consumer", "originVideoKey")
		}
		if master == "" {
			return nil, fmt.Errorf("未找到视频流地址")
		}
		cover := firstXHSImage(note)
		res.CoverURL = cover
		res.Medias = append(res.Medias, model.Media{Kind: "video", DirectURL: master, CoverURL: cover})
		return res, nil
	}
	// 图文：原图在 sns-webpic.xhscdn.com
	for _, img := range httpx.Arr(note, "imageList") {
		u := ""
		infoList := httpx.Arr(img, "infoList")
		if len(infoList) > 0 {
			u = httpx.S(infoList[len(infoList)-1], "url")
		}
		if u == "" {
			u = httpx.S(img, "url")
		}
		if u != "" {
			res.Medias = append(res.Medias, model.Media{Kind: "image", DirectURL: u})
		}
	}
	if len(res.Medias) == 0 {
		return nil, fmt.Errorf("未找到可下载的图片")
	}
	res.CoverURL = res.Medias[0].DirectURL
	return res, nil
}

// firstXHSImage 取笔记的第一张图（用作视频封面）。
func firstXHSImage(note any) string {
	imgs := httpx.Arr(note, "imageList")
	if len(imgs) == 0 {
		return ""
	}
	infoList := httpx.Arr(imgs[0], "infoList")
	if len(infoList) > 0 {
		if u := httpx.S(infoList[len(infoList)-1], "url"); u != "" {
			return u
		}
	}
	return httpx.S(imgs[0], "url")
}
