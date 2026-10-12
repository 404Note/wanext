// Package platform 实现各平台的解析器。
package platform

import (
	"context"
	"regexp"
	"strings"

	"wanext/internal/model"
)

// Parser 是单个平台的解析器。
type Parser interface {
	Name() string  // 平台 key，如 "douyin"
	Label() string // 平台中文名，如 "抖音"
	MatchHost(host string) bool
	Parse(ctx context.Context, rawURL string) (*model.Result, error)
}

// All 返回所有已注册的解析器。
func All() []Parser {
	return []Parser{
		Douyin{},
		TikTok{},
		Kuaishou{},
		Xiaohongshu{},
		Weibo{},
		Bilibili{},
		XTwitter{},
		Instagram{},
	}
}

// FirstSubmatch 返回正则在 s 中的第一个分组匹配，失败返回 ""。
func FirstSubmatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

// AllSubmatches 返回正则在 s 中的所有第一个分组匹配。
func AllSubmatches(re *regexp.Regexp, s string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		if len(m) >= 2 {
			out = append(out, m[1])
		}
	}
	return out
}

// ExtractJSON 从 start（应指向 '{'）开始做括号配平，提取完整 JSON 对象。
func ExtractJSON(s string, start int) string {
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// TruncateRunes 按 rune 截断字符串并追加省略号。
func TruncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
