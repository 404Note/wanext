package platform

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// Instagram 解析器：embed/captioned 页面（免登录）→ 内嵌 JSON 中的 video_url / display_url。
// 注意：Instagram CDN 对 UA 指纹敏感，下载时请使用极简 UA（代理层已处理）。
type Instagram struct{}

func (Instagram) Name() string  { return "instagram" }
func (Instagram) Label() string { return "Instagram" }
func (Instagram) MatchHost(host string) bool {
	return strings.Contains(host, "instagram.com")
}

var (
	igCodeRe    = regexp.MustCompile(`/(?:reel|reels|p|tv)/([A-Za-z0-9_-]{5,})/?`)
	igVideoRe   = regexp.MustCompile(`"video_url"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	igDisplayRe = regexp.MustCompile(`"display_url"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	igCaptionRe = regexp.MustCompile(`"edge_media_to_caption"\s*:\s*\{"edges":\s*\[{"node":\s*\{"text":\s*"((?:[^"\\]|\\.){1,300})"`)
	igUserRe    = regexp.MustCompile(`"username"\s*:\s*"([^"]+)"`)
)

func (p Instagram) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	code := FirstSubmatch(igCodeRe, rawURL)
	if code == "" {
		return nil, fmt.Errorf("未能从链接中提取帖子 ID")
	}
	seg := "p"
	for _, s := range []string{"reel", "reels", "tv"} {
		if strings.Contains(rawURL, "/"+s+"/") {
			seg = s
			break
		}
	}
	embedURL := "https://www.instagram.com/" + seg + "/" + code + "/embed/captioned/"
	_, body, err := httpx.Get(ctx, embedURL, map[string]string{
		"User-Agent":      httpx.DesktopUA,
		"Accept-Language": "en-US,en;q=0.9",
	})
	if err != nil {
		return nil, fmt.Errorf("获取帖子页失败: %w", err)
	}
	s := string(body)
	res := &model.Result{
		Platform: p.Name(),
		Title:    httpx.UnquoteJSONString(FirstSubmatch(igCaptionRe, s)),
		Author:   FirstSubmatch(igUserRe, s),
		PageURL:  "https://www.instagram.com/" + seg + "/" + code + "/",
	}
	if res.Title == "" {
		res.Title = "Instagram 帖子 " + code
	}
	if v := httpx.UnquoteJSONString(FirstSubmatch(igVideoRe, s)); v != "" {
		res.Medias = append(res.Medias, model.Media{Kind: "video", DirectURL: v})
	}
	// 多图帖：display_url 可能出现多次（含头像等），取前若干个大图去重
	seen := map[string]bool{}
	for _, m := range AllSubmatches(igDisplayRe, s) {
		u := httpx.UnquoteJSONString(m)
		if u == "" || seen[u] || strings.Contains(u, "profile_pic") {
			continue
		}
		seen[u] = true
		res.Medias = append(res.Medias, model.Media{Kind: "image", DirectURL: u})
		if len(res.Medias) >= 10 {
			break
		}
	}
	if len(res.Medias) == 0 {
		return nil, fmt.Errorf("未找到可下载的媒体（可能需要登录或被限流）")
	}
	return res, nil
}
