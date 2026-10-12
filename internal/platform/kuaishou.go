package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// Kuaishou 快手解析器。
//
// 解析策略（参考 https://github.com/x554960766/wechat-mp-tools 的快手模块思路）：
//
//  1. feedbyid 纯 API（主路径）：App 分享短链（v.kuaishou.com/xxx）的 302
//     Location 头里直接带 photoId + userId，不跟进移动端落地页；拿到双 ID 后调用
//     live.kuaishou.com/live_api/profile/feedbyid —— 纯 JSON 接口，无需签名，
//     机房 IP 可用。返回 currentWork：playUrl/mp4Url 视频直链、imgUrls /
//     ext_params.atlas 图集、author.name 作者名。
//  2. 网页 HTML（备选）：裸 short-video 链接只有 photoId 时，尝试解析分享页
//     __APOLLO_STATE__ / photoUrl（住宅 IP 或登录态下有效；机房 IP 会被内容降级）。
//  3. 双 ID 缺失且网页降级时报错，提示改用 App 分享短链。
type Kuaishou struct{}

func (Kuaishou) Name() string  { return "kuaishou" }
func (Kuaishou) Label() string { return "快手" }
func (Kuaishou) MatchHost(host string) bool {
	return strings.Contains(host, "kuaishou.com")
}

const ksFeedByIDAPI = "https://live.kuaishou.com/live_api/profile/feedbyid"

var (
	// 短链 host：v.kuaishou.com / v.kuaishouapp.com / c.kuaishou.com，及 /f/ 路径短链
	ksShortHostRe = regexp.MustCompile(`(?i)^(?:v\.kuaishou\.com|v\.kuaishouapp\.com|c\.kuaishou\.com)$`)
	ksShortPathRe = regexp.MustCompile(`(?i)/f/[A-Za-z0-9_-]+`)

	ksLivePathRe  = regexp.MustCompile(`live\.kuaishou\.com/u/([^/?#\s]+)/([^/?#\s]+)`)
	ksPhotoPathRe = regexp.MustCompile(`/(?:short-video|fw/photo|photo)/([A-Za-z0-9_-]+)`)
	ksBareIDRe    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

	// 网页降级备选：旧的 photoUrl 正则解析
	ksPhotoURLRe = regexp.MustCompile(`"photo(?:H265)?Url"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	ksCaptionRe  = regexp.MustCompile(`"caption"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	ksUserNameRe = regexp.MustCompile(`"userName"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	ksOgTitleRe  = regexp.MustCompile(`<meta[^>]+property="og:title"[^>]+content="([^"]+)"`)
	ksPhotoIDRe  = regexp.MustCompile(`/(?:short-video|f)/([A-Za-z0-9_-]+)`)
)

func (p Kuaishou) Parse(ctx context.Context, rawURL string) (*model.Result, error) {
	target := strings.TrimSpace(rawURL)

	// 短链：跟随 302（只读 Location 头，不请求移动端落地页），从跳转目标取双 ID。
	photoID, userID := ksExtractIDs(target)
	for i := 0; i < 4 && ksIsShortLink(target) && (photoID == "" || userID == ""); i++ {
		loc, err := httpx.Resolve(ctx, target, map[string]string{"User-Agent": httpx.DesktopUA})
		if err != nil || loc == target {
			break
		}
		target = loc
		id, uid := ksExtractIDs(target)
		if id != "" {
			photoID = id
		}
		if uid != "" {
			userID = uid
		}
	}

	if photoID == "" {
		return nil, fmt.Errorf("无法从链接中提取作品 ID")
	}

	// 主路径：双 ID 齐全 → feedbyid 纯 API（无需签名，机房 IP 可用）
	if userID != "" {
		work, err := ksFeedByID(ctx, photoID, userID)
		if err != nil {
			return nil, err
		}
		res := ksWorkToResult(work, photoID, target)
		if len(res.Medias) == 0 {
			return nil, fmt.Errorf("feedbyid 未返回媒体地址（photoId=%s）", photoID)
		}
		return res, nil
	}

	// 备选：裸 short-video 链接只有 photoId，走网页解析（住宅 IP / 登录态有效）
	if res, err := ksParseHTML(ctx, target, photoID); err == nil {
		return res, nil
	} else {
		return nil, fmt.Errorf("缺少作者 ID，裸 short-video 链接无法匿名解析，请使用快手 App 分享的短链（v.kuaishou.com/…)重试")
	}
}

// ksIsShortLink 判断是否为需要跟随 302 取 ID 的短链。
func ksIsShortLink(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if ksShortHostRe.MatchString(u.Hostname()) {
		return true
	}
	return ksShortPathRe.MatchString(u.Path)
}

// ksExtractIDs 从链接（或跳转目标）提取 (photoID, userID)，userID 可能为空。
func ksExtractIDs(rawURL string) (string, string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", ""
	}
	q := u.Query()
	photoID := q.Get("photoId")
	userID := ""
	for _, k := range []string{"userId", "authorId", "principalId"} {
		if v := q.Get(k); v != "" {
			userID = v
			break
		}
	}
	if m := ksLivePathRe.FindStringSubmatch(rawURL); m != nil {
		if userID == "" {
			userID = m[1]
		}
		if photoID == "" {
			photoID = m[2]
		}
	}
	if photoID == "" {
		photoID = FirstSubmatch(ksPhotoPathRe, u.Path)
	}
	if photoID == "" && ksBareIDRe.MatchString(strings.TrimSpace(rawURL)) {
		photoID = strings.TrimSpace(rawURL)
	}
	return photoID, userID
}

// ksFeedByID 调用 live_api/profile/feedbyid 获取作品详情（无需签名）。
func ksFeedByID(ctx context.Context, photoID, userID string) (map[string]any, error) {
	apiURL := ksFeedByIDAPI + "?photoId=" + url.QueryEscape(photoID) +
		"&principalId=" + url.QueryEscape(userID)
	_, body, err := httpx.Get(ctx, apiURL, map[string]string{
		"User-Agent": httpx.DesktopUA,
		"Accept":     "application/json, text/plain, */*",
		"Referer":    "https://www.kuaishou.com/short-video/" + photoID,
	})
	if err != nil {
		return nil, fmt.Errorf("feedbyid 请求失败: %w", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("feedbyid 响应解析失败: %w", err)
	}
	data, _ := resp["data"].(map[string]any)
	result := 0
	if data != nil {
		if f, ok := data["result"].(float64); ok {
			result = int(f)
		}
	}
	work, _ := httpx.J(data, "currentWork").(map[string]any)
	if result != 1 || work == nil {
		switch result {
		case 21, 11:
			return nil, fmt.Errorf("feedbyid result=%d：缺少作者 ID（photoId=%s）", result, photoID)
		case 3:
			return nil, fmt.Errorf("feedbyid result=3：作品不存在、私密或被平台限流（photoId=%s）", photoID)
		default:
			return nil, fmt.Errorf("feedbyid result=%d：获取作品详情失败（photoId=%s）", result, photoID)
		}
	}
	return work, nil
}

// ksWorkToResult 把 feedbyid 的 currentWork 转为解析结果。
func ksWorkToResult(work map[string]any, photoID, pageURL string) *model.Result {
	author, _ := work["author"].(map[string]any)
	authorName := httpx.S(author, "name")
	caption := strings.TrimSpace(httpx.S(work, "caption"))

	title := caption
	if title == "" {
		if authorName != "" {
			title = authorName + "的快手视频"
		} else {
			title = "快手视频 " + photoID
		}
	}

	// 视频直链候选：mp4Url > playUrl > photoUrl
	videoURL := httpx.S(work, "mp4Url")
	if videoURL == "" {
		videoURL = httpx.S(work, "playUrl")
	}
	if videoURL == "" {
		videoURL = httpx.S(work, "photoUrl")
	}

	// 图集：imgUrls（字符串或 {url} 对象），兜底 ext_params.atlas{cdn,list}
	var images []string
	for _, it := range httpx.Arr(work, "imgUrls") {
		switch v := it.(type) {
		case string:
			if v != "" {
				images = append(images, v)
			}
		case map[string]any:
			if u := httpx.S(v, "url"); u != "" {
				images = append(images, u)
			} else if u := httpx.S(v, "cdnUrl"); u != "" {
				images = append(images, u)
			}
		}
	}
	if len(images) == 0 {
		if atlas, ok := httpx.J(work, "ext_params", "atlas").(map[string]any); ok {
			cdn := ""
			switch c := atlas["cdn"].(type) {
			case string:
				cdn = c
			case []any:
				if len(c) > 0 {
					cdn, _ = c[0].(string)
				}
			}
			if cdn != "" {
				for _, p := range httpx.Arr(atlas, "list") {
					if s, ok := p.(string); ok && s != "" {
						images = append(images, "https://"+cdn+s)
					}
				}
			}
		}
	}

	cover := httpx.S(work, "poster")
	res := &model.Result{
		Platform: "kuaishou",
		Title:    title,
		Author:   authorName,
		CoverURL: cover,
		PageURL:  pageURL,
	}
	if len(images) > 0 && videoURL == "" {
		for _, u := range images {
			res.Medias = append(res.Medias, model.Media{Kind: "image", DirectURL: u})
		}
		return res
	}
	if videoURL != "" {
		res.Medias = append(res.Medias, model.Media{Kind: "video", DirectURL: videoURL, CoverURL: cover})
		return res
	}
	// 有图但也有视频（少见）：优先视频，图片丢弃以保持单一类型
	return res
}

// ksParseHTML 备选路径：从分享页 HTML 正则提取 photoUrl（住宅 IP / 登录态下有效）。
func ksParseHTML(ctx context.Context, target, photoID string) (*model.Result, error) {
	finalURL, body, err := httpx.Get(ctx, target, map[string]string{"User-Agent": httpx.DesktopUA})
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
		} else {
			title = "快手视频 " + photoID
		}
	}
	return &model.Result{
		Platform: "kuaishou",
		Title:    title,
		Author:   httpx.UnquoteJSONString(FirstSubmatch(ksUserNameRe, s)),
		PageURL:  finalURL,
		Medias:   []model.Media{{Kind: "video", DirectURL: photoURL}},
	}, nil
}
