package platform

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

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
	// Next.js RSC 数据块：self.__pace_f.push([1,"..."])
	douyinRSCPushRe = regexp.MustCompile(`(?s)self\.__pace_f\.push\(\[1,\s*"((?:[^"\\]|\\.)*)"\s*\]\)`)
)

// douyinMaxAttempts 壳页面/瞬时失败时的最大尝试次数（首次 + 重试）。
const douyinMaxAttempts = 3

// douyinRetryBackoff 每次重试前的等待时长。
const douyinRetryBackoff = 1500 * time.Millisecond

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

	// 3-4. 主路径 + 兜底：抖音间歇性返回无数据的壳页面（无 videoInfoRes /
	// webId=undefined），此时自动重试；每次重试都重新拉取分享页。
	made := 0
	for made < douyinMaxAttempts {
		made++
		if made > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(douyinRetryBackoff):
			}
		}
		res, sb, err := p.tryParseOnce(ctx, cli, itemID, finalURL, s)
		if err == nil {
			p.attachSharer(ctx, rawURL, res)
			return res, nil
		}
		lastErr = err
		if sb != nil {
			shareBody = sb
		}
		if !douyinRetryable(err) {
			break
		}
	}

	// 诊断信息：用分享页 body（若有）而非短链 body；明确区分壳页面的两种失败模式
	//（无 videoInfoRes vs webId=undefined），避免"含webId=true"的误导。
	diagBody := s
	if shareBody != nil {
		diagBody = string(shareBody)
	}
	diag := fmt.Sprintf("页面长度=%d, 含_ROUTER_DATA=%v, 含videoInfoRes=%v, webId=%s",
		len(diagBody), strings.Contains(diagBody, "_ROUTER_DATA"),
		strings.Contains(diagBody, "videoInfoRes"), douyinDiagWebID(diagBody))
	if made > 1 {
		diag += fmt.Sprintf("；已自动重试%d次", made-1)
	}
	return nil, fmt.Errorf("%v；%s", lastErr, diag)
}

// tryParseOnce 尝试一次完整解析：分享页主路径（_ROUTER_DATA/RSC）+ 详情接口兜底。
// 返回 (result, sharePageBody, error)。
func (p Douyin) tryParseOnce(ctx context.Context, cli *httpx.RawClient, itemID, finalURL, shortBody string) (*model.Result, []byte, error) {
	var lastErr error
	var shareBody []byte

	// 主路径：分享页内嵌的 _ROUTER_DATA（需 Chrome TLS 指纹 + 头顺序才会返回完整数据）
	if res, body, err := p.parseSharePage(ctx, cli, itemID, finalURL); err == nil {
		return res, body, nil
	} else {
		lastErr = err
		if body != nil {
			shareBody = body
		}
	}

	// 兜底：详情接口（需分享页 HTML 中的 webId/reflow token）
	// 用分享页的 body（若主路径已拿到），否则用短链的 body
	fallbackBody := shortBody
	if shareBody != nil {
		fallbackBody = string(shareBody)
	}
	if res, err := p.parseItemInfo(ctx, cli, itemID, fallbackBody); err == nil {
		res.PageURL = finalURL
		return res, shareBody, nil
	} else {
		lastErr = err
	}
	return nil, shareBody, lastErr
}

// douyinRetryable 判断解析失败是否值得重试：壳页面、空响应、网络抖动等瞬时
// 失败返回 true；作品本身不可用等确定性失败返回 false。
func douyinRetryable(err error) bool {
	if err == nil {
		return false
	}
	// 作品不可用是确定性失败，重试无意义
	if strings.Contains(err.Error(), "作品不可用(status_code=") {
		return false
	}
	return true
}

// douyinDiagWebID 返回分享页中 webId 的诊断状态，用于区分壳页面的失败模式：
// "undefined"（壳页面未下发设备 ID）、具体数字 ID、或"缺失"。
func douyinDiagWebID(html string) string {
	if regexp.MustCompile(`webId\s*=\s*undefined`).MatchString(html) ||
		regexp.MustCompile(`"webId"\s*:\s*"undefined"`).MatchString(html) {
		return "undefined"
	}
	if id := extractDouyinWebID(html); id != "" {
		return id
	}
	return "缺失"
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
	_, body, err := cli.RawGet(ctx, pageURL, douyinChromeHeaders(httpx.AndroidUA, "Android", "?1"))
	if err != nil {
		return nil, nil, err
	}
	item, err := douyinRouterItem(body)
	if err != nil {
		// 补充通道：Next.js RSC 内嵌数据（部分降级页仍带 RSC 数据块）
		if rscItem, rscErr := douyinRSCItem(body); rscErr == nil {
			item = rscItem
		} else {
			return nil, body, err
		}
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

// douyinRSCItem 从 Next.js RSC 内嵌数据（self.__pace_f.push）中提取作品 detail。
// 作为 _ROUTER_DATA 缺失时的补充通道：部分降级页仍带 RSC 数据块，且实况照片的
// 动态视频地址（images[i].video.play_addr）多见于 RSC/SSR 直出数据。
func douyinRSCItem(body []byte) (any, error) {
	s := string(body)
	if !strings.Contains(s, "__pace_f") {
		return nil, fmt.Errorf("分享页缺少 RSC 数据")
	}
	for _, m := range douyinRSCPushRe.FindAllStringSubmatch(s, -1) {
		chunk := m[1]
		// 预过滤：只看可能含作品详情的数据块
		if !strings.Contains(chunk, "detail") {
			continue
		}
		if !strings.Contains(chunk, "awemeId") && !strings.Contains(chunk, "aweme_id") &&
			!strings.Contains(chunk, "images") {
			continue
		}
		// 还原 JS 字符串转义
		var unescaped string
		if err := json.Unmarshal([]byte(`"`+chunk+`"`), &unescaped); err != nil {
			continue
		}
		// 从第一个 { 或 [ 起配平提取 JSON
		start := strings.IndexAny(unescaped, "{[")
		if start < 0 {
			continue
		}
		raw, ok := extractBalancedJSONAny(unescaped[start:])
		if !ok {
			continue
		}
		var node any
		if err := json.Unmarshal([]byte(raw), &node); err != nil {
			continue
		}
		if d := findAwemeDetail(node); d != nil {
			return d, nil
		}
	}
	return nil, fmt.Errorf("RSC 数据中未找到作品详情")
}

// extractBalancedJSONAny 从 s 开头提取第一个配平的 JSON 对象或数组。
func extractBalancedJSONAny(s string) (string, bool) {
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return "", false
	}
	open, close := s[start], byte('}')
	if open == '[' {
		close = ']'
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
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// findAwemeDetail 在解码后的 JSON 树中递归查找作品 detail 节点。
func findAwemeDetail(node any) map[string]any {
	switch n := node.(type) {
	case map[string]any:
		if aw, ok := n["aweme"].(map[string]any); ok {
			if d, ok := aw["detail"].(map[string]any); ok && isAwemeDetail(d) {
				return d
			}
		}
		if d, ok := n["detail"].(map[string]any); ok && isAwemeDetail(d) {
			return d
		}
		if d, ok := n["aweme_detail"].(map[string]any); ok {
			return d
		}
		for _, v := range n {
			if r := findAwemeDetail(v); r != nil {
				return r
			}
		}
	case []any:
		for _, v := range n {
			if r := findAwemeDetail(v); r != nil {
				return r
			}
		}
	}
	return nil
}

func isAwemeDetail(d map[string]any) bool {
	for _, k := range []string{"awemeId", "aweme_id", "images", "video"} {
		if _, ok := d[k]; ok {
			return true
		}
	}
	return false
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
	if token == "" {
		return nil, fmt.Errorf("分享页缺少 reflow token")
	}
	if webID == "" {
		// 壳页面变体中 webId 字面量为 undefined（服务端未下发设备 ID）。
		// webId 实为客户端生成的设备标识，服务端不校验其签发来源，
		// 实测用合成的 19 位随机数同样能调通详情接口。
		webID = synthDouyinWebID()
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

// synthDouyinWebID 合成一个 19 位数字 webId（格式同真实 webId，如 7695853187903473206）。
// 用于壳页面 webId=undefined 时的兜底：服务端不校验 webId 签发来源，合成值可正常调用详情接口。
func synthDouyinWebID() string {
	b := make([]byte, 19)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	b[0] = '1' + b[0]%9
	for i := 1; i < 19; i++ {
		b[i] = '0' + b[i]%10
	}
	return string(b)
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

// douyinItemToResult 把作品 item 对象转为 Result（分享页/_ROUTER_DATA、RSC 与详情接口共用）。
// 兼容 snake_case（详情接口）与 camelCase（RSC/SSR 直出）两种字段风格。
func douyinItemToResult(item any, platformName string) (*model.Result, error) {
	m, _ := item.(map[string]any)
	if m == nil {
		return nil, fmt.Errorf("作品数据格式异常")
	}
	res := &model.Result{
		Platform: platformName,
		Title:    dyStr(m, "desc", "item_title", "itemTitle", "preview_title"),
		Author:   dyStr(httpx.J(m, "author"), "nickname"),
	}
	video := httpx.J(m, "video")
	res.CoverURL = dyFirstURL(video, "origin_cover", "originCover", "cover", "dynamic_cover", "dynamicCover")
	if res.CoverURL == "" {
		res.CoverURL = dyFirstURL(video, "coverUrlList", "cover_url_list")
	}
	images := dyArr(m, "images", "imagesList", "imgList")
	if len(images) == 0 {
		if ipi, ok := httpx.J(m, "image_post_info", "imagePostInfo").(map[string]any); ok {
			images = dyArr(ipi, "images", "imagesList")
		}
	}
	// 图集（images 非空）时顶层 video.play_addr 是 BGM 配乐地址，不是正片视频，必须跳过；
	// 否则配乐 mp3 会被误标为 kind=video 混入结果。
	if len(images) == 0 {
		// 视频：playwm → play 即为无水印地址（水印由 App 客户端合成，CDN 文件本身干净）
		for _, s := range dyCollectPlayURLs(dyPick(video, "play_addr", "playAddr")) {
			clean := douyinCleanPlayURL(s)
			res.Medias = append(res.Medias, model.Media{Kind: "video", DirectURL: clean, CoverURL: res.CoverURL})
		}
	}
	// 图集：每张图片的原图 + 实况照片的动态视频轨（images[i].video.play_addr）
	for _, img := range images {
		if u := dyFirstURL(img, "url_list", "urlList", "download_url_list", "downloadUrlList"); u != "" {
			res.Medias = append(res.Medias, model.Media{Kind: "image", DirectURL: u})
		}
		if lv := douyinLivePhotoURL(img); lv != "" {
			res.Medias = append(res.Medias, model.Media{Kind: "video", DirectURL: lv})
		}
	}
	if len(res.Medias) == 0 {
		return nil, fmt.Errorf("未找到可下载的媒体地址")
	}
	return res, nil
}

// douyinLivePhotoURL 提取单张实况照片的动态视频地址（images[i].video.play_addr）。
// 注意：只看单张图片内的 video，图集顶层的 video.play_addr 是 BGM 配乐，不得混用。
func douyinLivePhotoURL(img any) string {
	v, _ := httpx.J(img, "video").(map[string]any)
	if v == nil {
		return ""
	}
	if urls := dyCollectPlayURLs(dyPick(v, "play_addr", "playAddr")); len(urls) > 0 {
		return douyinCleanPlayURL(urls[0])
	}
	return ""
}

func douyinCleanPlayURL(s string) string {
	return strings.Replace(s, "playwm", "play", 1)
}

// dyPick 按多个候选键依次取值（兼容 snake_case 与 camelCase），返回首个非 nil 值。
func dyPick(v any, keys ...string) any {
	m, _ := v.(map[string]any)
	if m == nil {
		return nil
	}
	for _, k := range keys {
		if val, ok := m[k]; ok && val != nil {
			return val
		}
	}
	return nil
}

// dyStr 按多个候选键依次取字符串。
func dyStr(v any, keys ...string) string {
	if s, ok := dyPick(v, keys...).(string); ok {
		return s
	}
	return ""
}

// dyArr 按多个候选键依次取数组。
func dyArr(v any, keys ...string) []any {
	if a, ok := dyPick(v, keys...).([]any); ok {
		return a
	}
	return nil
}

// dyFirstURL 取多个候选键中首个非空数组的第一个 URL 字符串；
// 也兼容键值直接为 URL 字符串的写法（如 cover 为字符串）。
func dyFirstURL(v any, keys ...string) string {
	if s, ok := v.(string); ok && strings.HasPrefix(s, "http") {
		return s
	}
	m, _ := v.(map[string]any)
	if m == nil {
		return ""
	}
	for _, k := range keys {
		switch t := m[k].(type) {
		case []any:
			for _, it := range t {
				if s, ok := it.(string); ok && s != "" {
					return s
				}
			}
		case map[string]any:
			// {url_list:[...]} / {urlList:[...]} 对象写法
			for _, u := range dyArr(t, "url_list", "urlList") {
				if s, ok := u.(string); ok && s != "" {
					return s
				}
			}
		case string:
			if strings.HasPrefix(t, "http") {
				return t
			}
		}
	}
	return ""
}

// dyCollectPlayURLs 收集播放地址的全部候选 URL，兼容 {url_list:[...]} 对象
// 与 [{src|url}] 数组两种写法。
func dyCollectPlayURLs(pa any) []string {
	var out []string
	switch t := pa.(type) {
	case map[string]any:
		for _, u := range dyArr(t, "url_list", "urlList") {
			if s, ok := u.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, it := range t {
			if s := dyStr(it, "src", "url"); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
