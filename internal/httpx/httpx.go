// Package httpx 提供各平台解析器共用的 HTTP 客户端与小工具。
package httpx

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

const (
	DesktopUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	IPhoneUA  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
	AndroidUA = "Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Mobile Safari/537.36"
	MinimalUA = "Mozilla/5.0"
)

var DefaultTimeout = 20 * time.Second

// NewClient 返回带 CookieJar、走系统代理(env)、20s 超时的客户端。
//
// TLS 握手使用 Chrome 指纹（uTLS）：抖音等平台按 JA3 指纹区别对待，
// 非浏览器指纹只能拿到空壳页面。实现上有两处关键：
//  1. Go 的 Transport 在 DialTLSContext 存在时会跳过代理 CONNECT，
//     因此 dialFingerprinted 内按 httpproxy 规则手动做 CONNECT；
//  2. 非 *tls.Conn 无法走 HTTP/2，故把 Chrome spec 的 ALPN 改为仅 http/1.1
//    （ALPN 取值不计入 JA3，指纹不受影响）。
func NewClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout: DefaultTimeout,
		Jar:     jar,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			DialTLSContext:        dialFingerprinted,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			MaxIdleConns:          32,
		},
	}
}

var (
	chromeSpecOnce sync.Once
	chromeSpec     utls.ClientHelloSpec
)

// chromeHTTP11Spec 返回 Chrome 124 指纹（curl-impersonate 实测序列）、
// 但 ALPN 仅声明 http/1.1 的 spec。
//
// 扩展顺序来自 2026-10-12 对 curl_cffi 0.16.3 chrome124 的本地实测抓包
// （ClientHello 首 flight 逐字节解析，非 JA3 字符串反推）：
// GREASE, key_share(51), supported_versions(43), psk_modes(45),
// renegotiation_info(65281), ech(65037), supported_groups(10),
// status_request(5), session_ticket(35), sct(18), compress_certificate(27),
// sig_algs(13), SNI(0), ALPS(17513), ems(23), alpn(16), point_formats(11), GREASE.
// 注意两点与直觉相悖的实测事实：SNI 排在第 13 位（非第 2 位）；
// curl-impersonate 会发送 ALPS(17513)=0003026832（ApplicationSettingsExtension
// SupportedProtocols=["h2"] 逐字节复刻）。
// ALPN 取值不计入 JA3，只声明 http/1.1 让 Go Transport 能用 HTTP/1.1 正常收发
// （它无法在非 *tls.Conn 上协商 HTTP/2）。
func chromeHTTP11Spec() utls.ClientHelloSpec {
	chromeSpecOnce.Do(func() {
		chromeSpec = utls.ClientHelloSpec{
			CipherSuites: []uint16{
				utls.GREASE_PLACEHOLDER,
				utls.TLS_AES_128_GCM_SHA256,
				utls.TLS_AES_256_GCM_SHA384,
				utls.TLS_CHACHA20_POLY1305_SHA256,
				utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
				utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
				utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
				utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
				utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
				utls.TLS_RSA_WITH_AES_128_CBC_SHA,
				utls.TLS_RSA_WITH_AES_256_CBC_SHA,
			},
			CompressionMethods: []byte{0x00},
			Extensions: []utls.TLSExtension{
				&utls.UtlsGREASEExtension{},
				&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
					{Group: utls.CurveID(utls.GREASE_PLACEHOLDER), Data: []byte{0}},
					{Group: utls.X25519Kyber768Draft00},
					{Group: utls.X25519},
				}},
				&utls.SupportedVersionsExtension{Versions: []uint16{
					utls.GREASE_PLACEHOLDER,
					utls.VersionTLS13,
					utls.VersionTLS12,
				}},
				&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
				&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
				utls.BoringGREASEECH(),
				&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
					utls.GREASE_PLACEHOLDER,
					utls.X25519Kyber768Draft00,
					utls.X25519,
					utls.CurveP256,
					utls.CurveP384,
				}},
				&utls.StatusRequestExtension{},
				&utls.SessionTicketExtension{},
				&utls.SCTExtension{},
				&utls.UtlsCompressCertExtension{Algorithms: []utls.CertCompressionAlgo{
					utls.CertCompressionBrotli,
				}},
				&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
					utls.ECDSAWithP256AndSHA256,
					utls.PSSWithSHA256,
					utls.PKCS1WithSHA256,
					utls.ECDSAWithP384AndSHA384,
					utls.PSSWithSHA384,
					utls.PKCS1WithSHA384,
					utls.PSSWithSHA512,
					utls.PKCS1WithSHA512,
				}},
				&utls.SNIExtension{},
				&utls.ApplicationSettingsExtension{SupportedProtocols: []string{"h2"}},
				&utls.ExtendedMasterSecretExtension{},
				&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
				&utls.SupportedPointsExtension{SupportedPoints: []byte{0x00}},
				&utls.UtlsGREASEExtension{},
			},
		}
	})
	return chromeSpec
}

// dialFingerprinted 先（按需）经代理 CONNECT，再用 Chrome 指纹完成 TLS 握手。
// 若 utls 握手失败（如 local error: tls: internal error），回退到标准 TLS，
// 保证至少能完成建连并返回可读的业务层错误，而不是直接崩。
func dialFingerprinted(ctx context.Context, network, addr string) (net.Conn, error) {
	rawConn, err := dialPossiblyViaProxy(ctx, addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		rawConn.Close()
		return nil, err
	}
	spec := chromeHTTP11Spec()
	uconn := utls.UClient(rawConn, &utls.Config{ServerName: host}, utls.HelloCustom)
	if err := uconn.ApplyPreset(&spec); err != nil {
		rawConn.Close()
		return nil, err
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		// utls 握手失败：关闭当前连接，重建一条走标准 TLS 的连接
		rawConn.Close()
		rawConn2, err2 := dialPossiblyViaProxy(ctx, addr)
		if err2 != nil {
			return nil, fmt.Errorf("utls握手失败(%v)，重建连接也失败: %w", err, err2)
		}
		tconn := tls.Client(rawConn2, &tls.Config{ServerName: host})
		if err3 := tconn.HandshakeContext(ctx); err3 != nil {
			rawConn2.Close()
			return nil, fmt.Errorf("utls握手失败(%v)，标准TLS也失败: %w", err, err3)
		}
		return tconn, nil
	}
	return uconn, nil
}

// proxyForHost 按 HTTPS_PROXY/https_proxy + NO_PROXY/no_proxy 环境变量返回代理地址。
func proxyForHost(host string) *url.URL {
	raw := firstEnv("HTTPS_PROXY", "https_proxy")
	if raw == "" {
		return nil
	}
	// no_proxy 匹配则直连
	noProxy := firstEnv("NO_PROXY", "no_proxy")
	if noProxy != "" {
		h := strings.ToLower(host)
		for _, p := range strings.Split(noProxy, ",") {
			p = strings.ToLower(strings.TrimSpace(p))
			if p == "" {
				continue
			}
			if p == "*" || h == p || strings.HasSuffix(h, "."+strings.TrimPrefix(p, ".")) {
				return nil
			}
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil
	}
	return u
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// dialPossiblyViaProxy 按环境代理规则决定是否经代理 CONNECT。
func dialPossiblyViaProxy(ctx context.Context, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	proxyURL := proxyForHost(host)
	if proxyURL == nil {
		return dialer.DialContext(ctx, "tcp", addr)
	}
	proxyConn, err := dialer.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}
	// 手动发送 CONNECT
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if ui := proxyURL.User; ui != nil {
		pw, _ := ui.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(ui.Username()+":"+pw)))
	}
	if err := req.Write(proxyConn); err != nil {
		proxyConn.Close()
		return nil, err
	}
	br := bufio.NewReader(proxyConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		proxyConn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		proxyConn.Close()
		return nil, fmt.Errorf("代理 CONNECT 失败: %s", resp.Status)
	}
	if br.Buffered() > 0 {
		// CONNECT 后不应有残留数据
		proxyConn.Close()
		return nil, fmt.Errorf("代理 CONNECT 后有残留数据")
	}
	return proxyConn, nil
}


// NewNoRedirectClient 返回不自动跟随跳转的客户端。
func NewNoRedirectClient() *http.Client {
	c := NewClient()
	c.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return c
}

// Get 发起 GET 请求并跟随跳转，返回最终 URL 与响应体（上限 8MB）。
// 每次调用使用独立 client（cookie 不跨调用保留）；需要 cookie 保持时用 Client.Get。
func Get(ctx context.Context, rawURL string, headers map[string]string) (finalURL string, body []byte, err error) {
	return NewClientWithJar().Get(ctx, rawURL, headers)
}

// Client 是带 cookie 保持的客户端封装，同一实例的多次请求共享 CookieJar。
type Client struct {
	*http.Client
}

// Get 发起 GET 请求并跟随跳转，返回最终 URL 与响应体（上限 8MB）。
func (c *Client) Get(ctx context.Context, rawURL string, headers map[string]string) (finalURL string, body []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DesktopUA)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, rawURL)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", nil, err
	}
	return resp.Request.URL.String(), b, nil
}

// NewClientWithJar 返回带 cookie 保持的 Client。
func NewClientWithJar() *Client {
	return &Client{NewClient()}
}

// Resolve 不跟随跳转，返回跳转目标（绝对 URL）；非跳转响应返回自身 URL。
func Resolve(ctx context.Context, rawURL string, headers map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DesktopUA)
	}
	resp, err := NewNoRedirectClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		if loc == "" {
			return "", fmt.Errorf("跳转响应缺少 Location")
		}
		u, err := url.Parse(loc)
		if err != nil {
			return "", err
		}
		return resp.Request.URL.ResolveReference(u).String(), nil
	}
	return resp.Request.URL.String(), nil
}

// J 在解码后的 JSON（map[string]any / []any）中按路径取值，
// 路径元素为 map 键名或数组下标（十进制字符串）。
func J(v any, path ...string) any {
	cur := v
	for _, p := range path {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[p]
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(c) {
				return nil
			}
			cur = c[i]
		default:
			return nil
		}
		if cur == nil {
			return nil
		}
	}
	return cur
}

// S 按路径取字符串，失败返回 ""。
func S(v any, path ...string) string {
	if s, ok := J(v, path...).(string); ok {
		return s
	}
	return ""
}

// I 按路径取整数（JSON 数字解码为 float64），失败返回 0。
func I(v any, path ...string) int {
	switch n := J(v, path...).(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return 0
}

// Arr 按路径取数组，失败返回 nil。
func Arr(v any, path ...string) []any {
	if a, ok := J(v, path...).([]any); ok {
		return a
	}
	return nil
}

// UnquoteJSONString 还原 JSON 字符串片段中的转义（如 \/、\u0026）。
// 用 encoding/json 而非 strconv.Unquote，因为后者不支持 JSON 特有的 \/ 转义。
func UnquoteJSONString(s string) string {
	if s == "" {
		return ""
	}
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		return s
	}
	return out
}

// RandomHex 返回 n 字节的随机十六进制串。
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

