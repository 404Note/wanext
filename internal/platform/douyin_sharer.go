package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"wanext/internal/httpx"
	"wanext/internal/model"
)

// 抖音分享者身份解析 —— 抖音解析模块的附加功能。
//
// 解析思路与关键参数参考 hiFOFA/douyin-share-resolver
// （https://github.com/hiFOFA/douyin-share-resolver），用 Go 重新实现：
// 抖音 App 生成分享链接时，会把分享者的身份以多层编码写进 URL 归因参数
// （拉新奖励、裂变统计用）。还原链路分四步：
//
//  1. 只跟第一跳 302：归因参数只出现在第一跳的 Location 里，完整跟随重定向反而拿不到；
//  2. 双通道提取分享者 uid 并交叉验证：activity_info（URL 编码 JSON）与 u_code（23 进制编码）；
//  3. 向字节跳动注册接口申请 ttwid，作为 web 接口的通行证；
//  4. 数字 uid 反查公开资料（user/profile/other 接口的 user_id 参数，数字 uid 唯一还活着的 web 入口）。
const (
	douyinTTWIDAPI   = "https://ttwid.bytedance.com/ttwid/union/register/"
	douyinProfileAPI = "https://www.douyin.com/aweme/v1/web/user/profile/other/"
	// douyinEdgeUA 与指纹参数中的 browser_name/version 保持一致。
	douyinEdgeUA  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0"
	uCodeAlphabet = "0123456789abcdefghijklm" // u_code 的 23 进制字符表
)

// douyinBrowserParams 完整的浏览器环境指纹参数：
// user/profile/other 网关会拦截最小参数集，必须带全约 25 个指纹参数。
var douyinBrowserParams = map[string]string{
	"device_platform": "webapp", "aid": "6383", "channel": "channel_pc_web",
	"pc_client_type": "1", "pc_libra_divert": "Windows", "update_version_code": "170400",
	"support_h265": "1", "support_dash": "0", "version_code": "290100",
	"version_name": "29.1.0", "cookie_enabled": "true",
	"screen_width": "1920", "screen_height": "1080",
	"browser_language": "zh-CN", "browser_platform": "Win32",
	"browser_name": "Edge", "browser_version": "131.0.0.0",
	"browser_online": "true", "engine_name": "Blink", "engine_version": "131.0.0.0",
	"os_name": "Windows", "os_version": "10", "cpu_core_num": "12",
	"device_memory": "8", "platform": "PC", "downlink": "10",
	"effective_type": "4g", "round_trip_time": "50",
	// 接口业务参数
	"max_cursor": "0", "locate_query": "false",
	"show_live_replay_strategy": "1", "need_time_list": "1",
	"time_list_query": "0", "whale_cut_token": "",
	"cut_version": "1", "count": "18",
	"publish_video_strategy_type": "2", "from_user_page": "1",
}

var beijingLoc = time.FixedZone("CST", 8*3600)

// DouyinShareResolution 一次分享者身份解析的结果。
type DouyinShareResolution struct {
	VideoID string            `json:"video_id,omitempty"`
	Sharer  *model.SharerInfo `json:"sharer,omitempty"`
	Author  *model.SharerInfo `json:"author,omitempty"`
}

// ResolveSharer 解析分享链接背后的分享者身份（与视频作者）。
// rawURL 应为 v.douyin.com 短链（或包含它的分享文案中提取出的短链）；
// 老版分享 / 私信转发不带归因参数时返回错误，调用方可按需降级。
func (Douyin) ResolveSharer(ctx context.Context, rawURL string) (*DouyinShareResolution, error) {
	// 1. 只跟第一跳 302，读 Location 里的归因参数
	jar, _ := cookiejar.New(nil)
	rawCli := httpx.NewRawClient(jar)
	code, location, err := rawCli.FirstHop(ctx, rawURL, []httpx.OrderedHeader{
		{"User-Agent", douyinEdgeUA},
		{"Accept", "text/html,*/*"},
	})
	if err != nil {
		return nil, fmt.Errorf("获取短链跳转失败: %w", err)
	}
	if location == "" {
		return nil, fmt.Errorf("短链未返回 302（HTTP %d），链接可能已失效或不是短链分享", code)
	}

	// 2. 双通道提取并交叉验证
	attr, err := parseShareAttribution(location)
	if err != nil {
		return nil, err
	}

	// 3. 注册 ttwid
	apiClient := httpx.NewClient()
	apiClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse // ttwid 回调不自动跳转，手动从 Set-Cookie 取
	}
	ttwid, err := registerTTWID(ctx, apiClient)
	if err != nil {
		return nil, err
	}

	// 4. uid 反查身份：分享者与视频作者各查一次
	res := &DouyinShareResolution{VideoID: attr.VideoID}
	res.Sharer = fetchDouyinUser(ctx, apiClient, ttwid, attr.ShareUID)
	if res.Sharer != nil {
		res.Sharer.ShareTime = attr.ShareTime
	}
	if attr.AuthorUID != "" {
		res.Author = fetchDouyinUser(ctx, apiClient, ttwid, attr.AuthorUID)
	}
	if res.Sharer == nil && res.Author == nil {
		return nil, fmt.Errorf("未能查到分享者与作者的公开资料（可能被平台限制）")
	}
	return res, nil
}

// shareAttribution 第一跳归因参数的解析结果。
type shareAttribution struct {
	ShareUID  string // 分享者数字 uid
	AuthorUID string // 视频作者数字 uid（可能缺失）
	ShareTime string // 分享时刻，北京时间 "2006-01-02 15:04:05"（可能缺失）
	VideoID   string // 作品 ID（可能缺失）
}

// parseShareAttribution 从第一跳 Location 的 query 中双通道提取归因信息（纯函数，便于单测）。
func parseShareAttribution(location string) (*shareAttribution, error) {
	u, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("归因跳转地址非法: %w", err)
	}
	q := u.Query()
	attr := &shareAttribution{}
	if m := douyinPathIDRe.FindStringSubmatch(u.Path); len(m) == 2 {
		attr.VideoID = m[1]
	}

	// 通道 A：activity_info（URL 编码的 JSON）
	if raw := q.Get("activity_info"); raw != "" {
		var act map[string]any
		if err := json.Unmarshal([]byte(raw), &act); err == nil {
			attr.ShareUID = anyToString(act["social_share_user_id"])
			attr.AuthorUID = anyToString(act["social_author_id"])
			if ts := anyToString(act["social_share_time"]); ts != "" {
				if sec, err := strconv.ParseInt(ts, 10, 64); err == nil && sec > 0 {
					attr.ShareTime = time.Unix(sec, 0).In(beijingLoc).Format("2006-01-02 15:04:05")
				}
			}
		}
	}

	// 通道 B：u_code（分享者 uid 的 23 进制编码）
	var uCodeUID string
	if code := q.Get("u_code"); code != "" {
		if uid, ok := decodeUCode(code); ok {
			uCodeUID = strconv.FormatInt(uid, 10)
		}
	}

	// 交叉验证：两通道同时存在时解码结果必须一致，否则说明平台已调整参数，不能静默采用
	switch {
	case attr.ShareUID != "" && uCodeUID != "" && attr.ShareUID != uCodeUID:
		return nil, fmt.Errorf("双通道归因参数不一致（activity_info=%s，u_code=%s），平台可能已调整参数", attr.ShareUID, uCodeUID)
	case attr.ShareUID == "" && uCodeUID != "":
		attr.ShareUID = uCodeUID // 通道 A 缺失时用通道 B 兜底
	}
	if attr.ShareUID == "" {
		return nil, fmt.Errorf("链接不携带分享者归因参数（老版分享/私信转发），无法识别分享者")
	}
	return attr, nil
}

// decodeUCode 把 u_code 解码为数字 uid：23 进制，字符表 0-9a-m，从左向右逐位累加。
func decodeUCode(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var uid int64
	for _, ch := range strings.ToLower(s) {
		d := strings.IndexRune(uCodeAlphabet, ch)
		if d < 0 {
			return 0, false
		}
		uid = uid*23 + int64(d)
	}
	return uid, uid > 0
}

// anyToString 把归因 JSON 中的值（字符串或数字）统一转为字符串。
func anyToString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

// registerTTWID 向字节跳动注册接口申请 ttwid，作为后续 web 接口的通行证。
func registerTTWID(ctx context.Context, client *http.Client) (string, error) {
	payload := map[string]any{
		"region": "cn", "aid": 1768, "needFid": false,
		"service":       "www.douyin.com",
		"migrate_info":  map[string]string{"ticket": "", "source": "node"},
		"cbUrlProtocol": "https", "union": true,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, douyinTTWIDAPI, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	setDouyinAPIHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ttwid 注册请求失败: %w", err)
	}
	defer resp.Body.Close()
	var reg struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil || reg.RedirectURL == "" {
		return "", fmt.Errorf("ttwid 注册失败（未返回 redirect_url）")
	}

	// 回调响应的 Set-Cookie 里拿 ttwid（不自动跳转）
	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.RedirectURL, nil)
	if err != nil {
		return "", err
	}
	setDouyinAPIHeaders(req2)
	resp2, err := client.Do(req2)
	if err != nil {
		return "", fmt.Errorf("ttwid 回调请求失败: %w", err)
	}
	defer resp2.Body.Close()
	for _, ck := range resp2.Cookies() {
		if ck.Name == "ttwid" && ck.Value != "" {
			return ck.Value, nil
		}
	}
	return "", fmt.Errorf("未获取到 ttwid cookie")
}

// setDouyinAPIHeaders 设置 user/profile/other 与 ttwid 接口的通用请求头。
func setDouyinAPIHeaders(req *http.Request) {
	req.Header.Set("User-Agent", douyinEdgeUA)
	req.Header.Set("Referer", "https://www.douyin.com/")
	req.Header.Set("Origin", "https://www.douyin.com")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
}

// fetchDouyinUser 用数字 uid 反查公开资料（昵称/抖音号/签名/sec_uid）。
// 查不到（status_code 非 0 / 账号不存在 / 被限制）时返回 nil，不视为错误。
func fetchDouyinUser(ctx context.Context, client *http.Client, ttwid, uid string) *model.SharerInfo {
	params := url.Values{}
	for k, v := range douyinBrowserParams {
		params.Set(k, v)
	}
	params.Set("user_id", uid) // 注意：参数名是 user_id（数字 uid），不是 sec_user_id
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, douyinProfileAPI+"?"+params.Encode(), nil)
	if err != nil {
		return nil
	}
	setDouyinAPIHeaders(req)
	req.AddCookie(&http.Cookie{Name: "ttwid", Value: ttwid})
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var data any
	if err := json.Unmarshal(b, &data); err != nil {
		return nil
	}
	if httpx.I(data, "status_code") != 0 {
		return nil
	}
	user := httpx.J(data, "user")
	if user == nil {
		return nil
	}
	douyinID := httpx.S(user, "unique_id")
	if douyinID == "" {
		douyinID = httpx.S(user, "short_id")
	}
	info := &model.SharerInfo{
		Nickname:  httpx.S(user, "nickname"),
		DouyinID:  douyinID,
		Signature: strings.ReplaceAll(httpx.S(user, "signature"), "\n", " "),
	}
	if info.Nickname == "" {
		return nil
	}
	if secUID := httpx.S(user, "sec_uid"); secUID != "" {
		info.ProfileURL = "https://www.douyin.com/user/" + secUID
	}
	return info
}
