package workspace

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/workspace/state"
)

// settingsRefreshInterval 是数据库设置重新载入的间隔。
// 没有通知机制，只能轮询；30 秒足够让“改完很快生效”，也不会给 SQLite 添压力。
const settingsRefreshInterval = 30 * time.Second

// settingsOverlay 把全局 settings 表叠加到配置文件快照上（库 > 文件）。
//
// 只覆盖 runtime 可调的流水线/资产参数；监听地址、数据库路径这类启动参数
// 仍然只认 config.yaml / 环境变量，否则会出现“读库配置需要先连库”的循环。
type settingsOverlay struct {
	base  config.Snapshot
	store *state.Store
	raw   atomic.Pointer[map[string]json.RawMessage]
}

func newSettingsOverlay(base config.Snapshot, store *state.Store, ctx context.Context) *settingsOverlay {
	overlay := &settingsOverlay{base: base, store: store}
	overlay.reload()
	if store != nil && ctx != nil {
		go overlay.loop(ctx)
	}
	return overlay
}

func (o *settingsOverlay) loop(ctx context.Context) {
	ticker := time.NewTicker(settingsRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.reload()
		}
	}
}

func (o *settingsOverlay) reload() {
	if o.store == nil {
		return
	}
	raw, err := o.store.LoadSettings()
	if err != nil {
		logutil.Warn("读取全局设置失败，继续用配置文件的值", "err", err)
		return
	}
	o.raw.Store(&raw)
}

// Snapshot 返回叠加后的配置。每次调用复制一份，旧快照保持不可变。
func (o *settingsOverlay) Snapshot() *config.Config {
	base := o.base()
	raw := o.raw.Load()
	if raw == nil || len(*raw) == 0 {
		return base
	}
	merged, err := config.ApplyRuntimeSettings(base, *raw)
	if err != nil {
		logutil.Warn("全局设置格式不合法，继续用配置文件的值", "err", err)
		return base
	}
	return merged
}
