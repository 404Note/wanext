package platform

import (
	"strings"
	"testing"
)

func TestBase62Decode(t *testing.T) {
	// 10 进制 "3" -> 62 进制仍是 "3"
	if n := Base62Decode("3"); n != 3 {
		t.Errorf("Base62Decode(3) = %d", n)
	}
	// "10"（62进制）= 62
	if n := Base62Decode("10"); n != 62 {
		t.Errorf("Base62Decode(10) = %d, want 62", n)
	}
	// 已知微博 mid62 -> mid10：Kzfc9... 形式太长，验证往返一致性
	mid62 := "M7y9aBcDeF"
	n := Base62Decode(mid62)
	if n <= 0 {
		t.Errorf("解码失败: %s", mid62)
	}
}

func TestExtractWeiboMID(t *testing.T) {
	cases := []struct{ url, want string }{
		// 纯数字 ID 必须原样使用，不能被当成 base62 解码（回归测试）
		{"https://weibo.com/6133795297/5319988795934006", "5319988795934006"},
		{"https://m.weibo.cn/status/5319988795934006", "5319988795934006"},
		{"https://m.weibo.cn/detail/5319988795934006", "5319988795934006"},
		{"https://weibo.com/detail/5319988795934006", "5319988795934006"},
		{"https://weibo.com/u/1234567890", ""},         // 个人主页，无状态 ID
		{"https://mapp.api.weibo.cn/fx/xxxx.html", ""}, // fx 链接需先解析跳转
		{"https://weibo.com/", ""},
	}
	for _, c := range cases {
		if got := extractWeiboMID(c.url); got != c.want {
			t.Errorf("extractWeiboMID(%q) = %q，期望 %q", c.url, got, c.want)
		}
	}
	// base62 往返：编码后应能解回
	mid10 := "5319988795934006"
	mid62 := encodeBase62(mid10)
	got := extractWeiboMID("https://weibo.com/1234567/" + mid62)
	if got != mid10 {
		t.Errorf("base62 %q 应解码为 %q，得到 %q", mid62, mid10, got)
	}
}

// encodeBase62 仅用于构造测试固件。
func encodeBase62(mid10 string) string {
	n := int64(0)
	for _, c := range mid10 {
		n = n*10 + int64(c-'0')
	}
	var b []byte
	for n > 0 {
		b = append([]byte{weiboBase62[n%62]}, b...)
		n /= 62
	}
	return string(b)
}

func TestParseWeiboStatus(t *testing.T) {
	fixture := `{
		"ok": 1,
		"text_raw": "测试微博 <a href=\"/n/x\">@某人</a> 看视频",
		"user": {"screen_name": "测试博主", "idstr": "1234567890"},
		"pic_ids": ["006AbCdEfGy1h12345", "006XyZ1234gy1h67890"],
		"page_info": {
			"type": "video",
			"title": "测试视频",
			"page_pic": {"url": "https://wx2.sinaimg.cn/large/cover.jpg"},
			"media_info": {
				"urls": {
					"mp4_sd_mp4": "https://f.video.weibocdn.com/sd.mp4",
					"mp4_720p_mp4": "https://f.video.weibocdn.com/720p.mp4",
					"mp4_hd_mp4": "https://f.video.weibocdn.com/hd.mp4"
				}
			}
		}
	}`
	res, err := parseWeiboStatus([]byte(fixture), "4791234567890123", "weibo")
	if err != nil {
		t.Fatal(err)
	}
	if res.Author != "测试博主" {
		t.Errorf("作者错误: %s", res.Author)
	}
	if len(res.Medias) != 3 {
		t.Fatalf("期望 3 个媒体（2图+1视频），得到 %d", len(res.Medias))
	}
	// 旧格式按参考实现顺序：pic_ids 图片在前，page_info 视频在后
	for _, m := range res.Medias[:2] {
		if m.Kind != "image" {
			t.Errorf("应为图片: %+v", m)
		}
	}
	v := res.Medias[2]
	if v.Kind != "video" || v.DirectURL != "https://f.video.weibocdn.com/hd.mp4" {
		t.Errorf("应选最高清晰度 hd: %+v", v)
	}
	if res.Title == "" || len([]rune(res.Title)) > 90 {
		t.Errorf("标题处理异常: %q", res.Title)
	}
}

func TestParseWeiboStatusMixMedia(t *testing.T) {
	// 新版 mix_media_info：pic 取 largest.url，video 取 media_info 直挂的 mp4_hd_url
	fixture := `{
		"ok": 1,
		"text_raw": "图文视频混排",
		"user": {"screen_name": "博主", "idstr": "123"},
		"mix_media_info": {"items": [
			{"type": "pic", "data": {"largest": {"url": "https://wx2.sinaimg.cn/large/a.jpg"}}},
			{"type": "video", "data": {"media_info": {
				"mp4_hd_url": "https://f.video.weibocdn.com/hd.mp4",
				"mp4_720p_mp4": "https://f.video.weibocdn.com/720p.mp4",
				"stream_url": "https://f.video.weibocdn.com/stream.mp4",
				"urls": {}
			}}},
			{"type": "pic", "data": {"largest": {"url": "https://wx2.sinaimg.cn/large/a.jpg"}}}
		]}
	}`
	res, err := parseWeiboStatus([]byte(fixture), "123456", "weibo")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Medias) != 2 {
		t.Fatalf("期望 2 个媒体（去重后 1 图 1 视频），得到 %d: %+v", len(res.Medias), res.Medias)
	}
	if res.Medias[0].Kind != "image" || res.Medias[0].DirectURL != "https://wx2.sinaimg.cn/large/a.jpg" {
		t.Errorf("图片应取 largest.url: %+v", res.Medias[0])
	}
	if res.Medias[1].Kind != "video" || res.Medias[1].DirectURL != "https://f.video.weibocdn.com/hd.mp4" {
		t.Errorf("视频应优先 mp4_hd_url: %+v", res.Medias[1])
	}
}

func TestParseWeiboStatusPicInfos(t *testing.T) {
	// 旧版 pic_ids + pic_infos：largest.url 优先，gif 取 pic.video
	fixture := `{
		"ok": 1,
		"text_raw": "旧版图文",
		"user": {"screen_name": "博主", "idstr": "123"},
		"pic_ids": ["p1", "p2"],
		"pic_infos": {
			"p1": {"largest": {"url": "https://wx1.sinaimg.cn/large/p1.jpg"}},
			"p2": {"type": "gif", "video": "https://f.video.weibocdn.com/g.gif.mp4"}
		}
	}`
	res, err := parseWeiboStatus([]byte(fixture), "123456", "weibo")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Medias) != 2 {
		t.Fatalf("期望 2 个媒体，得到 %d", len(res.Medias))
	}
	if res.Medias[0].DirectURL != "https://wx1.sinaimg.cn/large/p1.jpg" {
		t.Errorf("应取 pic_infos largest.url: %s", res.Medias[0].DirectURL)
	}
	if res.Medias[1].Kind != "video" || res.Medias[1].DirectURL != "https://f.video.weibocdn.com/g.gif.mp4" {
		t.Errorf("gif 应取 pic.video: %+v", res.Medias[1])
	}
}

func TestParseWeiboStatusErrorMsg(t *testing.T) {
	_, err := parseWeiboStatus([]byte(`{"ok": 0, "msg": "登录后可见"}`), "1", "weibo")
	if err == nil || !strings.Contains(err.Error(), "登录后可见") {
		t.Errorf("应透出接口错误信息，得到: %v", err)
	}
}

func TestPickWeiboVideoURL(t *testing.T) {
	// 新版：直接键优先，urls 为空也不影响
	mi := map[string]any{
		"mp4_hd_url": "https://x/hd.mp4",
		"stream_url": "https://x/stream.mp4",
		"urls":       map[string]any{},
	}
	if got := pickWeiboVideoURL(mi); got != "https://x/hd.mp4" {
		t.Errorf("应优先 mp4_hd_url，得到 %s", got)
	}
	// 旧版：回退 urls 子对象
	mi2 := map[string]any{
		"urls": map[string]any{"mp4_720p_mp4": "https://x/720p.mp4", "mp4_sd_mp4": "https://x/sd.mp4"},
	}
	if got := pickWeiboVideoURL(mi2); got != "https://x/720p.mp4" {
		t.Errorf("旧版应取 urls.mp4_720p_mp4，得到 %s", got)
	}
	if pickWeiboVideoURL(nil) != "" {
		t.Error("nil 应返回空")
	}
}
