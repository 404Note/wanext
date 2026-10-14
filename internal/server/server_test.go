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

func TestParseHandlerValidation(t *testing.T) {	h := New()
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
