package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// XTwitter 解析器：通过 vxtwitter 公开 API 获取推文媒体（选最高码率 MP4）。
type XTwitter struct{}

func (XTwitter) Name() string  { return "x" }
func (XTwitter) Label() string { return "X (Twitter)" }
func (XTwitter) MatchHost(host string) bool {
	return host == "x.com" || host == "twitter.com" ||
		strings.HasSuffix(host, ".x.com") || strings.HasSuffix(host, ".twitter.com")
}

var xStatusRe = regexp.MustCompile(`(?:x|twitter)\.com/([A-Za-z0-9_]{1,15})/status/(\d{5,})`)

func (p XTwitter) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	m := xStatusRe.FindStringSubmatch(rawURL)
	if len(m) != 3 {
		return nil, fmt.Errorf("未能从链接中提取推文 ID")
	}
	user, statusID := m[1], m[2]
	api := "https://api.vxtwitter.com/" + user + "/status/" + statusID
	_, body, err := httpx.Get(ctx, api, map[string]string{
		"User-Agent": httpx.DesktopUA,
		"Accept":     "application/json",
	})
	if err != nil {
		return nil, fmt.Errorf("获取推文数据失败: %w", err)
	}
	return parseXTweet(body, rawURL, p.Name())
}

// parseXTweet 解析 vxtwitter API 响应（纯函数，便于单测）。
func parseXTweet(body []byte, pageURL, platformName string) (*model.Result, error) {
	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("解析推文数据失败: %w", err)
	}
	res := &model.Result{
		Platform: platformName,
		Title:    TruncateRunes(httpx.S(data, "text"), 80),
		Author:   httpx.S(data, "user_name") + " (@" + httpx.S(data, "user_screen_name") + ")",
		PageURL:  pageURL,
	}
	if res.Title == "" {
		res.Title = "X 推文 " + httpx.S(data, "id")
	}
	for _, m := range httpx.Arr(data, "media_extended") {
		typ := httpx.S(m, "type")
		u := httpx.S(m, "url")
		if u == "" {
			continue
		}
		switch typ {
		case "video", "gif":
			res.Medias = append(res.Medias, model.Media{
				Kind: "video", DirectURL: u,
				CoverURL: httpx.S(m, "thumbnail_url"),
				Width:    httpx.I(m, "width"), Height: httpx.I(m, "height"),
			})
		case "image":
			res.Medias = append(res.Medias, model.Media{
				Kind: "image", DirectURL: u,
				Width: httpx.I(m, "width"), Height: httpx.I(m, "height"),
			})
		}
	}
	// 兜底：mediaURLs 只有直接链接时
	if len(res.Medias) == 0 {
		for _, u := range httpx.Arr(data, "mediaURLs") {
			if s, ok := u.(string); ok && s != "" {
				kind := "image"
				if strings.Contains(s, ".mp4") {
					kind = "video"
				}
				res.Medias = append(res.Medias, model.Media{Kind: kind, DirectURL: s})
			}
		}
	}
	if len(res.Medias) == 0 {
		return nil, fmt.Errorf("该推文没有可下载的媒体")
	}
	return res, nil
}
