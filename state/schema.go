package state

// 本文件只管"表长什么样、怎么把一份空库建出来"。
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
	// schemaVersion 当前表结构版本，写在 SQLite 的 PRAGMA user_version 里。
	//
	// 改表结构时把它 +1；旧库不做升级，换个文件重新建。
	schemaVersion = 17
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

// migrate 把一份新建的空库建成当前结构。
//
// 不做升级：旧库（有版本号或已经有表）直接报错，换个文件重新建。
func migrate(db *gorm.DB, path string) error {
	var version int
	if err := db.Raw(`PRAGMA user_version`).Scan(&version).Error; err != nil {
		return fmt.Errorf("读取 state.db 版本失败：%w", err)
	}
	if version >= schemaVersion {
		return nil
	}
	// 只有全新的空库（user_version 为 0 且一张表都没有）才往下建。
	// 有版本号、或者文件里已经有表，都算旧结构，不做原地升级。
	if version > 0 || tableExists(db, "messages") {
		return fmt.Errorf("state.db 是旧版结构（user_version=%d，当前 %d），开发阶段只支持全新库："+
			"删掉 %s 后重启即可重新生成", version, schemaVersion, path)
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS messages (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			message_id   TEXT NOT NULL,
			user_id      TEXT NOT NULL,
			content      TEXT NOT NULL,
			mentions     TEXT NOT NULL DEFAULT '[]',
			mentions_resolved INTEGER NOT NULL DEFAULT 1,
			images       TEXT NOT NULL DEFAULT '[]',
			image_ids    TEXT NOT NULL DEFAULT '[]',
			reply_to     TEXT NOT NULL DEFAULT '',
			ref_idx      TEXT NOT NULL DEFAULT '',
			ts           INTEGER NOT NULL,
			UNIQUE (workspace_id, group_id, message_id)
		)`,
		// 每张表都以 id 自增列作主键，原来的业务键降为 UNIQUE，
		// 写入口的"先 UPDATE 再 INSERT"按那组业务键定位（见各表的写入方法）。
		`CREATE TABLE IF NOT EXISTS anchors (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id    TEXT NOT NULL,
			group_id        TEXT NOT NULL,
			window_first_id TEXT NOT NULL DEFAULT '',
			window_last_id  TEXT NOT NULL DEFAULT '',
			updated_at      INTEGER NOT NULL,
			UNIQUE (workspace_id, group_id)
		)`,
		`CREATE TABLE IF NOT EXISTS members (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id TEXT NOT NULL,
			group_id     TEXT NOT NULL,
			user_id      TEXT NOT NULL,
			name         TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			extra        TEXT NOT NULL DEFAULT '{}',
			updated_at   INTEGER NOT NULL,
			UNIQUE (workspace_id, group_id, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS groups (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id    TEXT NOT NULL,
			group_id        TEXT NOT NULL,
			name            TEXT NOT NULL DEFAULT '',
			type            TEXT NOT NULL DEFAULT 'group',
			persona_keyword TEXT NOT NULL DEFAULT '',
			message_count   INTEGER NOT NULL DEFAULT 0,
			member_count    INTEGER NOT NULL DEFAULT 0,
			last_ts         INTEGER NOT NULL DEFAULT 0,
			extra           TEXT NOT NULL DEFAULT '{}',
			updated_at      INTEGER NOT NULL,
			UNIQUE (workspace_id, group_id)
		)`,
		`CREATE TABLE IF NOT EXISTS workspace_profile (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id    TEXT NOT NULL UNIQUE,
			platform        TEXT NOT NULL DEFAULT '',
			account_kind    TEXT NOT NULL DEFAULT '',
			account         TEXT NOT NULL DEFAULT '',
			name            TEXT NOT NULL DEFAULT '',
			self_user_id    TEXT NOT NULL DEFAULT '',
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
		// image_notes 是全局图库：不带 workspace_id，业务键是内容哈希 + 字节大小。
		`CREATE TABLE IF NOT EXISTS image_notes (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
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
			UNIQUE (sha256, byte_size)
		)`,
		// status：pending 待补 / running 已被某个进程领走（updated_at 当租约） /
		// failed 这次失败、还在重试名额内 / done 补好了 / missing 原始地址没了（终态）。
		`CREATE TABLE IF NOT EXISTS asset_tag_jobs (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
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
			UNIQUE (workspace_id, group_id, message_id, asset_index)
		)`,
		`CREATE TABLE IF NOT EXISTS send_ledger (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id TEXT NOT NULL,
			action_id    TEXT NOT NULL,
			message_id   TEXT NOT NULL DEFAULT '',
			text         TEXT NOT NULL DEFAULT '',
			sent         INTEGER NOT NULL DEFAULT 0,
			send_ok      INTEGER NOT NULL DEFAULT 0,
			updated_at   INTEGER NOT NULL,
			UNIQUE (workspace_id, action_id)
		)`,
		`CREATE TABLE IF NOT EXISTS alerts (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id TEXT NOT NULL DEFAULT '',
			kind         TEXT NOT NULL,
			detail       TEXT NOT NULL DEFAULT '{}',
			created_at   INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			key        TEXT NOT NULL UNIQUE,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("初始化 state.db 失败：%w", err)
		}
	}

	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages(workspace_id, group_id, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_mention_pending ON messages(workspace_id, group_id, mentions_resolved)`,
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

	if err := createGroupStatTriggers(db); err != nil {
		return err
	}

	if err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)).Error; err != nil {
		return fmt.Errorf("更新 state.db 版本失败：%w", err)
	}
	return nil
}

// createGroupStatTriggers 让 messages / members 的新插入自动维护 groups 的计数列。
//
// 用触发器而不是在 Go 里再发一条 UPDATE：插入和计数在同一个语句里原子完成，
// 不多一次往返；而且 AFTER INSERT 只在真的插了新行时触发，
// 重复消息走"先 UPDATE"分支不会把条数算两遍。
//
// 语句写成「先 UPDATE、没命中才 INSERT」而不是 INSERT ... ON CONFLICT DO UPDATE：
// upsert 在冲突检查之前就分配好自增 rowid 并推进 sqlite_sequence，即使最后只走
// 更新分支也白烧一个号，于是每存一条消息 groups.id 就 +1。UPDATE 命中不分配 rowid，
// 只有群第一次出现才真正插入。
func createGroupStatTriggers(db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_messages_group_stats`,
		`DROP TRIGGER IF EXISTS trg_members_group_stats`,
		`CREATE TRIGGER trg_messages_group_stats
		AFTER INSERT ON messages
		BEGIN
			UPDATE groups SET
				message_count = message_count + 1,
				last_ts = MAX(last_ts, NEW.ts),
				updated_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE workspace_id = NEW.workspace_id AND group_id = NEW.group_id;
			INSERT INTO groups
				(workspace_id, group_id, name, type, message_count, member_count, last_ts, extra, updated_at)
			SELECT
				NEW.workspace_id, NEW.group_id, '',
				CASE WHEN NEW.group_id LIKE 'private:%' THEN 'private' ELSE 'group' END,
				1, 0, NEW.ts, '{}', CAST(strftime('%s','now') AS INTEGER)
			WHERE NOT EXISTS (
				SELECT 1 FROM groups
				WHERE workspace_id = NEW.workspace_id AND group_id = NEW.group_id);
		END`,
		`CREATE TRIGGER trg_members_group_stats
		AFTER INSERT ON members
		BEGIN
			UPDATE groups SET member_count = member_count + 1
			WHERE workspace_id = NEW.workspace_id AND group_id = NEW.group_id;
			INSERT INTO groups
				(workspace_id, group_id, name, type, message_count, member_count, last_ts, extra, updated_at)
			SELECT
				NEW.workspace_id, NEW.group_id, '',
				CASE WHEN NEW.group_id LIKE 'private:%' THEN 'private' ELSE 'group' END,
				0, 1, 0, '{}', CAST(strftime('%s','now') AS INTEGER)
			WHERE NOT EXISTS (
				SELECT 1 FROM groups
				WHERE workspace_id = NEW.workspace_id AND group_id = NEW.group_id);
		END`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("创建群统计触发器失败：%w", err)
		}
	}
	return nil
}

// tableExists 表在不在（建库前的"这是不是空库"判断用）。
func tableExists(db *gorm.DB, table string) bool {
	var count int
	if err := db.Raw(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count).Error; err != nil {
		return false
	}
	return count > 0
}
