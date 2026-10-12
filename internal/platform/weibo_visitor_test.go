package platform

import (
	"testing"
)

func TestWeiboCallbackRe(t *testing.T) {
	// 真实 genvisitor2 响应格式：window.xxx && xxx({...});
	body := `window.visitor_gray_callback && visitor_gray_callback({"retcode":20000000,"msg":"succ","data":{"sub":"s3cr3t","subp":"p4ss","tid":"abc123","new_tid":true}});`
	m := weiboCallbackRe.FindSubmatch([]byte(body))
	if m == nil {
		t.Fatal("正则未匹配到 JSONP 回调")
	}
	if len(m) < 2 {
		t.Fatalf("正则缺少捕获分组，len(m) = %d（回归：(?s:...) 为非捕获分组）", len(m))
	}
	tid, sub, subp, err := parseGenVisitorJSONP(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if tid != "abc123" || sub != "s3cr3t" || subp != "p4ss" {
		t.Errorf("解析错误: tid=%q sub=%q subp=%q", tid, sub, subp)
	}
}

func TestParseGenVisitorJSONPErrors(t *testing.T) {
	if _, _, _, err := parseGenVisitorJSONP([]byte(`{bad json`)); err == nil {
		t.Error("非法 JSON 应报错")
	}
	if _, _, _, err := parseGenVisitorJSONP([]byte(`{"data":{}}`)); err == nil {
		t.Error("缺少 tid 应报错")
	}
	if weiboCallbackRe.FindSubmatch([]byte(`{"retcode":20000000}`)) != nil {
		t.Error("无回调包裹的响应不应匹配")
	}
}
