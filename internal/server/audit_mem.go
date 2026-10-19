package server

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Auditor 审计器：环形缓冲 + 当日聚合，全部内存实现。
type memAuditor struct {
	mu          sync.Mutex
	buf         []AuditEvent
	start       int
	count       int
	dayStart    time.Time
	perUser     map[string]*userAgg
	perPlatform map[string]int64
	perResult   map[string]int64 // endpoint + ":" + result → 计数
	failReasons map[string]int64
}

// newMemAuditor 构造纯内存审计器（SQLite 不可用时的降级）。
func newMemAuditor() *memAuditor {
	return &memAuditor{
		buf:         make([]AuditEvent, auditBufCap),
		dayStart:    time.Now(),
		perUser:     map[string]*userAgg{},
		perPlatform: map[string]int64{},
		perResult:   map[string]int64{},
		failReasons: map[string]int64{},
	}
}

// rollDay 跨天时清零当日聚合（调用方已持锁）。
func (a *memAuditor) rollDay(now time.Time) {
	y1, m1, d1 := a.dayStart.Date()
	y2, m2, d2 := now.Date()
	if y1 != y2 || m1 != m2 || d1 != d2 {
		a.dayStart = now
		a.perUser = map[string]*userAgg{}
		a.perPlatform = map[string]int64{}
		a.perResult = map[string]int64{}
		a.failReasons = map[string]int64{}
	}
}

// Record 记录一条审计事件。
func (a *memAuditor) Record(ev AuditEvent) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rollDay(ev.Time)
	a.buf[(a.start+a.count)%auditBufCap] = ev
	if a.count < auditBufCap {
		a.count++
	} else {
		a.start = (a.start + 1) % auditBufCap
	}
	a.perResult[ev.Endpoint+":"+ev.Result]++
	if ev.Platform != "" {
		a.perPlatform[ev.Platform]++
	}
	if ev.Result == AuditFail && ev.Reason != "" {
		a.failReasons[ev.Reason]++
	}
	if ev.UID == "" {
		return
	}
	ua, ok := a.perUser[ev.UID]
	if !ok {
		ua = &userAgg{UID: ev.UID, FirstSeen: ev.Time, Platforms: map[string]int64{}}
		a.perUser[ev.UID] = ua
	}
	ua.LastSeen = ev.Time
	ua.Calls++
	switch ev.Result {
	case AuditOK:
		ua.OK++
	case AuditFail:
		ua.Fail++
	case AuditDeny401, AuditLimit429, AuditDeny403, AuditBanned:
		ua.Denied++
	}
	if ev.Result == AuditLimit429 {
		ua.Limit429++
	}
	if ev.Platform != "" {
		ua.Platforms[ev.Platform]++
	}
	if ev.Endpoint == "media" && ev.Result == AuditOK {
		ua.MediaDowns++
	}
}

// Events 按条件检索审计流水（最新在前），Limit<=0 时默认 50。
func (a *memAuditor) Events(f EventFilter) []AuditEvent {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AuditEvent, 0, limit)
	for i := a.count - 1; i >= 0 && len(out) < limit; i-- {
		ev := a.buf[(a.start+i)%auditBufCap]
		if f.UID != "" && ev.UID != f.UID {
			continue
		}
		if f.Platform != "" && ev.Platform != f.Platform {
			continue
		}
		if f.Result != "" && ev.Result != f.Result {
			continue
		}
		if !f.Since.IsZero() && ev.Time.Before(f.Since) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// UserAgg 取单用户当日聚合，不存在返回 nil。
func (a *memAuditor) UserAgg(uid string) *userAgg {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ua, ok := a.perUser[uid]; ok {
		cp := *ua
		return &cp
	}
	return nil
}

// TopUsers 按调用量取 Top N 用户聚合（调用量相同按 Denied 倒序）。
func (a *memAuditor) TopUsers(n int) []*userAgg {
	a.mu.Lock()
	defer a.mu.Unlock()
	all := make([]*userAgg, 0, len(a.perUser))
	for _, ua := range a.perUser {
		cp := *ua
		all = append(all, &cp)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Calls != all[j].Calls {
			return all[i].Calls > all[j].Calls
		}
		return all[i].Denied > all[j].Denied
	})
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// SearchUsers 按 uid 短码前缀/包含检索用户。
func (a *memAuditor) SearchUsers(query string, n int) []*userAgg {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*userAgg
	q := strings.ToLower(query)
	for _, ua := range a.perUser {
		if q == "" || strings.Contains(strings.ToLower(ua.UID), q) {
			cp := *ua
			out = append(out, &cp)
			if len(out) >= n {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// Stats 取总览快照。
func (a *memAuditor) Stats() StatsSnapshot {
	a.mu.Lock()
	s := StatsSnapshot{
		DayStart:   a.dayStart,
		ByPlatform: map[string]int64{},
	}
	for k, v := range a.perPlatform {
		s.ByPlatform[k] = v
	}
	s.Logins = a.perResult["login:"+AuditOK]
	s.ParseOK = a.perResult["parse:"+AuditOK]
	s.ParseFail = a.perResult["parse:"+AuditFail]
	s.MediaDowns = a.perResult["media:"+AuditOK]
	for k, v := range a.perResult {
		switch {
		case strings.HasSuffix(k, ":"+AuditDeny401):
			s.Deny401 += v
		case strings.HasSuffix(k, ":"+AuditLimit429):
			s.Limit429 += v
		}
	}
	for k, v := range a.failReasons {
		s.FailReasons = append(s.FailReasons, kvCount{k, v})
	}
	sort.Slice(s.FailReasons, func(i, j int) bool { return s.FailReasons[i].Count > s.FailReasons[j].Count })
	if len(s.FailReasons) > 10 {
		s.FailReasons = s.FailReasons[:10]
	}
	a.mu.Unlock()
	s.TopUsers = a.TopUsers(10)
	return s
}
