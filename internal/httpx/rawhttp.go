// Package httpx raw HTTP 客户端：手动组装 HTTP/1.1 请求，精确控制头顺序。
// Go 标准 http.Transport 会按字母排序头，无法复刻 Chrome 的头顺序，
// 而抖音的风控会检测头顺序。此处手动写请求行与头。
package httpx

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// OrderedHeader 按顺序排列的 HTTP 头。
type OrderedHeader struct {
	Key   string
	Value string
}

// RawClient 手动 HTTP/1.1 客户端，头顺序完全可控。
type RawClient struct {
	Jar          http.CookieJar
	DialTLS      func(ctx context.Context, addr string) (net.Conn, error)
	MaxRedirects int
}

// NewRawClient 创建使用指纹 dialer 的 RawClient。
func NewRawClient(jar http.CookieJar) *RawClient {
	return &RawClient{
		Jar:          jar,
		DialTLS:      dialFingerprintedForRaw,
		MaxRedirects: 10,
	}
}

// dialFingerprintedForRaw 适配 dialFingerprinted 的签名。
func dialFingerprintedForRaw(ctx context.Context, addr string) (net.Conn, error) {
	return dialFingerprinted(ctx, "tcp", addr)
}

// RawGet 发送 GET 请求，headers 按给定顺序发出，自动跟随重定向，返回最终 URL、body。
func (c *RawClient) RawGet(ctx context.Context, rawURL string, headers []OrderedHeader) (finalURL string, body []byte, err error) {
	cur := rawURL
	for i := 0; i <= c.MaxRedirects; i++ {
		u, err := url.Parse(cur)
		if err != nil {
			return "", nil, err
		}
		code, respHeaders, respBody, err := c.doOnce(ctx, u, headers)
		if err != nil {
			return "", nil, err
		}
		// 存 cookie
		if c.Jar != nil {
			// 构造 http.Response 仅用于 cookie 解析
			hr := &http.Response{Header: respHeaders, Request: &http.Request{URL: u}}
			c.Jar.SetCookies(u, hr.Cookies())
		}
		if code >= 300 && code < 400 {
			loc := respHeaders.Get("Location")
			if loc == "" {
				return "", nil, fmt.Errorf("重定向无 Location")
			}
			nu, err := u.Parse(loc)
			if err != nil {
				return "", nil, err
			}
			cur = nu.String()
			continue
		}
		return cur, respBody, nil
	}
	return "", nil, fmt.Errorf("重定向次数超限")
}

// FirstHop 只发一次 GET，不跟随重定向，返回状态码与 Location 头（已做相对地址展开）。
//
// 抖音短链的归因参数（activity_info / u_code）只出现在第一跳的 Location 里，
// 完整跟随重定向反而拿不到，因此分享者身份解析需要此方法。
func (c *RawClient) FirstHop(ctx context.Context, rawURL string, headers []OrderedHeader) (int, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, "", err
	}
	code, respHeaders, _, err := c.doOnce(ctx, u, headers)
	if err != nil {
		return 0, "", err
	}
	if c.Jar != nil {
		hr := &http.Response{Header: respHeaders, Request: &http.Request{URL: u}}
		c.Jar.SetCookies(u, hr.Cookies())
	}
	loc := respHeaders.Get("Location")
	if loc != "" {
		if nu, err := u.Parse(loc); err == nil {
			loc = nu.String()
		}
	}
	return code, loc, nil
}

func (c *RawClient) doOnce(ctx context.Context, u *url.URL, headers []OrderedHeader) (int, http.Header, []byte, error) {
	addr := u.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr += ":443"
	}
	conn, err := c.DialTLS(ctx, addr)
	if err != nil {
		return 0, nil, nil, err
	}
	defer conn.Close()

	// 组装请求
	var sb strings.Builder
	path := u.RequestURI()
	sb.WriteString("GET " + path + " HTTP/1.1\r\n")
	sb.WriteString("Host: " + u.Host + "\r\n")
	hasAE := false
	// 按给定顺序写头
	for _, h := range headers {
		// Host 已写，跳过
		if strings.EqualFold(h.Key, "Host") {
			continue
		}
		if strings.EqualFold(h.Key, "Accept-Encoding") {
			hasAE = true
		}
		sb.WriteString(h.Key + ": " + h.Value + "\r\n")
	}
	// Cookie
	if c.Jar != nil {
		if cookies := c.Jar.Cookies(u); len(cookies) > 0 {
			var parts []string
			for _, ck := range cookies {
				parts = append(parts, ck.Name+"="+ck.Value)
			}
			sb.WriteString("Cookie: " + strings.Join(parts, "; ") + "\r\n")
		}
	}
	// Chrome 必然发送 Accept-Encoding；调用方未指定时补上 "gzip"，
	// 否则缺失该头本身就是 bot 信号。只声明 gzip 是刻意的：
	// 本函数的解压只处理了 gzip，若声明 br/zstd 而服务端真发 br，
	// 响应体会是压缩字节导致解析错乱。响应仍手动解压
	//（http.ReadResponse 不自动解）。
	if !hasAE {
		sb.WriteString("Accept-Encoding: gzip\r\n")
	}
	sb.WriteString("\r\n")

	if _, err := io.WriteString(conn, sb.String()); err != nil {
		return 0, nil, nil, err
	}

	// 读响应
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	// 手动 gzip 解压（http.ReadResponse 不自动解）
	if strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
		if zr, err := gzip.NewReader(strings.NewReader(string(body))); err == nil {
			if dec, err := io.ReadAll(zr); err == nil {
				body = dec
			}
			zr.Close()
		}
	}
	_ = strconv.Itoa // 占位，避免未使用
	return resp.StatusCode, resp.Header, body, nil
}
