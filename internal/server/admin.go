// Package server 管理页模块：审计查看与在线封禁处置。
//
// 设计要点（见《wx-mp API 认证授权设计文档》第 16 节）：
//   - 只供管理员本人单人使用，不做多管理员与 RBAC；
//   - 管理员凭证独立于用户 JWT：ADMIN_TOKEN 只进服务端 .env，登录后换
//     HttpOnly 会话 Cookie；写动作另带 CSRF 令牌；
//   - 未启用或未配置凭证时 /admin/* 与 /api/admin/* 一律 404（失败关闭）；
//   - 封禁即时生效并持久化到本地文件，重启不丢失。
package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wanext/internal/httpx"
)

//go:embed webadmin
var webadminFS embed.FS

// 封禁时长档位。
const (
	BanDur24h     = "24h"
	BanDur7d      = "7d"
	BanDurForever = "forever"
)

// adminSessionTTL 管理员会话有效期 8 小时。
const adminSessionTTL = 8 * time.Hour

// adminLoginMinuteLimit 管理员登录按 IP 限流：5 次/分钟。
const adminLoginMinuteLimit = 5

// BanRecord 单条封禁记录。
type BanRecord struct {
	UID       string    `json:"uid"`
	Reason    string    `json:"reason"`
	Until     time.Time `json:"until"` // 零值 = 永久
	CreatedAt time.Time `json:"created_at"`
	By        string    `json:"by"` // "admin" 或 "env"
}

// Active 是否仍在封禁中。
func (b *BanRecord) Active() bool {
	return b.Until.IsZero() || b.Until.After(time.Now())
}

// banStateFile 持久化文件结构。
type banStateFile struct {
	Bans    []*BanRecord `json:"bans"`
	History []*BanRecord `json:"history"`
	OpLog   []AdminOp    `json:"op_log"`
}

// AdminOp 管理员操作记录。
type AdminOp struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"` // ban、unban、reset-limit
	UID    string    `json:"uid"`
	Detail string    `json:"detail"`
}

// BanStore 封禁名单：内存 + 本地文件持久化。
type BanStore struct {
	mu      sync.Mutex
	active  map[string]*BanRecord
	history []*BanRecord
	opLog   []AdminOp
	file    string // 为空则不持久化
}

// NewBanStore 构造封禁存储；file 为持久化路径，为空表示不落盘。
func NewBanStore(file string) *BanStore {
	return &BanStore{active: map[string]*BanRecord{}, file: file}
}

// Load 从持久化文件加载（不存在则跳过）。
func (s *BanStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == "" {
		return nil
	}
	b, err := os.ReadFile(s.file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var st banStateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("解析封禁状态文件失败: %w", err)
	}
	for _, r := range st.Bans {
		if r.Active() {
			s.active[r.UID] = r
		} else {
			s.history = append(s.history, r)
		}
	}
	s.history = append(s.history, st.History...)
	s.opLog = append(s.opLog, st.OpLog...)
	return nil
}

// MergeEnvBans 合并 BANNED_UIDS 兜底名单（永久封禁，by=env）。
func (s *BanStore) MergeEnvBans(uids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	added := false
	for _, u := range uids {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, ok := s.active[u]; !ok {
			s.active[u] = &BanRecord{UID: u, Reason: "环境变量兜底", CreatedAt: now, By: "env"}
			added = true
		}
	}
	if added {
		s.saveLocked()
	}
}

// saveLocked 持久化（调用方已持锁）：临时文件 + 原子改名。
func (s *BanStore) saveLocked() {
	if s.file == "" {
		return
	}
	st := banStateFile{OpLog: s.opLog, History: s.history}
	for _, r := range s.active {
		st.Bans = append(st.Bans, r)
	}
	sort.Slice(st.Bans, func(i, j int) bool { return st.Bans[i].UID < st.Bans[j].UID })
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		log.Printf("[admin] 序列化封禁状态失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.file), 0o755); err != nil {
		log.Printf("[admin] 创建状态目录失败: %v", err)
		return
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("[admin] 写封禁状态失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.file); err != nil {
		log.Printf("[admin] 封禁状态改名失败: %v", err)
	}
}

// IsBanned 是否被封禁（到期自动转历史并持久化）。
func (s *BanStore) IsBanned(uid string) bool {
	if uid == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.active[uid]
	if !ok {
		return false
	}
	if !r.Active() {
		delete(s.active, uid)
		s.history = append(s.history, r)
		s.saveLocked()
		return false
	}
	return true
}

// Ban 封禁用户；until 零值表示永久。
func (s *BanStore) Ban(uid string, until time.Time, reason, by string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.active[uid]; ok {
		s.history = append(s.history, old)
	}
	s.active[uid] = &BanRecord{UID: uid, Reason: reason, Until: until, CreatedAt: time.Now(), By: by}
	s.opLog = append(s.opLog, AdminOp{Time: time.Now(), Action: "ban", UID: uid, Detail: reason})
	s.trimLocked()
	s.saveLocked()
}

// Unban 解封，返回是否曾在封禁中。
func (s *BanStore) Unban(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.active[uid]
	if !ok {
		return false
	}
	delete(s.active, uid)
	s.history = append(s.history, r)
	s.opLog = append(s.opLog, AdminOp{Time: time.Now(), Action: "unban", UID: uid, Detail: r.Reason})
	s.trimLocked()
	s.saveLocked()
	return true
}

// ActiveList 当前封禁中的名单。
func (s *BanStore) ActiveList() []*BanRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*BanRecord, 0, len(s.active))
	for _, r := range s.active {
		if r.Active() {
			cp := *r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// HistoryList 已到期/已解封的历史记录（最新在前，上限 200）。
func (s *BanStore) HistoryList() []*BanRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*BanRecord, 0, len(s.history))
	for _, r := range s.history {
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// OpLog 管理员操作记录（最新在前，上限 200）。
func (s *BanStore) OpLog() []AdminOp {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AdminOp, len(s.opLog))
	copy(out, s.opLog)
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// LogOp 记录一次管理员操作（供非封禁类动作）。
func (s *BanStore) LogOp(action, uid, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opLog = append(s.opLog, AdminOp{Time: time.Now(), Action: action, UID: uid, Detail: detail})
	s.trimLocked()
	s.saveLocked()
}

// banHistoryCap 封禁历史与操作记录的内存/落盘上限，防止无限增长。
const banHistoryCap = 1000

// trimLocked 截断历史与操作记录（调用方已持锁）。
func (s *BanStore) trimLocked() {
	if len(s.history) > banHistoryCap {
		s.history = s.history[len(s.history)-banHistoryCap:]
	}
	if len(s.opLog) > banHistoryCap {
		s.opLog = s.opLog[len(s.opLog)-banHistoryCap:]
	}
}

// parseBanDuration 解析封禁时长档位，返回到期时刻（零值=永久）。
func parseBanDuration(d string) (time.Time, error) {
	switch d {
	case BanDur24h:
		return time.Now().Add(24 * time.Hour), nil
	case BanDur7d:
		return time.Now().Add(7 * 24 * time.Hour), nil
	case BanDurForever, "":
		return time.Time{}, nil
	default:
		return time.Time{}, fmt.Errorf("未知时长档位: %s", d)
	}
}

// adminSession 管理员会话。
type adminSession struct {
	csrf    string
	expires time.Time
}

// AdminManager 管理页：会话、CSRF、接口装配。
type AdminManager struct {
	enabled      bool
	tokenHash    [32]byte
	mu           sync.Mutex
	sessions     map[string]*adminSession
	loginLimiter *rateLimiter
	bans         *BanStore
	audit        *Auditor
	auth         *Authenticator
	limiter      *rateLimiter // 业务限流器（供 reset-limit）
	registry     *Registry    // 媒体登记表（供封禁时作废令牌）
}

// NewAdminManager 构造管理页；token 为空或过短（<32 字节）时保持禁用。
func NewAdminManager(enabled bool, token string, bans *BanStore, audit *Auditor, auth *Authenticator, limiter *rateLimiter, registry *Registry) *AdminManager {
	m := &AdminManager{
		bans:         bans,
		audit:        audit,
		auth:         auth,
		limiter:      limiter,
		registry:     registry,
		sessions:     map[string]*adminSession{},
		loginLimiter: newRateLimiter(adminLoginMinuteLimit, 0),
	}
	if enabled && len(token) >= 32 {
		m.enabled = true
		m.tokenHash = sha256.Sum256([]byte(token))
	} else if enabled {
		log.Printf("[admin] ADMIN_TOKEN 未设置或过短（需≥32 字节），管理页保持关闭")
	}
	go m.cleanupSessions()
	return m
}

func (m *AdminManager) cleanupSessions() {
	for range time.Tick(10 * time.Minute) {
		now := time.Now()
		m.mu.Lock()
		for k, s := range m.sessions {
			if now.After(s.expires) {
				delete(m.sessions, k)
			}
		}
		m.mu.Unlock()
	}
}

// sessionFromRequest 校验会话 Cookie，返回会话（无效返回 nil）。
func (m *AdminManager) sessionFromRequest(r *http.Request) *adminSession {
	c, err := r.Cookie("wanext_admin")
	if err != nil || c.Value == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[c.Value]
	if !ok || time.Now().After(s.expires) {
		delete(m.sessions, c.Value)
		return nil
	}
	return s
}

// requireAdmin 管理接口鉴权中间件；未启用时一律 404。
func (m *AdminManager) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.enabled {
			http.NotFound(w, r)
			return
		}
		s := m.sessionFromRequest(r)
		if s == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "管理员未登录"})
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodDelete || r.Method == http.MethodPut {
			if r.Header.Get("X-CSRF-Token") != s.csrf {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF 校验失败"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleAdminPage 管理页静态页；未启用时 404。
func (m *AdminManager) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if !m.enabled {
		http.NotFound(w, r)
		return
	}
	b, err := webadminFS.ReadFile("webadmin/index.html")
	if err != nil {
		http.Error(w, "页面缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// handleAdminLogin POST /api/admin/login：唯一公开的管理端点。
func (m *AdminManager) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !m.enabled {
		http.NotFound(w, r)
		return
	}
	ip := clientIP(r)
	if !m.loginLimiter.allow(ip) {
		log.Printf("[auth] 管理员登录限流 ip=%s", ip)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "操作太频繁，请稍后再试"})
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体必须是 JSON"})
		return
	}
	// 常量时间比较：先各自算摘要再比，避免时序泄露。
	got := sha256.Sum256([]byte(req.Token))
	if !hmac.Equal(got[:], m.tokenHash[:]) {
		log.Printf("[auth] 管理员登录失败 ip=%s", ip)
		m.audit.Record(AuditEvent{Endpoint: "admin", IP: ip, Result: "deny401", Reason: "管理员凭证错误"})
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "凭证错误"})
		return
	}
	sid := httpx.RandomHex(32)
	csrf := httpx.RandomHex(16)
	m.mu.Lock()
	m.sessions[sid] = &adminSession{csrf: csrf, expires: time.Now().Add(adminSessionTTL)}
	m.mu.Unlock()
	cookie := &http.Cookie{
		Name:     "wanext_admin",
		Value:    sid,
		Path:     "/",
		MaxAge:   int(adminSessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
	if r.TLS != nil {
		cookie.Secure = true // 公网 HTTPS 下启用；本地 http 开发时不加（否则浏览器不回传）
	}
	http.SetCookie(w, cookie)
	log.Printf("[auth] 管理员登录成功 ip=%s", ip)
	m.audit.Record(AuditEvent{Endpoint: "admin", IP: ip, Result: "ok", Reason: "管理员登录"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "csrf": csrf})
}

// handleAdminLogout 注销。
func (m *AdminManager) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("wanext_admin"); err == nil {
		m.mu.Lock()
		delete(m.sessions, c.Value)
		m.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "wanext_admin", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminMe 会话自检（前端判断是否已登录）。
func (m *AdminManager) handleAdminMe(w http.ResponseWriter, r *http.Request) {
	s := m.sessionFromRequest(r)
	if s == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "管理员未登录"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "csrf": s.csrf})
}

func writeAdminJSON(w http.ResponseWriter, v any) {
	writeJSON(w, http.StatusOK, v)
}

// handleAdminStats 总览数据。
func (m *AdminManager) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	writeAdminJSON(w, m.audit.Stats())
}

// handleAdminUsers 用户列表（q 检索）。
func (m *AdminManager) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	users := m.audit.SearchUsers(q, 50)
	type row struct {
		UID       string           `json:"uid"`
		Short     string           `json:"short"`
		FirstSeen time.Time        `json:"first_seen"`
		LastSeen  time.Time        `json:"last_seen"`
		Calls     int64            `json:"calls"`
		OK        int64            `json:"ok"`
		Fail      int64            `json:"fail"`
		Denied    int64            `json:"denied"`
		Limit429  int64            `json:"limit429"`
		Banned    bool             `json:"banned"`
		Platforms map[string]int64 `json:"platforms"`
	}
	out := make([]row, 0, len(users))
	for _, u := range users {
		out = append(out, row{
			UID: u.UID, Short: uidShort(u.UID),
			FirstSeen: u.FirstSeen, LastSeen: u.LastSeen,
			Calls: u.Calls, OK: u.OK, Fail: u.Fail, Denied: u.Denied,
			Limit429: u.Limit429, Banned: m.bans.IsBanned(u.UID), Platforms: u.Platforms,
		})
	}
	writeAdminJSON(w, map[string]any{"users": out})
}

// handleAdminUserDetail 用户详情：聚合 + 近 50 条流水 + 封禁状态。
func (m *AdminManager) handleAdminUserDetail(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	agg := m.audit.UserAgg(uid)
	events := m.audit.Events(EventFilter{UID: uid, Limit: 50})
	writeAdminJSON(w, map[string]any{
		"uid": uid, "short": uidShort(uid),
		"agg": agg, "banned": m.bans.IsBanned(uid), "events": events,
	})
}

// handleAdminEvents 审计流水检索。
func (m *AdminManager) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := EventFilter{
		UID:      strings.TrimSpace(q.Get("uid")),
		Platform: strings.TrimSpace(q.Get("platform")),
		Result:   strings.TrimSpace(q.Get("result")),
		Limit:    50,
	}
	if n := q.Get("limit"); n != "" {
		var v int
		if _, err := fmt.Sscanf(n, "%d", &v); err == nil && v > 0 && v <= 200 {
			f.Limit = v
		}
	}
	writeAdminJSON(w, map[string]any{"events": m.audit.Events(f)})
}

// handleAdminBans 封禁名单（含历史）。
func (m *AdminManager) handleAdminBans(w http.ResponseWriter, r *http.Request) {
	writeAdminJSON(w, map[string]any{
		"active":  m.bans.ActiveList(),
		"history": m.bans.HistoryList(),
		"oplog":   m.bans.OpLog(),
	})
}

// handleAdminBan 封禁用户：三处同时生效（业务请求、登录签发、媒体令牌）。
func (m *AdminManager) handleAdminBan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID      string `json:"uid"`
		Duration string `json:"duration"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体必须是 JSON"})
		return
	}
	req.UID = strings.TrimSpace(req.UID)
	req.Reason = strings.TrimSpace(req.Reason)
	if req.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "uid 不能为空"})
		return
	}
	if req.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "封禁原因必填"})
		return
	}
	until, err := parseBanDuration(req.Duration)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	m.bans.Ban(req.UID, until, req.Reason, "admin")
	revoked := m.registry.RevokeByUID(req.UID)
	log.Printf("[admin] 封禁 uid=%s until=%v 原因=%s 作废媒体令牌=%d", uidShort(req.UID), until, req.Reason, revoked)
	m.audit.Record(AuditEvent{Endpoint: "admin", UID: req.UID, IP: clientIP(r), Result: "ok", Reason: "封禁:" + req.Reason})
	writeAdminJSON(w, map[string]any{"ok": true, "revoked_media": revoked})
}

// handleAdminUnban 解封。
func (m *AdminManager) handleAdminUnban(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !m.bans.Unban(uid) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "该用户不在封禁中"})
		return
	}
	log.Printf("[admin] 解封 uid=%s", uidShort(uid))
	m.audit.Record(AuditEvent{Endpoint: "admin", UID: uid, IP: clientIP(r), Result: "ok", Reason: "解封"})
	writeAdminJSON(w, map[string]any{"ok": true})
}

// handleAdminResetLimit 清除单个用户的限流计数（不清审计流水）。
func (m *AdminManager) handleAdminResetLimit(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	m.limiter.reset("u:" + uid)
	m.bans.LogOp("reset-limit", uid, "清除限流计数")
	log.Printf("[admin] 清除限流计数 uid=%s", uidShort(uid))
	writeAdminJSON(w, map[string]any{"ok": true})
}
