package state

// 本文件只管"表长什么样、怎么从旧版本走到当前版本"。
// 读写各表的代码在 messages.go / images.go / anchors.go / directory.go /
// persona.go / jobs.go / ops.go，一张表一个文件。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"good-review-master/conversation"
	"good-review-master/logutil"
	"good-review-master/onebot"

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
	// v11 groups 加 type（群聊/私聊）+ 消息数/成员数/最后时间三列，
	//     面板群列表只读这一张表，不再扫 messages 全表 GROUP BY。
	// v12 identities 并进 groups.self_user_id：两张表都是每会话一行，
	//     没必要分开存，并过来后面板查询少一次 JOIN、启动恢复少一次全表查询。
	// v13 group_personas 直接去掉，人格改存 groups.persona_keyword（旧值不搬）。
	// v14 messages 加 mentions（这条消息 @ 过谁）与 mentions_resolved（正文里还有没有
	//     "@用户(" 占位），面板与回填靠它们筛出还没换名字的正文。
	// v15 self_user_id 从 groups 提到 workspace_profile：openid 由账号决定，
	//     同一个机器人在所有群是同一个值（实测同账号 3 个群一个值），
	//     学到一个群就该全账号可用，不必逐群等一次 @。
	// v16 剩下的表也统一成「id INTEGER PRIMARY KEY AUTOINCREMENT + 原主键降为 UNIQUE」：
	//     每张表都有一个整数自增主键，原来那组业务键退成唯一约束（upsert 目标仍是它）。
	schemaVersion = 16
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
		// group_personas 现在的库里已经没有了，但 v1 的库里有，而且没有 workspace_id 列：
		// 留着它会让 v13 的合并语句读不到 workspace_id，所以这里照旧先清掉。
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
			mentions     TEXT NOT NULL DEFAULT '[]',
			mentions_resolved INTEGER NOT NULL DEFAULT 1,
			images       TEXT NOT NULL DEFAULT '[]',
			image_ids    TEXT NOT NULL DEFAULT '[]',
			reply_to     TEXT NOT NULL DEFAULT '',
			ref_idx      TEXT NOT NULL DEFAULT '',
			ts           INTEGER NOT NULL,
			UNIQUE (workspace_id, group_id, message_id)
		)`,
		// v16 起每张表都以 id 自增列作主键，原来的业务键降为 UNIQUE，
		// 写入口的 ON CONFLICT 目标仍是那组业务键（见各表的写入方法）。
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
	// v11：groups 加计数列。先补列、再回填、最后才建触发器——
	// 顺序反了的话回填会经过触发器，计数变成双倍。
	if version < 11 {
		for _, column := range []struct{ name, ddl string }{
			{"type", "TEXT NOT NULL DEFAULT 'group'"},
			{"message_count", "INTEGER NOT NULL DEFAULT 0"},
			{"member_count", "INTEGER NOT NULL DEFAULT 0"},
			{"last_ts", "INTEGER NOT NULL DEFAULT 0"},
		} {
			if err := ensureColumn(db, "groups", column.name, column.ddl); err != nil {
				return err
			}
		}
		if err := backfillGroupStats(db); err != nil {
			return err
		}
		if err := createGroupStatTriggers(db); err != nil {
			return err
		}
	}
	// v12：identities 并进 groups。必须先补列再搬数据，最后才删旧表。
	// 新库（version == 0）跳过：这一列 v15 就废弃了，补出来是空的，马上又要删。
	if version > 0 && version < 12 {
		if err := ensureColumn(db, "groups", "self_user_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := mergeIdentitiesIntoGroups(db); err != nil {
			return err
		}
	}
	// v13：group_personas 整个不要了，人格改存 groups.persona_keyword。
	// 老库的 groups 不会因为建表语句变了就多出列（CREATE TABLE IF NOT EXISTS 不动已存在的表），
	// 所以必须显式补；旧表里的关键字不搬 —— 就几个群的人格设置，升级后重新 #切换人格 一次即可。
	if version < 13 {
		if err := ensureColumn(db, "groups", "persona_keyword", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := db.Exec(`DROP TABLE IF EXISTS group_personas`).Error; err != nil {
			return fmt.Errorf("删除旧 group_personas 表失败：%w", err)
		}
	}
	// v14：messages 加 @ 相关两列。老消息只处理“正文里还有占位”的那一类：
	// 其余行不需要知道 @ 过谁，mentions 留 '[]' 即可。
	if version < 14 {
		for _, column := range []struct{ name, ddl string }{
			{"mentions", "TEXT NOT NULL DEFAULT '[]'"},
			{"mentions_resolved", "INTEGER NOT NULL DEFAULT 1"},
		} {
			if err := ensureColumn(db, "messages", column.name, column.ddl); err != nil {
				return err
			}
		}
		if err := backfillMentionColumns(db); err != nil {
			return err
		}
	}
	// v15：机器人 openid 提到账号级，老库里散在各群的值搬一个上来，搬完就把旧列删掉——
	// 两处留同一个事实，迟早会不一致（某个群留着旧值，别处已经是新的）。
	// 画像表在老库里没有 self_user_id 列，得先补（CREATE TABLE IF NOT EXISTS 不动已存在的表）。
	if version < 15 {
		if err := ensureColumn(db, "workspace_profile", "self_user_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := promoteSelfUserID(db); err != nil {
			return err
		}
		if err := dropColumn(db, "groups", "self_user_id"); err != nil {
			return err
		}
	}
	// v16：剩下的表补自增主键。SQLite 不能 ALTER TABLE 加主键，和 v9 一样重建表；
	// 放在建索引之前，重建过程中被带走的索引随后统一建回来。
	if version < 16 {
		if err := db.Transaction(addIDPrimaryKey); err != nil {
			return err
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

// promoteSelfUserID 把 groups 里学到的机器人 openid 提到账号级（v15 升级）。
//
// 同一个账号下各群的值相同，取每账号的第一条真值就行；哨兵值是拿不到身份时的
// 占位串，不算学到，不能搬——搬上去等于把「不知道」写成了答案。
// 老库才有 groups.self_user_id，新库直接返回。
func promoteSelfUserID(db *gorm.DB) error {
	has, err := hasColumn(db, "groups", "self_user_id")
	if err != nil || !has {
		return err
	}
	var rows []struct {
		WorkspaceID string `gorm:"column:workspace_id"`
		SelfUserID  string `gorm:"column:self_user_id"`
	}
	if err := db.Raw(`SELECT workspace_id, self_user_id FROM groups
		WHERE self_user_id <> '' ORDER BY workspace_id, group_id`).Scan(&rows).Error; err != nil {
		return fmt.Errorf("读取旧的机器人身份失败：%w", err)
	}
	// 目标行可能还不存在（升级前这个账号一次画像都没写过），补一条空画像兜着。
	promoted := make(map[string]string, len(rows))
	for _, row := range rows {
		if onebot.IsSelfPlaceholder(row.SelfUserID) {
			continue
		}
		if _, ok := promoted[row.WorkspaceID]; ok {
			continue
		}
		promoted[row.WorkspaceID] = row.SelfUserID
	}
	for workspaceID, selfUserID := range promoted {
		if err := db.Exec(`INSERT INTO workspace_profile
				(workspace_id, self_user_id, extra, created_at, updated_at)
			VALUES (?, ?, '{}', CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
			ON CONFLICT(workspace_id) DO UPDATE SET self_user_id = excluded.self_user_id
			WHERE workspace_profile.self_user_id = ''`, workspaceID, selfUserID).Error; err != nil {
			return fmt.Errorf("迁移机器人身份失败：%w", err)
		}
	}
	return nil
}

// ensureColumn 给已有表补一列（表已经存在时才用；新库直接按建表语句建）。
func ensureColumn(db *gorm.DB, table, column, ddl string) error {
	has, err := hasColumn(db, table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + ddl).Error; err != nil {
		return fmt.Errorf("给 %s 补列 %s 失败：%w", table, column, err)
	}
	return nil
}

// hasColumn 表里有没有这一列（表不存在也算没有）。
func hasColumn(db *gorm.DB, table, column string) (bool, error) {
	var count int
	if err := db.Raw(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count).Error; err != nil {
		return false, fmt.Errorf("读取 %s 表结构失败：%w", table, err)
	}
	return count > 0, nil
}

// dropColumn 删掉一列，列或表不存在时直接返回（幂等）。
// 用 SQLite 3.35 起的 ALTER TABLE DROP COLUMN；项目锁定的驱动版本远高于它。
func dropColumn(db *gorm.DB, table, column string) error {
	if !tableExists(db, table) {
		return nil
	}
	has, err := hasColumn(db, table, column)
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	if err := db.Exec(`ALTER TABLE ` + table + ` DROP COLUMN ` + column).Error; err != nil {
		return fmt.Errorf("删除 %s 的列 %s 失败：%w", table, column, err)
	}
	return nil
}

// backfillMentionColumns 把存量消息里的 @ 占位标成待办，并从正文里抽出 openid（v14 升级）。
//
// 只扫正文里还有 "@用户(" 的行：别的消息不需要知道 @ 过谁，mentions 留 '[]' 就够了。
// 名字回填不在这里做——那是运行时的事（见 workspace 的 @ 回填），启动后会自己跑。
func backfillMentionColumns(db *gorm.DB) error {
	rows, err := db.Raw(`SELECT id, content FROM messages WHERE content LIKE '%@用户(%'`).Rows()
	if err != nil {
		return fmt.Errorf("读取待回填的 @ 占位失败：%w", err)
	}
	defer rows.Close()
	type pendingMention struct {
		id      int64
		content string
	}
	var pending []pendingMention
	for rows.Next() {
		var row pendingMention
		if err := rows.Scan(&row.id, &row.content); err != nil {
			return fmt.Errorf("读取待回填的 @ 占位失败：%w", err)
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取待回填的 @ 占位失败：%w", err)
	}
	for _, row := range pending {
		payload := "[]"
		if ids := conversation.MentionIDs(row.content); len(ids) > 0 {
			if encoded, err := json.Marshal(ids); err == nil {
				payload = string(encoded)
			}
		}
		if err := db.Exec(`UPDATE messages SET mentions = ?, mentions_resolved = 0 WHERE id = ?`,
			payload, row.id).Error; err != nil {
			return fmt.Errorf("回填 @ 占位标记失败：%w", err)
		}
	}
	if len(pending) > 0 {
		logutil.Info("存量消息里还有未替换的 @ 占位", "rows", len(pending))
	}
	return nil
}

// backfillGroupStats 拿存量数据把 groups 的计数列填一遍，只跑一次（v11 升级）。
//
// 消息数直接 GROUP BY messages 算；成员数用关联子查询数 members。
// messages 里有但 groups 里没有的群（老库可能有）捎带建出档案行，
// type 按 group_id 前缀认：private: 开头是私聊，其余是群聊。
func backfillGroupStats(db *gorm.DB) error {
	// SELECT 必须有 WHERE：SQLite 不允许裸 SELECT 后面跟 ON CONFLICT，会当成语法歧义。
	if err := db.Exec(`INSERT INTO groups
			(workspace_id, group_id, name, type, message_count, member_count, last_ts, extra, updated_at)
		SELECT workspace_id, group_id, '',
		       CASE WHEN group_id LIKE 'private:%' THEN 'private' ELSE 'group' END,
		       COUNT(*), 0, COALESCE(MAX(ts), 0), '{}', CAST(strftime('%s','now') AS INTEGER)
		FROM messages WHERE workspace_id <> ''
		GROUP BY workspace_id, group_id
		ON CONFLICT(workspace_id, group_id) DO UPDATE SET
			message_count = excluded.message_count,
			last_ts = excluded.last_ts,
			type = excluded.type`).Error; err != nil {
		return fmt.Errorf("回填群消息统计失败：%w", err)
	}
	if err := db.Exec(`UPDATE groups SET member_count = (
			SELECT COUNT(*) FROM members
			WHERE members.workspace_id = groups.workspace_id AND members.group_id = groups.group_id
		)`).Error; err != nil {
		return fmt.Errorf("回填群成员统计失败：%w", err)
	}
	return nil
}

// createGroupStatTriggers 让 messages / members 的新插入自动维护 groups 的计数列。
//
// 用触发器而不是在 Go 里再发一条 UPDATE：插入和计数在同一个语句里原子完成，
// 不多一次往返；而且 AFTER INSERT 只在真的插了新行时触发，
// 重复消息走 ON CONFLICT DO UPDATE 分支不会把条数算两遍。
func createGroupStatTriggers(db *gorm.DB) error {
	statements := []string{
		`CREATE TRIGGER IF NOT EXISTS trg_messages_group_stats
		AFTER INSERT ON messages
		BEGIN
			INSERT INTO groups
				(workspace_id, group_id, name, type, message_count, member_count, last_ts, extra, updated_at)
			VALUES
				(NEW.workspace_id, NEW.group_id, '',
				 CASE WHEN NEW.group_id LIKE 'private:%' THEN 'private' ELSE 'group' END,
				 1, 0, NEW.ts, '{}', CAST(strftime('%s','now') AS INTEGER))
			ON CONFLICT(workspace_id, group_id) DO UPDATE SET
				message_count = message_count + 1,
				last_ts = MAX(last_ts, NEW.ts),
				updated_at = excluded.updated_at;
		END`,
		`CREATE TRIGGER IF NOT EXISTS trg_members_group_stats
		AFTER INSERT ON members
		BEGIN
			INSERT INTO groups
				(workspace_id, group_id, name, type, message_count, member_count, last_ts, extra, updated_at)
			VALUES
				(NEW.workspace_id, NEW.group_id, '',
				 CASE WHEN NEW.group_id LIKE 'private:%' THEN 'private' ELSE 'group' END,
				 0, 1, 0, '{}', CAST(strftime('%s','now') AS INTEGER))
			ON CONFLICT(workspace_id, group_id) DO UPDATE SET
				member_count = member_count + 1;
		END`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("创建群统计触发器失败：%w", err)
		}
	}
	return nil
}

// mergeIdentitiesIntoGroups 把旧 identities 表并进 groups，然后删掉旧表（v12 升级）。
//
// 身份是从消息里学到的，所以只发过消息的会话一般已经有 groups 行（由 messages
// 触发器建的），这里走 UPSERT 只补列；反过来只有身份没有消息的会话，这里补一行，
// 免得升级后身份丢掉。updated_at 写 0 表示“档案还没写过”，
// SaveGroupInfo 靠这个判断历史该不该记。
func mergeIdentitiesIntoGroups(db *gorm.DB) error {
	if !tableExists(db, "identities") {
		return nil
	}
	// SELECT 必须有 WHERE：SQLite 不允许裸 SELECT 后面跟 ON CONFLICT，会当成语法歧义。
	if err := db.Exec(`INSERT INTO groups
			(workspace_id, group_id, name, type, self_user_id, extra, updated_at)
		SELECT workspace_id, group_id, '',
		       CASE WHEN group_id LIKE 'private:%' THEN 'private' ELSE 'group' END,
		       self_user_id, '{}', 0
		FROM identities WHERE self_user_id <> ''
		ON CONFLICT(workspace_id, group_id) DO UPDATE SET
			self_user_id = excluded.self_user_id`).Error; err != nil {
		return fmt.Errorf("把身份并进群档案失败：%w", err)
	}
	if err := db.Exec(`DROP TABLE identities`).Error; err != nil {
		return fmt.Errorf("删除旧 identities 表失败：%w", err)
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

// tableWithID 描述一张要补自增主键的表（v16 升级）。
type tableWithID struct {
	table string
	// ddl 是新表结构，%s 由重建函数填成临时表名。
	ddl string
	// columns 是除 id 之外的全部列，搬家时按这个名单逐列复制。
	columns []string
	// orderBy 决定 id 的先后：按原来的主键排，重建前后的读取顺序一致。
	orderBy string
}

// tablesWithNewID 是 v16 要补自增主键的表。原来做主键的那组业务键降成 UNIQUE，
// 写入口的 ON CONFLICT 目标还是它们——换成 id 的话冲突永远不会发生，等于只插不更新。
var tablesWithNewID = []tableWithID{
	{
		table: "anchors",
		ddl: `CREATE TABLE %s (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id    TEXT NOT NULL,
			group_id        TEXT NOT NULL,
			window_first_id TEXT NOT NULL DEFAULT '',
			window_last_id  TEXT NOT NULL DEFAULT '',
			updated_at      INTEGER NOT NULL,
			UNIQUE (workspace_id, group_id)
		)`,
		columns: []string{"workspace_id", "group_id", "window_first_id", "window_last_id", "updated_at"},
		orderBy: "workspace_id, group_id",
	},
	{
		table: "members",
		ddl: `CREATE TABLE %s (
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
		columns: []string{"workspace_id", "group_id", "user_id", "name", "display_name", "extra", "updated_at"},
		orderBy: "workspace_id, group_id, user_id",
	},
	{
		table: "groups",
		ddl: `CREATE TABLE %s (
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
		columns: []string{
			"workspace_id", "group_id", "name", "type", "persona_keyword",
			"message_count", "member_count", "last_ts", "extra", "updated_at",
		},
		orderBy: "workspace_id, group_id",
	},
	{
		table: "workspace_profile",
		ddl: `CREATE TABLE %s (
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
		columns: []string{
			"workspace_id", "platform", "account_kind", "account", "name", "self_user_id",
			"impl", "identity_scheme", "identity_scope", "extra", "created_at", "updated_at",
		},
		orderBy: "workspace_id",
	},
	{
		table: "image_notes",
		ddl: `CREATE TABLE %s (
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
		columns: []string{
			"sha256", "byte_size", "mime", "width", "height", "animated", "note",
			"info_level", "has_text", "status", "attempts", "model", "prompt_version", "updated_at",
		},
		orderBy: "sha256, byte_size",
	},
	{
		table: "asset_tag_jobs",
		ddl: `CREATE TABLE %s (
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
		columns: []string{
			"workspace_id", "group_id", "message_id", "asset_index", "kind", "url",
			"status", "attempts", "last_error", "created_at", "updated_at",
		},
		orderBy: "workspace_id, group_id, message_id, asset_index",
	},
	{
		table: "send_ledger",
		ddl: `CREATE TABLE %s (
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
		columns: []string{"workspace_id", "action_id", "message_id", "text", "sent", "send_ok", "updated_at"},
		orderBy: "workspace_id, action_id",
	},
	{
		table: "settings",
		ddl: `CREATE TABLE %s (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			key        TEXT NOT NULL UNIQUE,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		columns: []string{"key", "value", "updated_at"},
		orderBy: "key",
	},
}

// addIDPrimaryKey 把还没有自增主键的表重建成「id 自增主键 + 原业务键 UNIQUE」（v16 升级）。
//
// SQLite 不能 ALTER TABLE 加主键，只能照 v9 重建 messages 的做法：建新表、按原主键
// 顺序搬数据、删旧表、改名。已经带 id 的表跳过，所以重复执行也安全。
// 群统计触发器挂在 members 上、又引用 groups（两张都要重建），先删掉，表建好再建回来。
func addIDPrimaryKey(db *gorm.DB) error {
	for _, name := range []string{"trg_messages_group_stats", "trg_members_group_stats"} {
		if err := db.Exec(`DROP TRIGGER IF EXISTS ` + name).Error; err != nil {
			return fmt.Errorf("删除群统计触发器 %s 失败：%w", name, err)
		}
	}
	for _, spec := range tablesWithNewID {
		if err := rebuildTableWithID(db, spec); err != nil {
			return err
		}
	}
	return createGroupStatTriggers(db)
}

// rebuildTableWithID 重建一张表，给它加上 id INTEGER PRIMARY KEY AUTOINCREMENT。
// 表不存在或已经有 id 列时直接返回。
func rebuildTableWithID(db *gorm.DB, spec tableWithID) error {
	if !tableExists(db, spec.table) {
		return nil
	}
	var count int
	if err := db.Raw(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'id'`, spec.table).Scan(&count).Error; err != nil {
		return fmt.Errorf("读取 %s 表结构失败：%w", spec.table, err)
	}
	if count > 0 {
		return nil
	}
	temp := spec.table + "_v16"
	columns := strings.Join(spec.columns, ", ")
	steps := []string{
		fmt.Sprintf(spec.ddl, temp),
		fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s ORDER BY %s`, temp, columns, columns, spec.table, spec.orderBy),
		`DROP TABLE ` + spec.table,
		`ALTER TABLE ` + temp + ` RENAME TO ` + spec.table,
	}
	for _, statement := range steps {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("重建 %s 表失败：%w", spec.table, err)
		}
	}
	logutil.Info("state.db 表已重建：新增自增主键 id", "table", spec.table)
	return nil
}

func tableExists(db *gorm.DB, table string) bool {
	var count int
	if err := db.Raw(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count).Error; err != nil {
		return false
	}
	return count > 0
}
