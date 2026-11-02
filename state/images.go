package state

// image_notes 表：全局图库。不带 workspace_id——一张图的描述只写图本身，
// 与聊天语境无关，可以跨群、跨账号复用。缓存 TTL 比成员信息长，因为描述相对稳定。

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"good-review-master/conversation"
)

// imageCacheTTL 图片描述的进程内缓存时长。
const imageCacheTTL = 30 * 24 * time.Hour

// ImageNote 是全局图库的一行（image_notes 表，不含 workspace_id）。
type ImageNote = conversation.ImageNote

func imageKVKey(id conversation.ImageID) string {
	return fmt.Sprintf("img:%s:%d", id.SHA, id.Size)
}

// GetImageNotes 按 (sha256, byte_size) 批量读取图片描述，返回以 ImageID 为键的 map。
// 先读 Ristretto，miss 的再查库并回填缓存。
func (s *Store) GetImageNotes(ids []conversation.ImageID) (map[conversation.ImageID]conversation.ImageNote, error) {
	if s == nil || len(ids) == 0 {
		return nil, nil
	}
	out := make(map[conversation.ImageID]conversation.ImageNote, len(ids))
	bySHA := make(map[string]conversation.ImageID, len(ids))
	missing := make([]conversation.ImageID, 0, len(ids))
	seen := make(map[conversation.ImageID]struct{}, len(ids))
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
			var note conversation.ImageNote
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
			var note conversation.ImageNote
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

func (s *Store) SaveImageNote(note conversation.ImageNote) error {
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

func (s *Store) MarkImageNoteFailed(note conversation.ImageNote) error {
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

func (s *Store) upsertImageNote(note conversation.ImageNote) error {
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
