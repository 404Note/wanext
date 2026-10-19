// Package server 审计模块：业务请求的服务端审计事件。
//
// 设计要点（见《wx-mp API 认证授权设计文档》第 16.2 节）：
//   - 事件只由服务端中间件/处理器产生，不信前端上报；
//   - 近实时层在内存：最近 10,000 条环形缓冲 + 当日按用户聚合，重启清零；
//   - 不记 JWT 全文、微信 code 全文、密钥与媒体文件内容。
package server

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// 审计结果常量。
const (
	AuditOK       = "ok"       // 成功
	AuditFail     = "fail"     // 业务失败（如解析失败）
	AuditDeny401  = "deny401"  // 鉴权拒绝
	AuditLimit429 = "limit429" // 限流
	AuditDeny403  = "deny403"  // 媒体异主带头
	AuditBanned   = "banned"   // 封禁命中
)

// auditBufCap 内存事件缓冲上限。
const auditBufCap = 10000

// AuditEvent 单条审计事件（每个业务请求一条）。
type AuditEvent struct {
	Time       time.Time `json:"time"`
	Endpoint   string    `json:"endpoint"` // login、parse、media、sharer、admin、auth
	UID        string    `json:"uid"`
	IP         string    `json:"ip"`
	Platform   string    `json:"platform,omitempty"`
	Target     string    `json:"target,omitempty"`
	Result     string    `json:"result"`
	Reason     string    `json:"reason,omitempty"`
	LatencyMs  int64     `json:"latency_ms"`
	MediaCount int       `json:"media_count,omitempty"`
}

// EventFilter 审计流水检索条件。
type EventFilter struct {
	UID      string
	Platform string
	Result   string
	Since    time.Time
	Limit    int
}

// userAgg 单用户当日聚合。
type userAgg struct {
	UID        string
	FirstSeen  time.Time
	LastSeen   time.Time
	Calls      int64
	OK         int64
	Fail       int64
	Denied     int64 // 401 + 429 + 403 + 封禁命中
	Platforms  map[string]int64
	Limit429   int64
	MediaDowns int64
}

// Auditor 审计器：环形缓冲 + 当日聚合，全部内存实现。
type Auditor struct {
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

// NewAuditor 构造审计器。
func NewAuditor() *Auditor {
	return &Auditor{
		buf:         make([]AuditEvent, auditBufCap),
		dayStart:    time.Now(),
		perUser:     map[string]*userAgg{},
		perPlatform: map[string]int64{},
		perResult:   map[string]int64{},
		failReasons: map[string]int64{},
	}
}

// rollDay 跨天时清零当日聚合（调用方已持锁）。
func (a *Auditor) rollDay(now time.Time) {
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
func (a *Auditor) Record(ev AuditEvent) {
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
func (a *Auditor) Events(f EventFilter) []AuditEvent {
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
func (a *Auditor) UserAgg(uid string) *userAgg {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ua, ok := a.perUser[uid]; ok {
		cp := *ua
		return &cp
	}
	return nil
}

// TopUsers 按调用量取 Top N 用户聚合（调用量相同按 Denied 倒序）。
func (a *Auditor) TopUsers(n int) []*userAgg {
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
func (a *Auditor) SearchUsers(query string, n int) []*userAgg {
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

// StatsSnapshot 总览数据快照（管理页用）。
type StatsSnapshot struct {
	DayStart    time.Time
	Logins      int64
	ParseOK     int64
	ParseFail   int64
	Deny401     int64
	Limit429    int64
	MediaDowns  int64
	ByPlatform  map[string]int64
	FailReasons []kvCount
	TopUsers    []*userAgg
}

type kvCount struct {
	Key   string
	Count int64
}

// Stats 取总览快照。
func (a *Auditor) Stats() StatsSnapshot {
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
