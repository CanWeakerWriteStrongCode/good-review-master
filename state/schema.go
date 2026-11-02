package state

// 本文件只管"表长什么样、怎么从旧版本走到当前版本"。
// 读写各表的代码在 messages.go / images.go / anchors.go / directory.go /
// persona.go / jobs.go / ops.go，一张表一个文件。

import (
	"fmt"
	"os"
	"path/filepath"

	"good-review-master/logutil"

	"gorm.io/gorm"
)

// FileName 是档案文件名，放在工作目录（config.yaml 同级）。
const FileName = "state.db"

const (
	// schemaVersion 当前表结构版本。
	// v1 是“每工作空间一个库”的旧结构；v2 起所有表带 workspace_id；
	// v3 groups；v4 messages.ref_idx；v5 messages.image_ids + image_notes；
	// v6 workspace_profile 取代 meta；v7 成员/群历史表；
	// v8 资产欠条、发送台账、报警、全局 settings、image_notes.prompt_version；
	// v9 messages 加自增主键 id（三元组降为唯一键），重建表以保留历史消息；
	// v10 anchors 的 start_id/last_sent_id 改名 window_first_id/window_last_id。
	schemaVersion = 10
)

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
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
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
			UNIQUE (workspace_id, group_id, message_id)
		)`,
		`CREATE TABLE IF NOT EXISTS anchors (
			workspace_id    TEXT NOT NULL,
			group_id        TEXT NOT NULL,
			window_first_id TEXT NOT NULL DEFAULT '',
			window_last_id  TEXT NOT NULL DEFAULT '',
			updated_at      INTEGER NOT NULL,
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
	// v9：老库的 messages 没有自增 id，SQLite 又不能 ALTER TABLE 加主键，只能重建。
	// 放在补列之后，拷贝时新表列已经齐全。
	if version < 9 {
		if err := rebuildMessagesWithID(db); err != nil {
			return err
		}
	}
	// v10：anchors 两列改名。老库的表已经建好了，上面的 CREATE TABLE IF NOT EXISTS
	// 不会动它，只能显式 RENAME COLUMN；新库走建表语句，这里什么都不做。
	if version < 10 {
		if err := renameColumn(db, "anchors", "start_id", "window_first_id"); err != nil {
			return err
		}
		if err := renameColumn(db, "anchors", "last_sent_id", "window_last_id"); err != nil {
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

// renameColumn 把 table 的 from 列改名成 to，保留数据。
// 幂等：表不存在、旧列不存在（新库或已改过）、新列已存在都直接返回。
func renameColumn(db *gorm.DB, table, from, to string) error {
	if !tableExists(db, table) {
		return nil
	}
	var oldCount, newCount int
	if err := db.Raw(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, from).Scan(&oldCount).Error; err != nil {
		return fmt.Errorf("读取 %s 表结构失败：%w", table, err)
	}
	if err := db.Raw(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, to).Scan(&newCount).Error; err != nil {
		return fmt.Errorf("读取 %s 表结构失败：%w", table, err)
	}
	if oldCount == 0 || newCount > 0 {
		return nil
	}
	if err := db.Exec(`ALTER TABLE ` + table + ` RENAME COLUMN ` + from + ` TO ` + to).Error; err != nil {
		return fmt.Errorf("给 %s 表重命名列 %s→%s 失败：%w", table, from, to, err)
	}
	return nil
}

// rebuildMessagesWithID 把 messages 重建成「自增主键 id + 三元组唯一键」。
// 只搬已有列，不增不删；按 ts 升序插入，历史消息的 id 就是时间顺序。
// 重建过（已带 id 列）或表还不存在时直接返回。
func rebuildMessagesWithID(db *gorm.DB) error {
	if !tableExists(db, "messages") {
		return nil
	}
	var count int
	if err := db.Raw(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'id'`).Scan(&count).Error; err != nil {
		return fmt.Errorf("读取 messages 表结构失败：%w", err)
	}
	if count > 0 {
		return nil
	}
	steps := []string{
		`CREATE TABLE messages_v9 (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
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
			UNIQUE (workspace_id, group_id, message_id)
		)`,
		`INSERT INTO messages_v9
			(workspace_id, group_id, message_id, user_id, content, images, image_ids, reply_to, ref_idx, ts)
			SELECT workspace_id, group_id, message_id, user_id, content, images, image_ids, reply_to, ref_idx, ts
			FROM messages ORDER BY ts`,
		`DROP TABLE messages`,
		`ALTER TABLE messages_v9 RENAME TO messages`,
	}
	for _, statement := range steps {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("重建 messages 表失败：%w", err)
		}
	}
	logutil.Info("state.db messages 表已重建：新增自增主键 id")
	return nil
}

func tableExists(db *gorm.DB, table string) bool {
	var count int
	if err := db.Raw(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count).Error; err != nil {
		return false
	}
	return count > 0
}
