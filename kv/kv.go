// Package kv 是通用 KV 缓存抽象。
//
// 值统一为 []byte，TTL 是一等公民；所有调用方都必须能接受 miss 与错误并回源。
// 进程内实现用 Ristretto（写是异步可见的），以后换 Redis 时业务代码不用改。
package kv

import (
	"context"
	"errors"
	"time"
)

// ErrClosed 缓存已关闭。
var ErrClosed = errors.New("kv: cache closed")

// KV 通用缓存接口。
type KV interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	GetMany(ctx context.Context, keys []string) (map[string][]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	Close() error
}

// Noop 永远 miss 的实现，用于未启用缓存的测试与降级路径。
type Noop struct{}

func (Noop) Get(context.Context, string) ([]byte, bool, error)            { return nil, false, nil }
func (Noop) GetMany(context.Context, []string) (map[string][]byte, error) { return nil, nil }
func (Noop) Set(context.Context, string, []byte, time.Duration) error     { return nil }
func (Noop) Delete(context.Context, string) error                         { return nil }
func (Noop) Close() error                                                 { return nil }
