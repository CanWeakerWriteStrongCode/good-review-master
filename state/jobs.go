package state

// asset_tag_jobs 表：Stage 2 没来得及打完标签的资产欠条。
// 后台 worker 按 status + updated_at 扫出来重试，done/missing 过了保留期才清。

import (
	"time"

	"gorm.io/gorm/clause"
)

// AssetTagJob 一条“某消息里的第几个资产”的补标签欠条。
type AssetTagJob struct {
	WorkspaceID string `gorm:"column:workspace_id;primaryKey"`
	GroupID     string `gorm:"column:group_id;primaryKey"`
	MessageID   string `gorm:"column:message_id;primaryKey"`
	AssetIndex  int    `gorm:"column:asset_index;primaryKey"`
	Kind        string `gorm:"column:kind"`
	URL         string `gorm:"column:url"`
	Status      string `gorm:"column:status"`
	Attempts    int    `gorm:"column:attempts"`
	LastError   string `gorm:"column:last_error"`
	CreatedAt   int64  `gorm:"column:created_at"`
	UpdatedAt   int64  `gorm:"column:updated_at"`
}

func (AssetTagJob) TableName() string { return "asset_tag_jobs" }

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
			"kind", "url", "status", "attempts", "last_error", "updated_at",
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
			"last_error": job.LastError,
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
