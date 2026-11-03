package state

// asset_tag_jobs 表：Stage 2 没来得及打完标签的资产欠条。
//
// 多个进程会同时补同一张表，所以"扫出来"和"接下来干"是两步：先按 status + updated_at
// 选出候选，再用一条条件 UPDATE 把某条从 pending/failed 改成 running——只有真改动了 1 行
// 的进程算领到，其余跳过。领走的欠条拿 updated_at 当租约：太久没动静说明那个进程已经没了，
// 别的进程可以重新领。done/missing 是终态，过了保留期才清。

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AssetTagJob 一条“某消息里的第几个资产”的补标签欠条。
type AssetTagJob struct {
	// ID 是自增主键；四列是业务键（表上是 UNIQUE），upsert 的冲突目标按它们写。
	ID          int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID string `gorm:"column:workspace_id"`
	GroupID     string `gorm:"column:group_id"`
	MessageID   string `gorm:"column:message_id"`
	AssetIndex  int    `gorm:"column:asset_index"`
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

// AssetTagClaim 是一次领取的时间边界，由调用方按当前配置算好传进来
// （时间从外面给，是为了让每个进程用自己的时钟判断同一份租约）。
type AssetTagClaim struct {
	Now         time.Time // 这次领取的时刻，写进 updated_at 当租约起点
	StaleBefore time.Time // updated_at 早于它，说明领走的那一方已经不在了
	MaxAttempts int       // 已经试到这个次数就不再候选
}

// ClaimableAssetTagJobs 返回可以试的欠条（跨工作空间），按更新时间升序。
// 这只是候选名单：真正能不能动手，得看 ClaimAssetTagJob 抢不抢得到。
func (s *Store) ClaimableAssetTagJobs(limit int, claim AssetTagClaim) ([]AssetTagJob, error) {
	if s == nil || limit <= 0 {
		return nil, nil
	}
	var jobs []AssetTagJob
	err := s.db.Where("attempts < ?", claim.MaxAttempts).
		Where("(status IN ? OR (status = ? AND updated_at < ?))",
			[]string{"pending", "failed"}, "running", claim.StaleBefore.Unix()).
		Order("updated_at ASC").Limit(limit).Find(&jobs).Error
	return jobs, err
}

// ClaimAssetTagJob 抢占一条欠条：把 pending/failed（或租约过期的 running）改成 running，
// 同时把 attempts 加一。返回 true 才算领到；false 说明别的进程先抢走了，跳过就行。
//
// 判断条件和赋值在同一条 UPDATE 里，SQLite 会把它当一次写事务串行执行，
// 所以两个进程同时抢，只会有一个拿到 RowsAffected == 1。
func (s *Store) ClaimAssetTagJob(job AssetTagJob, claim AssetTagClaim) (bool, error) {
	if s == nil {
		return false, nil
	}
	result := s.db.Model(&AssetTagJob{}).
		Where("workspace_id = ? AND group_id = ? AND message_id = ? AND asset_index = ?",
			job.WorkspaceID, job.GroupID, job.MessageID, job.AssetIndex).
		Where("attempts < ?", claim.MaxAttempts).
		Where("(status IN ? OR (status = ? AND updated_at < ?))",
			[]string{"pending", "failed"}, "running", claim.StaleBefore.Unix()).
		Updates(map[string]any{
			"status":     "running",
			"attempts":   gorm.Expr("attempts + 1"),
			"updated_at": claim.Now.Unix(),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// UpdateAssetTagJob 按主键写回最终状态。
//
// attempts 不在这里改：它在抢到的那一刻已经加过了，这里再用调用方手里的旧值覆盖，
// 会把别的进程加的次数抹掉。
func (s *Store) UpdateAssetTagJob(job AssetTagJob) error {
	if s == nil {
		return nil
	}
	job.UpdatedAt = time.Now().Unix()
	return s.db.Model(&AssetTagJob{}).
		Where("workspace_id = ? AND group_id = ? AND message_id = ? AND asset_index = ?",
			job.WorkspaceID, job.GroupID, job.MessageID, job.AssetIndex).
		Updates(map[string]any{
			"status": job.Status, "last_error": job.LastError,
			"updated_at": job.UpdatedAt,
		}).Error
}

// CleanupAssetTagJobs 删除超过保留期的 done/missing 台账。
// pending/failed/running 永不清理：前两个还要补，running 交给租约去回收。
func (s *Store) CleanupAssetTagJobs(now time.Time, retention time.Duration) (int64, error) {
	if s == nil || retention <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-retention).Unix()
	result := s.db.Where("status IN ? AND updated_at < ?", []string{"done", "missing"}, cutoff).
		Delete(&AssetTagJob{})
	return result.RowsAffected, result.Error
}
