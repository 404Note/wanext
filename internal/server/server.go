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
	"net/http"
	"net/url"
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

// 媒体链接有效期（与 video-parser 对齐：15 分钟）。
const mediaTTL = 15 * time.Minute

// 各平台下载时的 Referer（部分 CDN 校验）。
var platformReferer = map[string]string{
	"douyin":      "https://www.douyin.com/",
	"tiktok":      "https://www.tiktok.com/",
	"kuaishou":    "https://www.kuaishou.com/",
	"xiaohongshu": "https://www.xiaohongshu.com/",
	"weibo":       "https://weibo.com/",
	"bilibili":    "https://www.bilibili.com/",
	"x":           "https://x.com/",
	"instagram":   "https://www.instagram.com/",
}

// Instagram CDN 对 UA 指纹敏感，用极简 UA；
// B 站：API 接口（api.bilibili.com）在机房 IP 下用浏览器 UA 会被 412 拦截，用中性 UA；
// 但 CDN（*.bilivideo.com）反过来：curl 类 UA 直接 403，必须用浏览器 UA。
func platformUA(platformName string) string {
	switch platformName {
	case "instagram":
		return httpx.MinimalUA
	default:
		return httpx.DesktopUA
	}
}

type mediaEntry struct {
	url      string
	referer  string
	ua       string
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

// New 返回配置好的 http.Handler。
func New() http.Handler {
	reg := NewRegistry()
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

	mux.HandleFunc("GET /api/platforms", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"platforms": extract.SupportedPlatforms()})
	})

	mux.HandleFunc("POST /api/parse", func(w http.ResponseWriter, r *http.Request) {
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
				filename: sanitizeFilename(res.Title, m.Kind),
				expires:  time.Now().Add(mediaTTL),
				plainTLS: res.Platform == "bilibili",
			})
			proxy := "/api/media/" + token
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

// Serve 在 addr 上启动服务。
func Serve(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           New(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}
