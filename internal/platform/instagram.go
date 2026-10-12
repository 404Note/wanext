package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// Instagram 解析器：三级降级策略（参考 qinuan01/video-parser）。
// Tier 1: 公开页面 JSON → 递归搜索媒体对象
// Tier 2: GraphQL（需 LSD/CSRF token + Session Cookie）
// Tier 3: Open Graph meta 标签
//
// 注意：Instagram 对 TLS 指纹敏感，需配合 httpx 的 Chrome 指纹使用。
type Instagram struct{}

func (Instagram) Name() string  { return "instagram" }
func (Instagram) Label() string { return "Instagram" }
func (Instagram) MatchHost(host string) bool {
	return strings.Contains(host, "instagram.com")
}

const (
	igAppID          = "936619743392459"
	igPostDocID      = "27130156389949648"
	igPostFriendly   = "PolarisLoggedOutDesktopWWWPostRootContentQuery"
	igShortcodeAlpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	igUserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

var (
	igCodeRe  = regexp.MustCompile(`/(?:reel|reels|p|tv)/([A-Za-z0-9_-]{5,})/?`)
	igLSDRe   = regexp.MustCompile(`"LSD"[^}]*?"token"\s*:\s*"([^"]+)"`)
	igLSDRe2  = regexp.MustCompile(`\["LSD",\[\],\{"token":"([^"]+)"\}`)
	igOGVideo = regexp.MustCompile(`<meta[^>]+property="og:video(?::secure_url)?"[^>]+content="([^"]+)"`)
	igOGImage = regexp.MustCompile(`<meta[^>]+property="og:image(?::secure_url)?"[^>]+content="([^"]+)"`)
)

// shortcodeToMediaID 将 Instagram shortcode 转换为数字 media ID。
func shortcodeToMediaID(shortcode string) string {
	var n uint64
	for _, c := range shortcode {
		idx := strings.IndexRune(igShortcodeAlpha, c)
		if idx < 0 {
			return ""
		}
		n = n*64 + uint64(idx)
	}
	return fmt.Sprintf("%d", n)
}

// igSession 封装带 Cookie Jar 的 HTTP 会话，对应 Python 的 curl_cffi Session。
type igSession struct {
	client *httpx.Client
}

func newIGSession() *igSession {
	return &igSession{client: httpx.NewClientWithJar()}
}

// igMedia 统一的媒体提取结果。
type igMedia struct {
	videoURLs []string
	imageURLs []string
	username  string
	caption   string
}

func (p Instagram) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	code := FirstSubmatch(igCodeRe, rawURL)
	if code == "" {
		return nil, fmt.Errorf("未能从链接中提取帖子 ID")
	}
	mediaID := shortcodeToMediaID(code)
	if mediaID == "" {
		return nil, fmt.Errorf("shortcode 转 media ID 失败")
	}
	pageURL := "https://www.instagram.com/reel/" + code + "/"

	sess := newIGSession()

	// Tier 1: 页面 JSON
	if m, err := p.parsePage(ctx, sess, pageURL, code); err == nil && m != nil && (len(m.videoURLs) > 0 || len(m.imageURLs) > 0) {
		return p.toResult(code, pageURL, m), nil
	}

	// Tier 2: GraphQL
	if m, err := p.parseGraphQL(ctx, sess, pageURL, code, mediaID); err == nil && m != nil && (len(m.videoURLs) > 0 || len(m.imageURLs) > 0) {
		return p.toResult(code, pageURL, m), nil
	}

	// Tier 3: OG Meta（在 Tier 1 抓到的 HTML 里找）
	if m := p.parseOGMeta(ctx, sess, pageURL); m != nil && (len(m.videoURLs) > 0 || len(m.imageURLs) > 0) {
		return p.toResult(code, pageURL, m), nil
	}

	return nil, fmt.Errorf("未找到可下载的媒体（可能需要登录或被限流）")
}

func (p Instagram) toResult(code, pageURL string, m *igMedia) *model.Result {
	res := &model.Result{
		Platform: p.Name(),
		Title:    m.caption,
		Author:   m.username,
		PageURL:  pageURL,
	}
	if res.Title == "" {
		res.Title = "Instagram 帖子 " + code
	}
	for _, u := range m.videoURLs {
		res.Medias = append(res.Medias, model.Media{Kind: "video", DirectURL: u})
	}
	for _, u := range m.imageURLs {
		res.Medias = append(res.Medias, model.Media{Kind: "image", DirectURL: u})
	}
	return res
}

// parsePage Tier 1：抓取公开页面，解析 JSON scripts，递归搜索媒体对象。
func (p Instagram) parsePage(ctx context.Context, sess *igSession, pageURL, shortcode string) (*igMedia, error) {
	body, err := p.get(ctx, sess, pageURL, map[string]string{
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.9",
		"Cache-Control":   "no-cache",
		"Pragma":          "no-cache",
		"User-Agent":      igUserAgent,
	})
	if err != nil {
		return nil, err
	}
	s := string(body)

	// 提取所有 type="application/json" 的 script 内容
	payloads := extractJSONPayloads(s)
	for _, payload := range payloads {
		if media := findMediaByShortcode(payload, shortcode); media != nil {
			return mediaToIGMedia(media), nil
		}
	}
	return nil, fmt.Errorf("页面 JSON 中未找到媒体对象")
}

// parseGraphQL Tier 2：GraphQL 回退。
func (p Instagram) parseGraphQL(ctx context.Context, sess *igSession, pageURL, shortcode, mediaID string) (*igMedia, error) {
	// 先拿页面提取 LSD 和 CSRF
	body, err := p.get(ctx, sess, pageURL, map[string]string{
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.9",
		"User-Agent":      igUserAgent,
	})
	if err != nil {
		return nil, err
	}
	s := string(body)
	payloads := extractJSONPayloads(s)

	lsd := extractLSD(payloads, s)
	csrf := getCSRFToken(sess.client)

	// 如果缺 token，尝试请求首页补齐
	if lsd == "" || csrf == "" {
		if b2, err2 := p.get(ctx, sess, "https://www.instagram.com/", map[string]string{
			"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
			"Accept-Language": "en-US,en;q=0.9",
			"User-Agent":      igUserAgent,
		}); err2 == nil {
			s2 := string(b2)
			p2 := extractJSONPayloads(s2)
			if lsd == "" {
				lsd = extractLSD(p2, s2)
			}
			if csrf == "" {
				csrf = getCSRFToken(sess.client)
			}
		}
	}
	if lsd == "" || csrf == "" {
		return nil, fmt.Errorf("未能获取 LSD/CSRF token")
	}

	// 内容访问检查（logged-out content gate）
	commonHeaders := map[string]string{
		"Accept":       "*/*",
		"Origin":       "https://www.instagram.com",
		"User-Agent":   igUserAgent,
		"X-ASBD-ID":    "359341",
		"X-IG-App-ID":  igAppID,
		"X-IG-WWW-Claim": "0",
	}
	_, _ = p.get(ctx, sess, "https://i.instagram.com/api/v1/web/get_ruling_for_content/?content_type=MEDIA&target_id="+mediaID, commonHeaders)

	// GraphQL POST
	form := url.Values{}
	form.Set("lsd", lsd)
	form.Set("fb_api_caller_class", "RelayModern")
	form.Set("fb_api_req_friendly_name", igPostFriendly)
	form.Set("server_timestamps", "true")
	vars, _ := json.Marshal(map[string]string{"media_id": mediaID})
	form.Set("variables", string(vars))
	form.Set("doc_id", igPostDocID)

	gqlHeaders := map[string]string{
		"Accept":            "*/*",
		"Accept-Language":   "en-US,en;q=0.9",
		"Content-Type":      "application/x-www-form-urlencoded",
		"Origin":            "https://www.instagram.com",
		"Referer":           pageURL,
		"User-Agent":        igUserAgent,
		"X-ASBD-ID":         "359341",
		"X-CSRFToken":       csrf,
		"X-FB-Friendly-Name": igPostFriendly,
		"X-FB-LSD":          lsd,
		"X-IG-App-ID":       igAppID,
		"X-IG-WWW-Claim":    "0",
		"X-Requested-With":  "XMLHttpRequest",
	}

	respBody, err := p.post(ctx, sess, "https://www.instagram.com/api/graphql", form.Encode(), gqlHeaders)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(respBody))
	text = strings.TrimPrefix(text, "for (;;);")
	var gqlResp map[string]any
	if err := json.Unmarshal([]byte(text), &gqlResp); err != nil {
		return nil, fmt.Errorf("GraphQL 响应解析失败: %w", err)
	}
	if media := findMediaByShortcode(gqlResp, shortcode); media != nil {
		return mediaToIGMedia(media), nil
	}
	return nil, fmt.Errorf("GraphQL 未返回媒体对象")
}

// parseOGMeta Tier 3：Open Graph meta 标签降级。
func (p Instagram) parseOGMeta(ctx context.Context, sess *igSession, pageURL string) *igMedia {
	body, err := p.get(ctx, sess, pageURL, map[string]string{
		"User-Agent": igUserAgent,
	})
	if err != nil {
		return nil
	}
	s := string(body)
	m := &igMedia{}
	seen := map[string]bool{}
	for _, mm := range igOGVideo.FindAllStringSubmatch(s, -1) {
		u := mm[1]
		if u != "" && !seen[u] {
			seen[u] = true
			m.videoURLs = append(m.videoURLs, u)
		}
	}
	for _, mm := range igOGImage.FindAllStringSubmatch(s, -1) {
		u := mm[1]
		if u != "" && !seen[u] && !strings.Contains(u, "profile_pic") {
			seen[u] = true
			m.imageURLs = append(m.imageURLs, u)
		}
	}
	if len(m.videoURLs) == 0 && len(m.imageURLs) == 0 {
		return nil
	}
	return m
}

// get 执行 GET 请求（带 ctx）。
func (p Instagram) get(ctx context.Context, sess *igSession, urlStr string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := sess.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 5<<20))
}

// post 执行 POST 表单请求。
func (p Instagram) post(ctx context.Context, sess *igSession, urlStr, formBody string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", urlStr, strings.NewReader(formBody))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := sess.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 5<<20))
}

// extractJSONPayloads 提取 HTML 中所有 type="application/json" 的 script 内容并解析。
func extractJSONPayloads(html string) []any {
	var out []any
	// 简化：用正则找 script 标签
	re := regexp.MustCompile(`<script[^>]+type="application/json"[^>]*>(.*?)</script>`)
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		content := strings.TrimSpace(m[1])
		if content == "" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(content), &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// findMediaByShortcode 递归搜索匹配 shortcode 的媒体对象。
func findMediaByShortcode(v any, shortcode string) map[string]any {
	var best map[string]any
	bestScore := 0

	var walk func(x any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			// 检查是否匹配 shortcode
			code, _ := t["code"].(string)
			sc, _ := t["shortcode"].(string)
			if code == shortcode || sc == shortcode {
				score := 0
				if _, ok := t["video_versions"]; ok {
					score += 80
				}
				if _, ok := t["image_versions2"]; ok {
					score += 40
				}
				if _, ok := t["carousel_media"]; ok {
					score += 60
				}
				if v, ok := t["video_url"].(string); ok && v != "" {
					score += 100
				}
				if score > bestScore {
					bestScore = score
					best = t
				}
			}
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(v)
	return best
}

// mediaToIGMedia 将找到的媒体对象转换为统一结构。
func mediaToIGMedia(media map[string]any) *igMedia {
	m := &igMedia{}
	if u, ok := media["owner"].(map[string]any); ok {
		if name, ok := u["username"].(string); ok {
			m.username = name
		}
	}
	if user, ok := media["user"].(map[string]any); ok {
		if name, ok := user["username"].(string); ok && m.username == "" {
			m.username = name
		}
	}
	// caption
	if ec, ok := media["edge_media_to_caption"].(map[string]any); ok {
		if edges, ok := ec["edges"].([]any); ok && len(edges) > 0 {
			if node, ok := edges[0].(map[string]any)["node"].(map[string]any); ok {
				m.caption, _ = node["text"].(string)
			}
		}
	}
	if cap, ok := media["caption"].(map[string]any); ok {
		m.caption, _ = cap["text"].(string)
	}

	// 收集视频和图片（处理轮播）
	items := []map[string]any{media}
	if carousel, ok := media["carousel_media"].([]any); ok {
		for _, c := range carousel {
			if cm, ok := c.(map[string]any); ok {
				items = append(items, cm)
			}
		}
	}
	seen := map[string]bool{}
	for _, item := range items {
		// video_versions
		if vv, ok := item["video_versions"].([]any); ok {
			for _, v := range vv {
				if vm, ok := v.(map[string]any); ok {
					if u, ok := vm["url"].(string); ok && u != "" && !seen[u] {
						seen[u] = true
						m.videoURLs = append(m.videoURLs, u)
						break // 每个 item 取第一个（最高清晰度通常在前）
					}
				}
			}
		}
		if v, ok := item["video_url"].(string); ok && v != "" && !seen[v] {
			seen[v] = true
			m.videoURLs = append(m.videoURLs, v)
		}
		// image_versions2
		if iv, ok := item["image_versions2"].(map[string]any); ok {
			if cands, ok := iv["candidates"].([]any); ok {
				for _, c := range cands {
					if cm, ok := c.(map[string]any); ok {
						if u, ok := cm["url"].(string); ok && u != "" && !seen[u] {
							seen[u] = true
							m.imageURLs = append(m.imageURLs, u)
							break
						}
					}
				}
			}
		}
		if u, ok := item["display_url"].(string); ok && u != "" && !seen[u] {
			seen[u] = true
			m.imageURLs = append(m.imageURLs, u)
		}
	}
	return m
}

// extractLSD 从 payloads 或 HTML 中提取 LSD token。
func extractLSD(payloads []any, html string) string {
	var walk func(x any) string
	walk = func(x any) string {
		switch t := x.(type) {
		case []any:
			if len(t) > 0 {
				if s, ok := t[0].(string); ok && s == "LSD" {
					for _, item := range t[1:] {
						if m, ok := item.(map[string]any); ok {
							if token, ok := m["token"].(string); ok {
								return token
							}
						}
					}
				}
			}
			for _, v := range t {
				if r := walk(v); r != "" {
					return r
				}
			}
		case map[string]any:
			for _, v := range t {
				if r := walk(v); r != "" {
					return r
				}
			}
		}
		return ""
	}
	for _, p := range payloads {
		if r := walk(p); r != "" {
			return r
		}
	}
	if m := igLSDRe.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	if m := igLSDRe2.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

// getCSRFToken 从 Cookie Jar 中读取 csrftoken。
func getCSRFToken(client *httpx.Client) string {
	if client.Jar == nil {
		return ""
	}
	u, _ := url.Parse("https://www.instagram.com/")
	for _, c := range client.Jar.Cookies(u) {
		if c.Name == "csrftoken" {
			return c.Value
		}
	}
	return ""
}
