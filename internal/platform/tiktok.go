package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

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
	tiktokRehydrationRe = regexp.MustCompile(`(?s)<script[^>]*id="__UNIVERSAL_DATA_FOR_REHYDRATION__"[^>]*>(.*?)</script>`)
)

// tiktokNoDataError 表示页面未携带 __UNIVERSAL_DATA_FOR_REHYDRATION__。
// TikTok 会间歇性返回降级页（无作品数据），重试通常可恢复，故该错误可重试。
// 携带页面诊断信息，便于从服务端日志判断降级页类型。
type tiktokNoDataError struct {
	pageLen int
	hint    string
}

func (e *tiktokNoDataError) Error() string {
	if e.hint != "" {
		return fmt.Sprintf("页面未携带作品数据（可能被反爬拦截）：页面长度=%d，%s", e.pageLen, e.hint)
	}
	return fmt.Sprintf("页面未携带作品数据（可能被反爬拦截）：页面长度=%d", e.pageLen)
}

// tiktokPageHint 从降级页内容推测页面类型。
func tiktokPageHint(s string) string {
	ls := strings.ToLower(s)
	switch {
	case strings.Contains(ls, "captcha") || strings.Contains(ls, "verify-slider") || strings.Contains(ls, "challenge"):
		return "疑似验证页"
	case strings.Contains(ls, "login"):
		return "疑似登录墙"
	case len(s) < 1000:
		return "页面过短"
	default:
		return ""
	}
}

func (p TikTok) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	// 用保持 cookie 的客户端：TikTok CDN 校验 playAddr 的 tk=tt_chain_token
	// 与 Cookie 绑定，无 Cookie 下载会 403。
	cli := httpx.NewClientWithJar()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("获取分享页失败: %w", ctx.Err())
			case <-time.After(2 * time.Second):
			}
		}
		res, err := p.parseOnce(ctx, cli, rawURL)
		if err == nil {
			return res, nil
		}
		lastErr = err
		var noData *tiktokNoDataError
		if !errors.As(err, &noData) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (p TikTok) parseOnce(ctx context.Context, cli *httpx.Client, rawURL string) (*model.Result, error) {
	finalURL, body, err := cli.Get(ctx, rawURL, map[string]string{"User-Agent": httpx.DesktopUA})
	if err != nil {
		return nil, fmt.Errorf("获取分享页失败: %w", err)
	}
	s := string(body)
	raw := FirstSubmatch(tiktokRehydrationRe, s)
	if raw == "" {
		return nil, &tiktokNoDataError{pageLen: len(s), hint: tiktokPageHint(s)}
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
