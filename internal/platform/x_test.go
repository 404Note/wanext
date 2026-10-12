package platform

import "testing"

func TestParseXTweet(t *testing.T) {
	fixture := `{
		"id": "1234567890",
		"text": "测试推文内容",
		"user_name": "Test User",
		"user_screen_name": "testuser",
		"mediaURLs": ["https://pbs.twimg.com/media/img1.jpg", "https://video.twimg.com/v.mp4"],
		"media_extended": [
			{"type": "video", "url": "https://video.twimg.com/v_hd.mp4", "thumbnail_url": "https://pbs.twimg.com/thumb.jpg", "width": 1280, "height": 720},
			{"type": "image", "url": "https://pbs.twimg.com/media/img1.jpg:orig", "width": 1024, "height": 768}
		]
	}`
	res, err := parseXTweet([]byte(fixture), "https://x.com/testuser/status/1234567890", "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Medias) != 2 {
		t.Fatalf("期望 2 个媒体，得到 %d", len(res.Medias))
	}
	if res.Medias[0].Kind != "video" || res.Medias[0].DirectURL != "https://video.twimg.com/v_hd.mp4" {
		t.Errorf("视频解析错误: %+v", res.Medias[0])
	}
	if res.Medias[1].Kind != "image" {
		t.Errorf("图片解析错误: %+v", res.Medias[1])
	}
	if res.Author == "" || res.Title == "" {
		t.Errorf("标题/作者缺失: %+v", res)
	}
}

func TestParseXTweetFallback(t *testing.T) {
	// 无 media_extended 时走 mediaURLs 兜底
	fixture := `{"id":"1","text":"hi","user_name":"u","user_screen_name":"u","mediaURLs":["https://pbs.twimg.com/media/a.jpg"]}`
	res, err := parseXTweet([]byte(fixture), "https://x.com/u/status/1", "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Medias) != 1 || res.Medias[0].Kind != "image" {
		t.Errorf("兜底失败: %+v", res.Medias)
	}
}

func TestParseXTweetNewFormat(t *testing.T) {
	// 新版 fxtwitter 嵌套格式：{"tweet": {...}}
	fixture := `{
		"code": 200,
		"tweet": {
			"id": "1692260577866092863",
			"text": "Long duration test fire",
			"author": {"name": "SpaceX", "screen_name": "SpaceX"},
			"media": {
				"videos": [
					{"type": "video", "url": "https://video.twimg.com/v_hd.mp4", "thumbnail_url": "https://pbs.twimg.com/thumb.jpg", "width": 1920, "height": 1080}
				],
				"photos": [
					{"url": "https://pbs.twimg.com/media/img1.jpg", "width": 1024, "height": 768}
				]
			}
		}
	}`
	res, err := parseXTweet([]byte(fixture), "https://x.com/SpaceX/status/1692260577866092863", "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Medias) != 2 {
		t.Fatalf("期望 2 个媒体，得到 %d", len(res.Medias))
	}
	if res.Medias[0].Kind != "video" || res.Medias[0].DirectURL != "https://video.twimg.com/v_hd.mp4" {
		t.Errorf("视频解析错误: %+v", res.Medias[0])
	}
	if res.Medias[1].Kind != "image" {
		t.Errorf("图片解析错误: %+v", res.Medias[1])
	}
	if res.Author != "SpaceX (@SpaceX)" {
		t.Errorf("作者解析错误: %q", res.Author)
	}
}
