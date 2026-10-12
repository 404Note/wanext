// Package extract 负责从用户输入中提取链接、识别平台并分发解析。
package extract

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"wanext/internal/model"
	"wanext/internal/platform"
)

// urlRe 匹配输入文本中的 http(s) 链接（兼容中文标点结尾）。
// 链接提取：URL 字符白名单（RFC 3986 可见字符），中文/全角标点等自然截断。
// 避免 "https://v.douyin.com/xxx/复制此链接" 这类无空格粘连把中文吞进 URL 导致解析失败。
// 括号引号等不收录：处理 "(https://…)" "'https://…'" 这类被包裹粘贴的常见情况。
var urlRe = regexp.MustCompile(`https?://[A-Za-z0-9_.~:/?#@!$&*+,;=%-]+`)

// ExtractURLs 从任意文本（完整分享文案）中提取所有去重后的 URL。
func ExtractURLs(text string) []string {
	raw := urlRe.FindAllString(text, -1)
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, u := range raw {
		u = strings.TrimRight(u, ".,;:!?…~·")
		u = strings.TrimSuffix(u, `\`)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// Detect 根据 URL 的 host 识别平台，返回对应的解析器。
func Detect(rawURL string) platform.Parser {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	for _, p := range platform.All() {
		if p.MatchHost(host) {
			return p
		}
	}
	return nil
}

// ParseText 从文本中提取链接并依次尝试解析，返回第一个成功的结果。
func ParseText(ctx context.Context, text string) (*model.Result, error) {
	urls := ExtractURLs(text)
	if len(urls) == 0 {
		return nil, fmt.Errorf("未从输入中找到链接，请粘贴完整的分享链接或分享文案")
	}
	var lastErr error
	for _, u := range urls {
		p := Detect(u)
		if p == nil {
			lastErr = fmt.Errorf("不支持的平台链接: %s", u)
			continue
		}
		res, err := p.Parse(ctx, u)
		if err != nil {
			lastErr = fmt.Errorf("%s 解析失败: %w", p.Label(), err)
			continue
		}
		return res, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("解析失败")
	}
	return nil, lastErr
}

// SupportedPlatforms 返回支持的平台列表（供前端展示）。
func SupportedPlatforms() []map[string]string {
	var out []map[string]string
	for _, p := range platform.All() {
		out = append(out, map[string]string{"name": p.Name(), "label": p.Label()})
	}
	return out
}
