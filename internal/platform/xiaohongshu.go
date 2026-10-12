package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// Xiaohongshu 小红书解析器：笔记页 → window.__INITIAL_STATE__ → 原图 / 视频流。
// 注意：分享链接中的 xsec_token 一次性、短时有效，请用刚分享的新链接。
//
// 实现参考了 WhiteBr1ck/koishi-plugin-xhs-parser-fork 的三处关键修复：
//  1. 支持 xhslink.cn 新版短链域名；
//  2. 匿名请求优先用移动端 UA：小红书会把匿名桌面端请求重定向到登录页，
//     而移动端公开分享页仍在 __INITIAL_STATE__ 中携带笔记数据；
//  3. __INITIAL_STATE__ 是 JavaScript 而非严格 JSON，需先清洗
//     new Map([]) / undefined / NaN / Infinity 再解析。
type Xiaohongshu struct{}

func (Xiaohongshu) Name() string  { return "xiaohongshu" }
func (Xiaohongshu) Label() string { return "小红书" }
func (Xiaohongshu) MatchHost(host string) bool {
	return strings.Contains(host, "xiaohongshu.com") ||
		strings.Contains(host, "xhslink.com") ||
		strings.Contains(host, "xhslink.cn")
}

const xhsStateMarker = "window.__INITIAL_STATE__="

// xhsMobileUA 匿名移动端 UA（见包注释第 2 点）。
const xhsMobileUA = "Mozilla/5.0 (Linux; Android 15; Pixel 9 Pro) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Mobile Safari/537.36"

// xhsNewMapRe / xhsBadLiteralRe 清洗 __INITIAL_STATE__ 中的非 JSON 写法（见包注释第 3 点）。
var (
	xhsNewMapRe     = regexp.MustCompile(`\bnew\s+Map\(\s*\[\s*\]\s*\)`)
	xhsBadLiteralRe = regexp.MustCompile(`([:\[,{]\s*)(undefined|NaN|Infinity)(\s*[,}\]])`)
)

// sanitizeXHSState 把 __INITIAL_STATE__ 载荷中的 new Map([]) / undefined / NaN / Infinity
// 替换为合法 JSON 值（纯函数，便于单测）。
func sanitizeXHSState(payload string) string {
	payload = xhsNewMapRe.ReplaceAllString(payload, "{}")
	return xhsBadLiteralRe.ReplaceAllString(payload, "${1}null$3")
}

// isXHSShortLink 判断是否为小红书短链（xhslink.com / xhslink.cn）。
func isXHSShortLink(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "xhslink.com" || h == "xhslink.cn" ||
		strings.HasSuffix(h, ".xhslink.com") || strings.HasSuffix(h, ".xhslink.cn")
}

func (p Xiaohongshu) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	// 短链先手动解析一跳拿到真实笔记地址，避免自动跟随过程中的登录拦截
	pageURL := rawURL
	if isXHSShortLink(rawURL) {
		if resolved, err := httpx.Resolve(ctx, rawURL, map[string]string{"User-Agent": xhsMobileUA}); err == nil && resolved != "" {
			pageURL = resolved
		}
	}
	// 匿名请求用移动端 UA：桌面端 UA 会被重定向到登录页导致拿不到笔记数据
	finalURL, body, err := httpx.Get(ctx, pageURL, map[string]string{
		"User-Agent":      xhsMobileUA,
		"Referer":         "https://www.xiaohongshu.com/explore",
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
	if err := json.Unmarshal([]byte(sanitizeXHSState(raw)), &data); err != nil {
		return nil, fmt.Errorf("解析页面数据失败: %w", err)
	}
	// 笔记数据有两种页面结构，优先新版：
	//  - 新版分享页（discovery/item）：noteData.data.noteData
	//  - 旧版 explore 页：note.noteDetailMap[<id>].note
	note := httpx.J(data, "noteData", "data", "noteData")
	if note == nil {
		if detailMap, ok := httpx.J(data, "note", "noteDetailMap").(map[string]any); ok && len(detailMap) > 0 {
			for _, v := range detailMap {
				note = httpx.J(v, "note")
				break
			}
		}
	}
	if note == nil {
		return nil, fmt.Errorf("页面数据中未找到笔记（分享链接的 xsec_token 可能已过期，请重新分享获取新链接）")
	}
	title := httpx.S(note, "title")
	desc := httpx.S(note, "desc")
	if title == "" {
		title = TruncateRunes(desc, 60)
	}
	author := httpx.S(note, "user", "nickname")
	if author == "" {
		author = httpx.S(note, "user", "nickName") // 新版分享页用 nickName
	}
	res := &model.Result{
		Platform: platformName,
		Title:    title,
		Author:   author,
		PageURL:  pageURL,
	}
	noteType := httpx.S(note, "type") // "video" | "normal"
	if noteType == "video" {
		master := xhsOriginVideoURL(httpx.S(note, "video", "consumer", "originVideoKey"))
		if master == "" {
			master = bestXHSStreamURL(note)
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

// xhsOriginVideoURL 由 originVideoKey 构造直链：
// 已是完整 URL 则直接用，否则按 sns-video-bd.xhscdn.com 拼接。
func xhsOriginVideoURL(key string) string {
	if key == "" {
		return ""
	}
	if strings.Contains(key, "://") {
		return key
	}
	return "https://sns-video-bd.xhscdn.com/" + key
}

// bestXHSStreamURL 从 h265/h264 流中选分辨率最高的，优先用 backupUrls[0]，回退 masterUrl。
func bestXHSStreamURL(note any) string {
	var streams []any
	streams = append(streams, httpx.Arr(note, "video", "media", "stream", "h265")...)
	streams = append(streams, httpx.Arr(note, "video", "media", "stream", "h264")...)
	sort.SliceStable(streams, func(i, j int) bool {
		return xhsStreamHeight(streams[i]) < xhsStreamHeight(streams[j])
	})
	for i := len(streams) - 1; i >= 0; i-- {
		if u := httpx.S(streams[i], "backupUrls", "0"); u != "" {
			return u
		}
		if u := httpx.S(streams[i], "masterUrl"); u != "" {
			return u
		}
	}
	return ""
}

// xhsStreamHeight 取流的高度（数字或数字字符串），用于排序。
func xhsStreamHeight(stream any) int64 {
	switch h := httpx.J(stream, "height").(type) {
	case float64:
		return int64(h)
	case string:
		var n int64
		_, _ = fmt.Sscanf(h, "%d", &n)
		return n
	}
	return 0
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
