package kv

import (
	"context"
	"fmt"
	"time"

	"github.com/dgraph-io/ristretto/v2"
)

// Ristretto 基于 Ristretto v2 的进程内实现。
//
// Ristretto 的 Set 是缓冲写（最终可见），缓存未命中是正常路径；
// 需要“写完立刻读到”的测试请调用 Wait()。
type Ristretto struct {
	cache *ristretto.Cache[string, []byte]
}

// NewRistretto 创建进程内缓存。maxCost 是近似内存预算（字节）。
func NewRistretto(maxCost int64) (*Ristretto, error) {
	if maxCost <= 0 {
		maxCost = 64 << 20
	}
	cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: maxCost / 64, // 大约每 64 字节一个计数器
		MaxCost:     maxCost,
		BufferItems: 64,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化 Ristretto 失败：%w", err)
	}
	return &Ristretto{cache: cache}, nil
}

func (r *Ristretto) Get(_ context.Context, key string) ([]byte, bool, error) {
	if r == nil || r.cache == nil {
		return nil, false, ErrClosed
	}
	value, ok := r.cache.Get(key)
	if !ok {
		return nil, false, nil
	}
	return value, true, nil
}

func (r *Ristretto) GetMany(ctx context.Context, keys []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(keys))
	for _, key := range keys {
		value, ok, err := r.Get(ctx, key)
		if err != nil {
			return out, err
		}
		if ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *Ristretto) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	if r == nil || r.cache == nil {
		return ErrClosed
	}
	if key == "" {
		return nil
	}
	if ttl > 0 {
		r.cache.SetWithTTL(key, value, int64(len(value)), ttl)
		return nil
	}
	r.cache.Set(key, value, int64(len(value)))
	return nil
}

func (r *Ristretto) Delete(_ context.Context, key string) error {
	if r == nil || r.cache == nil {
		return ErrClosed
	}
	r.cache.Del(key)
	return nil
}

// Wait 等待缓冲区里的写落地（测试用）。
func (r *Ristretto) Wait() {
	if r != nil && r.cache != nil {
		r.cache.Wait()
	}
}

func (r *Ristretto) Close() error {
	if r == nil || r.cache == nil {
		return nil
	}
	r.cache.Close()
	return nil
}
