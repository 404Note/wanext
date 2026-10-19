package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryExpiry(t *testing.T) {
	r := NewRegistry()
	token := r.Put(&mediaEntry{url: "https://example.com/a.mp4", expires: time.Now().Add(-time.Second)})
	if e := r.Get(token); e != nil {
		t.Error("过期令牌应返回 nil")
	}
	token2 := r.Put(&mediaEntry{url: "https://example.com/b.mp4", expires: time.Now().Add(time.Minute)})
	if e := r.Get(token2); e == nil || e.url != "https://example.com/b.mp4" {
		t.Error("有效令牌应能取回")
	}
}

func TestSanitizeFilename(t *testing.T) {
	got := sanitizeFilename(`标题：测试/视频*?"<>|`, "video")
	if strings.ContainsAny(got, `/:*?"<>|`) || !strings.HasSuffix(got, ".mp4") {
		t.Errorf("文件名非法: %s", got)
	}
	if got := sanitizeFilename("", "image"); got != "media.jpg" {
		t.Errorf("空标题回退错误: %s", got)
	}
	long := strings.Repeat("长", 100)
	if got := sanitizeFilename(long, "video"); len([]rune(got)) > 64 {
		t.Errorf("超长标题未截断: %d rune", len([]rune(got)))
	}
}

func TestParseHandlerValidation(t *testing.T) {
	h := New()
	// 空 body
	req := httptest.NewRequest(http.MethodPost, "/api/parse", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("空 url 应 400，得到 %d", w.Code)
	}
	// 非 JSON
	req = httptest.NewRequest(http.MethodPost, "/api/parse", strings.NewReader(`not json`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应 400，得到 %d", w.Code)
	}
	// 无效令牌
	req = httptest.NewRequest(http.MethodGet, "/api/media/badtoken123", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("无效令牌应 404，得到 %d", w.Code)
	}
	// 首页
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "wanext") {
		t.Errorf("首页异常: %d", w.Code)
	}
	// 平台列表
	req = httptest.NewRequest(http.MethodGet, "/api/platforms", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "douyin") {
		t.Errorf("平台列表异常: %d", w.Code)
	}
	// 已移除的平台不应出现在列表中
	if strings.Contains(w.Body.String(), "instagram") || strings.Contains(w.Body.String(), `"x"`) {
		t.Errorf("平台列表不应含已移除的 X / Instagram: %s", w.Body.String())
	}
}

func TestHealth(t *testing.T) {
	h := New()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("健康检查异常: %d %s", w.Code, w.Body.String())
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	if got, _ := normalizeBaseURL(""); got != "" {
		t.Errorf("空应返回空，得到 %s", got)
	}
	if got, _ := normalizeBaseURL("https://api.example.com/"); got != "https://api.example.com" {
		t.Errorf("应去尾斜杠，得到 %s", got)
	}
	if _, err := normalizeBaseURL("http://api.example.com"); err == nil {
		t.Error("非 https 的公网地址应拒绝")
	}
	if got, err := normalizeBaseURL("http://127.0.0.1:8080"); err != nil || got != "http://127.0.0.1:8080" {
		t.Errorf("本地开发 http 应放行: %v %s", err, got)
	}
	if got := buildProxyURL("https://api.example.com", "abc"); got != "https://api.example.com/api/media/abc" {
		t.Errorf("绝对地址拼接错误: %s", got)
	}
	if got := buildProxyURL("", "abc"); got != "/api/media/abc" {
		t.Errorf("未配置时应保持相对路径: %s", got)
	}
	if err := ServeWithConfig("127.0.0.1:0", Config{BaseURL: "http://api.example.com"}); err == nil {
		t.Error("非法 BaseURL 启动应返回错误")
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(2, 3)
	if !l.allow("1.1.1.1") || !l.allow("1.1.1.1") {
		t.Error("限额内应放行")
	}
	if l.allow("1.1.1.1") {
		t.Error("超分钟限额应拒绝")
	}
	if !l.allow("2.2.2.2") {
		t.Error("不同 IP 不应互相影响")
	}
	if got := clientIP(httptest.NewRequest(http.MethodGet, "/", nil)); got == "" {
		t.Error("clientIP 不应为空")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(req); got != "203.0.113.9" {
		t.Errorf("应取 XFF 首个 IP，得到 %s", got)
	}
}

func TestRateLimiterPurge(t *testing.T) {
	l := newRateLimiter(100, 100)
	l.allow("fresh")
	l.allow("stale")
	l.mu.Lock()
	l.counts["stale"].minuteStart = time.Now().Add(-25 * time.Hour)
	l.mu.Unlock()
	l.purgeOlderThan(24 * time.Hour)
	l.mu.Lock()
	_, staleOK := l.counts["stale"]
	_, freshOK := l.counts["fresh"]
	n := len(l.counts)
	l.mu.Unlock()
	if staleOK {
		t.Error("24 小时未活跃的键应被清理")
	}
	if !freshOK || n != 1 {
		t.Errorf("活跃键不应被误删，剩余 %d", n)
	}
}

func TestNormalizeBasePath(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"/":              "",
		"wx-mp/wanext":   "/wx-mp/wanext",
		"/wx-mp/wanext":  "/wx-mp/wanext",
		"/wx-mp/wanext/": "/wx-mp/wanext",
		"  /a/b  ":       "/a/b",
	}
	for in, want := range cases {
		if got := normalizeBasePath(in); got != want {
			t.Errorf("normalizeBasePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func testBasePathConfig(t *testing.T) Config {
	t.Helper()
	cfg := testAdminConfig()
	cfg.BasePath = "/wx-mp/wanext"
	cfg.AdminDataDir = t.TempDir()
	return cfg
}

// TestBasePathRouting 子路径部署：所有路由挂在前缀下，无前缀请求 404，
// 裸前缀重定向到带斜杠版本。
func TestBasePathRouting(t *testing.T) {
	h := NewWithConfig(testBasePathConfig(t))

	// 健康检查：带前缀 200，无前缀 404
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/wx-mp/wanext/api/health", http.StatusOK},
		{"/api/health", http.StatusNotFound},
		{"/wx-mp/wanext/", http.StatusOK}, // 未登录 → 登录框
		{"/other/", http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Errorf("GET %s = %d, want %d", tc.path, w.Code, tc.code)
		}
	}

	// 裸前缀 → 302 到带斜杠版本
	req := httptest.NewRequest(http.MethodGet, "/wx-mp/wanext", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/wx-mp/wanext/" {
		t.Errorf("裸前缀应 302 到 /wx-mp/wanext/，得到 %d %q", w.Code, w.Header().Get("Location"))
	}
}

// TestBasePathLoginFlow 子路径下完整登录：cookie Path 跟随前缀，登录后首页为管理页。
func TestBasePathLoginFlow(t *testing.T) {
	h := NewWithConfig(testBasePathConfig(t))

	// 登录
	body := `{"username":"` + adminTestUser + `","password":"` + adminTestPassword + `"}`
	req := httptest.NewRequest(http.MethodPost, "/wx-mp/wanext/api/admin/login", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("登录应 200，得到 %d: %s", w.Code, w.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "sid" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("未拿到会话 cookie")
	}
	if cookie.Path != "/wx-mp/wanext" {
		t.Errorf("cookie Path 应为 /wx-mp/wanext，得到 %q", cookie.Path)
	}

	// 登录后首页 → 管理页
	req2 := httptest.NewRequest(http.MethodGet, "/wx-mp/wanext/", nil)
	req2.AddCookie(cookie)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), `data-tab="demo"`) {
		t.Errorf("登录后首页应为管理页，得到 %d", w2.Code)
	}

	// 管理接口在前缀下可用
	req3 := httptest.NewRequest(http.MethodGet, "/wx-mp/wanext/api/admin/stats", nil)
	req3.AddCookie(cookie)
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Errorf("管理接口应 200，得到 %d", w3.Code)
	}
}
