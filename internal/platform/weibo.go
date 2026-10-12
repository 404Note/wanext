package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// Weibo 微博解析器：分享链接 → MID → ajax/statuses/show 接口 → 视频流 / 原图。
//
// 媒体解析参考 belingud/weibo-downloader-skill：
//  1. 访客凭证：新浪访客系统换取 SUB/SUBP（见 weibo_visitor.go），匿名请求常被拦截；
//  2. fx 分享链接（mapp.api.weibo.cn/fx/）先解析出真实链接；
//  3. 新版 mix_media_info 与旧版 pic_ids/pic_infos 双格式解析；
//  4. 视频清晰度三级降级：mp4_hd_url → mp4_720p_mp4 → stream_url。
type Weibo struct{}

func (Weibo) Name() string  { return "weibo" }
func (Weibo) Label() string { return "微博" }
func (Weibo) MatchHost(host string) bool {
	return strings.Contains(host, "weibo.com") || strings.Contains(host, "weibo.cn")
}

var (
	// weiboStatusIDRe 匹配各类微博链接中的状态 ID（base62 或纯数字）：
	// weibo.com/<uid>/<id>、m.weibo.cn/status/<id>、m.weibo.cn/detail/<id>、weibo.com/detail/<id>
	weiboStatusIDRe  = regexp.MustCompile(`(?:weibo\.com/\d+/|m\.weibo\.cn/(?:status|detail)/|weibo\.(?:com|cn)/detail/)([0-9A-Za-z]+)`)
	weiboReturnURLRe = regexp.MustCompile(`return_url\s*=\s*"([^"]+)"`)
	weiboTagRe       = regexp.MustCompile(`<[^>]+>`)
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

// extractWeiboMID 从链接中提取数字 mid。
// 纯数字 ID 直接使用；含字母的 base62 ID 解码为数字（纯函数，便于单测）。
func extractWeiboMID(rawURL string) string {
	m := weiboStatusIDRe.FindStringSubmatch(rawURL)
	if len(m) != 2 {
		return ""
	}
	if isDigits(m[1]) {
		return m[1]
	}
	if n := Base62Decode(m[1]); n > 0 {
		return strconv.FormatInt(n, 10)
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// resolveWeiboShareLink 解析 fx 分享链接（mapp.api.weibo.cn/fx/）：
// 手动跟一跳取 Location；无跳转时回退到页面 HTML 中的 return_url。
func resolveWeiboShareLink(ctx context.Context, rawURL string) string {
	if !strings.Contains(rawURL, "mapp.api.weibo.cn/fx/") {
		return rawURL
	}
	if loc, err := httpx.Resolve(ctx, rawURL, map[string]string{"User-Agent": weiboUA}); err == nil && loc != "" && loc != rawURL {
		return loc
	}
	if _, body, err := httpx.Get(ctx, rawURL, map[string]string{"User-Agent": weiboUA}); err == nil {
		if m := weiboReturnURLRe.FindStringSubmatch(string(body)); m != nil {
			if u, err := url.Parse(m[1]); err == nil && u.IsAbs() {
				return u.String()
			}
		}
	}
	return rawURL
}

func (p Weibo) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	rawURL = resolveWeiboShareLink(ctx, rawURL)
	mid := extractWeiboMID(rawURL)
	if mid == "" {
		return nil, fmt.Errorf("未能从链接中提取微博 ID")
	}
	api := "https://weibo.com/ajax/statuses/show?id=" + mid + "&isGetLongText=true"
	_, body, err := weiboAuthedClient(ctx).Get(ctx, api, map[string]string{
		"User-Agent": weiboUA,
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
		msg := httpx.S(data, "msg")
		if msg == "" {
			msg = httpx.S(data, "message")
		}
		if msg == "" {
			msg = "未知错误"
		}
		return nil, fmt.Errorf("微博接口返回异常: %s", msg)
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
	seen := map[string]bool{}
	addMedia := func(kind, u, cover string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		res.Medias = append(res.Medias, model.Media{Kind: kind, DirectURL: u, CoverURL: cover})
		if kind == "video" && res.CoverURL == "" && cover != "" {
			res.CoverURL = cover
		}
	}

	// 1. 新版 mix_media_info（图文视频混排）
	for _, item := range httpx.Arr(data, "mix_media_info", "items") {
		d := httpx.J(item, "data")
		switch httpx.S(item, "type") {
		case "pic":
			addMedia("image", httpx.S(d, "largest", "url"), "")
		case "video":
			mi := httpx.J(d, "media_info")
			addMedia("video", pickWeiboVideoURL(mi), httpx.S(mi, "poster_img"))
		}
	}

	// 2. 旧版 pic_ids + pic_infos（mix 为空时兜底）
	if len(res.Medias) == 0 {
		picInfos := httpx.J(data, "pic_infos")
		for _, pid := range httpx.Arr(data, "pic_ids") {
			id, _ := pid.(string)
			if id == "" {
				continue
			}
			info := httpx.J(picInfos, id)
			// gif 动图：pic.video 是视频地址
			if httpx.S(info, "type") == "gif" {
				if u := httpx.S(info, "video"); u != "" {
					addMedia("video", u, "")
					continue
				}
			}
			u := httpx.S(info, "largest", "url")
			if u == "" {
				u = "https://wx2.sinaimg.cn/large/" + id + ".jpg"
			}
			addMedia("image", u, "")
		}
	}

	// 3. 独立 page_info 视频（去重）
	if pageInfo := httpx.J(data, "page_info"); pageInfo != nil {
		mi := httpx.J(pageInfo, "media_info")
		cover := httpx.S(pageInfo, "page_pic", "url")
		if cover == "" {
			cover = httpx.S(mi, "poster_img")
		}
		addMedia("video", pickWeiboVideoURL(mi), cover)
	}

	if len(res.Medias) == 0 {
		return nil, fmt.Errorf("该微博没有可下载的视频或图片")
	}
	return res, nil
}

// pickWeiboVideoURL 按清晰度优先级选择视频地址。
// 新版接口的清晰度键直接挂在 media_info 上（mp4_hd_url → mp4_720p_mp4 → stream_url），
// urls 子对象为空；旧版则在 urls 下。两种都兼容。
func pickWeiboVideoURL(mediaInfo any) string {
	if mediaInfo == nil {
		return ""
	}
	for _, k := range []string{"mp4_hd_url", "mp4_720p_mp4", "stream_url_hd", "stream_url"} {
		if s := httpx.S(mediaInfo, k); s != "" {
			return s
		}
	}
	if urls := httpx.J(mediaInfo, "urls"); urls != nil {
		for _, k := range []string{"mp4_hd_mp4", "mp4_720p_mp4", "stream_url_hd", "mp4_sd_mp4", "mp4_ld_mp4"} {
			if s := httpx.S(urls, k); s != "" {
				return s
			}
		}
	}
	for _, k := range []string{"playback_url", "mp4_sd_url", "h265_mp4_hd"} {
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
