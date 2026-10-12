package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// Bilibili 解析器：BV 号 → view 接口 → playurl 接口（fnval=0 取单段 MP4，含音轨）。
type Bilibili struct{}

func (Bilibili) Name() string  { return "bilibili" }
func (Bilibili) Label() string { return "B站" }
func (Bilibili) MatchHost(host string) bool {
	return strings.Contains(host, "bilibili.com") || strings.Contains(host, "b23.tv")
}

var bilibiliBVRe = regexp.MustCompile(`(BV[0-9A-Za-z]{10})`)

// BilibiliUA：B 站风控会对机房 IP + 浏览器 UA 的组合返回 412，
// 而中性 UA 可正常通过，故 B 站相关请求不用浏览器 UA。
const BilibiliUA = "curl/8.5.0"

func (p Bilibili) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	finalURL, _, err := httpx.Get(ctx, rawURL, map[string]string{"User-Agent": BilibiliUA})
	if err != nil {
		return nil, fmt.Errorf("获取分享页失败: %w", err)
	}
	bvid := FirstSubmatch(bilibiliBVRe, finalURL)
	if bvid == "" {
		bvid = FirstSubmatch(bilibiliBVRe, rawURL)
	}
	if bvid == "" {
		return nil, fmt.Errorf("未能从链接中提取 BV 号")
	}
	page := 1
	if u, err := url.Parse(finalURL); err == nil {
		if v := u.Query().Get("p"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				page = n
			}
		}
	}
	headers := map[string]string{
		"User-Agent": BilibiliUA,
		"Referer":    "https://www.bilibili.com",
	}
	// 1. 稿件信息
	_, viewBody, err := httpx.Get(ctx, "https://api.bilibili.com/x/web-interface/view?bvid="+bvid, headers)
	if err != nil {
		return nil, fmt.Errorf("获取稿件信息失败: %w", err)
	}
	var view any
	if err := json.Unmarshal(viewBody, &view); err != nil {
		return nil, fmt.Errorf("解析稿件信息失败: %w", err)
	}
	if code := httpx.I(view, "code"); code != 0 {
		return nil, fmt.Errorf("B站接口返回 code=%d: %s", code, httpx.S(view, "message"))
	}
	pages := httpx.Arr(view, "data", "pages")
	if page > len(pages) {
		return nil, fmt.Errorf("分 P 不存在（共 %d P）", len(pages))
	}
	cid := httpx.I(view, "data", "pages", strconv.Itoa(page-1), "cid")
	if cid == 0 {
		return nil, fmt.Errorf("未找到视频 CID")
	}
	// 2. 播放地址（fnval=0：单段 MP4，已含音轨，无需合并）
	playAPI := fmt.Sprintf("https://api.bilibili.com/x/player/playurl?bvid=%s&cid=%d&qn=80&fnval=0&fnver=0&fourk=1", bvid, cid)
	_, playBody, err := httpx.Get(ctx, playAPI, headers)
	if err != nil {
		return nil, fmt.Errorf("获取播放地址失败: %w", err)
	}
	var play any
	if err := json.Unmarshal(playBody, &play); err != nil {
		return nil, fmt.Errorf("解析播放地址失败: %w", err)
	}
	if code := httpx.I(play, "code"); code != 0 {
		return nil, fmt.Errorf("播放接口返回 code=%d: %s", code, httpx.S(play, "message"))
	}
	durl := httpx.J(play, "data", "durl", "0")
	videoURL := httpx.S(durl, "url")
	if videoURL == "" {
		return nil, fmt.Errorf("未找到可下载的视频地址（可能需要登录）")
	}
	title := httpx.S(view, "data", "title")
	if page > 1 {
		title += fmt.Sprintf(" P%d", page)
	}
	return &model.Result{
		Platform: p.Name(),
		Title:    title,
		Author:   httpx.S(view, "data", "owner", "name"),
		CoverURL: httpx.S(view, "data", "pic"),
		PageURL:  fmt.Sprintf("https://www.bilibili.com/video/%s?p=%d", bvid, page),
		Medias: []model.Media{{
			Kind:      "video",
			DirectURL: videoURL,
			CoverURL:  httpx.S(view, "data", "pic"),
		}},
	}, nil
}
