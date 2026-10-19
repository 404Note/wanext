// Package server SQLite 审计实现：用户累计与审计事件持久化。
//
// 设计：
//   - 请求路径上 Record() 只做非阻塞入队（channel 缓冲 10k，满则丢弃并计数），
//     后台协程每 5 秒或攒满 2000 条批量写入，单事务提交；
//   - users 表存累计口径（uid 主键，不存原始 openid）；
//   - events 表存原始事件，保留 30 天，每天 purge 一次；
//   - 单连接（SetMaxOpenConns(1)）+ WAL，避免 "database is locked"。
package server

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const (
	sqlFlushInterval = 5 * time.Second
	sqlBatchMax      = 2000
	sqlChanCap       = 10000
	// eventRetention 审计事件保留时长。
	eventRetention = 30 * 24 * time.Hour
)

const sqlSchema = `
CREATE TABLE IF NOT EXISTS users(
	uid TEXT PRIMARY KEY,
	first_seen INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	total_calls INTEGER NOT NULL DEFAULT 0,
	total_ok INTEGER NOT NULL DEFAULT 0,
	total_fail INTEGER NOT NULL DEFAULT 0,
	total_denied INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS events(
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	endpoint TEXT NOT NULL,
	uid TEXT NOT NULL DEFAULT '',
	ip TEXT NOT NULL DEFAULT '',
	platform TEXT NOT NULL DEFAULT '',
	target TEXT NOT NULL DEFAULT '',
	result TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL DEFAULT 0,
	media_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts DESC);
CREATE INDEX IF NOT EXISTS idx_events_uid_ts ON events(uid, ts DESC);
`

type sqlAuditor struct {
	db        *sql.DB
	ch        chan AuditEvent
	dropped   int64
	mu        sync.Mutex
	lastPurge time.Time
}

// openSQLAuditor 打开（或创建）dataDir/analytics.db 并启动后台刷盘。
func openSQLAuditor(dataDir string) (Auditor, error) {
	s, err := newSQLAuditor(dataDir)
	if err != nil {
		return nil, err
	}
	go s.run()
	return s, nil
}

// newSQLAuditor 只建库不启动后台协程（测试用确定性刷盘）。
func newSQLAuditor(dataDir string) (*sqlAuditor, error) {
	if dataDir == "" {
		dataDir = "data"
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建审计数据目录失败: %w", err)
	}
	dbPath := filepath.Join(dataDir, "analytics.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置 WAL 失败: %w", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置 synchronous 失败: %w", err)
	}
	if _, err := db.Exec(sqlSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}
	s := &sqlAuditor{db: db, ch: make(chan AuditEvent, sqlChanCap), lastPurge: time.Now()}
	return s, nil
}

// Record 非阻塞入队；channel 满时丢弃并计数（请求路径不受影响）。
func (s *sqlAuditor) Record(ev AuditEvent) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	select {
	case s.ch <- ev:
	default:
		atomic.AddInt64(&s.dropped, 1)
	}
}

// Dropped 返回因队列满丢弃的事件数（排查用）。
func (s *sqlAuditor) Dropped() int64 {
	return atomic.LoadInt64(&s.dropped)
}

func (s *sqlAuditor) run() {
	t := time.NewTicker(sqlFlushInterval)
	defer t.Stop()
	var batch []AuditEvent
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.writeBatch(batch); err != nil {
			log.Printf("[audit] 批量写入失败(%d 条): %v", len(batch), err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case ev := <-s.ch:
			batch = append(batch, ev)
			if len(batch) >= sqlBatchMax {
				flush()
			}
		case <-t.C:
			flush()
			s.maybePurge()
		}
	}
}

type userDelta struct {
	first, last     int64
	calls, ok, fail int64
	denied          int64
}

// writeBatch 单事务写入一批事件：原始事件逐条插入，用户累计先内存聚合再 upsert。
func (s *sqlAuditor) writeBatch(batch []AuditEvent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	estmt, err := tx.Prepare(`INSERT INTO events(ts,endpoint,uid,ip,platform,target,result,reason,latency_ms,media_count)
		VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	for _, ev := range batch {
		if _, err := estmt.Exec(ev.Time.Unix(), ev.Endpoint, ev.UID, ev.IP, ev.Platform,
			ev.Target, ev.Result, ev.Remark, ev.LatencyMs, ev.MediaCount); err != nil {
			estmt.Close()
			return err
		}
	}
	estmt.Close()

	// 用户累计：先在内存按 uid 聚合，一次 upsert 一行。
	um := map[string]*userDelta{}
	for _, ev := range batch {
		if ev.UID == "" {
			continue
		}
		d, ok := um[ev.UID]
		if !ok {
			d = &userDelta{first: ev.Time.Unix(), last: ev.Time.Unix()}
			um[ev.UID] = d
		}
		ts := ev.Time.Unix()
		if ts < d.first {
			d.first = ts
		}
		if ts > d.last {
			d.last = ts
		}
		d.calls++
		switch ev.Result {
		case AuditOK:
			d.ok++
		case AuditFail:
			d.fail++
		case AuditDeny401, AuditLimit429, AuditDeny403, AuditBanned:
			d.denied++
		}
	}
	ustmt, err := tx.Prepare(`INSERT INTO users(uid,first_seen,last_seen,total_calls,total_ok,total_fail,total_denied)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(uid) DO UPDATE SET
			first_seen=min(users.first_seen, excluded.first_seen),
			last_seen=max(users.last_seen, excluded.last_seen),
			total_calls=users.total_calls+excluded.total_calls,
			total_ok=users.total_ok+excluded.total_ok,
			total_fail=users.total_fail+excluded.total_fail,
			total_denied=users.total_denied+excluded.total_denied`)
	if err != nil {
		return err
	}
	for uid, d := range um {
		if _, err := ustmt.Exec(uid, d.first, d.last, d.calls, d.ok, d.fail, d.denied); err != nil {
			ustmt.Close()
			return err
		}
	}
	ustmt.Close()

	return tx.Commit()
}

// maybePurge 每天清理一次过期事件。
func (s *sqlAuditor) maybePurge() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.lastPurge) < 24*time.Hour {
		return
	}
	s.lastPurge = time.Now()
	cutoff := time.Now().Add(-eventRetention).Unix()
	res, err := s.db.Exec(`DELETE FROM events WHERE ts < ?`, cutoff)
	if err != nil {
		log.Printf("[audit] 清理过期事件失败: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[audit] 清理 %d 条过期审计事件", n)
	}
}

// flushSync 同步刷盘：排空 channel 并写入（测试用）。
func (s *sqlAuditor) flushSync() {
	var batch []AuditEvent
	for {
		select {
		case ev := <-s.ch:
			batch = append(batch, ev)
		default:
			goto done
		}
	}
done:
	if len(batch) > 0 {
		if err := s.writeBatch(batch); err != nil {
			log.Printf("[audit] 批量写入失败(%d 条): %v", len(batch), err)
		}
	}
}

// todayStartUnix 本地日期零点的时间戳。
func todayStartUnix() int64 {
	now := time.Now()
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).Unix()
}

// Events 按条件检索审计流水（最新在前）。
func (s *sqlAuditor) Events(f EventFilter) []AuditEvent {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var sb strings.Builder
	var args []any
	sb.WriteString(`SELECT ts,endpoint,uid,ip,platform,target,result,reason,latency_ms,media_count FROM events WHERE 1=1`)
	if f.UID != "" {
		sb.WriteString(` AND uid=?`)
		args = append(args, f.UID)
	}
	if f.Platform != "" {
		sb.WriteString(` AND platform=?`)
		args = append(args, f.Platform)
	}
	if f.Result != "" {
		sb.WriteString(` AND result=?`)
		args = append(args, f.Result)
	}
	if !f.Since.IsZero() {
		sb.WriteString(` AND ts>=?`)
		args = append(args, f.Since.Unix())
	}
	sb.WriteString(` ORDER BY ts DESC, id DESC LIMIT ?`)
	args = append(args, limit)

	rows, err := s.db.Query(sb.String(), args...)
	if err != nil {
		log.Printf("[audit] 检索事件失败: %v", err)
		return nil
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var ev AuditEvent
		var ts int64
		if err := rows.Scan(&ts, &ev.Endpoint, &ev.UID, &ev.IP, &ev.Platform, &ev.Target,
			&ev.Result, &ev.Remark, &ev.LatencyMs, &ev.MediaCount); err != nil {
			continue
		}
		ev.Time = time.Unix(ts, 0)
		out = append(out, ev)
	}
	return out
}

// scanUserAgg 扫描 users 表行到 userAgg。
func scanUserAgg(rows *sql.Rows) (*userAgg, error) {
	var ua userAgg
	var first, last int64
	if err := rows.Scan(&ua.UID, &first, &last, &ua.Calls, &ua.OK, &ua.Fail, &ua.Denied); err != nil {
		return nil, err
	}
	ua.FirstSeen = time.Unix(first, 0)
	ua.LastSeen = time.Unix(last, 0)
	return &ua, nil
}

const userColumns = `uid,first_seen,last_seen,total_calls,total_ok,total_fail,total_denied`

// UserAgg 取单用户累计聚合，不存在返回 nil。
func (s *sqlAuditor) UserAgg(uid string) *userAgg {
	row := s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE uid=?`, uid)
	var ua userAgg
	var first, last int64
	if err := row.Scan(&ua.UID, &first, &last, &ua.Calls, &ua.OK, &ua.Fail, &ua.Denied); err != nil {
		return nil
	}
	ua.FirstSeen = time.Unix(first, 0)
	ua.LastSeen = time.Unix(last, 0)
	// 今日平台分布（管理页展示用）。
	rows, err := s.db.Query(`SELECT platform,COUNT(*) FROM events
		WHERE uid=? AND ts>=? AND platform!='' GROUP BY platform`, uid, todayStartUnix())
	if err == nil {
		defer rows.Close()
		ua.Platforms = map[string]int64{}
		for rows.Next() {
			var p string
			var c int64
			if rows.Scan(&p, &c) == nil {
				ua.Platforms[p] = c
			}
		}
	}
	return &ua
}

// TopUsers 今日调用量 Top N 用户。
func (s *sqlAuditor) TopUsers(n int) []*userAgg {
	if n <= 0 {
		n = 10
	}
	rows, err := s.db.Query(`
		SELECT e.uid, u.first_seen, u.last_seen,
			COUNT(*) AS calls,
			SUM(CASE WHEN e.result='ok' THEN 1 ELSE 0 END) AS ok,
			SUM(CASE WHEN e.result='fail' THEN 1 ELSE 0 END) AS fail,
			SUM(CASE WHEN e.result IN ('deny401','limit429','deny403','banned') THEN 1 ELSE 0 END) AS denied,
			SUM(CASE WHEN e.result='limit429' THEN 1 ELSE 0 END) AS l429,
			SUM(CASE WHEN e.endpoint='media' AND e.result='ok' THEN 1 ELSE 0 END) AS media
		FROM events e LEFT JOIN users u ON u.uid=e.uid
		WHERE e.ts>=? AND e.uid!=''
		GROUP BY e.uid ORDER BY calls DESC LIMIT ?`, todayStartUnix(), n)
	if err != nil {
		log.Printf("[audit] TopUsers 查询失败: %v", err)
		return nil
	}
	defer rows.Close()
	var out []*userAgg
	for rows.Next() {
		var ua userAgg
		var first, last sql.NullInt64
		if err := rows.Scan(&ua.UID, &first, &last, &ua.Calls, &ua.OK, &ua.Fail,
			&ua.Denied, &ua.Limit429, &ua.MediaDowns); err != nil {
			continue
		}
		if first.Valid {
			ua.FirstSeen = time.Unix(first.Int64, 0)
		}
		if last.Valid {
			ua.LastSeen = time.Unix(last.Int64, 0)
		}
		out = append(out, &ua)
	}
	return out
}

// SearchUsers 按 uid 短码检索用户（累计口径）。
func (s *sqlAuditor) SearchUsers(query string, n int) []*userAgg {
	if n <= 0 {
		n = 50
	}
	rows, err := s.db.Query(`SELECT `+userColumns+` FROM users
		WHERE uid LIKE '%'||?||'%' ORDER BY last_seen DESC LIMIT ?`,
		strings.ToLower(query), n)
	if err != nil {
		log.Printf("[audit] SearchUsers 查询失败: %v", err)
		return nil
	}
	defer rows.Close()
	var out []*userAgg
	for rows.Next() {
		ua, err := scanUserAgg(rows)
		if err != nil {
			continue
		}
		out = append(out, ua)
	}
	return out
}

// count 单值计数查询辅助。
func (s *sqlAuditor) count(query string, args ...any) int64 {
	var n int64
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		log.Printf("[audit] 计数查询失败: %v", err)
	}
	return n
}

// Stats 总览快照（今日口径）。
func (s *sqlAuditor) Stats() StatsSnapshot {
	t0 := todayStartUnix()
	snap := StatsSnapshot{
		DayStart:   time.Unix(t0, 0),
		ByPlatform: map[string]int64{},
	}
	snap.Logins = s.count(`SELECT COUNT(*) FROM events WHERE ts>=? AND endpoint='login' AND result='ok'`, t0)
	snap.ParseOK = s.count(`SELECT COUNT(*) FROM events WHERE ts>=? AND endpoint='parse' AND result='ok'`, t0)
	snap.ParseFail = s.count(`SELECT COUNT(*) FROM events WHERE ts>=? AND endpoint='parse' AND result='fail'`, t0)
	snap.Deny401 = s.count(`SELECT COUNT(*) FROM events WHERE ts>=? AND result='deny401'`, t0)
	snap.Limit429 = s.count(`SELECT COUNT(*) FROM events WHERE ts>=? AND result='limit429'`, t0)
	snap.MediaDowns = s.count(`SELECT COUNT(*) FROM events WHERE ts>=? AND endpoint='media' AND result='ok'`, t0)

	rows, err := s.db.Query(`SELECT platform,COUNT(*) FROM events
		WHERE ts>=? AND platform!='' GROUP BY platform ORDER BY 2 DESC`, t0)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var p string
			var c int64
			if rows.Scan(&p, &c) == nil {
				snap.ByPlatform[p] = c
			}
		}
	}
	rows, err = s.db.Query(`SELECT reason,COUNT(*) AS c FROM events
		WHERE ts>=? AND result='fail' AND reason!='' GROUP BY reason ORDER BY c DESC LIMIT 10`, t0)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var kv kvCount
			if rows.Scan(&kv.Key, &kv.Count) == nil {
				snap.FailReasons = append(snap.FailReasons, kv)
			}
		}
	}
	snap.TopUsers = s.TopUsers(10)
	return snap
}

// purgeUID 删除指定 uid 与 endpoint="admin" 的历史数据
// （审计口径变更：管理员调试行为不再计入）。
func (s *sqlAuditor) purgeUID(uid string) {
	if _, err := s.db.Exec(`DELETE FROM events WHERE uid=? OR endpoint='admin'`, uid); err != nil {
		log.Printf("[audit] 清理历史事件失败: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE uid=?`, uid); err != nil {
		log.Printf("[audit] 清理历史用户失败: %v", err)
	}
}
