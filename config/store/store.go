// Package store 提供配置的**不可变快照**，以及驱动它更新的 Informer 骨架。
//
// 概念上对标 client-go 的 cache.Store / SharedInformer：
//
//	Store[T]     ↔  cache.Store        持有一份不可变快照，读者无锁
//	Source       ↔  Watch 接口         只提供"变了"的信号
//	Informer[T]  ↔  SharedInformer     List → Watch → 交换快照 → 通知 handler
//
// **必须说清的取舍**：本项目只有 3 个 YAML 要盯，纯轮询实际只要 30 行。
// 之所以仍然拆出这三个概念，是因为要学的正是它们各自解决什么问题——
// 读者与写者如何解耦（不可变快照）、变更如何被感知（Source）、
// 感知到之后如何保证读者永远看到自洽的一份（Informer 的整体替换）。
// 生产上真正省事的替代方案是把 config 直接做成一堆 atomic.Value，
// 那样代码更少，但"变更"这件事会散在各处、没有一个统一的入口。
package store

import "sync/atomic"

// Store 持有一份不可变快照。读者拿到的指针在其生命周期内不会被修改——
// 更新是整体替换，不是就地改写，所以读者永远看到彼此自洽的一份配置，
// 不会读到"改了一半"的中间态（这正是共享可变配置最典型的故障）。
type Store[T any] struct {
	current atomic.Pointer[T]
}

// NewStore 以一份初始快照构造。
func NewStore[T any](initial *T) *Store[T] {
	instance := &Store[T]{}
	instance.current.Store(initial)
	return instance
}

// Get 返回当前快照。
//
// **调用方不要缓存返回值**：一旦把它存进字段或局部变量长期持有，
// 就退回到了共享可变状态——热更新对它不再生效，而且不会有任何报错提示。
func (s *Store[T]) Get() *T {
	return s.current.Load()
}

// replace 整体替换快照。只有 Informer 会调它。
func (s *Store[T]) replace(next *T) {
	s.current.Store(next)
}
