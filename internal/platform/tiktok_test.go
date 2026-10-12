package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const tiktokRehydrationFixture = `<html><head></head><body><script id="__UNIVERSAL_DATA_FOR_REHYDRATION__" type="application/json">{"__DEFAULT_SCOPE__":{"webapp.video-detail":{"itemInfo":{"itemStruct":{"desc":"测试标题","author":{"uniqueId":"测试作者"},"video":{"playAddr":"https://v16-webapp-prime.us.tiktok.com/video/x/?tk=tt_chain_token","cover":"https://p19.tiktokcdn.com/cover.jpg"}}}}}}</script></body></html>`

// 降级页（无作品数据）应自动重试，第三次拿到正常页后成功。
func TestTikTokParseRetriesDegradedPage(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		if n < 3 {
			w.Write([]byte(`<html><body>shell page</body></html>`))
			return
		}
		w.Write([]byte(tiktokRehydrationFixture))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := TikTok{}.Parse(ctx, srv.URL+"/@u/video/1234567890123456789")
	if err != nil {
		t.Fatalf("应重试后成功: %v", err)
	}
	if res.Title != "测试标题" || res.Author != "测试作者" {
		t.Errorf("标题/作者错误: %+v", res)
	}
	if len(res.Medias) != 1 || res.Medias[0].Kind != "video" {
		t.Fatalf("期望 1 个视频: %+v", res.Medias)
	}
	if got := atomic.LoadInt64(&hits); got != 3 {
		t.Errorf("期望请求 3 次，实际 %d 次", got)
	}
}

// 持续降级时应返回最后一次的错误（而非一直重试）。
func TestTikTokParseGivesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body>shell page</body></html>`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := TikTok{}.Parse(ctx, srv.URL+"/@u/video/1234567890123456789")
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "页面未携带作品数据") {
		t.Errorf("错误信息不对: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 4*time.Second {
		t.Errorf("应重试 3 次（约 4 秒间隔），实际只用了 %v", elapsed)
	}
}
