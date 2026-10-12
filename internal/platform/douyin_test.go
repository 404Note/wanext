package platform

import (
	"strings"
	"testing"
)

const douyinItemInfoFixture = `{
  "status_code": 0,
  "item_list": [{
    "aweme_id": "7382910471827361",
    "desc": "测试视频标题",
    "author": {"nickname": "测试作者"},
    "video": {
      "play_addr": {"url_list": [
        "https://v3-dy-o.zjcdn.com/abc/playwm/xyz.mp4",
        "https://v26-web.douyinvod.com/def/playwm/xyz.mp4"
      ]},
      "cover": {"url_list": ["https://p3-sign.douyinpic.com/cover.jpg"]}
    }
  }]
}`

const douyinImagesFixture = `{
  "status_code": 0,
  "item_list": [{
    "aweme_id": "7382910471827362",
    "desc": "测试图集",
    "author": {"nickname": "图作者"},
    "video": {"play_addr": {"url_list": []}, "cover": {"url_list": []}},
    "images": [
      {"url_list": ["https://p3-sign.douyinpic.com/img1.jpg"]},
      {"url_list": ["https://p3-sign.douyinpic.com/img2.jpg"]}
    ]
  }]
}`

func TestParseDouyinItemInfoVideo(t *testing.T) {
	res, err := parseDouyinItemInfo([]byte(douyinItemInfoFixture), "douyin")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "测试视频标题" || res.Author != "测试作者" {
		t.Errorf("标题/作者错误: %+v", res)
	}
	if len(res.Medias) != 2 {
		t.Fatalf("期望 2 个视频地址，得到 %d", len(res.Medias))
	}
	for _, m := range res.Medias {
		if m.Kind != "video" {
			t.Errorf("类型应为 video: %+v", m)
		}
		if strings.Contains(m.DirectURL, "playwm") {
			t.Errorf("仍含 playwm（未去水印）: %s", m.DirectURL)
		}
		if !strings.Contains(m.DirectURL, "/play/") {
			t.Errorf("playwm 未被替换为 play: %s", m.DirectURL)
		}
	}
}

func TestParseDouyinItemInfoImages(t *testing.T) {
	res, err := parseDouyinItemInfo([]byte(douyinImagesFixture), "douyin")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Medias) != 2 {
		t.Fatalf("期望 2 张图片，得到 %d", len(res.Medias))
	}
	for _, m := range res.Medias {
		if m.Kind != "image" {
			t.Errorf("类型应为 image: %+v", m)
		}
	}
}

func TestParseDouyinItemInfoError(t *testing.T) {
	if _, err := parseDouyinItemInfo([]byte(`{"status_code": 2000}`), "douyin"); err == nil {
		t.Error("status_code 非 0 应报错")
	}
	if _, err := parseDouyinItemInfo([]byte(`{"status_code":0,"item_list":[]}`), "douyin"); err == nil {
		t.Error("空 item_list 应报错")
	}
}
