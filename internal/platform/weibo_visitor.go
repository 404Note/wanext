package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"wanext/internal/httpx"
)

// 微博访客凭证 —— 参考 belingud/weibo-downloader-skill 的访客绕过思路：
//
// 新浪访客系统：POST passport.weibo.com/visitor/genvisitor2 拿 tid/sub/subp，
// 再 GET visitor/visitor crossdomain 换取 SUB/SUBP cookie（约 365 天有效）。
// ajax/statuses/show 匿名请求经常被拦截（ok != 1），带访客 cookie 后可正常访问公开微博。
//
// 凭证在进程内缓存（成功 12 小时，失败 5 分钟后重试）；获取失败时降级为匿名请求，
// 调用方行为与之前一致。
const (
	weiboGenVisitorAPI = "https://passport.weibo.com/visitor/genvisitor2"
	weiboVisitorAPI    = "https://passport.weibo.com/visitor/visitor"
)

// weiboUA 微博请求 UA（与参考项目一致的 Android UA）。
const weiboUA = "Mozilla/5.0 (Linux; Android 8.0.0; SM-G955U Build/R16NW) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/116.0.0.0 Mobile Safari/537.36"

var weiboCallbackRe = regexp.MustCompile(`visitor_gray_callback\((?s:(\{.*\}))\)`)

var weiboVisitorCache = struct {
	sync.Mutex
	jar     http.CookieJar
	ok      bool
	fetched time.Time
}{}

// weiboAuthedClient 返回带访客 cookie 的客户端。
func weiboAuthedClient(ctx context.Context) *httpx.Client {
	c := &httpx.Client{httpx.NewClient()}
	if jar := weiboVisitorJar(ctx); jar != nil {
		c.Jar = jar
	}
	return c
}

// weiboVisitorJar 返回缓存的访客 cookie jar（best-effort，失败返回空 jar）。
func weiboVisitorJar(ctx context.Context) http.CookieJar {
	weiboVisitorCache.Lock()
	defer weiboVisitorCache.Unlock()
	ttl := 12 * time.Hour
	if !weiboVisitorCache.ok {
		ttl = 5 * time.Minute // 获取失败时短暂缓存，避免每次重试拖慢解析
	}
	if weiboVisitorCache.jar != nil && time.Since(weiboVisitorCache.fetched) < ttl {
		return weiboVisitorCache.jar
	}
	jar, _ := cookiejar.New(nil)
	ok := false
	fctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if err := fetchWeiboVisitorCookies(fctx, jar); err == nil {
		ok = true
	}
	weiboVisitorCache.jar, weiboVisitorCache.ok, weiboVisitorCache.fetched = jar, ok, time.Now()
	return jar
}

// parseGenVisitorJSONP 从 genvisitor2 的 JSONP 响应体中提取 tid/sub/subp（纯函数，便于单测）。
func parseGenVisitorJSONP(payload []byte) (tid, sub, subp string, err error) {
	var gv struct {
		Data struct {
			Tid  string `json:"tid"`
			Sub  string `json:"sub"`
			Subp string `json:"subp"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &gv); err != nil {
		return "", "", "", fmt.Errorf("genvisitor2 JSON 解析失败: %w", err)
	}
	if gv.Data.Tid == "" {
		return "", "", "", fmt.Errorf("genvisitor2 未返回 tid")
	}
	return gv.Data.Tid, gv.Data.Sub, gv.Data.Subp, nil
}

// fetchWeiboVisitorCookies 走新浪访客系统获取 SUB/SUBP，写入 jar。
func fetchWeiboVisitorCookies(ctx context.Context, jar http.CookieJar) error {
	c := httpx.NewClient()
	c.Jar = jar
	c.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

	// 1. genvisitor2：拿 tid / sub / subp（JSONP 响应）
	form := url.Values{
		"cb": {"visitor_gray_callback"}, "ver": {"20250916"}, "tid": {""},
		"from": {"weibo"}, "webdriver": {"false"}, "return_url": {"https://weibo.com/"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, weiboGenVisitorAPI, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://weibo.com/")
	req.Header.Set("User-Agent", weiboUA)
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("genvisitor2 请求失败: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	m := weiboCallbackRe.FindSubmatch(body)
	if m == nil {
		return fmt.Errorf("genvisitor2 响应解析失败")
	}
	tid, sub, subp, err := parseGenVisitorJSONP(m[1])
	if err != nil {
		return err
	}

	// 2. visitor crossdomain：换 SUB/SUBP（不跟随跳转，直接读 Set-Cookie）
	q := url.Values{
		"a": {"crossdomain"}, "t": {tid}, "sp": {subp}, "s": {sub},
		"from": {"weibo"}, "_rand": {fmt.Sprintf("%08d", time.Now().UnixNano()%100000000)},
		"url": {"https://weibo.com/"},
	}
	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, weiboVisitorAPI+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req2.Header.Set("Referer", "https://weibo.com/")
	req2.Header.Set("User-Agent", weiboUA)
	resp2, err := c.Do(req2)
	if err != nil {
		return fmt.Errorf("visitor crossdomain 请求失败: %w", err)
	}
	resp2.Body.Close()
	for _, ck := range resp2.Cookies() {
		if (ck.Name == "SUB" || ck.Name == "SUBP") && ck.Value != "" {
			return nil
		}
	}
	return fmt.Errorf("未获取到 SUB/SUBP cookie")
}
