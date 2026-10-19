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
	"path/filepath"
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

	// AuthRequired 为 true 时强制微信登录鉴权（环境变量 AUTH_REQUIRED=1）。
	// 此时 WXAppID / WXSecret / JWTSecret 缺失或过短会导致启动失败（失败关闭）。
	AuthRequired bool
	// WXAppID / WXSecret 微信小程序凭证，只进服务端 .env。
	WXAppID  string
	WXSecret string
	// JWTSecret JWT 签名密钥，至少 32 字节，只进服务端 .env，与 WXSecret 分开。
	JWTSecret string
	// BannedUIDs 紧急兜底封禁名单（管理页不可用时用，环境变量 BANNED_UIDS 逗号分隔）。
	BannedUIDs []string

	// AdminEnabled 管理页开关（环境变量 ADMIN_ENABLED=1）。
	AdminEnabled bool
	// AdminToken 管理员凭证，至少 32 字节，只进服务端 .env。
	AdminToken string
	// AdminDataDir 封禁名单与管理员操作记录的持久化目录，缺省 "data"。
	AdminDataDir string
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
// dayLimit<=0 时不做按天限制（供登录/管理页等纯分钟级限流器）。
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
	if l.dayLimit > 0 && now.Sub(c.dayStart) >= 24*time.Hour {
		c.dayStart = now
		c.dayN = 0
	}
	if c.minuteN >= l.minuteLimit || (l.dayLimit > 0 && c.dayN >= l.dayLimit) {
		return false
	}
	c.minuteN++
	if l.dayLimit > 0 {
		c.dayN++
	}
	return true
}

// reset 清除指定键的计数（管理页误伤恢复用）。
func (l *rateLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.counts, key)
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
	// ownerUID 签发该媒体令牌的用户标识（鉴权开启时绑定，关闭时为空）。
	ownerUID string
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

// RevokeByUID 作废指定用户名下的全部媒体令牌（封禁时调用），返回作废数量。
func (r *Registry) RevokeByUID(uid string) int {
	if uid == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k, e := range r.m {
		if e.ownerUID == uid {
			delete(r.m, k)
			n++
		}
	}
	return n
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

// parseIPCoarseLimit IP 粗粒度兜底限流：120 次/分钟（无按天限制）。
// 必须宽于用户额度：同一 NAT 出口下大量真实用户共用一个 IP 是常态。
const parseIPCoarseLimit = 120

// parseAuthSwitch 解析 0/1 开关：只认 "1"、"0" 与空（缺省 0），其他值报错，
// 避免拼写错误造成静默裸奔。
func parseAuthSwitch(s string) (bool, error) {
	switch strings.TrimSpace(s) {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("开关值非法（只接受 0/1）: %q", s)
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ConfigFromEnv 从环境变量构造 Config。
func ConfigFromEnv() (Config, error) {
	cfg := Config{BaseURL: os.Getenv("BASE_URL")}
	var err error
	if cfg.AuthRequired, err = parseAuthSwitch(os.Getenv("AUTH_REQUIRED")); err != nil {
		return cfg, fmt.Errorf("AUTH_REQUIRED: %w", err)
	}
	cfg.WXAppID = os.Getenv("WX_APPID")
	cfg.WXSecret = os.Getenv("WX_SECRET")
	cfg.JWTSecret = os.Getenv("JWT_SECRET")
	cfg.BannedUIDs = splitCSV(os.Getenv("BANNED_UIDS"))
	if cfg.AdminEnabled, err = parseAuthSwitch(os.Getenv("ADMIN_ENABLED")); err != nil {
		return cfg, fmt.Errorf("ADMIN_ENABLED: %w", err)
	}
	cfg.AdminToken = os.Getenv("ADMIN_TOKEN")
	if d := os.Getenv("ADMIN_DATA_DIR"); d != "" {
		cfg.AdminDataDir = d
	}
	return cfg, nil
}

// New 返回配置好的 http.Handler（配置取环境变量）。
func New() http.Handler {
	cfg, err := ConfigFromEnv()
	if err != nil {
		log.Printf("环境变量解析失败（测试/开发路径，鉴权保持关闭）: %v", err)
		cfg = Config{BaseURL: os.Getenv("BASE_URL")}
	}
	return NewWithConfig(cfg)
}

// NewWithConfig 返回配置好的 http.Handler。BaseURL 非法时按空处理，
// 启动期校验由 ServeWithConfig 负责返回错误。
func NewWithConfig(cfg Config) http.Handler {
	baseURL, _ := normalizeBaseURL(cfg.BaseURL)
	reg := NewRegistry()
	limiter := newRateLimiter(parseMinuteLimit, parseDayLimit)
	ipCoarse := newRateLimiter(parseIPCoarseLimit, 0)
	audit := NewAuditor()

	dataDir := cfg.AdminDataDir
	if dataDir == "" {
		dataDir = "data"
	}
	bans := NewBanStore(filepath.Join(dataDir, "admin-state.json"))
	if err := bans.Load(); err != nil {
		log.Printf("[admin] 加载封禁状态失败: %v", err)
	}
	bans.MergeEnvBans(cfg.BannedUIDs)

	auth := NewAuthenticator(cfg.AuthRequired, []byte(cfg.JWTSecret), cfg.WXAppID, cfg.WXSecret, bans, audit)
	admin := NewAdminManager(cfg.AdminEnabled, cfg.AdminToken, bans, audit, auth, limiter, reg)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// A9：鉴权开启时生产环境关闭 Web 测试页。
		if cfg.AuthRequired {
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
		// A8：鉴权开启时关闭平台列表（会暴露支持平台清单，且小程序从未调用）。
		if cfg.AuthRequired {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"platforms": extract.SupportedPlatforms()})
	})

	// 登录入口：唯一不需要令牌的业务接口。
	mux.HandleFunc("POST /api/login", auth.handleLogin)

	// 限流键：鉴权开启时按用户（u:uid），关闭时回退按 IP（与旧行为一致）。
	limitKey := func(r *http.Request) string {
		if uid := UIDFromContext(r.Context()); uid != "" {
			return "u:" + uid
		}
		return "ip:" + clientIP(r)
	}
	checkLimits := func(w http.ResponseWriter, r *http.Request, start time.Time, endpoint string) bool {
		ip := clientIP(r)
		if !ipCoarse.allow(ip) {
			audit.Record(AuditEvent{Endpoint: endpoint, UID: UIDFromContext(r.Context()), IP: ip,
				Result: AuditLimit429, Reason: "IP 兜底限流", LatencyMs: time.Since(start).Milliseconds()})
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "操作太频繁，请稍后再试"})
			return false
		}
		if !limiter.allow(limitKey(r)) {
			audit.Record(AuditEvent{Endpoint: endpoint, UID: UIDFromContext(r.Context()), IP: ip,
				Result: AuditLimit429, Reason: "用户限流", LatencyMs: time.Since(start).Milliseconds()})
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "操作太频繁，请稍后再试"})
			return false
		}
		return true
	}

	mux.Handle("POST /api/parse", auth.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if !checkLimits(w, r, start, "parse") {
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
		uid := UIDFromContext(r.Context())
		if err != nil {
			logParseError(err, uid)
			audit.Record(AuditEvent{Endpoint: "parse", UID: uid, IP: clientIP(r),
				Platform: platformOf(err), Target: shortTarget(req.URL),
				Result: AuditFail, Reason: parseFailReason(err), LatencyMs: time.Since(start).Milliseconds()})
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
				ownerUID: uid,
			})
			proxy := buildProxyURL(baseURL, token)
			out.Medias = append(out.Medias, apiMedia{
				Media:       m,
				ProxyURL:    proxy,
				DownloadURL: proxy + "?download=1",
			})
		}
		audit.Record(AuditEvent{Endpoint: "parse", UID: uid, IP: clientIP(r),
			Platform: res.Platform, Target: shortTarget(req.URL),
			Result: AuditOK, MediaCount: len(out.Medias), LatencyMs: time.Since(start).Milliseconds()})
		writeJSON(w, http.StatusOK, out)
	})))

	mux.HandleFunc("GET /api/media/{token}", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		token := r.PathValue("token")
		e := reg.Get(token)
		if e == nil {
			audit.Record(AuditEvent{Endpoint: "media", IP: clientIP(r),
				Result: AuditFail, Reason: "令牌无效或过期", LatencyMs: time.Since(start).Milliseconds()})
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "链接已过期，请重新解析"})
			return
		}
		// 带头则校验：带了有效 JWT 且其 uid 与令牌绑定用户不一致 → 403。
		// 组件的无头请求照常放行（持有 128 位随机令牌本身即凭证）。
		if cfg.AuthRequired && e.ownerUID != "" {
			if uid2, ok := auth.validBearerForMedia(r); ok && uid2 != e.ownerUID {
				audit.Record(AuditEvent{Endpoint: "media", UID: uid2, IP: clientIP(r),
					Result: AuditDeny403, Reason: "媒体令牌异主", LatencyMs: time.Since(start).Milliseconds()})
				log.Printf("[auth] 403 媒体令牌异主 uid=%s owner=%s", uidShort(uid2), uidShort(e.ownerUID))
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权访问该媒体"})
				return
			}
		}
		audit.Record(AuditEvent{Endpoint: "media", UID: e.ownerUID, IP: clientIP(r),
			Result: AuditOK, LatencyMs: time.Since(start).Milliseconds()})
		proxyMedia(w, r, e)
	})

	// 抖音分享者身份解析：从分享文案/短链还原「这条链接是谁分享的」
	//（分享者归因参数解析，思路参考 hiFOFA/douyin-share-resolver）。
	mux.Handle("POST /api/douyin/sharer", auth.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if !checkLimits(w, r, start, "sharer") {
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
		uid := UIDFromContext(r.Context())
		if err != nil {
			audit.Record(AuditEvent{Endpoint: "sharer", UID: uid, IP: clientIP(r),
				Target: shortTarget(req.URL), Result: AuditFail, Reason: err.Error(),
				LatencyMs: time.Since(start).Milliseconds()})
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		audit.Record(AuditEvent{Endpoint: "sharer", UID: uid, IP: clientIP(r),
			Target: shortTarget(req.URL), Result: AuditOK, LatencyMs: time.Since(start).Milliseconds()})
		writeJSON(w, http.StatusOK, res)
	})))

	// 管理页：页面与接口（未启用时一律 404，见 AdminManager）。
	mux.HandleFunc("GET /admin/", admin.handleAdminPage)
	mux.HandleFunc("POST /api/admin/login", admin.handleAdminLogin)
	adminAPI := func(h http.HandlerFunc) http.Handler { return admin.requireAdmin(h) }
	mux.Handle("GET /api/admin/me", adminAPI(admin.handleAdminMe))
	mux.Handle("POST /api/admin/logout", adminAPI(admin.handleAdminLogout))
	mux.Handle("GET /api/admin/stats", adminAPI(admin.handleAdminStats))
	mux.Handle("GET /api/admin/users", adminAPI(admin.handleAdminUsers))
	mux.Handle("GET /api/admin/users/{uid}", adminAPI(admin.handleAdminUserDetail))
	mux.Handle("POST /api/admin/users/{uid}/reset-limit", adminAPI(admin.handleAdminResetLimit))
	mux.Handle("GET /api/admin/events", adminAPI(admin.handleAdminEvents))
	mux.Handle("GET /api/admin/bans", adminAPI(admin.handleAdminBans))
	mux.Handle("POST /api/admin/bans", adminAPI(admin.handleAdminBan))
	mux.Handle("DELETE /api/admin/bans/{uid}", adminAPI(admin.handleAdminUnban))

	return mux
}

// shortTarget 审计用的目标链接摘要：截断到 120 字符。
func shortTarget(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

// platformOf 从解析错误中提取平台英文名。
func platformOf(err error) string {
	var pe *extract.ParseError
	if errors.As(err, &pe) {
		return pe.Platform
	}
	return ""
}

// parseFailReason 从解析错误中提取原因短文（不含用户输入原文）。
func parseFailReason(err error) string {
	var pe *extract.ParseError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// proxyMedia 代理上游媒体，支持 Range 断点续传；?download=1 时作为附件下载。
func proxyMedia(w http.ResponseWriter, r *http.Request, e *mediaEntry) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	// HEAD 仅取元信息（小程序展示文件大小用）：上游同样用 HEAD，避免拉取整个文件。
	method := http.MethodGet
	if r.Method == http.MethodHead {
		method = http.MethodHead
	}
	req, err := http.NewRequestWithContext(ctx, method, e.url, nil)
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
// 日志格式：[douyin] v.douyin.com/xxx/ 具体原因 uid=短码（含排查信息）
func logParseError(err error, uid string) {
	suffix := ""
	if uid != "" {
		suffix = " uid=" + uidShort(uid)
	}
	var pe *extract.ParseError
	if errors.As(err, &pe) {
		u := strings.TrimPrefix(strings.TrimPrefix(pe.URL, "https://"), "http://")
		log.Printf("[%s] %s %v%s", pe.Platform, u, pe.Err, suffix)
		return
	}
	log.Printf("[parse] %v%s", err, suffix)
}

// Serve 在 addr 上启动服务（配置取环境变量）。
func Serve(addr string) error {
	cfg, err := ConfigFromEnv()
	if err != nil {
		return err
	}
	return ServeWithConfig(addr, cfg)
}

// validateAuthConfig 失败关闭校验：AUTH_REQUIRED=1 时密钥缺失或过短直接报错，
// 绝不降级为无鉴权运行（沿用 BASE_URL 非法即启动失败的模式）。
func validateAuthConfig(cfg Config) error {
	if !cfg.AuthRequired {
		return nil
	}
	if cfg.WXAppID == "" || cfg.WXSecret == "" {
		return fmt.Errorf("AUTH_REQUIRED=1 但 WX_APPID / WX_SECRET 未配置")
	}
	if len(cfg.JWTSecret) < 32 {
		return fmt.Errorf("AUTH_REQUIRED=1 但 JWT_SECRET 未配置或过短（需≥32 字节）")
	}
	return nil
}

// ServeWithConfig 在 addr 上启动服务，先校验 BaseURL，非法时直接返回错误。
func ServeWithConfig(addr string, cfg Config) error {
	if _, err := normalizeBaseURL(cfg.BaseURL); err != nil {
		return err
	}
	if err := validateAuthConfig(cfg); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           NewWithConfig(cfg),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}
