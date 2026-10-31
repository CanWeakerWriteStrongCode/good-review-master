// Package state 是本程序的本地档案（SQLite）。
//
// 所有工作空间共用同一个 state.db，靠每张表的 workspace_id 列隔离。
// 消息与身份信息同步写：一条消息处理完成（Ack）之前必须已经落库，
// 不再保留“写队列满了就丢库写”的旧语义。读路径优先走 Ristretto 缓存，miss 再回源。
//
// 失败模式刻意是“降级”：打不开或写失败只记日志，调用方继续用内存缓存。
// 档案丢了大不了没有历史，不能让一次磁盘满把回复也一起弄没。
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"good-review-master/cache"
	"good-review-master/cache/kv"
	"good-review-master/logutil"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// FileName 是档案文件名，放在工作目录（config.yaml 同级）。
const FileName = "state.db"

const (
	// schemaVersion 当前表结构版本。
	// v1 是“每工作空间一个库”的旧结构；v2 起所有表带 workspace_id；
	// v3 groups；v4 messages.ref_idx；v5 messages.image_ids + image_notes；
	// v6 workspace_profile 取代 meta；v7 成员/群历史表；
	// v8 资产欠条、发送台账、报警、全局 settings、image_notes.prompt_version。
	schemaVersion = 8

	// warnRows / warnBytes 启动统计的告警阈值（清理是人工决定）。
	warnRows  = 100_000
	warnBytes = 100 << 20

	// kvBudget 是进程内缓存的近似内存预算。
	kvBudget = 64 << 20
	// 缓存 TTL：成员/群信息会被平台改写，不能永久缓存；图片描述相对稳定。
	memberCacheTTL = 7 * 24 * time.Hour
	groupCacheTTL  = 7 * 24 * time.Hour
	imageCacheTTL  = 30 * 24 * time.Hour
)

// Store 共享的 SQLite 档案。Scope 负责补上 workspace_id。
type Store struct {
	db   *gorm.DB
	path string
	kv   kv.KV

	closed   atomic.Bool
	failures atomic.Int64
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
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("打开 %s 失败：%w", path, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取 %s 的连接失败：%w", path, err)
	}
	// 单进程单连接：WAL 下多写并发只会换来 database is locked，不会更快。
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接 %s 失败：%w", path, err)
	}
	if err := migrate(db, path); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	store := &Store{db: db, path: path, kv: kv.Noop{}}
	if cache, err := kv.NewRistretto(kvBudget); err != nil {
		logutil.Warn("初始化 Ristretto 缓存失败，退化为每次回源", "err", err)
	} else {
		store.kv = cache
	}
	return store, nil
}

// Scope 返回某个工作空间的视图。Store 为 nil 时返回 nil，调用方方法可安全空调用。
func (s *Store) Scope(workspaceID string) *Scope {
	if s == nil {
		return nil
	}
	return &Scope{store: s, workspaceID: workspaceID}
}

// KV 返回进程内缓存（测试与面板用）。
func (s *Store) KV() kv.KV {
	if s == nil {
		return kv.Noop{}
	}
	return s.kv
}

// ensureFile 先以 0600 建出文件再交给驱动：消息原文与昵称属于隐私数据。
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

func migrate(db *gorm.DB, path string) error {
	var version int
	if err := db.Raw(`PRAGMA user_version`).Scan(&version).Error; err != nil {
		return fmt.Errorf("读取 state.db 版本失败：%w", err)
	}
	if version >= schemaVersion {
		return nil
	}
	if version > 0 && version < 2 {
		logutil.Warn("state.db 是旧版结构（无 workspace_id），重建档案表", "path", path, "version", version)
	}
	if version < 2 {
		for _, table := range []string{
			"messages", "anchors", "identities", "members", "group_personas", "meta", "groups",
			"workspace_profile", "member_info_history", "group_info_history",
			"asset_tag_jobs", "send_ledger", "alerts", "settings", "image_notes",
		} {
			if err := db.Exec(`DROP TABLE IF EXISTS ` + table).Error; err != nil {
				return fmt.Errorf("重建 state.db 失败：%w", err)
			}
		}
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS messages (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			message_id   TEXT NOT NULL,
			user_id      TEXT NOT NULL,
			content      TEXT NOT NULL,
			images       TEXT NOT NULL DEFAULT '[]',
			image_ids    TEXT NOT NULL DEFAULT '[]',
			reply_to     TEXT NOT NULL DEFAULT '',
			ref_idx      TEXT NOT NULL DEFAULT '',
			ts           INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id, message_id)
		)`,
		`CREATE TABLE IF NOT EXISTS anchors (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			start_id     TEXT NOT NULL DEFAULT '',
			last_sent_id TEXT NOT NULL DEFAULT '',
			updated_at   INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id)
		)`,
		`CREATE TABLE IF NOT EXISTS identities (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			self_user_id TEXT NOT NULL,
			learned_at   INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id)
		)`,
		`CREATE TABLE IF NOT EXISTS members (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			user_id      TEXT NOT NULL,
			name         TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			extra        TEXT NOT NULL DEFAULT '{}',
			updated_at   INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS groups (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			name         TEXT NOT NULL DEFAULT '',
			extra        TEXT NOT NULL DEFAULT '{}',
			updated_at   INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id)
		)`,
		`CREATE TABLE IF NOT EXISTS group_personas (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			keyword      TEXT NOT NULL,
			PRIMARY KEY (workspace_id, group_id)
		)`,
		`CREATE TABLE IF NOT EXISTS workspace_profile (
			workspace_id    TEXT PRIMARY KEY,
			platform        TEXT NOT NULL DEFAULT '',
			account_kind    TEXT NOT NULL DEFAULT '',
			account         TEXT NOT NULL DEFAULT '',
			name            TEXT NOT NULL DEFAULT '',
			impl            TEXT NOT NULL DEFAULT '',
			identity_scheme TEXT NOT NULL DEFAULT '',
			identity_scope  TEXT NOT NULL DEFAULT '',
			extra           TEXT NOT NULL DEFAULT '{}',
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS member_info_history (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id      TEXT NOT NULL,
			group_id          TEXT NOT NULL,
			user_id           TEXT NOT NULL,
			name              TEXT NOT NULL DEFAULT '',
			display_name      TEXT NOT NULL DEFAULT '',
			source            TEXT NOT NULL DEFAULT '',
			source_message_id TEXT NOT NULL DEFAULT '',
			extra             TEXT NOT NULL DEFAULT '{}',
			changed_at        INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS group_info_history (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id      TEXT NOT NULL,
			group_id          TEXT NOT NULL,
			name              TEXT NOT NULL DEFAULT '',
			source            TEXT NOT NULL DEFAULT '',
			source_message_id TEXT NOT NULL DEFAULT '',
			extra             TEXT NOT NULL DEFAULT '{}',
			changed_at        INTEGER NOT NULL
		)`,
		// image_notes 是全局图库：不带 workspace_id，主键是内容哈希 + 字节大小。
		`CREATE TABLE IF NOT EXISTS image_notes (
			sha256         TEXT NOT NULL,
			byte_size      INTEGER NOT NULL,
			mime           TEXT NOT NULL DEFAULT '',
			width          INTEGER NOT NULL DEFAULT 0,
			height         INTEGER NOT NULL DEFAULT 0,
			animated       INTEGER NOT NULL DEFAULT 0,
			note           TEXT NOT NULL DEFAULT '',
			info_level     TEXT NOT NULL DEFAULT 'unknown',
			has_text       INTEGER NOT NULL DEFAULT 0,
			status         TEXT NOT NULL DEFAULT 'ok',
			attempts       INTEGER NOT NULL DEFAULT 0,
			model          TEXT NOT NULL DEFAULT '',
			prompt_version TEXT NOT NULL DEFAULT '',
			updated_at     INTEGER NOT NULL,
			PRIMARY KEY (sha256, byte_size)
		)`,
		`CREATE TABLE IF NOT EXISTS asset_tag_jobs (
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			message_id   TEXT NOT NULL,
			asset_index  INTEGER NOT NULL DEFAULT 0,
			kind         TEXT NOT NULL DEFAULT 'image',
			url          TEXT NOT NULL DEFAULT '',
			local_path   TEXT NOT NULL DEFAULT '',
			status       TEXT NOT NULL DEFAULT 'pending',
			attempts     INTEGER NOT NULL DEFAULT 0,
			last_error   TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id, message_id, asset_index)
		)`,
		`CREATE TABLE IF NOT EXISTS send_ledger (
			workspace_id TEXT NOT NULL,
			action_id    TEXT NOT NULL,
			message_id   TEXT NOT NULL DEFAULT '',
			text         TEXT NOT NULL DEFAULT '',
			sent         INTEGER NOT NULL DEFAULT 0,
			send_ok      INTEGER NOT NULL DEFAULT 0,
			updated_at   INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, action_id)
		)`,
		`CREATE TABLE IF NOT EXISTS alerts (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id TEXT NOT NULL DEFAULT '',
			kind         TEXT NOT NULL,
			detail       TEXT NOT NULL DEFAULT '{}',
			created_at   INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("初始化 state.db 失败：%w", err)
		}
	}

	// 老库补列。必须在建索引/写数据之前。
	if version < 4 {
		if err := ensureColumn(db, "messages", "ref_idx", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if version < 5 {
		if err := ensureColumn(db, "messages", "image_ids", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
			return err
		}
	}
	if version < 8 {
		if err := ensureColumn(db, "members", "extra", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
			return err
		}
		if err := ensureColumn(db, "groups", "extra", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
			return err
		}
		if err := ensureColumn(db, "image_notes", "prompt_version", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}

	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages(workspace_id, group_id, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_ref ON messages(workspace_id, group_id, ref_idx)`,
		`CREATE INDEX IF NOT EXISTS idx_member_info_history ON member_info_history(workspace_id, group_id, user_id, changed_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_member_info_history_source
			ON member_info_history(workspace_id, group_id, user_id, source_message_id)
			WHERE source_message_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_group_info_history ON group_info_history(workspace_id, group_id, changed_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_group_info_history_source
			ON group_info_history(workspace_id, group_id, source_message_id)
			WHERE source_message_id <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_workspace_profile_identity
			ON workspace_profile(platform, account_kind, account)
			WHERE platform <> '' AND account_kind <> '' AND account <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_asset_tag_jobs_pending ON asset_tag_jobs(status, updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_alerts_created ON alerts(created_at)`,
	}
	for _, statement := range indexes {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("创建 state.db 索引失败：%w", err)
		}
	}

	// v6：旧 meta 透视进 workspace_profile，然后删除 meta。
	if version < 6 {
		if tableExists(db, "meta") {
			if err := db.Exec(`INSERT INTO workspace_profile
				(workspace_id, platform, account_kind, account, name, created_at, updated_at)
				SELECT workspace_id,
				       COALESCE(MAX(CASE WHEN key = 'platform' THEN value END), ''),
				       COALESCE(MAX(CASE WHEN key = 'account_kind' THEN value END), ''),
				       COALESCE(MAX(CASE WHEN key = 'account' THEN value END), ''),
				       COALESCE(MAX(CASE WHEN key = 'name' THEN value END), ''),
				       strftime('%s', 'now'),
				       strftime('%s', 'now')
				FROM meta
				GROUP BY workspace_id
				ON CONFLICT(workspace_id) DO NOTHING`).Error; err != nil {
				return fmt.Errorf("迁移 workspace_profile 失败：%w", err)
			}
			if err := db.Exec(`DROP TABLE meta`).Error; err != nil {
				return fmt.Errorf("删除旧 meta 表失败：%w", err)
			}
		}
	}
	if err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)).Error; err != nil {
		return fmt.Errorf("更新 state.db 版本失败：%w", err)
	}
	return nil
}

func ensureColumn(db *gorm.DB, table, column, ddl string) error {
	var count int
	if err := db.Raw(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count).Error; err != nil {
		return fmt.Errorf("读取 %s 表结构失败：%w", table, err)
	}
	if count > 0 {
		return nil
	}
	if err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + ddl).Error; err != nil {
		return fmt.Errorf("给 %s 补列 %s 失败：%w", table, column, err)
	}
	return nil
}

func tableExists(db *gorm.DB, table string) bool {
	var count int
	if err := db.Raw(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count).Error; err != nil {
		return false
	}
	return count > 0
}

// SaveMessage 同步写入一条消息。失败只记日志，不阻塞调用方。
func (s *Scope) SaveMessage(msg cache.Message) {
	if s == nil || s.store == nil || msg.MessageID == "" || s.store.closed.Load() {
		return
	}
	images, err := json.Marshal(msg.Images)
	if err != nil {
		images = []byte("[]")
	}
	imageIDs, err := json.Marshal(msg.ImageIDs)
	if err != nil {
		imageIDs = []byte("[]")
	}
	row := messageRow{
		WorkspaceID: s.workspaceID, GroupID: msg.GroupID, MessageID: msg.MessageID,
		UserID: msg.UserID, Content: msg.Content, Images: string(images),
		ImageIDs: string(imageIDs), ReplyTo: msg.ReplyToID, RefIdx: msg.RefIdx, TS: msg.Time,
	}
	if err := s.store.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
		s.store.noteFailure("写入消息", err)
	}
}

// UpdateMessageImages 在 Stage 2 把图片内容标识补写到已入库的消息上。
func (s *Scope) UpdateMessageImages(groupID, messageID string, ids []cache.ImageID) error {
	if s == nil || s.store == nil || messageID == "" || s.store.closed.Load() {
		return nil
	}
	payload, err := json.Marshal(ids)
	if err != nil {
		payload = []byte("[]")
	}
	if err := s.store.db.Model(&messageRow{}).
		Where("workspace_id = ? AND group_id = ? AND message_id = ?", s.workspaceID, groupID, messageID).
		Update("image_ids", string(payload)).Error; err != nil {
		s.store.noteFailure("更新消息图片标识", err)
		return err
	}
	return nil
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
	rows, err := s.store.db.Raw(`
		SELECT group_id, message_id, user_id, content, images, image_ids, reply_to, ref_idx, ts FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY group_id ORDER BY ts DESC) AS rn
			FROM messages WHERE workspace_id = ?
		) WHERE rn <= ? ORDER BY group_id, ts`, s.workspaceID, limit).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []cache.Message
	for rows.Next() {
		var msg cache.Message
		var images, imageIDs string
		if err := rows.Scan(&msg.GroupID, &msg.MessageID, &msg.UserID, &msg.Content,
			&images, &imageIDs, &msg.ReplyToID, &msg.RefIdx, &msg.Time); err != nil {
			return nil, err
		}
		if images != "" {
			_ = json.Unmarshal([]byte(images), &msg.Images)
		}
		if imageIDs != "" {
			_ = json.Unmarshal([]byte(imageIDs), &msg.ImageIDs)
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.hydrateImageNotes(out)
	return out, nil
}

func (s *Scope) hydrateImageNotes(msgs []cache.Message) {
	var ids []cache.ImageID
	for _, msg := range msgs {
		ids = append(ids, msg.ImageIDs...)
	}
	if len(ids) == 0 {
		return
	}
	notes, err := s.store.GetImageNotes(ids)
	if err != nil {
		s.store.noteFailure("读取图片描述", err)
		return
	}
	for i := range msgs {
		if len(msgs[i].ImageIDs) == 0 {
			continue
		}
		notesForMsg := make([]cache.ImageNote, len(msgs[i].ImageIDs))
		for j, id := range msgs[i].ImageIDs {
			notesForMsg[j] = notes[id]
		}
		msgs[i].ImageNotes = notesForMsg
	}
}

// ImageNote 是全局图库的一行（image_notes 表，不含 workspace_id）。
type ImageNote = cache.ImageNote

func imageKVKey(id cache.ImageID) string {
	return fmt.Sprintf("img:%s:%d", id.SHA, id.Size)
}

// GetImageNotes 按 (sha256, byte_size) 批量读取图片描述，返回以 ImageID 为键的 map。
// 先读 Ristretto，miss 的再查库并回填缓存。
func (s *Store) GetImageNotes(ids []cache.ImageID) (map[cache.ImageID]cache.ImageNote, error) {
	if s == nil || len(ids) == 0 {
		return nil, nil
	}
	out := make(map[cache.ImageID]cache.ImageNote, len(ids))
	bySHA := make(map[string]cache.ImageID, len(ids))
	missing := make([]cache.ImageID, 0, len(ids))
	seen := make(map[cache.ImageID]struct{}, len(ids))
	for _, id := range ids {
		if id.SHA == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		bySHA[id.SHA] = id
		if value, ok, _ := s.kv.Get(nil, imageKVKey(id)); ok {
			var note cache.ImageNote
			if json.Unmarshal(value, &note) == nil {
				out[id] = note
				continue
			}
		}
		missing = append(missing, id)
	}
	if len(missing) == 0 {
		return out, nil
	}

	args := make([]any, 0, len(missing))
	for _, id := range missing {
		args = append(args, id.SHA)
	}
	const chunkSize = 400
	for start := 0; start < len(args); start += chunkSize {
		end := min(start+chunkSize, len(args))
		chunk := args[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		rows, err := s.db.Raw(`SELECT sha256, byte_size, mime, width, height, animated, note, info_level, has_text, status, attempts, model, prompt_version
			FROM image_notes WHERE sha256 IN (`+placeholders+`)`, chunk...).Rows()
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var note cache.ImageNote
			var animated, hasText int
			if err := rows.Scan(&note.SHA, &note.Size, &note.MIME, &note.Width, &note.Height,
				&animated, &note.Note, &note.InfoLevel, &hasText, &note.Status, &note.Attempts,
				&note.Model, &note.PromptVersion); err != nil {
				rows.Close()
				return out, err
			}
			note.Animated = animated != 0
			note.HasText = hasText != 0
			if want, ok := bySHA[note.SHA]; ok && want.Size == note.Size {
				out[want] = note
				if payload, err := json.Marshal(note); err == nil {
					_ = s.kv.Set(nil, imageKVKey(want), payload, imageCacheTTL)
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *Store) SaveImageNote(note cache.ImageNote) error {
	if s == nil || note.SHA == "" {
		return nil
	}
	if note.Status == "" {
		note.Status = "ok"
	}
	if err := s.upsertImageNote(note); err != nil {
		return err
	}
	if payload, err := json.Marshal(note); err == nil {
		_ = s.kv.Set(nil, imageKVKey(note.ImageID), payload, imageCacheTTL)
	}
	return nil
}

func (s *Store) MarkImageNoteFailed(note cache.ImageNote) error {
	if s == nil || note.SHA == "" {
		return nil
	}
	note.Status = "failed"
	if err := s.upsertImageNote(note); err != nil {
		return err
	}
	_ = s.kv.Delete(nil, imageKVKey(note.ImageID))
	return nil
}

func (s *Store) upsertImageNote(note cache.ImageNote) error {
	animated, hasText := 0, 0
	if note.Animated {
		animated = 1
	}
	if note.HasText {
		hasText = 1
	}
	return s.db.Exec(`INSERT INTO image_notes
		(sha256, byte_size, mime, width, height, animated, note, info_level, has_text, status, attempts, model, prompt_version, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sha256, byte_size) DO UPDATE SET
			mime = CASE WHEN excluded.mime <> '' THEN excluded.mime ELSE image_notes.mime END,
			width = CASE WHEN excluded.width > 0 THEN excluded.width ELSE image_notes.width END,
			height = CASE WHEN excluded.height > 0 THEN excluded.height ELSE image_notes.height END,
			animated = excluded.animated,
			note = excluded.note,
			info_level = excluded.info_level,
			has_text = excluded.has_text,
			status = excluded.status,
			attempts = excluded.attempts,
			model = excluded.model,
			prompt_version = excluded.prompt_version,
			updated_at = excluded.updated_at`,
		note.SHA, note.Size, note.MIME, note.Width, note.Height, animated, note.Note,
		note.InfoLevel, hasText, note.Status, note.Attempts, note.Model, note.PromptVersion, time.Now().Unix()).Error
}

// ResolveMessageID 用平台引用索引反查本群被引消息的 message_id。
func (s *Scope) ResolveMessageID(groupID, refIdx string) string {
	if s == nil || s.store == nil || groupID == "" || refIdx == "" {
		return ""
	}
	var id string
	err := s.store.db.Raw(`SELECT message_id FROM messages
		WHERE workspace_id = ? AND group_id = ? AND ref_idx = ? AND ref_idx <> ''
		ORDER BY ts DESC LIMIT 1`, s.workspaceID, groupID, refIdx).Scan(&id).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			s.store.noteFailure("反查引用消息", err)
		}
		return ""
	}
	return id
}

// LoadAnchors 返回本工作空间全部群的选窗锚点。
func (s *Scope) LoadAnchors() (map[string]cache.LLMAnchor, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	var rows []anchorRow
	if err := s.store.db.Where("workspace_id = ?", s.workspaceID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]cache.LLMAnchor, len(rows))
	for _, row := range rows {
		out[row.GroupID] = cache.LLMAnchor{Start: row.StartID, LastSent: row.LastSentID}
	}
	return out, nil
}

// SaveAnchor 锚点变更很少，直接同步写。
func (s *Scope) SaveAnchor(groupID string, anchor cache.LLMAnchor) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	row := anchorRow{WorkspaceID: s.workspaceID, GroupID: groupID, StartID: anchor.Start,
		LastSentID: anchor.LastSent, UpdatedAt: time.Now().Unix()}
	if err := s.store.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
		s.store.noteFailure("保存锚点", err)
	}
}

// SaveIdentity 记录本群学到的机器人 user_id（可能是哨兵）。
func (s *Scope) SaveIdentity(groupID, selfUserID string) {
	if s == nil || s.store == nil || selfUserID == "" || s.store.closed.Load() {
		return
	}
	row := identityRow{WorkspaceID: s.workspaceID, GroupID: groupID, SelfUserID: selfUserID,
		LearnedAt: time.Now().Unix()}
	if err := s.store.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
		s.store.noteFailure("保存身份", err)
	}
}

// LoadIdentities 返回各群学到的机器人 user_id。
func (s *Scope) LoadIdentities() (map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	var rows []identityRow
	if err := s.store.db.Where("workspace_id = ?", s.workspaceID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.GroupID] = row.SelfUserID
	}
	return out, nil
}

// MemberInfo 一次成员信息写入。Source/SourceMessageID 用于历史去重与回溯。
type MemberInfo struct {
	GroupID         string
	UserID          string
	Name            string
	DisplayName     string
	Source          string
	SourceMessageID string
	Extra           string
}

func memberKVKey(workspaceID, groupID, userID string) string {
	return fmt.Sprintf("ws:%s:member:%s:%s", workspaceID, groupID, userID)
}

// SaveMemberInfo 更新成员当前值；信息确实变化时追加一行历史。
// 先写历史、再更新当前值，不依赖跨表事务；source_message_id 唯一键防重试重复写。
func (s *Scope) SaveMemberInfo(info MemberInfo) (bool, error) {
	if s == nil || s.store == nil || info.UserID == "" || s.store.closed.Load() {
		return false, nil
	}
	extra := strings.TrimSpace(info.Extra)
	if extra == "" {
		extra = "{}"
	}
	var current memberRow
	err := s.store.db.Where("workspace_id = ? AND group_id = ? AND user_id = ?",
		s.workspaceID, info.GroupID, info.UserID).First(&current).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		s.store.noteFailure("读取成员", err)
		return false, err
	}

	name, display := info.Name, info.DisplayName
	if name == "" {
		name = current.Name
	}
	if display == "" {
		display = current.DisplayName
	}
	extraChanged := current.UpdatedAt == 0 || (extra != "{}" && extra != current.Extra)
	changed := current.UpdatedAt == 0 || name != current.Name || display != current.DisplayName || extraChanged
	if !changed {
		return false, nil
	}

	now := time.Now().Unix()
	// 历史先写：即使随后更新当前值失败，也不会漏记一次变更。
	if info.Source != "" || info.SourceMessageID != "" {
		if err := s.store.db.Exec(`INSERT INTO member_info_history
			(workspace_id, group_id, user_id, name, display_name, source, source_message_id, extra, changed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING`,
			s.workspaceID, info.GroupID, info.UserID, name, display,
			info.Source, info.SourceMessageID, extra, now).Error; err != nil {
			s.store.noteFailure("写成员历史", err)
		}
	}
	if err := s.store.db.Exec(`INSERT INTO members
		(workspace_id, group_id, user_id, name, display_name, extra, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id, group_id, user_id) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE members.name END,
			display_name = CASE WHEN excluded.display_name <> '' THEN excluded.display_name ELSE members.display_name END,
			extra = CASE WHEN excluded.extra <> '{}' THEN excluded.extra ELSE members.extra END,
			updated_at = excluded.updated_at`,
		s.workspaceID, info.GroupID, info.UserID, name, display, extra, now).Error; err != nil {
		s.store.noteFailure("保存成员名", err)
		return false, err
	}
	if display == "" {
		display = name
	}
	if display != "" {
		_ = s.store.kv.Set(nil, memberKVKey(s.workspaceID, info.GroupID, info.UserID), []byte(display), memberCacheTTL)
	}
	return true, nil
}

// SaveMember 兼容旧调用：没有触发消息编号，历史只按信息变化追加。
func (s *Scope) SaveMember(groupID, userID, name, displayName string) {
	_, _ = s.SaveMemberInfo(MemberInfo{
		GroupID: groupID, UserID: userID, Name: name, DisplayName: displayName,
		Source: "member_info",
	})
}

// RenameSelfMember 把「机器人自己」在某个群的成员行改成新名字，并追加 self_name 历史。
func (s *Scope) RenameSelfMember(groupID, userID, oldName, newName string) {
	if s == nil || s.store == nil || groupID == "" || userID == "" || newName == "" || s.store.closed.Load() {
		return
	}
	var current memberRow
	err := s.store.db.Where("workspace_id = ? AND group_id = ? AND user_id = ?",
		s.workspaceID, groupID, userID).First(&current).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		s.store.noteFailure("读取成员", err)
		return
	}
	display := current.DisplayName
	if display == "" || display == oldName {
		display = newName
	}
	if current.Name == newName && current.DisplayName == display {
		return
	}
	_, _ = s.SaveMemberInfo(MemberInfo{
		GroupID: groupID, UserID: userID, Name: newName, DisplayName: display,
		Source: "self_name",
	})
}

// LoadMembers 返回 group → user → 显示名。
func (s *Scope) LoadMembers() (map[string]map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	var rows []memberRow
	if err := s.store.db.Where("workspace_id = ?", s.workspaceID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]map[string]string)
	for _, row := range rows {
		if out[row.GroupID] == nil {
			out[row.GroupID] = make(map[string]string)
		}
		display := row.DisplayName
		if display == "" {
			display = row.Name
		}
		out[row.GroupID][row.UserID] = display
	}
	return out, nil
}

// LookupMemberNames 按群批量查成员显示名：先读 KV，miss 的按批回源并回填缓存。
func (s *Scope) LookupMemberNames(groupID string, userIDs []string) map[string]string {
	if s == nil || s.store == nil || groupID == "" || len(userIDs) == 0 || s.store.closed.Load() {
		return nil
	}
	out := make(map[string]string, len(userIDs))
	keys := make([]string, 0, len(userIDs))
	byKey := make(map[string]string, len(userIDs))
	for _, id := range userIDs {
		if id == "" {
			continue
		}
		key := memberKVKey(s.workspaceID, groupID, id)
		keys = append(keys, key)
		byKey[key] = id
	}
	cached, _ := s.store.kv.GetMany(nil, keys)
	missing := make([]string, 0, len(userIDs))
	for _, key := range keys {
		if value, ok := cached[key]; ok && len(value) > 0 {
			out[byKey[key]] = string(value)
			continue
		}
		missing = append(missing, byKey[key])
	}
	if len(missing) == 0 {
		return out
	}

	const chunk = 400
	for start := 0; start < len(missing); start += chunk {
		end := min(start+chunk, len(missing))
		args := make([]any, 0, end-start+2)
		args = append(args, s.workspaceID, groupID)
		holders := make([]string, 0, end-start)
		for _, id := range missing[start:end] {
			args = append(args, id)
			holders = append(holders, "?")
		}
		query := `SELECT user_id, name, display_name FROM members
			WHERE workspace_id = ? AND group_id = ? AND user_id IN (` + strings.Join(holders, ",") + `)`
		rows, err := s.store.db.Raw(query, args...).Rows()
		if err != nil {
			s.store.noteFailure("批量查询成员名", err)
			return out
		}
		for rows.Next() {
			var userID, name, display string
			if err := rows.Scan(&userID, &name, &display); err != nil {
				s.store.noteFailure("批量查询成员名", err)
				break
			}
			value := display
			if value == "" {
				value = name
			}
			if value == "" {
				continue
			}
			out[userID] = value
			_ = s.store.kv.Set(nil, memberKVKey(s.workspaceID, groupID, userID), []byte(value), memberCacheTTL)
		}
		if err := rows.Err(); err != nil {
			s.store.noteFailure("批量查询成员名", err)
		}
		_ = rows.Close()
	}
	return out
}

func groupKVKey(workspaceID, groupID string) string {
	return fmt.Sprintf("ws:%s:group:%s", workspaceID, groupID)
}

// SaveGroupInfo 更新群档案；群名或 extra 变化时追加历史。
func (s *Scope) SaveGroupInfo(groupID, name, source, sourceMessageID, extra string) (bool, error) {
	if s == nil || s.store == nil || groupID == "" || s.store.closed.Load() {
		return false, nil
	}
	if strings.TrimSpace(extra) == "" {
		extra = "{}"
	}
	var current groupRow
	err := s.store.db.Where("workspace_id = ? AND group_id = ?", s.workspaceID, groupID).First(&current).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		s.store.noteFailure("读取群档案", err)
		return false, err
	}
	effectiveName := name
	if effectiveName == "" {
		effectiveName = current.Name
	}
	extraChanged := current.UpdatedAt == 0 || (extra != "{}" && extra != current.Extra)
	changed := current.UpdatedAt == 0 || effectiveName != current.Name || extraChanged
	if !changed {
		return false, nil
	}
	now := time.Now().Unix()
	if source != "" || sourceMessageID != "" {
		if err := s.store.db.Exec(`INSERT INTO group_info_history
			(workspace_id, group_id, name, source, source_message_id, extra, changed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING`,
			s.workspaceID, groupID, effectiveName, source, sourceMessageID, extra, now).Error; err != nil {
			s.store.noteFailure("写群历史", err)
		}
	}
	if err := s.store.db.Exec(`INSERT INTO groups (workspace_id, group_id, name, extra, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id, group_id) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE groups.name END,
			extra = CASE WHEN excluded.extra <> '{}' THEN excluded.extra ELSE groups.extra END,
			updated_at = excluded.updated_at`,
		s.workspaceID, groupID, effectiveName, extra, now).Error; err != nil {
		s.store.noteFailure("保存群档案", err)
		return false, err
	}
	if effectiveName != "" {
		_ = s.store.kv.Set(nil, groupKVKey(s.workspaceID, groupID), []byte(effectiveName), groupCacheTTL)
	}
	return true, nil
}

// SaveGroup 兼容旧调用。
func (s *Scope) SaveGroup(groupID, name string) {
	_, _ = s.SaveGroupInfo(groupID, name, "group_info", "", "{}")
}

// GroupName 读群名：先 KV，miss 再回源。
func (s *Scope) GroupName(groupID string) string {
	if s == nil || s.store == nil || groupID == "" {
		return ""
	}
	if value, ok, _ := s.store.kv.Get(nil, groupKVKey(s.workspaceID, groupID)); ok {
		return string(value)
	}
	var name string
	if err := s.store.db.Raw(`SELECT name FROM groups WHERE workspace_id = ? AND group_id = ?`,
		s.workspaceID, groupID).Scan(&name).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		s.store.noteFailure("读取群名", err)
	}
	if name != "" {
		_ = s.store.kv.Set(nil, groupKVKey(s.workspaceID, groupID), []byte(name), groupCacheTTL)
	}
	return name
}

// LoadGroups 返回 group_id → 群名（没有名字的群值为空串）。
func (s *Scope) LoadGroups() (map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	var rows []groupRow
	if err := s.store.db.Where("workspace_id = ?", s.workspaceID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.GroupID] = row.Name
	}
	return out, nil
}

// SaveGroupPersona 记录某群当前人格的源关键字。
func (s *Scope) SaveGroupPersona(groupID, keyword string) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	row := groupPersonaRow{WorkspaceID: s.workspaceID, GroupID: groupID, Keyword: keyword}
	if err := s.store.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
		s.store.noteFailure("保存群人格", err)
	}
}

// DeleteGroupPersona 清除某群人格。
func (s *Scope) DeleteGroupPersona(groupID string) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	if err := s.store.db.Where("workspace_id = ? AND group_id = ?", s.workspaceID, groupID).
		Delete(&groupPersonaRow{}).Error; err != nil {
		s.store.noteFailure("删除群人格", err)
	}
}

// LoadGroupPersonas 返回 group → 人格关键字。
func (s *Scope) LoadGroupPersonas() (map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	var rows []groupPersonaRow
	if err := s.store.db.Where("workspace_id = ?", s.workspaceID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.GroupID] = row.Keyword
	}
	return out, nil
}

// LoadProfile 读取工作空间画像。第二个返回值表示记录是否存在。
func (s *Scope) LoadProfile() (WorkspaceProfile, bool, error) {
	if s == nil || s.store == nil {
		return WorkspaceProfile{}, false, nil
	}
	var profile WorkspaceProfile
	err := s.store.db.Where("workspace_id = ?", s.workspaceID).First(&profile).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return WorkspaceProfile{}, false, nil
	}
	if err != nil {
		return WorkspaceProfile{}, false, err
	}
	return profile, true, nil
}

// SaveProfile 写入/更新画像。CreatedAt 只在首次写入时设置。
func (s *Scope) SaveProfile(profile WorkspaceProfile) error {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return nil
	}
	profile.WorkspaceID = s.workspaceID
	now := time.Now().Unix()
	if profile.CreatedAt == 0 {
		profile.CreatedAt = now
	}
	profile.UpdatedAt = now
	if strings.TrimSpace(profile.Extra) == "" {
		profile.Extra = "{}"
	}
	return s.store.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "workspace_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"platform", "account_kind", "account", "name", "impl",
			"identity_scheme", "identity_scope", "extra", "updated_at",
		}),
	}).Create(&profile).Error
}

// WorkspaceMeta 是重启恢复工作空间所需的最小信息。
type WorkspaceMeta struct {
	ID          string
	Platform    string
	AccountKind string
	Account     string
	Name        string
}

// Workspaces 返回库里记录过的全部工作空间（来自 workspace_profile）。
func (s *Store) Workspaces() ([]WorkspaceMeta, error) {
	if s == nil {
		return nil, nil
	}
	var profiles []WorkspaceProfile
	if err := s.db.Find(&profiles).Error; err != nil {
		return nil, err
	}
	out := make([]WorkspaceMeta, 0, len(profiles))
	for _, profile := range profiles {
		out = append(out, WorkspaceMeta{
			ID: profile.WorkspaceID, Platform: profile.Platform,
			AccountKind: profile.AccountKind, Account: profile.Account, Name: profile.Name,
		})
	}
	return out, nil
}

// SaveAssetTagJob 记录/更新一条欠条。已存在时只更新可变字段，保留 created_at。
func (s *Store) SaveAssetTagJob(job AssetTagJob) error {
	if s == nil || job.MessageID == "" {
		return nil
	}
	if job.Status == "" {
		job.Status = "pending"
	}
	if job.Kind == "" {
		job.Kind = "image"
	}
	now := time.Now().Unix()
	if job.CreatedAt == 0 {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	return s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "workspace_id"}, {Name: "group_id"}, {Name: "message_id"}, {Name: "asset_index"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"kind", "url", "local_path", "status", "attempts", "last_error", "updated_at",
		}),
	}).Create(&job).Error
}

// PendingAssetTagJobs 返回待补/可重试的欠条（跨工作空间），按更新时间升序。
func (s *Store) PendingAssetTagJobs(limit, maxAttempts int) ([]AssetTagJob, error) {
	if s == nil || limit <= 0 {
		return nil, nil
	}
	var jobs []AssetTagJob
	err := s.db.Where("status IN ? AND attempts < ?", []string{"pending", "failed"}, maxAttempts).
		Order("updated_at ASC").Limit(limit).Find(&jobs).Error
	return jobs, err
}

// UpdateAssetTagJob 按主键更新欠条状态。
func (s *Store) UpdateAssetTagJob(job AssetTagJob) error {
	if s == nil {
		return nil
	}
	job.UpdatedAt = time.Now().Unix()
	return s.db.Model(&AssetTagJob{}).
		Where("workspace_id = ? AND group_id = ? AND message_id = ? AND asset_index = ?",
			job.WorkspaceID, job.GroupID, job.MessageID, job.AssetIndex).
		Updates(map[string]any{
			"status": job.Status, "attempts": job.Attempts,
			"last_error": job.LastError, "local_path": job.LocalPath,
			"updated_at": job.UpdatedAt,
		}).Error
}

// CleanupAssetTagJobs 删除超过保留期的 done/missing 台账；pending/failed 永不清理。
func (s *Store) CleanupAssetTagJobs(now time.Time, retention time.Duration) (int64, error) {
	if s == nil || retention <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-retention).Unix()
	result := s.db.Where("status IN ? AND updated_at < ?", []string{"done", "missing"}, cutoff).
		Delete(&AssetTagJob{})
	return result.RowsAffected, result.Error
}

// LoadSendRecord 读取一个动作的发送台账。
func (s *Scope) LoadSendRecord(actionID string) (SendRecord, bool, error) {
	if s == nil || s.store == nil || actionID == "" {
		return SendRecord{}, false, nil
	}
	var record SendRecord
	err := s.store.db.Where("workspace_id = ? AND action_id = ?", s.workspaceID, actionID).
		First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return SendRecord{}, false, nil
	}
	if err != nil {
		return SendRecord{}, false, err
	}
	return record, true, nil
}

// MarkSend 写入/更新发送台账。
func (s *Scope) MarkSend(record SendRecord) {
	if s == nil || s.store == nil || record.ActionID == "" || s.store.closed.Load() {
		return
	}
	record.WorkspaceID = s.workspaceID
	record.UpdatedAt = time.Now().Unix()
	if err := s.store.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&record).Error; err != nil {
		s.store.noteFailure("保存发送台账", err)
	}
}

// SaveAlert 写入一条异常现场。
func (s *Store) SaveAlert(alert Alert) {
	if s == nil || alert.Kind == "" {
		return
	}
	if alert.CreatedAt == 0 {
		alert.CreatedAt = time.Now().Unix()
	}
	if err := s.db.Create(&alert).Error; err != nil {
		s.noteFailure("保存报警", err)
	}
}

// LoadSettings 读取全局运行时设置；value 是 JSON。
func (s *Store) LoadSettings() (map[string]json.RawMessage, error) {
	if s == nil {
		return nil, nil
	}
	var rows []Setting
	if err := s.db.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(rows))
	for _, row := range rows {
		out[row.Key] = json.RawMessage(row.Value)
	}
	return out, nil
}

// SaveSetting 写入全局运行时设置；value 会被编码成 JSON。
func (s *Store) SaveSetting(key string, value any) error {
	if s == nil || key == "" {
		return nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	row := Setting{Key: key, Value: string(payload), UpdatedAt: time.Now().Unix()}
	return s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

// Report 统计消息条数与库文件体积（含 WAL），超阈值时用 warn 提示人工清理。
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
	rows, err := s.db.Raw(`SELECT workspace_id, COUNT(*) FROM messages GROUP BY workspace_id`).Rows()
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
	if err := s.db.Raw(`SELECT COUNT(*) FROM messages`).Scan(&rows).Error; err != nil {
		s.noteFailure("统计消息条数", err)
	}
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			bytes += info.Size()
		}
	}
	return rows, bytes
}

// Close 关闭数据库与缓存。可重复调用。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	if s.closed.Swap(true) {
		return nil
	}
	if err := s.kv.Close(); err != nil {
		logutil.Warn("关闭 KV 缓存失败", "err", err)
	}
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
