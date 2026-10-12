package platform

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// Kuaishou 快手解析器：短链跳转 → 视频页 HTML → 提取 photoUrl。
type Kuaishou struct{}

func (Kuaishou) Name() string  { return "kuaishou" }
func (Kuaishou) Label() string { return "快手" }
func (Kuaishou) MatchHost(host string) bool {
	return strings.Contains(host, "kuaishou.com")
}

var (
	ksPhotoURLRe = regexp.MustCompile(`"photo(?:H265)?Url"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	ksCaptionRe  = regexp.MustCompile(`"caption"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	ksUserNameRe = regexp.MustCompile(`"userName"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	ksOgTitleRe  = regexp.MustCompile(`<meta[^>]+property="og:title"[^>]+content="([^"]+)"`)
	ksPhotoIDRe  = regexp.MustCompile(`/(?:short-video|f)/([A-Za-z0-9_-]+)`)
)

func (p Kuaishou) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	finalURL, body, err := httpx.Get(ctx, rawURL, map[string]string{"User-Agent": httpx.DesktopUA})
	if err != nil {
		return nil, fmt.Errorf("获取分享页失败: %w", err)
	}
	s := string(body)
	photoURL := ""
	for _, m := range AllSubmatches(ksPhotoURLRe, s) {
		u := httpx.UnquoteJSONString(m)
		if u != "" {
			photoURL = u
			break
		}
	}
	if photoURL == "" {
		return nil, fmt.Errorf("页面未找到视频地址（可能被反爬拦截）")
	}
	title := httpx.UnquoteJSONString(FirstSubmatch(ksCaptionRe, s))
	if title == "" {
		title = FirstSubmatch(ksOgTitleRe, s)
	}
	if title == "" {
		if id := FirstSubmatch(ksPhotoIDRe, finalURL); id != "" {
			title = "快手视频 " + id
		}
	}
	return &model.Result{
		Platform: p.Name(),
		Title:    title,
		Author:   httpx.UnquoteJSONString(FirstSubmatch(ksUserNameRe, s)),
		PageURL:  finalURL,
		Medias:   []model.Media{{Kind: "video", DirectURL: photoURL}},
	}, nil
}
