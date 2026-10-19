package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testAdminSetup(t *testing.T, username, password string) (*AdminManager, *Authenticator, *rateLimiter, *Registry, Auditor, *BanStore) {
	t.Helper()
	audit := newMemAuditor()
	bans := NewBanStore("")
	auth := NewAuthenticator(true, testSecret, "test-appid", "test-secret", bans, audit)
	limiter := newRateLimiter(20, 300)
	reg := NewRegistry()
	mgr := NewAdminManager(true, username, password, bans, audit, auth, limiter, reg)
	return mgr, auth, limiter, reg, audit, bans
}

const adminTestUser = "robinson"
const adminTestPassword = "test-password-0123"

func adminLogin(t *testing.T, mgr *AdminManager) (sessionCookie, csrf string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"username":"`+adminTestUser+`","password":"`+adminTestPassword+`"}`))
	w := httptest.NewRecorder()
	mgr.handleAdminLogin(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("管理员登录应 200，得到 %d: %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "sid" {
			sessionCookie = c.Name + "=" + c.Value
		}
	}
	if sessionCookie == "" {
		t.Fatal("登录未设置会话 Cookie")
	}
	csrf = strings.Split(strings.Split(w.Body.String(), `"csrf":"`)[1], `"`)[0]
	return sessionCookie, csrf
}

func adminReq(t *testing.T, mgr *AdminManager, method, path, body, cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/api/admin/", mgr.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/admin/stats" && method == "GET":
			mgr.handleAdminStats(w, r)
		case r.URL.Path == "/api/admin/bans" && method == "POST":
			mgr.handleAdminBan(w, r)
		case r.URL.Path == "/api/admin/bans" && method == "GET":
			mgr.handleAdminBans(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/admin/users/") && strings.HasSuffix(r.URL.Path, "/reset-limit"):
			r.SetPathValue("uid", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/admin/users/"), "/reset-limit"))
			mgr.handleAdminResetLimit(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})))
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestAdminLoginWrongPassword(t *testing.T) {
	mgr, _, _, _, _, _ := testAdminSetup(t, adminTestUser, adminTestPassword)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"username":"`+adminTestUser+`","password":"wrong-password"}`))
	w := httptest.NewRecorder()
	mgr.handleAdminLogin(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("错误凭证应 401，得到 %d", w.Code)
	}
	if strings.Contains(w.Body.String(), adminTestPassword) {
		t.Error("错误响应不应泄露凭证")
	}
	// 错误账号同样 401
	req2 := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"username":"nobody","password":"`+adminTestPassword+`"}`))
	w2 := httptest.NewRecorder()
	mgr.handleAdminLogin(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("错误账号应 401，得到 %d", w2.Code)
	}
}

func TestAdminDisabled404(t *testing.T) {
	// 未启用：管理接口一律 404
	cfg := testAdminConfig()
	cfg.AdminEnabled = false
	cfg.AdminDataDir = t.TempDir()
	h := NewWithConfig(cfg)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/admin/login"},
		{http.MethodGet, "/api/admin/stats"},
		{http.MethodGet, "/admin/"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s 未启用时应 404，得到 %d", tc.method, tc.path, w.Code)
		}
	}
	// 密码过短也保持关闭
	mgr2 := NewAdminManager(true, "robinson", "short", NewBanStore(""), newMemAuditor(), nil, nil, nil)
	if mgr2.enabled {
		t.Error("过短密码不应启用管理页")
	}
}

func TestAdminAPIRequiresSession(t *testing.T) {
	mgr, _, _, _, _, _ := testAdminSetup(t, adminTestUser, adminTestPassword)
	// 无会话访问管理接口 → 401
	if w := adminReq(t, mgr, "GET", "/api/admin/stats", "", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("无会话应 401，得到 %d", w.Code)
	}
	// 用户 JWT 不能当管理员会话用
	tok, _ := issueToken("user-1", testSecret)
	if w := adminReq(t, mgr, "GET", "/api/admin/stats", "", "sid="+tok, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("用户 JWT 访问管理接口应 401，得到 %d", w.Code)
	}
	// 有会话无 CSRF 的写动作 → 403
	cookie, csrf := adminLogin(t, mgr)
	if w := adminReq(t, mgr, "POST", "/api/admin/bans", `{"uid":"u","duration":"24h","reason":"x"}`, cookie, ""); w.Code != http.StatusForbidden {
		t.Errorf("缺 CSRF 应 403，得到 %d", w.Code)
	}
	// 有会话有 CSRF → 200
	if w := adminReq(t, mgr, "POST", "/api/admin/bans", `{"uid":"u","duration":"24h","reason":"x"}`, cookie, csrf); w.Code != http.StatusOK {
		t.Errorf("合法管理请求应 200，得到 %d: %s", w.Code, w.Body.String())
	}
}

func TestBanFlowEndToEnd(t *testing.T) {
	mgr, auth, _, reg, _, bans := testAdminSetup(t, adminTestUser, adminTestPassword)
	cookie, csrf := adminLogin(t, mgr)

	// 用户登录拿令牌（跳过微信，用签发代替）
	uid := "victim-user"
	tok, _ := issueToken(uid, testSecret)

	// 给该用户登记一个媒体令牌
	mediaToken := reg.Put(&mediaEntry{url: "https://example.com/v.mp4", ownerUID: uid, expires: time.Now().Add(time.Minute)})

	// 封禁（原因必填）
	body := `{"uid":"` + uid + `","duration":"24h","reason":"测试滥用"}`
	if w := adminReq(t, mgr, "POST", "/api/admin/bans", body, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("封禁应 200，得到 %d: %s", w.Code, w.Body.String())
	}
	if !bans.IsBanned(uid) {
		t.Fatal("封禁后 IsBanned 应为 true")
	}
	// 1) 业务请求被 401
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	req := httptest.NewRequest(http.MethodPost, "/api/parse", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	auth.RequireAuth(next).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("被封禁用户请求应 401，得到 %d", w.Code)
	}
	// 2) 媒体令牌被作废
	if e := reg.Get(mediaToken); e != nil {
		t.Error("封禁应作废该用户媒体令牌")
	}
	// 解封后恢复
	req2 := httptest.NewRequest(http.MethodDelete, "/api/admin/bans/"+uid, nil)
	req2.SetPathValue("uid", uid)
	req2.Header.Set("Cookie", cookie)
	req2.Header.Set("X-CSRF-Token", csrf)
	w2 := httptest.NewRecorder()
	mgr.requireAdmin(http.HandlerFunc(mgr.handleAdminUnban)).ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("解封应 200，得到 %d: %s", w2.Code, w2.Body.String())
	}
	if bans.IsBanned(uid) {
		t.Error("解封后不应再被封禁")
	}
	w = httptest.NewRecorder()
	auth.RequireAuth(next).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("解封后请求应放行，得到 %d", w.Code)
	}
}

func TestBanRequiresReason(t *testing.T) {
	mgr, _, _, _, _, _ := testAdminSetup(t, adminTestUser, adminTestPassword)
	cookie, csrf := adminLogin(t, mgr)
	if w := adminReq(t, mgr, "POST", "/api/admin/bans", `{"uid":"u","duration":"24h","reason":""}`, cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("缺原因应 400，得到 %d", w.Code)
	}
}

func TestBanStorePersistence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "admin-state.json")
	s := NewBanStore(file)
	s.Ban("u1", time.Now().Add(time.Hour), "r1", "admin")
	s.Ban("u2", time.Time{}, "r2", "admin") // 永久
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("封禁应落盘: %v", err)
	}
	s2 := NewBanStore(file)
	if err := s2.Load(); err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !s2.IsBanned("u1") || !s2.IsBanned("u2") {
		t.Error("重启后封禁应保留")
	}
	// 过期自动转历史
	s3 := NewBanStore("")
	s3.Ban("u3", time.Now().Add(-time.Second), "r3", "admin")
	if s3.IsBanned("u3") {
		t.Error("过期封禁不应命中")
	}
	if len(s3.HistoryList()) != 1 {
		t.Error("过期封禁应进历史")
	}
	s3.MergeEnvBans([]string{"env-u", ""})
	if !s3.IsBanned("env-u") {
		t.Error("环境变量兜底名单应合并")
	}
}

func TestResetLimit(t *testing.T) {
	mgr, _, limiter, _, _, _ := testAdminSetup(t, adminTestUser, adminTestPassword)
	cookie, csrf := adminLogin(t, mgr)
	small := newRateLimiter(1, 0)
	// 手动构造一个耗尽的限流器并替换
	mgr.limiter = small
	if small.allow("u:target") {
		// 第一次放行
	}
	if small.allow("u:target") {
		t.Fatal("第二次应被限流")
	}
	_ = limiter
	req := httptest.NewRequest(http.MethodPost, "/api/admin/users/target/reset-limit", nil)
	req.SetPathValue("uid", "target")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	mgr.requireAdmin(http.HandlerFunc(mgr.handleAdminResetLimit)).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reset-limit 应 200，得到 %d", w.Code)
	}
	if !small.allow("u:target") {
		t.Error("清除后应恢复放行")
	}
}

func TestBanStoreCaps(t *testing.T) {
	s := NewBanStore("")
	for i := 0; i < banHistoryCap+50; i++ {
		uid := strings.Repeat("u", 8) + string(rune('0'+i%10)) + string(rune('a'+i%26))
		s.Ban(uid, time.Time{}, "r", "admin")
		s.Unban(uid)
	}
	s.mu.Lock()
	hn, on := len(s.history), len(s.opLog)
	s.mu.Unlock()
	if hn > banHistoryCap || on > banHistoryCap {
		t.Errorf("历史/操作记录应截断在 %d，得到 %d/%d", banHistoryCap, hn, on)
	}
}

// adminSessionCookie 走完整登录流程拿会话 cookie（首页门控测试用）。
func adminSessionCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	body := `{"username":"` + adminTestUser + `","password":"` + adminTestPassword + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("登录应 200，得到 %d: %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "sid" {
			return c
		}
	}
	t.Fatal("登录未返回会话 cookie")
	return nil
}

func testAdminConfig() Config {
	return Config{
		AuthRequired:  true,
		JWTSecret:     string(testSecret),
		WXAppID:       "test-appid",
		WXSecret:      "test-secret",
		AdminEnabled:  true,
		AdminUser:     adminTestUser,
		AdminPassword: adminTestPassword,
	}
}

// TestHomepageLoginGate 管理页启用时：首页默认是登录框，登录后进演示页。
func TestHomepageLoginGate(t *testing.T) {
	cfg := testAdminConfig()
	cfg.AdminDataDir = t.TempDir()
	h := NewWithConfig(cfg)

	// 未登录 → 登录框
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("未登录首页应 200，得到 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "api/admin/login") {
		t.Error("未登录首页应为登录框")
	}

	// 登录后 → 演示页
	cookie := adminSessionCookie(t, h)
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(cookie)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("登录后首页应 200，得到 %d", w2.Code)
	}
	body := w2.Body.String()
	if !strings.Contains(body, "管理页") || !strings.Contains(body, `data-tab="demo"`) {
		t.Error("登录后首页应为含演示模块的管理界面")
	}

	// 管理员会话可直接调业务接口（无需用户 JWT）：不应 401
	req3 := httptest.NewRequest(http.MethodPost, "/api/parse",
		strings.NewReader(`{"url":"https://example.com/x"}`))
	req3.AddCookie(cookie)
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req3)
	if w3.Code == http.StatusUnauthorized {
		t.Error("管理员会话调用业务接口不应 401")
	}
}

// TestHomepageNoAdmin 管理页未启用时保持旧行为。
func TestHomepageNoAdmin(t *testing.T) {
	// 鉴权开启 → 404
	cfg := testAdminConfig()
	cfg.AdminEnabled = false
	cfg.AdminDataDir = t.TempDir()
	h := NewWithConfig(cfg)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("鉴权开启且管理页关闭时首页应 404，得到 %d", w.Code)
	}

	// 鉴权关闭（本地开发）→ 演示页直达
	cfg.AuthRequired = false
	h2 := NewWithConfig(cfg)
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/", nil))
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "万能提取") {
		t.Errorf("本地开发模式首页应直达演示页，得到 %d", w2.Code)
	}
}

func TestAdminUserDetail(t *testing.T) {
	mgr, _, _, _, _, _ := testAdminSetup(t, adminTestUser, adminTestPassword)
	uid := "u1234567890abcdef"
	mgr.audit.Record(AuditEvent{Endpoint: "parse", UID: uid, Platform: "douyin",
		Result: AuditOK, Remark: "2 个媒体"})
	mgr.audit.Record(AuditEvent{Endpoint: "parse", UID: uid, Platform: "douyin",
		Target: "https://v.douyin.com/x/", Result: AuditFail, Remark: "链接失效"})
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/"+uid, nil)
	req.SetPathValue("uid", uid)
	w := httptest.NewRecorder()
	mgr.handleAdminUserDetail(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("用户详情应 200，得到 %d", w.Code)
	}
	body := w.Body.String()
	for _, k := range []string{`"Calls":2`, `"OK":1`, `"Fail":1`, `"remark":"链接失效"`, `"remark":"2 个媒体"`} {
		if !strings.Contains(body, k) {
			t.Errorf("详情 JSON 应包含 %s，得到 %s", k, body)
		}
	}
}
