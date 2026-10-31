package config

import (
	"encoding/json"
	"time"
)

// ApplyRuntimeSettings 把 settings 表里的动态覆盖应用到一份配置副本上。
//
// config.yaml 是默认值，数据库是运行时覆盖（库 > 文件）。只支持明确列出的
// 全局参数；监听地址、数据库路径这类启动参数仍然只认文件/环境变量。
// 非法值不会让进程崩溃：保留文件里的值并返回错误，由调用方记一条 warn。
func ApplyRuntimeSettings(base *Config, raw map[string]json.RawMessage) (*Config, error) {
	if base == nil || len(raw) == 0 {
		return base, nil
	}
	out := *base
	out.Pipeline = base.Pipeline
	out.Assets = base.Assets

	var firstErr error
	if payload, ok := raw["pipeline"]; ok {
		var overlay runtimePipelineSettings
		if err := json.Unmarshal(payload, &overlay); err != nil {
			firstErr = err
		} else {
			overlay.applyTo(&out.Pipeline)
		}
	}
	if payload, ok := raw["assets"]; ok {
		var overlay runtimeAssetsSettings
		if err := json.Unmarshal(payload, &overlay); err != nil && firstErr == nil {
			firstErr = err
		} else {
			overlay.applyTo(&out.Assets)
		}
	}
	return &out, firstErr
}

type runtimePipelineSettings struct {
	SyncBatch        *int `json:"sync_batch"`
	SyncLingerMs     *int `json:"sync_linger_ms"`
	SyncConcurrency  *int `json:"sync_concurrency"`
	SyncRetries      *int `json:"sync_retries"`
	AssetBatch       *int `json:"asset_batch"`
	AssetLingerMs    *int `json:"asset_linger_ms"`
	AssetConcurrency *int `json:"asset_concurrency"`
	AssetRetries     *int `json:"asset_retries"`
	AgentBatch       *int `json:"agent_batch"`
	AgentLingerMs    *int `json:"agent_linger_ms"`
	AgentConcurrency *int `json:"agent_concurrency"`
	AgentRetries     *int `json:"agent_retries"`
	IdleTTLSec       *int `json:"idle_ttl_sec"`
	DrainTimeoutSec  *int `json:"drain_timeout_sec"`
}

func (s runtimePipelineSettings) applyTo(out *PipelineConf) {
	applyPositive := func(value *int, target *int) {
		if value != nil && *value > 0 {
			*target = *value
		}
	}
	applyNonNegative := func(value *int, target *int) {
		if value != nil && *value >= 0 {
			*target = *value
		}
	}
	applyMs := func(value *int, target *time.Duration) {
		if value != nil && *value >= 0 {
			*target = time.Duration(*value) * time.Millisecond
		}
	}
	applySec := func(value *int, target *time.Duration) {
		if value != nil && *value >= 0 {
			*target = time.Duration(*value) * time.Second
		}
	}
	applyPositive(s.SyncBatch, &out.SyncBatch)
	applyMs(s.SyncLingerMs, &out.SyncLinger)
	applyPositive(s.SyncConcurrency, &out.SyncConcurrency)
	applyNonNegative(s.SyncRetries, &out.SyncRetries)
	applyPositive(s.AssetBatch, &out.AssetBatch)
	applyMs(s.AssetLingerMs, &out.AssetLinger)
	applyPositive(s.AssetConcurrency, &out.AssetConcurrency)
	applyNonNegative(s.AssetRetries, &out.AssetRetries)
	applyPositive(s.AgentBatch, &out.AgentBatch)
	applyMs(s.AgentLingerMs, &out.AgentLinger)
	applyPositive(s.AgentConcurrency, &out.AgentConcurrency)
	applyNonNegative(s.AgentRetries, &out.AgentRetries)
	applySec(s.IdleTTLSec, &out.IdleTTL)
	applySec(s.DrainTimeoutSec, &out.DrainTimeout)
}

type runtimeAssetsSettings struct {
	BackfillEnabled     *bool   `json:"backfill_enabled"`
	BackfillIntervalSec *int    `json:"backfill_interval_sec"`
	BackfillBatch       *int    `json:"backfill_batch"`
	BackfillMaxAttempts *int    `json:"backfill_max_attempts"`
	JobRetentionDays    *int    `json:"job_retention_days"`
	MessageBudgetSec    *int    `json:"message_budget_sec"`
	TagConcurrency      *int    `json:"tag_concurrency"`
	PromptVersion       *string `json:"prompt_version"`
}

func (s runtimeAssetsSettings) applyTo(out *AssetsConf) {
	applyNonNegative := func(value *int, target *int) {
		if value != nil && *value >= 0 {
			*target = *value
		}
	}
	applyPositive := func(value *int, target *int) {
		if value != nil && *value > 0 {
			*target = *value
		}
	}
	applySec := func(value *int, target *time.Duration) {
		if value != nil && *value >= 0 {
			*target = time.Duration(*value) * time.Second
		}
	}
	if s.BackfillEnabled != nil {
		out.BackfillEnabled = *s.BackfillEnabled
	}
	applySec(s.BackfillIntervalSec, &out.BackfillInterval)
	applyPositive(s.BackfillBatch, &out.BackfillBatch)
	applyNonNegative(s.BackfillMaxAttempts, &out.BackfillMaxAttempts)
	if s.JobRetentionDays != nil && *s.JobRetentionDays >= 0 {
		out.JobRetention = time.Duration(*s.JobRetentionDays) * 24 * time.Hour
	}
	applySec(s.MessageBudgetSec, &out.MessageBudget)
	applyPositive(s.TagConcurrency, &out.TagConcurrency)
	if s.PromptVersion != nil && *s.PromptVersion != "" {
		out.PromptVersion = *s.PromptVersion
	}
}
