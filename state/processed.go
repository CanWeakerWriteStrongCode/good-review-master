package state

// processed_items：消息处理台账，把重投变成幂等。
//
// 为什么现在就要有：消息天然会被重投——进程崩溃后入站的去重表（内存 TTL）就没了、
// AstrBot 那边也会重推、以后接 MQ 是「至少一次投递」。没有这张表，重投就是
// 重新生成一次回复（白烧一次模型调用）、重新执行一次指令（副作用做两遍）。
//
// 语义是「成功之后才记账」：先处理、成功了再 MarkProcessed。崩溃时那一条没记上，
// 重投会重做一遍——宁可重做，也不要留一条「已认领但没做完」把重投永久拦掉。
//
// 接入点：域在自己的 handler 里，解码出 Item 之后先 Processed(item.ID) 判一次，
// 处理成功再 MarkProcessed。本期只建表与接口，还没有域调用它——接的时候要同时
// 加一个清理任务（PurgeProcessed），否则这张表会一直长。

import (
	"time"

	"gorm.io/gorm/clause"
)

// ProcessedItem 一条已经处理完的消息。
type ProcessedItem struct {
	// ID 是自增主键；item_id 是业务键（表上是 UNIQUE），即 pipeline.Item.ID。
	ID          int64  `gorm:"column:id;primaryKey;autoIncrement"`
	ItemID      string `gorm:"column:item_id"`
	WorkspaceID string `gorm:"column:workspace_id"`
	Stage       string `gorm:"column:stage"`
	CreatedAt   int64  `gorm:"column:created_at"`
}

func (ProcessedItem) TableName() string { return "processed_items" }

// Processed 报告这条消息是否已经处理完。
//
// 没有库时返回 false（纯内存运行没有重投风险，不拦）；查询出错也返回 false，
// 让消息照常处理——幂等表是安全网，不是闸门，它坏了不该让整条链停下。
func (s *Scope) Processed(itemID string) bool {
	if s == nil || s.store == nil || itemID == "" {
		return false
	}
	var count int64
	err := s.store.db.Model(&ProcessedItem{}).Where("item_id = ?", itemID).Count(&count).Error
	if err != nil {
		s.store.noteFailure("查询处理台账", err)
	}
	return count > 0
}

// MarkProcessed 记下这条消息已处理完。重复调用无副作用（冲突就什么都不做）。
//
// 用 DoNothing 而不是 UPDATE 后 INSERT：这里只关心「这一行在不在」，
// 没有要更新的列，冲突时不该消耗一次写。
func (s *Scope) MarkProcessed(itemID, stage string) {
	if s == nil || s.store == nil || itemID == "" || s.store.closed.Load() {
		return
	}
	row := ProcessedItem{
		ItemID:      itemID,
		WorkspaceID: s.workspaceID,
		Stage:       stage,
		CreatedAt:   time.Now().Unix(),
	}
	res := s.store.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "item_id"}},
		DoNothing: true,
	}).Create(&row)
	if res.Error != nil {
		s.store.noteFailure("写处理台账", res.Error)
	}
}

// PurgeProcessed 删掉 before 之前记下的台账，返回删除条数。
//
// 台账只需要覆盖「消息还可能被重投」的窗口：重投通常发生在秒到分钟级，
// 留一天足够宽松。调用方负责按周期跑（本期还没有人调）。
func (s *Store) PurgeProcessed(before int64) int64 {
	if s == nil || before <= 0 || s.closed.Load() {
		return 0
	}
	res := s.db.Where("created_at < ?", before).Delete(&ProcessedItem{})
	if res.Error != nil {
		s.noteFailure("清理处理台账", res.Error)
		return 0
	}
	return res.RowsAffected
}
