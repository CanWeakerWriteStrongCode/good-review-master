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
	Images      string `gorm:"column:images"`
	ImageIDs    string `gorm:"column:image_ids"`
	ReplyTo     string `gorm:"column:reply_to"`
	RefIdx      string `gorm:"column:ref_idx"`
	TS          int64  `gorm:"column:ts"`
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
	row := messageRow{
		WorkspaceID: s.workspaceID, GroupID: msg.GroupID, MessageID: msg.MessageID,
		UserID: msg.UserID, Content: msg.Content, Images: string(images),
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
		SELECT id, group_id, message_id, user_id, content, images, image_ids, reply_to, ref_idx, ts FROM (
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
		var images, imageIDs string
		if err := rows.Scan(&msg.RowID, &msg.GroupID, &msg.MessageID, &msg.UserID, &msg.Content,
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
