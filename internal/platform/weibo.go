package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// Weibo 微博解析器：分享链接 → MID → ajax/statuses/show 接口 → 视频流 / 原图。
type Weibo struct{}

func (Weibo) Name() string  { return "weibo" }
func (Weibo) Label() string { return "微博" }
func (Weibo) MatchHost(host string) bool {
	return strings.Contains(host, "weibo.com") || strings.Contains(host, "weibo.cn")
}

var (
	weiboMIDRe62 = regexp.MustCompile(`/(\d+)/([0-9A-Za-z]{4,})`)
	weiboMIDRe10 = regexp.MustCompile(`(?:m\.weibo\.cn/(?:detail|status)|weibo\.com/\d+/)/(\d{9,})`)
	weiboTagRe   = regexp.MustCompile(`<[^>]+>`)
)

const weiboBase62 = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// Base62Decode 微博 mid62 → mid10。
func Base62Decode(s string) int64 {
	var n int64
	for _, c := range s {
		i := strings.IndexRune(weiboBase62, c)
		if i < 0 {
			return 0
		}
		n = n*62 + int64(i)
	}
	return n
}

func (p Weibo) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	mid := ""
	if m := weiboMIDRe62.FindStringSubmatch(rawURL); len(m) == 3 {
		mid = fmt.Sprintf("%d", Base62Decode(m[2]))
	}
	if mid == "" || mid == "0" {
		mid = FirstSubmatch(weiboMIDRe10, rawURL)
	}
	if mid == "" || mid == "0" {
		return nil, fmt.Errorf("未能从链接中提取微博 ID")
	}
	api := "https://weibo.com/ajax/statuses/show?id=" + mid
	_, body, err := httpx.Get(ctx, api, map[string]string{
		"User-Agent": httpx.DesktopUA,
		"Referer":    "https://weibo.com/",
		"Accept":     "application/json",
	})
	if err != nil {
		return nil, fmt.Errorf("获取微博详情失败: %w", err)
	}
	return parseWeiboStatus(body, mid, p.Name())
}

// parseWeiboStatus 解析 statuses/show 接口 JSON（纯函数，便于单测）。
func parseWeiboStatus(body []byte, mid, platformName string) (*model.Result, error) {
	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("解析微博详情失败: %w", err)
	}
	if httpx.I(data, "ok") != 1 {
		return nil, fmt.Errorf("微博接口返回异常")
	}
	title := TruncateRunes(stripWeiboTags(httpx.S(data, "text_raw")), 80)
	if title == "" {
		title = "微博 " + mid
	}
	res := &model.Result{
		Platform: platformName,
		Title:    title,
		Author:   httpx.S(data, "user", "screen_name"),
		PageURL:  "https://weibo.com/" + httpx.S(data, "user", "idstr") + "/" + mid,
	}
	pageInfo := httpx.J(data, "page_info")
	if pageInfo != nil {
		// 视频
		if mediaInfo := httpx.J(pageInfo, "media_info"); mediaInfo != nil {
			if u := pickWeiboVideoURL(mediaInfo); u != "" {
				cover := httpx.S(pageInfo, "page_pic", "url")
				if cover == "" {
					cover = httpx.S(mediaInfo, "poster_img")
				}
				res.CoverURL = cover
				res.Medias = append(res.Medias, model.Media{Kind: "video", DirectURL: u, CoverURL: cover})
			}
		}
		// 配图
		for _, pid := range httpx.Arr(data, "pic_ids") {
			if id, ok := pid.(string); ok && id != "" {
				res.Medias = append(res.Medias, model.Media{
					Kind:      "image",
					DirectURL: "https://wx2.sinaimg.cn/large/" + id + ".jpg",
				})
			}
		}
	} else {
		for _, pid := range httpx.Arr(data, "pic_ids") {
			if id, ok := pid.(string); ok && id != "" {
				res.Medias = append(res.Medias, model.Media{
					Kind:      "image",
					DirectURL: "https://wx2.sinaimg.cn/large/" + id + ".jpg",
				})
			}
		}
	}
	if len(res.Medias) == 0 {
		return nil, fmt.Errorf("该微博没有可下载的视频或图片")
	}
	return res, nil
}

// pickWeiboVideoURL 按清晰度优先级选择视频地址。
func pickWeiboVideoURL(mediaInfo any) string {
	urls := httpx.J(mediaInfo, "urls")
	if m, ok := urls.(map[string]any); ok {
		for _, k := range []string{"mp4_hd_mp4", "mp4_720p_mp4", "stream_url_hd", "mp4_sd_mp4", "mp4_ld_mp4"} {
			if s, ok := m[k].(string); ok && s != "" {
				return s
			}
		}
	}
	for _, k := range []string{"stream_url_hd", "stream_url", "playback_url"} {
		if s := httpx.S(mediaInfo, k); s != "" {
			return s
		}
	}
	return ""
}

func stripWeiboTags(s string) string {
	s = weiboTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}
