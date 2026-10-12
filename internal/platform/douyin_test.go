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

// 回归：合成 webId 必须为 19 位数字（首位非零），用于壳页面 webId=undefined 时的兜底
func TestSynthDouyinWebID(t *testing.T) {
	for i := 0; i < 50; i++ {
		w := synthDouyinWebID()
		if len(w) != 19 {
			t.Fatalf("webId 长度应为 19，得到 %q", w)
		}
		if w[0] == '0' {
			t.Fatalf("webId 首位不应为 0，得到 %q", w)
		}
		for _, c := range w {
			if c < '0' || c > '9' {
				t.Fatalf("webId 应全为数字，得到 %q", w)
			}
		}
	}
}

// 回归：webId=undefined 不应再导致直接失败（合成 webId 兜底）
func TestExtractDouyinWebIDUndefined(t *testing.T) {
	html := `<div id='douyin_reflow_webId' webId=undefined></div>`
	if w := extractDouyinWebID(html); w != "" {
		t.Fatalf("webId=undefined 应被拒绝，得到 %q", w)
	}
}

// 回归：图集的 video.play_addr 是 BGM 配乐（mp3），不得被误标为 kind=video
func TestParseDouyinItemInfoAlbumSkipsBGM(t *testing.T) {
	fixture := `{
  "status_code": 0,
  "item_list": [{
    "aweme_id": "7695750443233985418",
    "desc": "测试图集带BGM",
    "author": {"nickname": "作者"},
    "video": {
      "play_addr": {"url_list": [
        "https://aweme.snssdk.com/aweme/v1/play/?video_id=https://lf26-music-east.douyinstatic.com/obj/ies-music-hj/7690954378844343099.mp3&ratio=720p&line=0"
      ]},
      "cover": {"url_list": ["https://p3-sign.douyinpic.com/cover.webp"]}
    },
    "images": [
      {"url_list": ["https://p26-sign.douyinpic.com/img1.webp"]},
      {"url_list": ["https://p26-sign.douyinpic.com/img2.webp"]}
    ]
  }]
}`
	res, err := parseDouyinItemInfo([]byte(fixture), "douyin")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Medias) != 2 {
		t.Fatalf("期望 2 张图片（BGM 不应混入），得到 %d: %+v", len(res.Medias), res.Medias)
	}
	for _, m := range res.Medias {
		if m.Kind != "image" {
			t.Errorf("BGM 被误标为视频: %+v", m)
		}
	}
}
