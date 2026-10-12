package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractJSON(t *testing.T) {
	s := `xxx window.__INITIAL_STATE__={"a":{"b":[1,2,{"c":"x}y"}]},"d":"e"};</script> yyy`
	idx := strings.Index(s, "{")
	raw := ExtractJSON(s, idx)
	if raw == "" {
		t.Fatal("未能提取 JSON")
	}
	// 提取结果应为合法 JSON，且字符串内的 } 不应干扰配平
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("提取结果不是合法 JSON: %v\nraw=%s", err, raw)
	}
	b := v["a"].(map[string]any)["b"].([]any)
	if got := b[2].(map[string]any)["c"]; got != "x}y" {
		t.Errorf("字符串内 } 处理错误，得到 %v", got)
	}
	// 不应吞掉后面的 </script>
	if strings.Contains(raw, "</script>") {
		t.Errorf("提取越界: %s", raw)
	}
}

func TestParseXiaohongshuImageNote(t *testing.T) {
	html := `<html><script>window.__INITIAL_STATE__=` +
		`{"note":{"noteDetailMap":{"abc123":{"note":{` +
		`"title":"测试笔记","desc":"描述文字","type":"normal",` +
		`"user":{"nickname":"小红薯"},"imageList":[` +
		// 标准 CDN 结构 → 拼出原图直链
		`{"url":"http://sns-webpic-qc.xhscdn.com/20261013/aaa/notes_pre_post/tok1!h5_1080jpg",` +
		`"infoList":[{"url":"http://sns-webpic-qc.xhscdn.com/20261013/aaa/notes_pre_post/tok1!h5_1080jpg"},{"url":"http://sns-webpic-qc.xhscdn.com/20261013/bbb/notes_pre_post/tok1!style_x"}]},` +
		// 非标准结构 → 回退原地址
		`{"url":"https://sns-webpic.xhscdn.com/e.jpg","infoList":[]}` +
		`]}}}}};` + `</script></html>`
	res, err := parseXiaohongshuPage(html, "https://www.xiaohongshu.com/explore/abc123", "xiaohongshu")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "测试笔记" || res.Author != "小红薯" {
		t.Errorf("标题/作者错误: %+v", res)
	}
	if len(res.Medias) != 2 {
		t.Fatalf("期望 2 张图，得到 %d", len(res.Medias))
	}
	if want := "https://sns-img-bd.xhscdn.com/notes_pre_post/tok1"; res.Medias[0].DirectURL != want {
		t.Errorf("应拼出原图直链，得到 %s，期望 %s", res.Medias[0].DirectURL, want)
	}
	if res.Medias[1].DirectURL != "https://sns-webpic.xhscdn.com/e.jpg" {
		t.Errorf("非标准结构应回退原地址: %s", res.Medias[1].DirectURL)
	}
}

func TestXhsOriginalImageURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"http://sns-webpic-qc.xhscdn.com/20261013/aaa/notes_pre_post/tok1!h5_1080jpg",
			"https://sns-img-bd.xhscdn.com/notes_pre_post/tok1",
		},
		{
			"http://sns-webpic-qc.xhscdn.com/20261013/bbb/notes_pre_post/tok1!style_d4c824bab532bfe9",
			"https://sns-img-bd.xhscdn.com/notes_pre_post/tok1",
		},
		{
			"https://sns-webpic.xhscdn.com/e.jpg",
			"https://sns-webpic.xhscdn.com/e.jpg", // 非标准结构回退
		},
		{"", ""},
	}
	for _, c := range cases {
		if got := xhsOriginalImageURL(c.in); got != c.want {
			t.Errorf("xhsOriginalImageURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestParseXiaohongshuVideoNote(t *testing.T) {
	html := `<script>window.__INITIAL_STATE__=` +
		`{"note":{"noteDetailMap":{"v1":{"note":{` +
		`"title":"","desc":"视频描述内容","type":"video",` +
		`"user":{"nickname":"视频号"},` +
		`"video":{"media":{"stream":{"h264":[{"masterUrl":"https://sns-video-bd.xhscdn.com/v.mp4"}]}}},` +
		`"imageList":[{"url":"https://sns-webpic.xhscdn.com/cover.jpg","infoList":[]}]` +
		`}}}}};` + `</script>`
	res, err := parseXiaohongshuPage(html, "https://www.xiaohongshu.com/explore/v1", "xiaohongshu")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Medias) != 1 || res.Medias[0].Kind != "video" {
		t.Fatalf("期望 1 个视频: %+v", res.Medias)
	}
	if res.Medias[0].DirectURL != "https://sns-video-bd.xhscdn.com/v.mp4" {
		t.Errorf("视频流地址错误: %s", res.Medias[0].DirectURL)
	}
	// 无标题时用描述截断
	if res.Title != "视频描述内容" {
		t.Errorf("标题回退错误: %q", res.Title)
	}
}
