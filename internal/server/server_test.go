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
}
