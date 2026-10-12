package platform

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// Douyin 抖音解析器：短链 → 作品 ID → 详情接口 → CDN 地址（playwm → play 去水印）。
type Douyin struct{}

func (Douyin) Name() string  { return "douyin" }
func (Douyin) Label() string { return "抖音" }
func (Douyin) MatchHost(host string) bool {
	return strings.Contains(host, "douyin.com") || strings.Contains(host, "iesdouyin.com")
}

var (
	douyinPathIDRe  = regexp.MustCompile(`/(?:video|note)/(\d{6,})`)
	douyinJSONIDRe  = regexp.MustCompile(`"itemId"\s*:\s*"(\d{6,})"`)
	douyinVideoIDRe = regexp.MustCompile(`"video_id"\s*:\s*"([0-9a-fA-F]{32})"`)
)

func (p Douyin) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	// 全程共用一个 RawClient，保持 cookie；头按 Chrome 顺序发出（抖音检测头顺序）
	jar, _ := cookiejar.New(nil)
	cli := httpx.NewRawClient(jar)

	// 1. 跟随短链跳转拿到最终页面（短链只需 UA，避免多余头干扰）
	finalURL, body, err := cli.RawGet(ctx, rawURL, []httpx.OrderedHeader{
		{"User-Agent", httpx.DesktopUA},
	})
	if err != nil {
		return nil, fmt.Errorf("获取分享页失败: %w", err)
	}
	s := string(body)

	// 2. 提取作品 ID
	itemID := FirstSubmatch(douyinPathIDRe, finalURL)
	if itemID == "" {
		itemID = FirstSubmatch(douyinJSONIDRe, s)
	}
	if itemID == "" {
		itemID = FirstSubmatch(douyinPathIDRe, s)
	}
	if itemID == "" {
		return nil, fmt.Errorf("未能从分享链接中提取作品 ID")
	}

	var lastErr error
	var shareBody []byte

	// 3. 主路径：分享页内嵌的 _ROUTER_DATA（需 Chrome TLS 指纹 + 头顺序才会返回完整数据）
	if res, body, err := p.parseSharePage(ctx, cli, itemID, finalURL); err == nil {
		p.attachSharer(ctx, rawURL, res)
		return res, nil
	} else {
		lastErr = err
		if body != nil {
			shareBody = body
		}
	}

	// 4. 兜底：详情接口（需分享页 HTML 中的 webId/reflow token）
	// 用分享页的 body（若主路径已拿到），否则用短链的 body
	fallbackBody := s
	if shareBody != nil {
		fallbackBody = string(shareBody)
	}
	if res, err := p.parseItemInfo(ctx, cli, itemID, fallbackBody); err == nil {
		res.PageURL = finalURL
		p.attachSharer(ctx, rawURL, res)
		return res, nil
	} else {
		lastErr = err
	}

	// 诊断信息：用分享页 body（若有）而非短链 body
	diagBody := s
	if shareBody != nil {
		diagBody = string(shareBody)
	}
	diag := fmt.Sprintf("页面长度=%d, 含_ROUTER_DATA=%v, 含webId=%v",
		len(diagBody), strings.Contains(diagBody, "_ROUTER_DATA"), strings.Contains(diagBody, "webId"))
	return nil, fmt.Errorf("%v；%s；可尝试更换出口 IP", lastErr, diag)
}

// attachSharer 尽力解析分享链接背后的分享者身份并挂到结果上。
// 附加功能：失败时静默跳过，不影响媒体解析主流程。
func (p Douyin) attachSharer(ctx context.Context, rawURL string, res *model.Result) {
	if sr, err := p.ResolveSharer(ctx, rawURL); err == nil && sr != nil && sr.Sharer != nil {
		res.Sharer = sr.Sharer
	}
}

// douyinChromeHeaders 构造 Chrome 顺序的请求头（按实测成功的组合）。
// 注意：不含 Upgrade:h2c 等头 —— 实测带上反而拿不到数据。
func douyinChromeHeaders(ua, platform, mobile string) []httpx.OrderedHeader {
	return []httpx.OrderedHeader{
		{"sec-ch-ua", `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`},
		{"sec-ch-ua-mobile", mobile},
		{"sec-ch-ua-platform", `"` + platform + `"`},
		{"Upgrade-Insecure-Requests", "1"},
		{"User-Agent", ua},
		{"Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		{"Sec-Fetch-Site", "none"},
		{"Sec-Fetch-Mode", "navigate"},
		{"Sec-Fetch-Dest", "document"},
		{"Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8"},
	}
}

// parseSharePage 拉取分享页并解析内嵌 _ROUTER_DATA 中的 videoInfoRes。
// 返回 (result, sharePageBody, error)；即使解析失败也返回 body 供兜底路径使用。
func (p Douyin) parseSharePage(ctx context.Context, cli *httpx.RawClient, itemID, finalURL string) (*model.Result, []byte, error) {
	pageURL := "https://www.douyin.com/share/video/" + itemID + "/"
	_, body, err := cli.RawGet(ctx, pageURL, douyinChromeHeaders(httpx.AndroidUA, "macOS", "?1"))
	if err != nil {
		return nil, nil, err
	}
	item, err := douyinRouterItem(body)
	if err != nil {
		return nil, body, err
	}
	res, err := douyinItemToResult(item, p.Name())
	if err != nil {
		return nil, body, err
	}
	res.PageURL = finalURL
	return res, body, nil
}

// douyinRouterItem 从分享页 HTML 的 window._ROUTER_DATA 中提取 videoInfoRes.item_list[0]。
func douyinRouterItem(body []byte) (any, error) {
	s := string(body)
	idx := strings.Index(s, "window._ROUTER_DATA")
	if idx < 0 {
		return nil, fmt.Errorf("分享页缺少 _ROUTER_DATA")
	}
	eq := strings.Index(s[idx:], "=")
	if eq < 0 {
		return nil, fmt.Errorf("分享页 _ROUTER_DATA 格式异常")
	}
	raw, ok := extractBalancedJSON(s[idx+eq+1:])
	if !ok {
		return nil, fmt.Errorf("分享页 _ROUTER_DATA 解析失败")
	}
	var data any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("分享页 _ROUTER_DATA 非法 JSON: %w", err)
	}
	loader := httpx.J(data, "loaderData")
	if loader == nil {
		return nil, fmt.Errorf("分享页缺少 loaderData")
	}
	m, ok := loader.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("分享页 loaderData 格式异常")
	}
	for route, v := range m {
		if !strings.HasSuffix(route, "/page") {
			continue
		}
		vir := httpx.J(v, "videoInfoRes")
		if vir == nil {
			continue
		}
		if code := httpx.I(vir, "status_code"); code != 0 {
			return nil, fmt.Errorf("作品不可用(status_code=%d)", code)
		}
		items := httpx.Arr(vir, "item_list")
		if len(items) == 0 {
			return nil, fmt.Errorf("分享页未返回作品数据（可能被平台限制）")
		}
		return items[0], nil
	}
	return nil, fmt.Errorf("分享页缺少 videoInfoRes")
}

// extractBalancedJSON 从 s 开头提取第一个配平的 JSON 对象。
func extractBalancedJSON(s string) (string, bool) {
	start := strings.Index(s, "{")
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

func (p Douyin) parseItemInfo(ctx context.Context, cli *httpx.RawClient, itemID, shareHTML string) (*model.Result, error) {
	// 从分享页提取 webId 与 xsstoken（reflow_token），AES-CBC 构造 reflow_id。
	webID := extractDouyinWebID(shareHTML)
	token := extractDouyinReflowToken(shareHTML)
	if webID == "" || token == "" {
		return nil, fmt.Errorf("分享页缺少 webId 或 reflow token")
	}
	reflowID, err := encryptReflowToken(token, webID)
	if err != nil {
		return nil, err
	}
	params := url.Values{
		"reflow_source": {"reflow_page"},
		"web_id":        {webID},
		"device_id":     {webID},
		"item_ids":      {itemID},
		"reflow_id":     {reflowID},
	}
	api := "https://www.douyin.com/web/api/v2/aweme/iteminfo/?" + params.Encode()
	_, body, err := cli.RawGet(ctx, api, []httpx.OrderedHeader{
		{"User-Agent", httpx.DesktopUA},
		{"Referer", "https://www.douyin.com/share/video/" + itemID + "/"},
		{"Accept", "application/json, text/plain, */*"},
	})
	if err != nil {
		return nil, err
	}
	return parseDouyinItemInfo(body, p.Name())
}

// extractDouyinWebID 从分享页 HTML 提取 webId（兼容 JSON 内嵌与元素属性两种写法）。
func extractDouyinWebID(html string) string {
	// 元素属性（无引号）：<div id=douyin_reflow_webId webId=...>
	if m := regexp.MustCompile(`id=douyin_reflow_webId\s+webId=([^\s>]+)`).FindStringSubmatch(html); m != nil {
		if isValidWebID(m[1]) {
			return m[1]
		}
	}
	// 元素属性（有引号）
	if m := regexp.MustCompile(`id=["']douyin_reflow_webId["'][^>]*?\bwebId=["']([^"']+)["']`).FindStringSubmatch(html); m != nil {
		if isValidWebID(m[1]) {
			return m[1]
		}
	}
	// JSON 内嵌
	if m := regexp.MustCompile(`"webId"\s*:\s*"(\d+)"`).FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

// isValidWebID 校验 webId 是否有效（排除 undefined 等）。
func isValidWebID(s string) bool {
	if s == "" || s == "undefined" || s == "null" {
		return false
	}
	// webId 通常是 19 位数字
	if len(s) < 10 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// extractDouyinReflowToken 从分享页 HTML 提取 xsstoken（reflow_token）。
func extractDouyinReflowToken(html string) string {
	// 元素属性（无引号）：<div id=douyin_reflow_token xsstoken=...>
	if m := regexp.MustCompile(`id=douyin_reflow_token\s+xsstoken=([^\s>]+)`).FindStringSubmatch(html); m != nil {
		return strings.Trim(m[1], `"'`)
	}
	// 元素属性（有引号）
	if m := regexp.MustCompile(`id=["']douyin_reflow_token["'][^>]*?\bxsstoken=["']([^"']+)["']`).FindStringSubmatch(html); m != nil {
		return m[1]
	}
	// JSON 内嵌
	if m := regexp.MustCompile(`"xsstoken"\s*:\s*"([^"]+)"`).FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

// encryptReflowToken 用 webId 前 16 字节做 key/iv，AES-CBC 加密 token（PKCS7 填充）。
func encryptReflowToken(token, webID string) (string, error) {
	if len(webID) < 16 {
		return "", fmt.Errorf("webId 无效")
	}
	key := []byte(webID[:16])
	plain := []byte(token)
	pad := 16 - len(plain)%16
	plain = append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	enc := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(enc, plain)
	return base64.StdEncoding.EncodeToString(enc), nil
}

// parseDouyinItemInfo 解析详情接口 JSON（纯函数，便于单测）。
func parseDouyinItemInfo(body []byte, platformName string) (*model.Result, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("详情接口返回空响应（平台可能限制了当前网络）")
	}
	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("解析详情接口失败: %w", err)
	}
	if code := httpx.I(data, "status_code"); code != 0 {
		return nil, fmt.Errorf("详情接口返回 status_code=%d", code)
	}
	item := httpx.J(data, "item_list", "0")
	if item == nil {
		return nil, fmt.Errorf("详情接口未返回作品数据")
	}
	return douyinItemToResult(item, platformName)
}

// douyinItemToResult 把作品 item 对象转为 Result（分享页/_ROUTER_DATA 与详情接口共用）。
func douyinItemToResult(item any, platformName string) (*model.Result, error) {
	res := &model.Result{
		Platform: platformName,
		Title:    httpx.S(item, "desc"),
		Author:   httpx.S(item, "author", "nickname"),
	}
	video := httpx.J(item, "video")
	res.CoverURL = httpx.S(video, "cover", "url_list", "0")
	images := httpx.Arr(item, "images")
	// 图集（images 非空）时 video.play_addr 是 BGM 配乐地址，不是正片视频，必须跳过；
	// 否则配乐 mp3 会被误标为 kind=video 混入结果。
	if len(images) == 0 {
		// 视频：playwm → play 即为无水印地址（水印由 App 客户端合成，CDN 文件本身干净）
		for _, u := range httpx.Arr(video, "play_addr", "url_list") {
			if s, ok := u.(string); ok && s != "" {
				clean := strings.Replace(s, "playwm", "play", 1)
				res.Medias = append(res.Medias, model.Media{Kind: "video", DirectURL: clean, CoverURL: res.CoverURL})
			}
		}
	}
	// 图集
	for _, img := range images {
		urls := httpx.Arr(img, "url_list")
		if len(urls) == 0 {
			continue
		}
		if s, ok := urls[0].(string); ok && s != "" {
			res.Medias = append(res.Medias, model.Media{Kind: "image", DirectURL: s})
		}
	}
	if len(res.Medias) == 0 {
		return nil, fmt.Errorf("未找到可下载的媒体地址")
	}
	return res, nil
}
