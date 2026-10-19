// Package server 审计模块：业务请求的服务端审计事件。
//
// 设计要点：
//   - 事件只由服务端中间件/处理器产生，不信前端上报；
//   - 默认 SQLite 持久化（data/analytics.db，纯 Go 驱动，单二进制部署不变），
//     审计事件保留 30 天，用户累计数据永久保留；
//   - SQLite 不可用时降级为纯内存实现（最近 10,000 条 + 当日聚合，重启清零）；
//   - 不记 JWT 全文、微信 code 全文、密钥与媒体文件内容；
//     用户标识用 HMAC 摘要 uid，不存原始 openid。
package server

import (
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

// Auditor 审计器接口：SQLite 实现（默认）与纯内存实现（降级）都满足它。
// 管理页只依赖这个接口，存储换实现不影响调用方。
type Auditor interface {
	// Record 记录一条审计事件。实现必须是非阻塞的（请求路径上调用），
	// 内部自行批量落盘。
	Record(ev AuditEvent)
	// Events 按条件检索审计流水（最新在前）。
	Events(f EventFilter) []AuditEvent
	// UserAgg 取单用户聚合（累计口径），不存在返回 nil。
	UserAgg(uid string) *userAgg
	// TopUsers 按调用量取 Top N 用户（今日口径）。
	TopUsers(n int) []*userAgg
	// SearchUsers 按 uid 短码检索用户。
	SearchUsers(query string, n int) []*userAgg
	// Stats 取总览快照（今日口径）。
	Stats() StatsSnapshot
}

// NewAuditor 打开 SQLite 审计库（dataDir/analytics.db）。
// 打开失败时返回错误，调用方应降级为 newMemAuditor()。
func NewAuditor(dataDir string) (Auditor, error) {
	return openSQLAuditor(dataDir)
}
