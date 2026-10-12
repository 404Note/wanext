package platform

import (
	"net/url"
	"strings"
	"testing"
)

// encodeUCode 是 decodeUCode 的逆操作，仅用于构造测试固件。
func encodeUCode(uid int64) string {
	if uid <= 0 {
		return ""
	}
	var b []byte
	for uid > 0 {
		b = append([]byte{uCodeAlphabet[uid%23]}, b...)
		uid /= 23
	}
	return string(b)
}

func TestDecodeUCodeRoundTrip(t *testing.T) {
	for _, uid := range []int64{1, 123456789, 987654321012345, 1 << 40, 7300000000000000001} {
		enc := encodeUCode(uid)
		if enc == "" {
			t.Fatalf("encodeUCode(%d) 返回空", uid)
		}
		got, ok := decodeUCode(enc)
		if !ok || got != uid {
			t.Errorf("decodeUCode(%q) = %d,%v，期望 %d,true", enc, got, ok, uid)
		}
	}
}

func TestDecodeUCodeInvalid(t *testing.T) {
	for _, s := range []string{"", "xyz!", "abc n", "0123456789abcdefghijklmn"} {
		// 字符表为 0-9a-m：n-z 与符号均为非法
		if _, ok := decodeUCode(s); ok {
			t.Errorf("decodeUCode(%q) 应失败", s)
		}
	}
}

func TestDecodeUCodeCaseInsensitive(t *testing.T) {
	uid := int64(123456789)
	enc := encodeUCode(uid)
	got, ok := decodeUCode(strings.ToUpper(enc))
	if !ok || got != uid {
		t.Errorf("大写 u_code 应同样解码: %q -> %d,%v", strings.ToUpper(enc), got, ok)
	}
}

// buildAttributionLocation 构造第一跳 Location 固件。
func buildAttributionLocation(t *testing.T, activityInfo, uCode string) string {
	t.Helper()
	q := url.Values{}
	if activityInfo != "" {
		q.Set("activity_info", activityInfo)
	}
	if uCode != "" {
		q.Set("u_code", uCode)
	}
	return "https://www.douyin.com/video/7382910471827361/?" + q.Encode()
}

func TestParseShareAttributionBothChannels(t *testing.T) {
	shareUID, authorUID := "123456789", "987654321"
	act := `{"social_share_user_id":"` + shareUID + `","social_author_id":"` + authorUID +
		`","social_share_time":"1746196931","social_share_id":"abc"}`
	loc := buildAttributionLocation(t, act, encodeUCode(123456789))
	attr, err := parseShareAttribution(loc)
	if err != nil {
		t.Fatal(err)
	}
	if attr.ShareUID != shareUID {
		t.Errorf("ShareUID = %q，期望 %q", attr.ShareUID, shareUID)
	}
	if attr.AuthorUID != authorUID {
		t.Errorf("AuthorUID = %q，期望 %q", attr.AuthorUID, authorUID)
	}
	if attr.ShareTime != "2025-05-02 22:42:11" {
		t.Errorf("ShareTime = %q，期望 2025-05-02 22:42:11（北京时间）", attr.ShareTime)
	}
	if attr.VideoID != "7382910471827361" {
		t.Errorf("VideoID = %q", attr.VideoID)
	}
}

func TestParseShareAttributionNumericJSON(t *testing.T) {
	// 归因 JSON 的值可能是数字而非字符串
	act := `{"social_share_user_id":123456789,"social_author_id":987654321,"social_share_time":1746196931}`
	loc := buildAttributionLocation(t, act, encodeUCode(123456789))
	attr, err := parseShareAttribution(loc)
	if err != nil {
		t.Fatal(err)
	}
	if attr.ShareUID != "123456789" || attr.AuthorUID != "987654321" {
		t.Errorf("数字型 JSON 值解析错误: %+v", attr)
	}
}

func TestParseShareAttributionUCodeOnly(t *testing.T) {
	loc := buildAttributionLocation(t, "", encodeUCode(555666777))
	attr, err := parseShareAttribution(loc)
	if err != nil {
		t.Fatal(err)
	}
	if attr.ShareUID != "555666777" {
		t.Errorf("ShareUID = %q，期望 555666777（u_code 兜底）", attr.ShareUID)
	}
	if attr.ShareTime != "" {
		t.Errorf("无 activity_info 时 ShareTime 应为空，得到 %q", attr.ShareTime)
	}
}

func TestParseShareAttributionMismatch(t *testing.T) {
	// 双通道不一致：必须报错，不能静默采用任一通道
	act := `{"social_share_user_id":"111111111","social_share_time":"1746196931"}`
	loc := buildAttributionLocation(t, act, encodeUCode(222222222))
	_, err := parseShareAttribution(loc)
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Errorf("双通道不一致应报错，得到: %v", err)
	}
}

func TestParseShareAttributionMissing(t *testing.T) {
	loc := "https://www.douyin.com/video/7382910471827361/"
	if _, err := parseShareAttribution(loc); err == nil {
		t.Error("无归因参数应报错")
	}
	if _, err := parseShareAttribution("https://www.douyin.com/video/1/?u_code=!!!"); err == nil {
		t.Error("非法 u_code 且无 activity_info 应报错")
	}
}

func TestParseShareAttributionMalformedActivityInfo(t *testing.T) {
	// activity_info 损坏时，用 u_code 兜底而非直接失败
	loc := buildAttributionLocation(t, "not-json{{{", encodeUCode(888999000))
	attr, err := parseShareAttribution(loc)
	if err != nil {
		t.Fatal(err)
	}
	if attr.ShareUID != "888999000" {
		t.Errorf("ShareUID = %q，期望 888999000", attr.ShareUID)
	}
}
