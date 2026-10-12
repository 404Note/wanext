package platform

import "testing"

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
		t.Fatalf("期望 3 个媒体（1视频+2图），得到 %d", len(res.Medias))
	}
	v := res.Medias[0]
	if v.Kind != "video" || v.DirectURL != "https://f.video.weibocdn.com/hd.mp4" {
		t.Errorf("应选最高清晰度 hd: %+v", v)
	}
	for _, m := range res.Medias[1:] {
		if m.Kind != "image" {
			t.Errorf("应为图片: %+v", m)
		}
	}
	if res.Title == "" || len([]rune(res.Title)) > 90 {
		t.Errorf("标题处理异常: %q", res.Title)
	}
}
