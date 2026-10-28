// Package state 是本程序的本地档案（SQLite）。
//
// 所有工作空间共用同一个 state.db，靠每张表的 workspace_id 列隔离：
// 一个库文件更好备份、统计口径一致，也不需要在不同工作空间之间搬数据。
// 内存缓存仍是读路径与工作窗口；本包只负责"重启之后还在"的那部分：
// 消息、选窗锚点、按群学到的 self.user_id、群人格关键字、成员显示名。
//
// 失败模式刻意是"降级"：打不开或写失败只记日志，调用方继续用内存缓存。
// 档案丢了大不了没有历史，不能让一次磁盘满把回复也一起弄没。
package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"good-review-master/cache"
	"good-review-master/logutil"

	_ "modernc.org/sqlite"
)

// FileName 是档案文件名，放在工作目录（config.yaml 同级）。
const FileName = "state.db"

const (
	// schemaVersion 当前表结构版本。v1 是"每工作空间一个库"的旧结构，
	// v2 起所有表都带 workspace_id；遇到更旧的版本会重建本包自己的表。
	schemaVersion = 2

	// writeQueueSize 异步写队列容量。群聊消息量很低，这个值足够吸收一次突发；
	// 满了丢 DB 写（内存缓存仍有），入站路径绝不能被磁盘拖住。
	writeQueueSize = 1024

	// warnRows / warnBytes 启动统计的告警阈值（刻意不做成配置：清理是人工决定）。
	warnRows  = 100_000
	warnBytes = 100 << 20
)

// Store 共享的 SQLite 档案。Scope 负责补上 workspace_id。
type Store struct {
	db   *sql.DB
	path string

	queue chan writeItem
	wg    sync.WaitGroup

	// queueMu 保护"关闭时关 channel"与"并发入队"之间的竞争：
	// 只靠 closed 标志会在检查之后、发送之前被 Close 插进来，往已关闭的 channel 发送会 panic。
	queueMu   sync.RWMutex
	closeOnce sync.Once
	closed    atomic.Bool
	dropped   atomic.Int64
	failures  atomic.Int64
}

type writeItem struct {
	workspaceID string
	msg         cache.Message
}

// Scope 一个工作空间的视图。所有方法都自动带上 workspace_id。
type Scope struct {
	store       *Store
	workspaceID string
}

// Open 打开（必要时创建）指定路径的 state.db。
func Open(path string) (*Store, error) {
	if err := ensureFile(path); err != nil {
		return nil, err
	}

	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 %s 失败：%w", path, err)
	}
	// 单进程单连接：WAL 下多写并发只会换来 database is locked，不会更快。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := migrate(db, path); err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{db: db, path: path, queue: make(chan writeItem, writeQueueSize)}
	s.wg.Add(1)
	go s.writeLoop()
	return s, nil
}

// Scope 返回某个工作空间的视图。Store 为 nil 时返回 nil，调用方方法可安全空调用。
func (s *Store) Scope(workspaceID string) *Scope {
	if s == nil {
		return nil
	}
	return &Scope{store: s, workspaceID: workspaceID}
}

// ensureFile 先以 0600 建出文件再交给驱动：消息原文与昵称属于隐私数据，
// 不能让 sqlite 按 umask 建出 0644。
func ensureFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 %s 的目录失败：%w", path, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("创建 %s 失败：%w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭 %s 失败：%w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		logutil.Warn("无法把 state.db 权限设为 0600", "path", path, "err", err)
	}
	return nil
}

func migrate(db *sql.DB, path string) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("读取 state.db 版本失败：%w", err)
	}
	if version >= schemaVersion {
		return nil
	}
	// v1 是"每工作空间一个库"的结构：表里没有 workspace_id，而且同一个库
	// 只会属于一个工作空间，无法推断归属。这里是全新文件（version=0）或
	// 未发布的旧结构，重建本包自己的表即可，不动其它数据。
	if version > 0 {
		logutil.Warn("state.db 是旧版结构（无 workspace_id），重建档案表", "path", path, "version", version)
	}
	for _, table := range []string{"messages", "anchors", "identities", "members", "group_personas", "meta"} {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			return fmt.Errorf("重建 state.db 失败：%w", err)
		}
	}
	statements := []string{
		`CREATE TABLE messages (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			message_id   TEXT NOT NULL,
			user_id      TEXT NOT NULL,
			content      TEXT NOT NULL,
			images       TEXT NOT NULL DEFAULT '[]',
			reply_to     TEXT NOT NULL DEFAULT '',
			ts           INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id, message_id)
		)`,
		`CREATE INDEX idx_messages_ts ON messages(workspace_id, group_id, ts)`,
		`CREATE TABLE anchors (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			start_id     TEXT NOT NULL DEFAULT '',
			last_sent_id TEXT NOT NULL DEFAULT '',
			updated_at   INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id)
		)`,
		`CREATE TABLE identities (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			self_user_id TEXT NOT NULL,
			learned_at   INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id)
		)`,
		`CREATE TABLE members (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			user_id      TEXT NOT NULL,
			name         TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			updated_at   INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id, user_id)
		)`,
		`CREATE TABLE group_personas (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			keyword      TEXT NOT NULL,
			PRIMARY KEY (workspace_id, group_id)
		)`,
		`CREATE TABLE meta (
			workspace_id TEXT NOT NULL,
			key          TEXT NOT NULL,
			value        TEXT NOT NULL,
			PRIMARY KEY (workspace_id, key)
		)`,
		`PRAGMA user_version = 2`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("初始化 state.db 失败：%w", err)
		}
	}
	return nil
}

// SaveMessage 异步写入一条消息。队列满时丢弃 DB 写并记计数，不阻塞调用方。
func (s *Scope) SaveMessage(msg cache.Message) {
	if s == nil || s.store == nil || msg.MessageID == "" {
		return
	}
	st := s.store
	st.queueMu.RLock()
	defer st.queueMu.RUnlock()
	if st.closed.Load() {
		return
	}
	select {
	case st.queue <- writeItem{workspaceID: s.workspaceID, msg: msg}:
	default:
		if n := st.dropped.Add(1); n%100 == 1 {
			logutil.Warn("state.db 写队列已满，消息只留在内存缓存", "dropped", n, "path", st.path)
		}
	}
}

func (s *Store) writeLoop() {
	defer s.wg.Done()
	const insert = `INSERT OR REPLACE INTO messages
		(workspace_id, group_id, message_id, user_id, content, images, reply_to, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	for item := range s.queue {
		msg := item.msg
		images, err := json.Marshal(msg.Images)
		if err != nil {
			images = []byte("[]")
		}
		if _, err := s.db.Exec(insert, item.workspaceID, msg.GroupID, msg.MessageID, msg.UserID,
			msg.Content, string(images), msg.ReplyToID, msg.Time); err != nil {
			s.noteFailure("写入消息", err)
		}
	}
}

func (s *Store) noteFailure(action string, err error) {
	if n := s.failures.Add(1); n%100 == 1 {
		logutil.Error("state.db "+action+"失败，继续用内存缓存", "err", err, "path", s.path, "failures", n)
	}
}

// LoadRecent 每个群读回最近 limit 条（时间升序），供启动时填内存缓存。
func (s *Scope) LoadRecent(limit int) ([]cache.Message, error) {
	if s == nil || s.store == nil || limit <= 0 {
		return nil, nil
	}
	rows, err := s.store.db.Query(`
		SELECT group_id, message_id, user_id, content, images, reply_to, ts FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY group_id ORDER BY ts DESC) AS rn
			FROM messages WHERE workspace_id = ?
		) WHERE rn <= ? ORDER BY group_id, ts`, s.workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []cache.Message
	for rows.Next() {
		var msg cache.Message
		var images string
		if err := rows.Scan(&msg.GroupID, &msg.MessageID, &msg.UserID, &msg.Content,
			&images, &msg.ReplyToID, &msg.Time); err != nil {
			return nil, err
		}
		if images != "" {
			_ = json.Unmarshal([]byte(images), &msg.Images)
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// LoadAnchors 返回本工作空间全部群的选窗锚点。
func (s *Scope) LoadAnchors() (map[string]cache.LLMAnchor, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	rows, err := s.store.db.Query(`SELECT group_id, start_id, last_sent_id FROM anchors WHERE workspace_id = ?`,
		s.workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]cache.LLMAnchor)
	for rows.Next() {
		var group string
		var anchor cache.LLMAnchor
		if err := rows.Scan(&group, &anchor.Start, &anchor.LastSent); err != nil {
			return nil, err
		}
		out[group] = anchor
	}
	return out, rows.Err()
}

// SaveAnchor 锚点变更很少，直接同步写：异步丢了它会让下次选窗退回重置。
func (s *Scope) SaveAnchor(groupID string, anchor cache.LLMAnchor) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	_, err := s.store.db.Exec(`INSERT OR REPLACE INTO anchors
		(workspace_id, group_id, start_id, last_sent_id, updated_at) VALUES (?, ?, ?, ?, ?)`,
		s.workspaceID, groupID, anchor.Start, anchor.LastSent, time.Now().Unix())
	if err != nil {
		s.store.noteFailure("保存锚点", err)
	}
}

// SaveIdentity 记录本群学到的机器人 user_id（可能是哨兵）。
func (s *Scope) SaveIdentity(groupID, selfUserID string) {
	if s == nil || s.store == nil || selfUserID == "" || s.store.closed.Load() {
		return
	}
	_, err := s.store.db.Exec(`INSERT OR REPLACE INTO identities
		(workspace_id, group_id, self_user_id, learned_at) VALUES (?, ?, ?, ?)`,
		s.workspaceID, groupID, selfUserID, time.Now().Unix())
	if err != nil {
		s.store.noteFailure("保存身份", err)
	}
}

// LoadIdentities 返回各群学到的机器人 user_id。
func (s *Scope) LoadIdentities() (map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	rows, err := s.store.db.Query(`SELECT group_id, self_user_id FROM identities WHERE workspace_id = ?`,
		s.workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var group, selfID string
		if err := rows.Scan(&group, &selfID); err != nil {
			return nil, err
		}
		out[group] = selfID
	}
	return out, rows.Err()
}

// SaveMember 记录群成员的显示名（get_group_member_info 的结果）。
func (s *Scope) SaveMember(groupID, userID, name, displayName string) {
	if s == nil || s.store == nil || userID == "" || s.store.closed.Load() {
		return
	}
	_, err := s.store.db.Exec(`INSERT OR REPLACE INTO members
		(workspace_id, group_id, user_id, name, display_name, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		s.workspaceID, groupID, userID, name, displayName, time.Now().Unix())
	if err != nil {
		s.store.noteFailure("保存成员名", err)
	}
}

// LoadMembers 返回 group → user → 显示名。
func (s *Scope) LoadMembers() (map[string]map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	rows, err := s.store.db.Query(`SELECT group_id, user_id, name, display_name FROM members WHERE workspace_id = ?`,
		s.workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]map[string]string)
	for rows.Next() {
		var group, userID, name, display string
		if err := rows.Scan(&group, &userID, &name, &display); err != nil {
			return nil, err
		}
		if out[group] == nil {
			out[group] = make(map[string]string)
		}
		if display == "" {
			display = name
		}
		out[group][userID] = display
	}
	return out, rows.Err()
}

// SaveGroupPersona 记录某群当前人格的源关键字。
func (s *Scope) SaveGroupPersona(groupID, keyword string) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	_, err := s.store.db.Exec(`INSERT OR REPLACE INTO group_personas (workspace_id, group_id, keyword)
		VALUES (?, ?, ?)`, s.workspaceID, groupID, keyword)
	if err != nil {
		s.store.noteFailure("保存群人格", err)
	}
}

// DeleteGroupPersona 清除某群人格。
func (s *Scope) DeleteGroupPersona(groupID string) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	if _, err := s.store.db.Exec(`DELETE FROM group_personas WHERE workspace_id = ? AND group_id = ?`,
		s.workspaceID, groupID); err != nil {
		s.store.noteFailure("删除群人格", err)
	}
}

// LoadGroupPersonas 返回 group → 人格关键字。
func (s *Scope) LoadGroupPersonas() (map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	rows, err := s.store.db.Query(`SELECT group_id, keyword FROM group_personas WHERE workspace_id = ?`,
		s.workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var group, keyword string
		if err := rows.Scan(&group, &keyword); err != nil {
			return nil, err
		}
		out[group] = keyword
	}
	return out, rows.Err()
}

// SetMeta 写一条工作空间元信息（platform / account / name）。
func (s *Scope) SetMeta(key, value string) {
	if s == nil || s.store == nil || key == "" || s.store.closed.Load() {
		return
	}
	if _, err := s.store.db.Exec(`INSERT OR REPLACE INTO meta (workspace_id, key, value) VALUES (?, ?, ?)`,
		s.workspaceID, key, value); err != nil {
		s.store.noteFailure("写入 meta", err)
	}
}

// Meta 读一条元信息，不存在返回空串。
func (s *Scope) Meta(key string) string {
	if s == nil || s.store == nil {
		return ""
	}
	var value string
	if err := s.store.db.QueryRow(`SELECT value FROM meta WHERE workspace_id = ? AND key = ?`,
		s.workspaceID, key).Scan(&value); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.store.noteFailure("读取 meta", err)
		}
		return ""
	}
	return value
}

// WorkspaceMeta 是重启恢复工作空间所需的最小信息。
// 工作空间不再有目录或配置文件，meta 表就是它在磁盘上的身份。
type WorkspaceMeta struct {
	ID       string
	Platform string
	Account  string
}

// Workspaces 返回库里记录过的全部工作空间（来自 meta 的 platform/account）。
// 缺任一字段的条目由调用方决定是否跳过——这里只负责把库里的东西原样拿出来。
func (s *Store) Workspaces() ([]WorkspaceMeta, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT workspace_id, key, value FROM meta WHERE key IN ('platform', 'account')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byID := make(map[string]*WorkspaceMeta)
	for rows.Next() {
		var id, key, value string
		if err := rows.Scan(&id, &key, &value); err != nil {
			return nil, err
		}
		meta := byID[id]
		if meta == nil {
			meta = &WorkspaceMeta{ID: id}
			byID[id] = meta
		}
		switch key {
		case "platform":
			meta.Platform = value
		case "account":
			meta.Account = value
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]WorkspaceMeta, 0, len(byID))
	for _, meta := range byID {
		out = append(out, *meta)
	}
	return out, nil
}

// Report 统计消息条数与库文件体积（含 WAL），超阈值时用 warn 提示人工清理。
// 不自动删除：这些是用户的聊天档案，删不删由人决定。
func (s *Store) Report() {
	if s == nil {
		return
	}
	rows, size := s.Stats()
	logutil.Info("本地档案统计", "path", s.path, "messages", rows, "db_bytes", size)

	perWorkspace, err := s.countByWorkspace()
	if err != nil {
		s.noteFailure("统计各工作空间条数", err)
	} else {
		for workspaceID, count := range perWorkspace {
			logutil.Info("工作空间档案", "workspace", workspaceID, "messages", count)
		}
	}

	if rows > warnRows || size > warnBytes {
		logutil.Warn("state.db 数据偏多，确认是否需要清理",
			"path", s.path, "messages", rows, "db_bytes", size,
			"提示", "停服后删除 state.db（含 -wal/-shm）重建，或手工 DELETE + VACUUM")
	}
}

func (s *Store) countByWorkspace() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT workspace_id, COUNT(*) FROM messages GROUP BY workspace_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var workspaceID string
		var count int64
		if err := rows.Scan(&workspaceID, &count); err != nil {
			return nil, err
		}
		out[workspaceID] = count
	}
	return out, rows.Err()
}

// Stats 返回库里的消息总条数与库文件体积（含 -wal）。
func (s *Store) Stats() (rows int64, bytes int64) {
	if s == nil {
		return 0, 0
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&rows); err != nil {
		s.noteFailure("统计消息条数", err)
	}
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			bytes += info.Size()
		}
	}
	return rows, bytes
}

// Close 停止写入并关闭数据库。可重复调用。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() {
		s.queueMu.Lock()
		s.closed.Store(true)
		close(s.queue)
		s.queueMu.Unlock()
		s.wg.Wait()
		err = s.db.Close()
	})
	return err
}
