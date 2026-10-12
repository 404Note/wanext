package httpx

import "testing"

func TestJ(t *testing.T) {
	data := map[string]any{
		"a": map[string]any{
			"b": []any{"x", map[string]any{"c": "found"}},
		},
	}
	if got := S(data, "a", "b", "1", "c"); got != "found" {
		t.Errorf("嵌套取值失败: %q", got)
	}
	if got := S(data, "a", "b", "5", "c"); got != "" {
		t.Errorf("越界应返回空: %q", got)
	}
	if got := S(data, "a", "missing"); got != "" {
		t.Errorf("缺失键应返回空: %q", got)
	}
	if n := I(map[string]any{"n": float64(42)}, "n"); n != 42 {
		t.Errorf("数字取值失败: %d", n)
	}
	if a := Arr(data, "a", "b"); len(a) != 2 {
		t.Errorf("数组取值失败: %v", a)
	}
}

func TestUnquoteJSONString(t *testing.T) {
	in := `https:\/\/p3-sign.douyinpic.com\/x.jpg?x-expires=1\u0026x-signature=abc`
	want := "https://p3-sign.douyinpic.com/x.jpg?x-expires=1&x-signature=abc"
	if got := UnquoteJSONString(in); got != want {
		t.Errorf("反转义失败:\n got: %s\nwant: %s", got, want)
	}
	if got := UnquoteJSONString(""); got != "" {
		t.Errorf("空串应返回空")
	}
}
