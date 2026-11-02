package state

// messages 表：所有入站消息的落库台账，也是发给大模型的 message_id 的来源。
// 读写都带 workspace_id + group_id，跨工作空间共用一张表。

import (
	"encoding/json"
	"errors"

	"good-review-master/conversation"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type messageRow struct {
	// ID 是自增主键，也是发给大模型看的 message_id：跨窗口、跨重启都不变。
	ID int64 `gorm:"column:id;primaryKey;autoIncrement"`
	// 三元组是原来的主键，现在降为唯一键：upsert 的冲突目标显式按它写，见 SaveMessage。
	WorkspaceID string `gorm:"column:workspace_id"`
	GroupID     string `gorm:"column:group_id"`
	MessageID   string `gorm:"column:message_id"`
	UserID      string `gorm:"column:user_id"`
	Content     string `gorm:"column:content"`
	// Mentions 是正文里 @ 到的 openid 列表（JSON 数组文本），MentionsResolved 表示正文里
	// 已经没有 "@用户(" 占位。两者都由 SaveMessage 从正文算出来，回填时只改正文与后者。
	Mentions         string `gorm:"column:mentions"`
	MentionsResolved int    `gorm:"column:mentions_resolved"`
	Images           string `gorm:"column:images"`
	ImageIDs         string `gorm:"column:image_ids"`
	ReplyTo          string `gorm:"column:reply_to"`
	RefIdx           string `gorm:"column:ref_idx"`
	TS               int64  `gorm:"column:ts"`
}

func (messageRow) TableName() string { return "messages" }

// SaveMessage 同步写入一条消息，返回该行的自增主键（写入失败返回 0）。
// 失败只记日志，不阻塞调用方；调用方拿到 0 就当作"这条没有行号"。
func (s *Scope) SaveMessage(msg conversation.Message) int64 {
	if s == nil || s.store == nil || msg.MessageID == "" || s.store.closed.Load() {
		return 0
	}
	images, err := json.Marshal(msg.Images)
	if err != nil {
		images = []byte("[]")
	}
	imageIDs, err := json.Marshal(msg.ImageIDs)
	if err != nil {
		imageIDs = []byte("[]")
	}
	mentions, err := json.Marshal(msg.Mentions)
	if err != nil || len(msg.Mentions) == 0 {
		mentions = []byte("[]")
	}
	// 正文里还有 @用户( 就是还没学到名字，等回填；
	// 没占位的消息直接算完了，不进待办索引。
	resolved := 1
	if conversation.HasMentionPlaceholder(msg.Content) {
		resolved = 0
	}
	row := messageRow{
		WorkspaceID: s.workspaceID, GroupID: msg.GroupID, MessageID: msg.MessageID,
		UserID: msg.UserID, Content: msg.Content,
		Mentions: string(mentions), MentionsResolved: resolved,
		Images:   string(images),
		ImageIDs: string(imageIDs), ReplyTo: msg.ReplyToID, RefIdx: msg.RefIdx, TS: msg.Time,
	}
	// 冲突目标必须显式指定三元组：主键换成自增 id 后，默认目标会变成 id，等于永不更新。
	err = s.store.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "workspace_id"}, {Name: "group_id"}, {Name: "message_id"}},
		UpdateAll: true,
	}).Create(&row).Error
	if err != nil {
		s.store.noteFailure("写入消息", err)
		return 0
	}
	if row.ID > 0 {
		return row.ID
	}
	// 走 ON CONFLICT DO UPDATE 分支时驱动的 RETURNING 由 SQLite 负责，个别版本不回填，回查一次。
	var id int64
	if err := s.store.db.Raw(`SELECT id FROM messages WHERE workspace_id = ? AND group_id = ? AND message_id = ?`,
		s.workspaceID, msg.GroupID, msg.MessageID).Scan(&id).Error; err != nil {
		s.store.noteFailure("回查消息自增ID", err)
		return 0
	}
	return id
}

// UpdateMessageImages 在 Stage 2 把图片内容标识补写到已入库的消息上。
func (s *Scope) UpdateMessageImages(groupID, messageID string, ids []conversation.ImageID) error {
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

// LoadRecent 每个群读回最近 limit 条（时间升序），供启动时填内存缓存。
func (s *Scope) LoadRecent(limit int) ([]conversation.Message, error) {
	if s == nil || s.store == nil || limit <= 0 {
		return nil, nil
	}
	rows, err := s.store.db.Raw(`
		SELECT id, group_id, message_id, user_id, content, mentions, images, image_ids, reply_to, ref_idx, ts FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY group_id ORDER BY ts DESC) AS rn
			FROM messages WHERE workspace_id = ?
		) WHERE rn <= ? ORDER BY group_id, ts`, s.workspaceID, limit).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []conversation.Message
	for rows.Next() {
		var msg conversation.Message
		var mentions, images, imageIDs string
		if err := rows.Scan(&msg.RowID, &msg.GroupID, &msg.MessageID, &msg.UserID, &msg.Content,
			&mentions, &images, &imageIDs, &msg.ReplyToID, &msg.RefIdx, &msg.Time); err != nil {
			return nil, err
		}
		if mentions != "" {
			_ = json.Unmarshal([]byte(mentions), &msg.Mentions)
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

// Messages 是面板消息详情页的读取入口：最近 limit 条（按时间倒序，最新的在最前），
// 外加库里这个群的总条数。昵称从 members 表补——messages 只存 user_id，
// 名字在 members 里，取那里的最新值（成员改过名也能对上人）。
// 图片描述也从全局图库补：messages 里只有 [图片] 这个占位，描述在 image_notes 表。
func (s *Scope) Messages(groupID string, limit int) ([]conversation.Message, int, error) {
	if s == nil || s.store == nil || groupID == "" || limit <= 0 || s.store.closed.Load() {
		return []conversation.Message{}, 0, nil
	}
	var total int64
	if err := s.store.db.Model(&messageRow{}).
		Where("workspace_id = ? AND group_id = ?", s.workspaceID, groupID).
		Count(&total).Error; err != nil {
		s.store.noteFailure("统计群消息", err)
		return nil, 0, err
	}
	if total == 0 {
		return []conversation.Message{}, 0, nil
	}
	var rows []messageRow
	if err := s.store.db.
		Where("workspace_id = ? AND group_id = ?", s.workspaceID, groupID).
		Order("ts DESC, id DESC").Limit(limit).
		Find(&rows).Error; err != nil {
		s.store.noteFailure("读取群消息", err)
		return nil, int(total), err
	}
	// 查询本来就是按时间倒序取最近 limit 条，这里不再翻转发：
	// 页面从上往下读，第一条就是库里最新的那条。
	out := make([]conversation.Message, 0, len(rows))
	for _, row := range rows {
		out = append(out, messageFromRow(row))
	}
	s.fillMemberNames(groupID, out)
	s.hydrateImageNotes(out)
	return out, int(total), nil
}

func messageFromRow(row messageRow) conversation.Message {
	msg := conversation.Message{
		RowID: row.ID, MessageID: row.MessageID, GroupID: row.GroupID,
		UserID: row.UserID, Content: row.Content,
		ReplyToID: row.ReplyTo, RefIdx: row.RefIdx, Time: row.TS,
	}
	if row.Images != "" {
		_ = json.Unmarshal([]byte(row.Images), &msg.Images)
	}
	if row.Mentions != "" {
		_ = json.Unmarshal([]byte(row.Mentions), &msg.Mentions)
	}
	if row.ImageIDs != "" {
		_ = json.Unmarshal([]byte(row.ImageIDs), &msg.ImageIDs)
	}
	return msg
}

// fillMemberNames 用 members 表里的名字补消息的 nick / card。
// 查不到的保持空串：那些人多半是早期消息里的，成员目录没收录。
func (s *Scope) fillMemberNames(groupID string, msgs []conversation.Message) {
	seen := make(map[string]struct{}, len(msgs))
	ids := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		if msg.UserID == "" {
			continue
		}
		if _, ok := seen[msg.UserID]; ok {
			continue
		}
		seen[msg.UserID] = struct{}{}
		ids = append(ids, msg.UserID)
	}
	if len(ids) == 0 {
		return
	}
	var rows []memberRow
	if err := s.store.db.Select("user_id, name, display_name").
		Where("workspace_id = ? AND group_id = ? AND user_id IN ?", s.workspaceID, groupID, ids).
		Find(&rows).Error; err != nil {
		s.store.noteFailure("补消息昵称", err)
		return
	}
	names := make(map[string]memberRow, len(rows))
	for _, row := range rows {
		names[row.UserID] = row
	}
	for i := range msgs {
		row, ok := names[msgs[i].UserID]
		if !ok {
			continue
		}
		msgs[i].Nickname = row.Name
		msgs[i].Card = row.DisplayName
	}
}

func (s *Scope) hydrateImageNotes(msgs []conversation.Message) {
	var ids []conversation.ImageID
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
		notesForMsg := make([]conversation.ImageNote, len(msgs[i].ImageIDs))
		for j, id := range msgs[i].ImageIDs {
			notesForMsg[j] = notes[id]
		}
		msgs[i].ImageNotes = notesForMsg
	}
}

// MentionRow 是一行等着换 @ 名字的消息：只要主键和正文，够拼新正文用。
type MentionRow struct {
	ID      int64  `gorm:"column:id"`
	Content string `gorm:"column:content"`
}

// PendingMentionRows 取该群还没换完 @ 的名字的消息（按 id 升序，先老的先换）。
//
// 分批取而不是一次全取：SQLite 只有一个写者，一批改完就释放写锁，
// 入站消息落库不会被回填挡在后面。
func (s *Scope) PendingMentionRows(groupID string, limit int) ([]MentionRow, error) {
	if s == nil || s.store == nil || groupID == "" || limit <= 0 || s.store.closed.Load() {
		return nil, nil
	}
	var rows []MentionRow
	err := s.store.db.Raw(`SELECT id, content FROM messages
		WHERE workspace_id = ? AND group_id = ? AND mentions_resolved = 0
		ORDER BY id LIMIT ?`, s.workspaceID, groupID, limit).Scan(&rows).Error
	if err != nil {
		s.store.noteFailure("读取待回填的 @ 占位", err)
	}
	return rows, err
}

// RewriteMention 写回一行改好的正文与待办标记，返回是否真的改到了。
//
// WHERE 带上旧正文：多个进程同时改同一行时，慢的那一下会命中 0 行，直接跳过。
// 下一轮 PendingMentionRows 读到的是别人写进去的新正文，不会丢。
func (s *Scope) RewriteMention(rowID int64, oldContent, newContent string, resolved int) (bool, error) {
	if s == nil || s.store == nil || rowID <= 0 || s.store.closed.Load() {
		return false, nil
	}
	result := s.store.db.Model(&messageRow{}).
		Where("id = ? AND content = ?", rowID, oldContent).
		Updates(map[string]any{"content": newContent, "mentions_resolved": resolved})
	if result.Error != nil {
		s.store.noteFailure("回填 @ 占位", result.Error)
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// PendingMentionGroups 返回还有 @ 占位等着换的群。启动时用它补一遍——
// 上次可能在“名字已学到、正文还没换”的位置就退出了。
func (s *Scope) PendingMentionGroups() ([]string, error) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return nil, nil
	}
	var groups []string
	err := s.store.db.Model(&messageRow{}).
		Where("workspace_id = ? AND mentions_resolved = 0", s.workspaceID).
		Distinct().Pluck("group_id", &groups).Error
	if err != nil {
		s.store.noteFailure("读取待回填 @ 的群", err)
	}
	return groups, err
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
