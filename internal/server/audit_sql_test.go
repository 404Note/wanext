package server

import (
	"os"
	"testing"
	"time"
)

func testSQLAuditor(t *testing.T) *sqlAuditor {
	t.Helper()
	// 测试用 newSQLAuditor：不启动后台刷盘协程，flushSync 确定性刷盘。
	s, err := newSQLAuditor(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite 审计库失败: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}

func TestSQLAuditorRoundtrip(t *testing.T) {
	s := testSQLAuditor(t)
	now := time.Now()
	events := []AuditEvent{
		{Time: now, Endpoint: "login", UID: "user-a", IP: "1.1.1.1", Result: AuditOK, LatencyMs: 10},
		{Time: now, Endpoint: "parse", UID: "user-a", IP: "1.1.1.1", Platform: "douyin", Target: "v.douyin.com/x/", Result: AuditOK, MediaCount: 3, LatencyMs: 100},
		{Time: now, Endpoint: "parse", UID: "user-a", IP: "1.1.1.1", Platform: "douyin", Result: AuditFail, Reason: "链接失效", LatencyMs: 50},
		{Time: now, Endpoint: "parse", UID: "user-b", IP: "2.2.2.2", Platform: "weibo", Result: AuditDeny401, LatencyMs: 1},
		{Time: now, Endpoint: "media", UID: "user-a", IP: "1.1.1.1", Result: AuditOK, LatencyMs: 200},
	}
	for _, ev := range events {
		s.Record(ev)
	}
	s.flushSync()

	// 流水检索
	got := s.Events(EventFilter{UID: "user-a", Limit: 10})
	if len(got) != 4 {
		t.Fatalf("user-a 应有 4 条事件，得到 %d", len(got))
	}
	if got[0].Time.Before(got[1].Time) {
		t.Error("流水应最新在前")
	}
	if n := len(s.Events(EventFilter{Result: AuditDeny401})); n != 1 {
		t.Errorf("按结果过滤应得 1 条，得到 %d", n)
	}

	// 用户累计
	ua := s.UserAgg("user-a")
	if ua == nil {
		t.Fatal("UserAgg 不应为 nil")
	}
	if ua.Calls != 4 || ua.OK != 3 || ua.Fail != 1 {
		t.Errorf("累计错误: %+v", ua)
	}
	if ua.Platforms["douyin"] != 2 {
		t.Errorf("平台分布错误: %v", ua.Platforms)
	}
	if s.UserAgg("no-such-user") != nil {
		t.Error("不存在的用户应返回 nil")
	}

	// 检索
	users := s.SearchUsers("user-", 10)
	if len(users) != 2 {
		t.Errorf("应检索到 2 用户，得到 %d", len(users))
	}

	// Top
	top := s.TopUsers(10)
	if len(top) != 2 || top[0].UID != "user-a" || top[0].Calls != 4 {
		t.Errorf("TopUsers 错误: %+v", top)
	}

	// 总览
	st := s.Stats()
	if st.Logins != 1 || st.ParseOK != 1 || st.ParseFail != 1 || st.Deny401 != 1 || st.MediaDowns != 1 {
		t.Errorf("总览错误: %+v", st)
	}
	if st.ByPlatform["douyin"] != 2 || st.ByPlatform["weibo"] != 1 {
		t.Errorf("平台分布错误: %v", st.ByPlatform)
	}
	if len(st.FailReasons) != 1 || st.FailReasons[0].Key != "链接失效" {
		t.Errorf("失败原因 Top 错误: %v", st.FailReasons)
	}
}

func TestSQLAuditorPersistence(t *testing.T) {
	dir := t.TempDir()
	a1, err := newSQLAuditor(dir)
	if err != nil {
		t.Fatal(err)
	}
	a1.Record(AuditEvent{Endpoint: "parse", UID: "persist-u", Result: AuditOK})
	a1.flushSync()
	a1.db.Close()

	// 模拟重启：重新打开同一目录
	a2, err := newSQLAuditor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a2.db.Close()
	ua := a2.UserAgg("persist-u")
	if ua == nil || ua.Calls != 1 {
		t.Errorf("重启后用户累计应保留，得到 %+v", ua)
	}
	if n := len(a2.Events(EventFilter{UID: "persist-u"})); n != 1 {
		t.Errorf("重启后事件应保留，得到 %d", n)
	}
}

func TestSQLAuditorFallback(t *testing.T) {
	// dataDir 指向一个已存在的文件 → MkdirAll 失败 → 返回错误
	dir := t.TempDir()
	blocker := dir + "/blocker"
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuditor(blocker + "/sub"); err == nil {
		t.Error("非法数据目录应返回错误")
	}
	// 降级路径可用
	m := newMemAuditor()
	m.Record(AuditEvent{Endpoint: "parse", UID: "u", Result: AuditOK})
	if len(m.Events(EventFilter{})) != 1 {
		t.Error("内存降级实现应可用")
	}
}

func TestSQLAuditorDropped(t *testing.T) {
	// 不启动后台刷盘协程，直接验证 Record 的非阻塞语义：channel 满时丢弃并计数。
	s := &sqlAuditor{ch: make(chan AuditEvent, 10)}
	for i := 0; i < 25; i++ {
		s.Record(AuditEvent{Endpoint: "parse", UID: "u", Result: AuditOK})
	}
	if d := s.Dropped(); d != 15 {
		t.Errorf("应丢弃 15 条，实际 %d", d)
	}
	if n := len(s.ch); n != 10 {
		t.Errorf("channel 应保留 10 条，实际 %d", n)
	}
}
