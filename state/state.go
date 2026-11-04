// Package state 是本程序的本地档案（SQLite）。
//
// 所有工作空间共用同一个 state.db，靠每张表的 workspace_id 列隔离。
// 消息与身份信息同步写：一条消息处理完成（Ack）之前必须已经落库，
// 不再保留“写队列满了就丢库写”的旧语义。读路径优先走 Ristretto 缓存，miss 再回源。
//
// 失败模式刻意是“降级”：打不开或写失败只记日志，调用方继续用内存缓存。
// 档案丢了大不了没有历史，不能让一次磁盘满把回复也一起弄没。
//
// 按表拆文件，一个文件一类数据：
//
//	schema.go     表结构（只负责把空库建出来）
//	messages.go   messages（消息台账，也是发给大模型的 message_id 来源）
//	images.go     image_notes（全局图库，不带 workspace_id）
//	anchors.go    anchors（上次发出去的窗口首尾，前缀缓存的命门）
//	directory.go  members / groups / 两张 *_info_history
//	persona.go    workspace_profile（群人格在 groups.persona_keyword 列上）
//	jobs.go       asset_tag_jobs（assets 的补标签欠条）
//	ops.go        send_ledger / alerts / settings / 工作空间列表
//	processed.go  processed_items（消息处理台账，把重投变幂等）
//
// 所有方法都挂在 Store（共享连接）或 Scope（带 workspace_id 的视图）上，
// 没有额外的 Repo 接口：调用方拿到的仍然是 *state.Scope。
package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"good-review-master/kv"
	"good-review-master/logutil"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	// warnRows / warnBytes 启动统计的告警阈值（清理是人工决定）。
	warnRows  = 100_000
	warnBytes = 100 << 20

	// kvBudget 是进程内缓存的近似内存预算。
	kvBudget = 64 << 20
)

// Store 共享的 SQLite 档案。Scope 负责补上 workspace_id。
type Store struct {
	db   *gorm.DB
	path string
	kv   kv.KV

	closed   atomic.Bool
	failures atomic.Int64
}

// Scope 一个工作空间的视图。所有方法都自动带上 workspace_id。
type Scope struct {
	store       *Store
	workspaceID string
}

// Store 返回作用域所属的共享库。
// 面板做跨账号批量查询时，一两个视图的 scope 就能反查到同一个库，
// 不用把 *Store 一路传下去。纯内存运行时为 nil。
func (s *Scope) Store() *Store {
	if s == nil {
		return nil
	}
	return s.store
}

// Open 打开（必要时创建）指定路径的 state.db。
func Open(path string) (*Store, error) {
	if err := ensureFile(path); err != nil {
		return nil, err
	}

	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("打开 %s 失败：%w", path, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取 %s 的连接失败：%w", path, err)
	}
	// 单进程单连接：WAL 下多写并发只会换来 database is locked，不会更快。
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接 %s 失败：%w", path, err)
	}
	if err := migrate(db, path); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	store := &Store{db: db, path: path, kv: kv.Noop{}}
	if cache, err := kv.NewRistretto(kvBudget); err != nil {
		logutil.Warn("初始化 Ristretto 缓存失败，退化为每次回源", "err", err)
	} else {
		store.kv = cache
	}
	return store, nil
}

// Scope 返回某个工作空间的视图。Store 为 nil 时返回 nil，调用方方法可安全空调用。
func (s *Store) Scope(workspaceID string) *Scope {
	if s == nil {
		return nil
	}
	return &Scope{store: s, workspaceID: workspaceID}
}

// KV 返回进程内缓存（测试与面板用）。
func (s *Store) KV() kv.KV {
	if s == nil {
		return kv.Noop{}
	}
	return s.kv
}

func (s *Store) noteFailure(action string, err error) {
	if n := s.failures.Add(1); n%100 == 1 {
		logutil.Error("state.db "+action+"失败，继续用内存缓存", "err", err, "path", s.path, "failures", n)
	}
}

// Report 统计消息条数与库文件体积（含 WAL），超阈值时用 warn 提示人工清理。
func (s *Store) Report() {
	if s == nil {
		return
	}
	rows, size := s.Stats()
	logutil.Info("本地档案统计", "path", s.path, "messages", rows, "db_bytes", size)

	perWorkspace, err := s.countByWorkspace()
	if err != nil {
		s.noteFailure("统计各工作空间条数", err)
	} else {
		for workspaceID, count := range perWorkspace {
			logutil.Info("工作空间档案", "workspace", workspaceID, "messages", count)
		}
	}
	if rows > warnRows || size > warnBytes {
		logutil.Warn("state.db 数据偏多，确认是否需要清理",
			"path", s.path, "messages", rows, "db_bytes", size,
			"提示", "停服后删除 state.db（含 -wal/-shm）重建，或手工 DELETE + VACUUM")
	}
}

func (s *Store) countByWorkspace() (map[string]int64, error) {
	rows, err := s.db.Raw(`SELECT workspace_id, COUNT(*) FROM messages GROUP BY workspace_id`).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var workspaceID string
		var count int64
		if err := rows.Scan(&workspaceID, &count); err != nil {
			return nil, err
		}
		out[workspaceID] = count
	}
	return out, rows.Err()
}

// Stats 返回库里的消息总条数与库文件体积（含 -wal）。
func (s *Store) Stats() (rows int64, bytes int64) {
	if s == nil {
		return 0, 0
	}
	if err := s.db.Raw(`SELECT COUNT(*) FROM messages`).Scan(&rows).Error; err != nil {
		s.noteFailure("统计消息条数", err)
	}
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			bytes += info.Size()
		}
	}
	return rows, bytes
}

// Close 关闭数据库与缓存。可重复调用。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	if s.closed.Swap(true) {
		return nil
	}
	if err := s.kv.Close(); err != nil {
		logutil.Warn("关闭 KV 缓存失败", "err", err)
	}
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
