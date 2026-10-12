package extract

import "testing"

func TestExtractURLs(t *testing.T) {
text:= `7.77 复制打开抖音，看看 https://v.douyin.com/abc123/ ，再看这个 https://www.bilibili.com/video/BV1xx411c7mD?p=2。`
urls:= ExtractURLs(text)
if len(urls)!= 2 {
t.Fatalf("期望 2 个链接，得到 %v", urls)
}
if urls[0]!= "https://v.douyin.com/abc123/" {
t.Errorf("链接1不符合: %s", urls[0])
}
if urls[1]!= "https://www.bilibili.com/video/BV1xx411c7mD?p=2" {
t.Errorf("链接2不符合: %s", urls[1])
}
// 中文标点结尾不应吞入
urls = ExtractURLs(`看这个：https://xhslink.com/a/b。`)
if len(urls)!= 1 || urls[0]!= "https://xhslink.com/a/b" {
t.Errorf("中文标点处理错误: %v", urls)
}
// 去重
urls = ExtractURLs(`https://v.douyin.com/x/ https://v.douyin.com/x/`)
if len(urls)!= 1 {
t.Errorf("去重失败: %v", urls)
}
// 无链接
if urls:= ExtractURLs("今天天气不错"); len(urls)!= 0 {
t.Errorf("误提取: %v", urls)
}
}

func TestDetect(t *testing.T) {
cases:= map[string]string{
"https://v.douyin.com/abc/": "douyin",
"https://www.douyin.com/video/1234567890123456789": "douyin",
"https://v.kuaishou.com/xyz": "kuaishou",
"https://xhslink.com/a/123": "xiaohongshu",
"https://www.xiaohongshu.com/explore/123": "xiaohongshu",
"https://weibo.com/1234567890/AbCdEfGh": "weibo",
"https://www.bilibili.com/video/BV1xx411c7mD": "bilibili",
"https://b23.tv/abc123": "bilibili",
"https://vm.tiktok.com/xyz/": "tiktok",
"https://x.com/user/status/1234567890123456789": "x",
"https://twitter.com/user/status/1234567890": "x",
"https://www.instagram.com/reel/AbCdEfGhIjK/": "instagram",
"https://www.instagram.com/p/AbCdEfGhIjK/": "instagram",
}
for u, want:= range cases {
p:= Detect(u)
if p == nil {
t.Errorf("%s: 未识别出平台", u)
continue
}
if p.Name()!= want {
t.Errorf("%s: 期望 %s，得到 %s", u, want, p.Name())
}
}
if p:= Detect("https://example.com/foo"); p!= nil {
t.Errorf("未知平台应返回 nil，得到 %s", p.Name())
}
}
