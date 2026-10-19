// Package server 鉴权模块：微信登录换 openid、JWT 签发与验签、鉴权中间件。
//
// 设计要点（见《wx-mp API 认证授权设计文档》第 5、6 节）：
//   - 前端不持有任何秘密，只持有服务端签发的短期 JWT；
//   - JWT 用标准库自研 HS256 实现，go.mod 不新增依赖；
//   - 日志与令牌中不用原始 openid，而用 HMAC 摘要 uid。
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wanext/internal/httpx"
)

const (
	jwtIssuer   = "wanext"
	jwtAudience = "wx-mp"
	// tokenTTL 访问令牌有效期（A3：2 小时）。
	tokenTTL = 2 * time.Hour
	// clockSkew 验签允许的时钟偏差。
	clockSkew = 30 * time.Second
	// loginBodyLimit 登录请求体上限 4KB（比 parse 的 8KB 更收紧）。
	loginBodyLimit = 4 << 10
	// loginMinuteLimit 登录接口按 IP 限流：30 次/分钟。
	loginMinuteLimit = 30
)

// wechatAPIBase 微信登录凭证校验接口基地址，做成包级变量供测试注入替身。
var wechatAPIBase = "https://api.weixin.qq.com"

// uidCtxKey 请求上下文中的用户标识键。
type uidCtxKey struct{}

// UIDFromContext 取出鉴权中间件放入的用户标识；开关关闭或未鉴权时为空。
func UIDFromContext(ctx context.Context) string {
	if uid, ok := ctx.Value(uidCtxKey{}).(string); ok {
		return uid
	}
	return ""
}

// jwtClaims 令牌载荷。
type jwtClaims struct {
	Sub string `json:"sub"`
	Iss string `json:"iss"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

// issueToken 签发 JWT。
func issueToken(uid string, secret []byte) (string, error) {
	now := time.Now()
	return issueTokenWithTimes(uid, secret, now, now.Add(tokenTTL))
}

// issueTokenWithTimes 按指定签发/过期时刻签发（供测试构造边界令牌）。
func issueTokenWithTimes(uid string, secret []byte, iat, exp time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(jwtClaims{
		Sub: uid, Iss: jwtIssuer, Aud: jwtAudience,
		Iat: iat.Unix(), Exp: exp.Unix(), Jti: httpx.RandomHex(16),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signing + "." + sig, nil
}

var (
	errTokenMalformed = errors.New("令牌格式错误")
	errTokenAlg       = errors.New("令牌算法不被接受")
	errTokenSignature = errors.New("令牌签名无效")
	errTokenClaims    = errors.New("令牌声明无效")
	errTokenExpired   = errors.New("令牌已过期")
)

// verifyToken 按固定顺序验签：结构→alg→签名→iss/aud→exp。
// 签名未通过之前不信任任何声明内容。
func verifyToken(token string, secret []byte) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", errTokenMalformed
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if hb, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil ||
		json.Unmarshal(hb, &hdr) != nil || hdr.Alg != "HS256" {
		return "", errTokenAlg
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return "", errTokenSignature
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errTokenMalformed
	}
	var c jwtClaims
	if err := json.Unmarshal(pb, &c); err != nil || c.Sub == "" {
		return "", errTokenClaims
	}
	if c.Iss != jwtIssuer || c.Aud != jwtAudience {
		return "", errTokenClaims
	}
	if time.Unix(c.Exp, 0).Before(time.Now().Add(-clockSkew)) {
		return "", errTokenExpired
	}
	if time.Unix(c.Iat, 0).After(time.Now().Add(clockSkew)) {
		return "", errTokenClaims
	}
	return c.Sub, nil
}

// uidFor 计算用户标识：HMAC-SHA256(JWT_SECRET, AppID + openid) 的十六进制摘要。
// 同一用户稳定可追，不同应用间不可关联，日志泄露不暴露 openid（A11）。
func uidFor(appID, openid string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(appID + openid))
	return hex.EncodeToString(mac.Sum(nil))
}

// uidShort 取 uid 前 8 位作日志短码，足够定位、不落地完整标识。
func uidShort(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// maskCode code 脱敏：只记前 4 位加长度，绝不记全文。
func maskCode(code string) string {
	if len(code) <= 4 {
		return fmt.Sprintf("****(len=%d)", len(code))
	}
	return fmt.Sprintf("%s****(len=%d)", code[:4], len(code))
}

// wechatError 微信接口返回的业务错误。
type wechatError struct {
	Code int
	Msg  string
}

func (e *wechatError) Error() string {
	return fmt.Sprintf("微信接口错误 errcode=%d errmsg=%s", e.Code, e.Msg)
}

// exchangeCode 用一次性 code 向微信换 openid。
// 微信返回 errcode!=0 时返回 *wechatError；网络/超时/解析失败返回普通 error。
func exchangeCode(ctx context.Context, appID, appSecret, code string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wechatAPIBase+"/sns/jscode2session", nil)
	if err != nil {
		return "", err
	}
	q := req.URL.Query()
	q.Set("appid", appID)
	q.Set("secret", appSecret)
	q.Set("js_code", code)
	q.Set("grant_type", "authorization_code")
	req.URL.RawQuery = q.Encode()

	client := &http.Client{
		Timeout: 10 * time.Second,
		// 不跟随到非微信域名的重定向。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if h := strings.ToLower(req.URL.Hostname()); !strings.HasSuffix(h, ".qq.com") && h != "qq.com" {
				return errors.New("重定向到非微信域名，已拦截")
			}
			if len(via) >= 3 {
				return errors.New("重定向次数过多")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		OpenID  string `json:"openid"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("解析微信响应失败: %w", err)
	}
	if out.ErrCode != 0 {
		return "", &wechatError{Code: out.ErrCode, Msg: out.ErrMsg}
	}
	if out.OpenID == "" {
		return "", errors.New("微信未返回 openid")
	}
	return out.OpenID, nil
}

// Authenticator 收口鉴权逻辑：登录、中间件、封禁校验。
type Authenticator struct {
	required     bool
	secret       []byte
	appID        string
	appSecret    string
	bans         *BanStore
	audit        *Auditor
	loginLimiter *rateLimiter
}

// NewAuthenticator 构造鉴权器；secret 为 JWT 签名密钥。
func NewAuthenticator(required bool, secret []byte, appID, appSecret string, bans *BanStore, audit *Auditor) *Authenticator {
	return &Authenticator{
		required:     required,
		secret:       secret,
		appID:        appID,
		appSecret:    appSecret,
		bans:         bans,
		audit:        audit,
		loginLimiter: newRateLimiter(loginMinuteLimit, 0),
	}
}

// bearerToken 从 Authorization 头取 Bearer 令牌。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// RequireAuth 鉴权中间件：开关关闭时直通；开启时验签 Bearer 令牌，
// 通过后把 uid 放入请求上下文。失败统一 401，不区分原因。
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.required {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		deny := func(reason string) {
			a.audit.Record(AuditEvent{
				Endpoint: "auth", UID: "", IP: clientIP(r),
				Result: "deny401", Reason: reason,
				LatencyMs: time.Since(start).Milliseconds(),
			})
			log.Printf("[auth] 401 %s %s", r.URL.Path, reason)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "登录已失效，请重试"})
		}
		tok := bearerToken(r)
		if tok == "" {
			deny("无令牌")
			return
		}
		uid, err := verifyToken(tok, a.secret)
		if err != nil {
			deny("令牌无效:" + err.Error())
			return
		}
		if a.bans.IsBanned(uid) {
			deny("用户被封禁")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), uidCtxKey{}, uid)))
	})
}

// validBearerForMedia 供媒体接口的「带头则校验」规则：
// 返回 (uid, ok)。无头或头非法时 ok=false，调用方按能力令牌放行。
func (a *Authenticator) validBearerForMedia(r *http.Request) (string, bool) {
	tok := bearerToken(r)
	if tok == "" {
		return "", false
	}
	uid, err := verifyToken(tok, a.secret)
	if err != nil || a.bans.IsBanned(uid) {
		return "", false
	}
	return uid, true
}

// handleLogin POST /api/login：唯一不需要令牌的业务接口。
func (a *Authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ip := clientIP(r)
	if !a.loginLimiter.allow(ip) {
		a.audit.Record(AuditEvent{Endpoint: "login", IP: ip, Result: "limit429", Reason: "登录限流", LatencyMs: time.Since(start).Milliseconds()})
		log.Printf("[auth] 429 登录限流 ip=%s", ip)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "操作太频繁，请稍后再试"})
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, loginBodyLimit)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体必须是 JSON：{\"code\":\"...\"}"})
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "code 不能为空"})
		return
	}
	if !a.required {
		// 开关关闭时登录未启用（本地开发走直通模式）。
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "登录未启用"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	openid, err := exchangeCode(ctx, a.appID, a.appSecret, req.Code)
	if err != nil {
		var we *wechatError
		if errors.As(err, &we) {
			log.Printf("[auth] 微信换 openid 失败 code=%s errcode=%d errmsg=%s", maskCode(req.Code), we.Code, we.Msg)
			a.audit.Record(AuditEvent{Endpoint: "login", IP: ip, Result: "fail", Reason: fmt.Sprintf("微信errcode=%d", we.Code), LatencyMs: time.Since(start).Milliseconds()})
			// 40029: code 无效或已用过 → 前端重新 wx.login 取新 code 重试一次。
			if we.Code == 40029 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "登录已失效，请重试"})
				return
			}
		} else {
			log.Printf("[auth] 微信接口调用失败 code=%s err=%v", maskCode(req.Code), err)
		}
		a.audit.Record(AuditEvent{Endpoint: "login", IP: ip, Result: "fail", Reason: "微信接口异常", LatencyMs: time.Since(start).Milliseconds()})
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "登录服务暂时不可用，请稍后重试"})
		return
	}
	uid := uidFor(a.appID, openid, a.secret)
	if a.bans.IsBanned(uid) {
		// 封禁用户：回与普通登录失败一致的中性文案，不给明确信号。
		log.Printf("[auth] 被封禁用户尝试登录 uid=%s", uidShort(uid))
		a.audit.Record(AuditEvent{Endpoint: "login", UID: uid, IP: ip, Result: "banned", Reason: "封禁命中", LatencyMs: time.Since(start).Milliseconds()})
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "登录已失效，请重试"})
		return
	}
	token, err := issueToken(uid, a.secret)
	if err != nil {
		log.Printf("[auth] 签发令牌失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "登录服务暂时不可用，请稍后重试"})
		return
	}
	log.Printf("[auth] 登录成功 uid=%s", uidShort(uid))
	a.audit.Record(AuditEvent{Endpoint: "login", UID: uid, IP: ip, Result: "ok", LatencyMs: time.Since(start).Milliseconds()})
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires_in": int(tokenTTL / time.Second)})
}
