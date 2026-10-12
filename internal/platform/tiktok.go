package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// TikTok 解析器：从页面 __UNIVERSAL_DATA_FOR_REHYDRATION__ JSON 中取 playAddr（网页端为无水印 H.264）。
type TikTok struct{}

func (TikTok) Name() string  { return "tiktok" }
func (TikTok) Label() string { return "TikTok" }
func (TikTok) MatchHost(host string) bool {
	return strings.Contains(host, "tiktok.com")
}

var (
	tiktokIDRe          = regexp.MustCompile(`/(?:video|photo)/(\d{5,})`)
	tiktokRehydrationRe = regexp.MustCompile(`(?s)<script id="__UNIVERSAL_DATA_FOR_REHYDRATION__" type="application/json">(.*?)</script>`)
)

func (p TikTok) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	// 用保持 cookie 的客户端：TikTok CDN 校验 playAddr 的 tk=tt_chain_token
	// 与 Cookie 绑定，无 Cookie 下载会 403。
	cli := httpx.NewClientWithJar()
	finalURL, body, err := cli.Get(ctx, rawURL, map[string]string{"User-Agent": httpx.DesktopUA})
	if err != nil {
		return nil, fmt.Errorf("获取分享页失败: %w", err)
	}
	s := string(body)
	raw := FirstSubmatch(tiktokRehydrationRe, s)
	if raw == "" {
		return nil, fmt.Errorf("页面未携带作品数据（可能被反爬拦截）")
	}
	var data any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("解析页面数据失败: %w", err)
	}
	item := httpx.J(data, "__DEFAULT_SCOPE__", "webapp.video-detail", "itemInfo", "itemStruct")
	if item == nil {
		item = httpx.J(data, "__DEFAULT_SCOPE__", "webapp.photo-detail", "itemInfo", "itemStruct")
	}
	if item == nil {
		return nil, fmt.Errorf("页面数据中未找到作品信息")
	}
	res := &model.Result{
		Platform: p.Name(),
		Title:    httpx.S(item, "desc"),
		Author:   httpx.S(item, "author", "uniqueId"),
		PageURL:  finalURL,
	}
	video := httpx.J(item, "video")
	if playAddr := httpx.UnquoteJSONString(httpx.S(video, "playAddr")); playAddr != "" {
		cover := httpx.UnquoteJSONString(httpx.S(video, "dynamicCover"))
		if cover == "" {
			cover = httpx.UnquoteJSONString(httpx.S(video, "cover"))
		}
		res.CoverURL = cover
		res.Medias = append(res.Medias, model.Media{
			Kind:      "video",
			DirectURL: playAddr,
			CoverURL:  cover,
			Cookie:    httpx.CookieHeader(cli.Jar, playAddr),
		})
	}
	// 图文帖
	for _, img := range httpx.Arr(item, "imagePost", "images") {
		urls := httpx.Arr(img, "imageURL", "urlList")
		if len(urls) == 0 {
			continue
		}
		if u, ok := urls[0].(string); ok && u != "" {
			res.Medias = append(res.Medias, model.Media{Kind: "image", DirectURL: httpx.UnquoteJSONString(u)})
		}
	}
	if len(res.Medias) == 0 {
		return nil, fmt.Errorf("未找到可下载的媒体地址")
	}
	return res, nil
}
