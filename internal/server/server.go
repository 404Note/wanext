// Package server 提供 HTTP 服务：内嵌 Web 测试页、解析 API 与媒体代理下载。
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"wanext/internal/extract"
	"wanext/internal/httpx"
	"wanext/internal/model"
	"wanext/internal/platform"
)

//go:embed web
var webFS embed.FS

// serverVersion 与 cmd/wanext/main.go 的 version 保持同步，供 /api/health 返回。
const serverVersion = "0.1.0"

// 媒体链接有效期（与 video-parser 对齐：15 分钟）。
const mediaTTL = 15 * time.Minute

// 解析接口限流默认值：每 IP 每分钟 20 次、每天 300 次。
const (
	parseMinuteLimit = 20
	parseDayLimit    = 300
)

// 各平台下载时的 Referer（部分 CDN 校验）。
// wx-mp 分支：仅保留大陆受众常用平台，已移除 X / Instagram。
var platformReferer = map[string]string{
	"douyin":      "https://www.douyin.com/",
	"kuaishou":    "https://www.kuaishou.com/",
	"xiaohongshu": "https://www.xiaohongshu.com/",
	"weibo":       "https://weibo.com/",
	"bilibili":    "https://www.bilibili.com/",
}

// platformUA 返回媒体代理下载时的 UA。
// B 站：API 接口（api.bilibili.com）在机房 IP 下用浏览器 UA 会被 412 拦截，用中性 UA；
// 但 CDN（*.bilivideo.com）反过来：curl 类 UA 直接 403，必须用浏览器 UA。
// （plainTLS 特例见 mediaEntry：B 站、抖音 CDN 会拦截伪装 TLS 指纹，
// 必须用标准 Go TLS + 浏览器 UA。）
func platformUA(platformName string) string {
	_ = platformName
	return httpx.DesktopUA
}

// Config 是 HTTP 服务的配置。
type Config struct {
	// BaseURL 对外绝对地址前缀，如 https://api.example.com。
	// 为空时 proxy_url / download_url 保持相对路径（Web 页同源可用）。
	// 环境变量 BASE_URL 优先于 serve 的 -base-url 参数。
	BaseURL string
}

// normalizeBaseURL 清洗并校验 BaseURL：去尾斜杠；非空时必须 https:// 开头，
// 本地开发允许 http://127.0.0.1 / http://localhost。
// 不根据请求 Host 头自动拼接：Host 可伪造，且经反向代理时易拼出内网地址。
func normalizeBaseURL(s string) (string, error) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "/")
	if s == "" {
		return "", nil
	}
	if strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "http://127.0.0.1") ||
		strings.HasPrefix(s, "http://localhost") {
		return s, nil
	}
	return "", fmt.Errorf("BASE_URL 必须以 https:// 开头（本地开发可用 http://127.0.0.1 或 http://localhost）")
}

// buildProxyURL 拼接媒体代理地址：配置了 BaseURL 返回绝对地址，否则相对路径。
func buildProxyURL(baseURL, token string) string {
	if baseURL == "" {
		return "/api/media/" + token
	}
	return baseURL + "/api/media/" + token
}

// clientIP 取客户端 IP：反向代理部署时信任 X-Forwarded-For 首个 IP，
// 否则用 X-Real-IP，最后回退 RemoteAddr。仅用于限流计数，不做鉴权依据。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if ip := strings.TrimSpace(strings.Split(xff, ",")[0]); ip != "" {
			return ip
		}
	}
	if rip := strings.TrimSpace(r.Header.Get("X-Real-IP")); rip != "" {
		return rip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

type ipCount struct {
	minuteStart time.Time
	minuteN     int
	dayStart    time.Time
	dayN        int
}

// rateLimiter 是固定窗口 IP 限流器（内存实现，进程重启清零）。
type rateLimiter struct {
	mu          sync.Mutex
	counts      map[string]*ipCount
	minuteLimit int
	dayLimit    int
}

func newRateLimiter(minuteLimit, dayLimit int) *rateLimiter {
	return &rateLimiter{counts: map[string]*ipCount{}, minuteLimit: minuteLimit, dayLimit: dayLimit}
}

// allow 计数并判断是否放行：超限返回 false，不计数。
func (l *rateLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.counts[ip]
	if !ok {
		c = &ipCount{minuteStart: now, dayStart: now}
		l.counts[ip] = c
	}
	if now.Sub(c.minuteStart) >= time.Minute {
		c.minuteStart = now
		c.minuteN = 0
	}
	if now.Sub(c.dayStart) >= 24*time.Hour {
		c.dayStart = now
		c.dayN = 0
	}
	if c.minuteN >= l.minuteLimit || c.dayN >= l.dayLimit {
		return false
	}
	c.minuteN++
	c.dayN++
	return true
}

type mediaEntry struct {
	url      string
	referer  string
	ua       string
	cookie   string
	filename string
	expires  time.Time
	// plainTLS：为 true 时用标准 Go TLS（不做指纹伪装）。
	// B 站 CDN 会拦截指纹伪装的 TLS 握手，必须用标准 TLS + 浏览器 UA。
	plainTLS bool
}

// Registry 是媒体代理令牌的内存注册表。
type Registry struct {
	mu sync.Mutex
	m  map[string]*mediaEntry
}

func NewRegistry() *Registry {
	r := &Registry{m: map[string]*mediaEntry{}}
	go r.cleanup()
	return r
}

func (r *Registry) Put(e *mediaEntry) string {
	token := httpx.RandomHex(16)
	r.mu.Lock()
	r.m[token] = e
	r.mu.Unlock()
	return token
}

func (r *Registry) Get(token string) *mediaEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[token]
	if !ok || time.Now().After(e.expires) {
		delete(r.m, token)
		return nil
	}
	return e
}

func (r *Registry) cleanup() {
	for range time.Tick(5 * time.Minute) {
		now := time.Now()
		r.mu.Lock()
		for k, e := range r.m {
			if now.After(e.expires) {
				delete(r.m, k)
			}
		}
		r.mu.Unlock()
	}
}

type apiMedia struct {
	model.Media
	ProxyURL    string `json:"proxy_url"`
	DownloadURL string `json:"download_url"`
}

type apiResult struct {
	Platform string            `json:"platform"`
	Title    string            `json:"title"`
	Author   string            `json:"author"`
	CoverURL string            `json:"cover_url,omitempty"`
	PageURL  string            `json:"page_url,omitempty"`
	Medias   []apiMedia        `json:"medias"`
	Sharer   *model.SharerInfo `json:"sharer,omitempty"`
}

var illegalFilenameRe = regexp.MustCompile(`[\\/:*?"<>|]`)

// sanitizeFilename 生成安全的下载文件名。
func sanitizeFilename(title, kind string) string {
	t := illegalFilenameRe.ReplaceAllString(title, "")
	t = strings.TrimSpace(t)
	if r := []rune(t); len(r) > 60 {
		t = string(r[:60])
	}
	if t == "" {
		t = "media"
	}
	ext := ".mp4"
	if kind == "image" {
		ext = ".jpg"
	}
	return t + ext
}

// New 返回配置好的 http.Handler（BaseURL 取环境变量 BASE_URL）。
func New() http.Handler {
	return NewWithConfig(Config{BaseURL: os.Getenv("BASE_URL")})
}

// NewWithConfig 返回配置好的 http.Handler。BaseURL 非法时按空处理，
// 启动期校验由 ServeWithConfig 负责返回错误。
func NewWithConfig(cfg Config) http.Handler {
	baseURL, _ := normalizeBaseURL(cfg.BaseURL)
	reg := NewRegistry()
	limiter := newRateLimiter(parseMinuteLimit, parseDayLimit)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "页面缺失", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": serverVersion})
	})

	mux.HandleFunc("GET /api/platforms", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"platforms": extract.SupportedPlatforms()})
	})

	mux.HandleFunc("POST /api/parse", func(w http.ResponseWriter, r *http.Request) {
		if !limiter.allow(clientIP(r)) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "操作太频繁，请稍后再试"})
			return
		}
		var req struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体必须是 JSON：{\"url\":\"...\"}"})
			return
		}
		if strings.TrimSpace(req.URL) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url 不能为空"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		res, err := extract.ParseText(ctx, req.URL)
		if err != nil {
			logParseError(err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "解析失败！"})
			return
		}
		out := apiResult{
			Platform: res.Platform, Title: res.Title, Author: res.Author,
			CoverURL: res.CoverURL, PageURL: res.PageURL, Sharer: res.Sharer,
		}
		for _, m := range res.Medias {
			token := reg.Put(&mediaEntry{
				url:      m.DirectURL,
				referer:  platformReferer[res.Platform],
				ua:       platformUA(res.Platform),
				cookie:   m.Cookie,
				filename: sanitizeFilename(res.Title, m.Kind),
				expires:  time.Now().Add(mediaTTL),
				plainTLS: res.Platform == "bilibili" || res.Platform == "douyin",
			})
			proxy := buildProxyURL(baseURL, token)
			out.Medias = append(out.Medias, apiMedia{
				Media:       m,
				ProxyURL:    proxy,
				DownloadURL: proxy + "?download=1",
			})
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET /api/media/{token}", func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		e := reg.Get(token)
		if e == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "链接已过期，请重新解析"})
			return
		}
		proxyMedia(w, r, e)
	})

	// 抖音分享者身份解析：从分享文案/短链还原「这条链接是谁分享的」
	//（分享者归因参数解析，思路参考 hiFOFA/douyin-share-resolver）。
	mux.HandleFunc("POST /api/douyin/sharer", func(w http.ResponseWriter, r *http.Request) {
		if !limiter.allow(clientIP(r)) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "操作太频繁，请稍后再试"})
			return
		}
		var req struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体必须是 JSON：{\"url\":\"...\"}"})
			return
		}
		shortURL := ""
		for _, u := range extract.ExtractURLs(req.URL) {
			if pu, err := url.Parse(u); err == nil && strings.Contains(strings.ToLower(pu.Hostname()), "v.douyin.com") {
				shortURL = u
				break
			}
		}
		if shortURL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未从输入中找到 v.douyin.com 短链"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		res, err := platform.Douyin{}.ResolveSharer(ctx, shortURL)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	return mux
}

// proxyMedia 代理上游媒体，支持 Range 断点续传；?download=1 时作为附件下载。
func proxyMedia(w http.ResponseWriter, r *http.Request, e *mediaEntry) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url, nil)
	if err != nil {
		http.Error(w, "构造请求失败", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", e.ua)
	if e.referer != "" {
		req.Header.Set("Referer", e.referer)
	}
	if e.cookie != "" {
		req.Header.Set("Cookie", e.cookie)
	}
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	client := httpx.NewClient()
	if e.plainTLS {
		client = httpx.NewPlainClient()
	}
	client.Timeout = 10 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "上游请求失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		http.Error(w, fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode), http.StatusBadGateway)
		return
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if r.URL.Query().Get("download") == "1" {
		disposition := fmt.Sprintf("attachment; filename*=UTF-8''%s", url.QueryEscape(path.Base(e.filename)))
		w.Header().Set("Content-Disposition", disposition)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// logParseError 把解析失败的详情打到服务端日志，页面只向用户展示"解析失败！"。
// 日志格式：[douyin] v.douyin.com/xxx/ 具体原因（含排查信息）
func logParseError(err error) {
	var pe *extract.ParseError
	if errors.As(err, &pe) {
		u := strings.TrimPrefix(strings.TrimPrefix(pe.URL, "https://"), "http://")
		log.Printf("[%s] %s %v", pe.Platform, u, pe.Err)
		return
	}
	log.Printf("[parse] %v", err)
}

// Serve 在 addr 上启动服务（BaseURL 取环境变量 BASE_URL）。
func Serve(addr string) error {
	return ServeWithConfig(addr, Config{BaseURL: os.Getenv("BASE_URL")})
}

// ServeWithConfig 在 addr 上启动服务，先校验 BaseURL，非法时直接返回错误。
func ServeWithConfig(addr string, cfg Config) error {
	if _, err := normalizeBaseURL(cfg.BaseURL); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           NewWithConfig(cfg),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}
