package platform

import (
	"strings"
	"testing"
)

func TestIsXHSShortLink(t *testing.T) {
	short := []string{
		"https://xhslink.com/a/AbC123",
		"http://xhslink.com/m/AixEkyLwpfs",
		"https://xhslink.cn/o/AJFDFovR15M",
		"https://sub.xhslink.cn/x/yz",
	}
	for _, u := range short {
		if !isXHSShortLink(u) {
			t.Errorf("应识别为短链: %s", u)
		}
		if p := (Xiaohongshu{}); !p.MatchHost(hostOf(t, u)) {
			t.Errorf("MatchHost 应匹配: %s", u)
		}
	}
	notShort := []string{
		"https://www.xiaohongshu.com/explore/abc",
		"https://xhslink.com.evil.com/x",
		"https://notxhslink.cn/x",
		"not a url",
	}
	for _, u := range notShort {
		if isXHSShortLink(u) {
			t.Errorf("不应识别为短链: %s", u)
		}
	}
}

func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	i := strings.Index(rawURL, "://")
	h := rawURL[i+3:]
	if j := strings.Index(h, "/"); j >= 0 {
		h = h[:j]
	}
	return h
}

func TestSanitizeXHSState(t *testing.T) {
	in := `{"a":new Map([]),"b":undefined,"c":NaN,"d":Infinity,"e":[undefined,1],"f":"undefined 保留"}`
	out := sanitizeXHSState(in)
	if strings.Contains(out, "new Map([])") {
		t.Errorf("new Map([]) 未被清洗: %s", out)
	}
	if !strings.Contains(out, `"a":{}`) {
		t.Errorf("new Map([]) 应替换为 {{}}: %s", out)
	}
	if !strings.Contains(out, `"b":null`) || !strings.Contains(out, `"c":null`) || !strings.Contains(out, `"d":null`) {
		t.Errorf("undefined/NaN/Infinity 应替换为 null: %s", out)
	}
	if !strings.Contains(out, `"f":"undefined 保留"`) {
		t.Errorf("字符串内的 undefined 不应被替换: %s", out)
	}
}

func TestParseXiaohongshuWithJSState(t *testing.T) {
	// __INITIAL_STATE__ 含 new Map([]) 时也应正常解析
	html := `<script>window.__INITIAL_STATE__={"note":{"noteDetailMap":{"k":{"note":{` +
		`"title":"JS 载荷","desc":"","type":"normal","user":{"nickname":"作者"},` +
		`"meta":new Map([]),"flag":undefined,` +
		`"imageList":[{"url":"https://sns-webpic.xhscdn.com/a.jpg","infoList":[]}]` +
		`}}}}}};</script>`
	res, err := parseXiaohongshuPage(html, "https://www.xiaohongshu.com/explore/k", "xiaohongshu")
	if err != nil {
		t.Fatalf("含 new Map([])/undefined 的载荷应可解析: %v", err)
	}
	if res.Title != "JS 载荷" || len(res.Medias) != 1 {
		t.Errorf("解析结果错误: %+v", res)
	}
}

func TestParseXiaohongshuVideoOriginKey(t *testing.T) {
	// originVideoKey 为裸 key 时应拼接为直链，且优先级高于 stream
	html := `<script>window.__INITIAL_STATE__={"note":{"noteDetailMap":{"v":{"note":{` +
		`"title":"t","desc":"d","type":"video","user":{"nickname":"u"},` +
		`"video":{"consumer":{"originVideoKey":"abc123.mp4"},` +
		`"media":{"stream":{"h264":[{"masterUrl":"https://x/y.mp4","height":720}]}}},` +
		`"imageList":[]}}}}}};</script>`
	res, err := parseXiaohongshuPage(html, "https://www.xiaohongshu.com/explore/v", "xiaohongshu")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://sns-video-bd.xhscdn.com/abc123.mp4"
	if res.Medias[0].DirectURL != want {
		t.Errorf("originVideoKey 直链错误: %s，期望 %s", res.Medias[0].DirectURL, want)
	}
}

func TestParseXiaohongshuVideoBestStream(t *testing.T) {
	// 无 originVideoKey 时选分辨率最高的流，优先 backupUrls
	html := `<script>window.__INITIAL_STATE__={"note":{"noteDetailMap":{"v":{"note":{` +
		`"title":"t","desc":"d","type":"video","user":{"nickname":"u"},` +
		`"video":{"media":{"stream":{` +
		`"h265":[{"masterUrl":"https://x/low.mp4","height":720}],` +
		`"h264":[{"masterUrl":"https://x/high.mp4","height":1080,"backupUrls":["https://b/high.mp4"]}]` +
		`}}},"imageList":[]}}}}}};</script>`
	res, err := parseXiaohongshuPage(html, "https://www.xiaohongshu.com/explore/v", "xiaohongshu")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Medias[0].DirectURL; got != "https://b/high.mp4" {
		t.Errorf("应选最高分辨率流的 backupUrls，得到 %s", got)
	}
}

func TestParseXiaohongshuNoteDataFormat(t *testing.T) {
	// 新版分享页（discovery/item）结构：noteData.data.noteData，且作者字段为 nickName
	html := `<script>window.__INITIAL_STATE__={"global":{},"noteData":{"data":{"noteData":{` +
		`"noteId":"6aca565f000000001b01dc71","title":"新版笔记","desc":"描述",` +
		`"type":"normal","user":{"userId":"u1","nickName":"新作者"},` +
		`"imageList":[{"url":"http://sns-webpic-qc.xhscdn.com/20261013/aaa/notes_pre_post/nt1!h5_1080jpg",` +
		`"infoList":[{"url":"http://sns-webpic-qc.xhscdn.com/20261013/aaa/notes_pre_post/nt1!h5_1080jpg"},{"url":"http://sns-webpic-qc.xhscdn.com/20261013/bbb/notes_pre_post/nt1!style_x"}]}]` +
		`}}}};</script>`
	res, err := parseXiaohongshuPage(html, "https://www.xiaohongshu.com/discovery/item/6aca565f000000001b01dc71", "xiaohongshu")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "新版笔记" {
		t.Errorf("标题错误: %q", res.Title)
	}
	if res.Author != "新作者" {
		t.Errorf("作者应取 user.nickName，得到 %q", res.Author)
	}
	if len(res.Medias) != 1 || res.Medias[0].DirectURL != "https://sns-img-bd.xhscdn.com/notes_pre_post/nt1" {
		t.Errorf("图片应为原图直链: %+v", res.Medias)
	}
}
