package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func TestIssueVerifyRoundtrip(t *testing.T) {
	tok, err := issueToken("uid-abc", testSecret)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	uid, err := verifyToken(tok, testSecret)
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	if uid != "uid-abc" {
		t.Errorf("sub 不一致: %s", uid)
	}
}

func TestVerifyRejects(t *testing.T) {
	good, _ := issueToken("u1", testSecret)
	parts := strings.Split(good, ".")

	cases := []struct {
		name  string
		token string
	}{
		{"段数不足", "a.b"},
		{"空段", parts[0] + ".." + parts[2]},
		{"篡改载荷", parts[0] + "." + parts[1][:len(parts[1])-1] + "A" + "." + parts[2]},
		{"换密钥", func() string { x, _ := issueToken("u1", []byte("ffffffffffffffffffffffffffffffff")); return x }()},
		{"alg=none", "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJzdWIiOiJ1MSJ9."},
		{"过期", func() string {
			x, _ := issueTokenWithTimes("u1", testSecret, time.Now().Add(-3*time.Hour), time.Now().Add(-time.Hour))
			return x
		}()},
		{"签发于未来超偏差", func() string {
			x, _ := issueTokenWithTimes("u1", testSecret, time.Now().Add(time.Hour), time.Now().Add(3*time.Hour))
			return x
		}()},
	}
	for _, c := range cases {
		if _, err := verifyToken(c.token, testSecret); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}
	// 30 秒时钟偏差内应放行
	justExpired, _ := issueTokenWithTimes("u1", testSecret, time.Now().Add(-2*time.Hour), time.Now().Add(-10*time.Second))
	if _, err := verifyToken(justExpired, testSecret); err != nil {
		t.Errorf("偏差内过期应放行: %v", err)
	}
}

func TestUIDFor(t *testing.T) {
	a := uidFor("appid1", "openid1", testSecret)
	b := uidFor("appid1", "openid1", testSecret)
	if a != b || len(a) != 64 {
		t.Errorf("uid 应稳定且为 64 位 hex: %s", a)
	}
	if c := uidFor("appid2", "openid1", testSecret); c == a {
		t.Error("不同 AppID 的 uid 应不同")
	}
	if uidShort(a) != a[:8] {
		t.Error("uidShort 应取前 8 位")
	}
}

func TestMaskCode(t *testing.T) {
	if got := maskCode("abcdefg"); !strings.HasPrefix(got, "abcd") || strings.Contains(got, "efg") {
		t.Errorf("code 应脱敏: %s", got)
	}
}

// wechatStub 起一个微信接口替身，返回固定响应。
func wechatStub(t *testing.T, body string, delay time.Duration) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	old := wechatAPIBase
	wechatAPIBase = s.URL
	t.Cleanup(func() { wechatAPIBase = old })
	return s
}

func testAuthenticator(required bool) *Authenticator {
	return NewAuthenticator(required, testSecret, "test-appid", "test-secret", NewBanStore(""), newMemAuditor())
}

func doLogin(a *Authenticator, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.handleLogin(w, req)
	return w
}

func TestHandleLoginSuccess(t *testing.T) {
	wechatStub(t, `{"openid":"openid-123","session_key":"sk","errcode":0}`, 0)
	a := testAuthenticator(true)
	w := doLogin(a, `{"code":"fakecode"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"token"`) || !strings.Contains(w.Body.String(), `"expires_in":7200`) {
		t.Errorf("响应缺令牌: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "openid-123") {
		t.Error("响应不应回显 openid")
	}
}

func TestHandleLoginCases(t *testing.T) {
	a := testAuthenticator(true)
	// code 为空
	if w := doLogin(a, `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("空 code 应 400，得到 %d", w.Code)
	}
	// code 无效（40029）
	wechatStub(t, `{"errcode":40029,"errmsg":"invalid code"}`, 0)
	if w := doLogin(a, `{"code":"used"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("40029 应 401，得到 %d", w.Code)
	}
	// 微信限流（45011）
	wechatStub(t, `{"errcode":45011,"errmsg":"frequency limit"}`, 0)
	if w := doLogin(a, `{"code":"x"}`); w.Code != http.StatusBadGateway {
		t.Errorf("45011 应 502，得到 %d", w.Code)
	}
	// 微信超时（替身 sleep 超过客户端 10 秒超时）
	wechatStub(t, `{"openid":"o","errcode":0}`, 11*time.Second)
	if w := doLogin(a, `{"code":"x"}`); w.Code != http.StatusBadGateway {
		t.Errorf("微信超时应 502，得到 %d", w.Code)
	}
	// 被封禁用户登录
	wechatStub(t, `{"openid":"banned-openid","errcode":0}`, 0)
	uid := uidFor("test-appid", "banned-openid", testSecret)
	a.bans.Ban(uid, time.Now().Add(24*time.Hour), "测试封禁", "test")
	if w := doLogin(a, `{"code":"y"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("被封禁登录应 401，得到 %d", w.Code)
	}
	if !strings.Contains(doLogin(a, `{"code":"y"}`).Body.String(), "登录已失效") {
		t.Error("被封禁登录应回中性文案")
	}
}

func TestRequireAuthMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("uid=" + UIDFromContext(r.Context())))
	})
	// 开关开：无令牌 401
	a := testAuthenticator(true)
	req := httptest.NewRequest(http.MethodPost, "/api/parse", nil)
	w := httptest.NewRecorder()
	a.RequireAuth(next).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("无令牌应 401，得到 %d", w.Code)
	}
	// 开关开：坏令牌 401
	req = httptest.NewRequest(http.MethodPost, "/api/parse", nil)
	req.Header.Set("Authorization", "Bearer bad.token.here")
	w = httptest.NewRecorder()
	a.RequireAuth(next).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("坏令牌应 401，得到 %d", w.Code)
	}
	// 开关开：合法令牌放行且 uid 进上下文
	tok, _ := issueToken("user-1", testSecret)
	req = httptest.NewRequest(http.MethodPost, "/api/parse", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	a.RequireAuth(next).ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "uid=user-1" {
		t.Errorf("合法令牌应放行，得到 %d %s", w.Code, w.Body.String())
	}
	// 开关开：被封禁用户 401
	a.bans.Ban("user-1", time.Now().Add(24*time.Hour), "x", "test")
	req = httptest.NewRequest(http.MethodPost, "/api/parse", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	a.RequireAuth(next).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("被封禁应 401，得到 %d", w.Code)
	}
	// 开关关：直通
	a2 := testAuthenticator(false)
	req = httptest.NewRequest(http.MethodPost, "/api/parse", nil)
	w = httptest.NewRecorder()
	a2.RequireAuth(next).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("开关关闭应直通，得到 %d", w.Code)
	}
}
