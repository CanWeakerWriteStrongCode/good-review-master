package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// pipelineSection 三段流水线的缓冲/并发参数（config.yaml 的 pipeline 段）。
//
// 所有字段都用指针：显式 0 有自己的业务含义（见各字段注释），
// 不能和“没写、用缺省值”混为一谈。
type pipelineSection struct {
	SyncBatch       *int `yaml:"sync_batch"`
	SyncLingerMs    *int `yaml:"sync_linger_ms"`
	SyncConcurrency *int `yaml:"sync_concurrency"`
	SyncRetries     *int `yaml:"sync_retries"`

	AssetBatch       *int `yaml:"asset_batch"`
	AssetLingerMs    *int `yaml:"asset_linger_ms"`
	AssetConcurrency *int `yaml:"asset_concurrency"`
	AssetRetries     *int `yaml:"asset_retries"`

	AgentBatch       *int `yaml:"agent_batch"`
	AgentLingerMs    *int `yaml:"agent_linger_ms"`
	AgentConcurrency *int `yaml:"agent_concurrency"`
	AgentRetries     *int `yaml:"agent_retries"`

	PerChatCapacity *int `yaml:"per_chat_capacity"`
	GlobalCapacity  *int `yaml:"global_capacity"`

	IdleTTLSec      *int `yaml:"idle_ttl_sec"`
	DrainTimeoutSec *int `yaml:"drain_timeout_sec"`
}

const (
	defaultSyncBatch        = 20
	defaultAssetBatch       = 4
	defaultAgentBatch       = 1
	defaultSyncLingerMs     = 20
	defaultAssetLingerMs    = 20
	defaultAgentLingerMs    = 0
	defaultSyncConcurrency  = 4
	defaultAssetConcurrency = 2
	defaultAgentConcurrency = 2
	defaultStageRetries     = 3
	defaultPerChatCapacity  = 500
	defaultGlobalCapacity   = 5000
	defaultIdleTTLSec       = 600
	defaultDrainTimeoutSec  = 10
)

func (s *pipelineSection) Name() string { return "pipeline" }

func (s *pipelineSection) Unmarshal(document *yaml.Node) error {
	return decodeSection(document, s.Name(), s)
}

func (s *pipelineSection) SetDefaults() {
	set := func(target **int, value int) {
		if *target == nil {
			v := value
			*target = &v
		}
	}
	set(&s.SyncBatch, defaultSyncBatch)
	set(&s.SyncLingerMs, defaultSyncLingerMs)
	set(&s.SyncConcurrency, defaultSyncConcurrency)
	set(&s.SyncRetries, defaultStageRetries)
	set(&s.AssetBatch, defaultAssetBatch)
	set(&s.AssetLingerMs, defaultAssetLingerMs)
	set(&s.AssetConcurrency, defaultAssetConcurrency)
	set(&s.AssetRetries, defaultStageRetries)
	set(&s.AgentBatch, defaultAgentBatch)
	set(&s.AgentLingerMs, defaultAgentLingerMs)
	set(&s.AgentConcurrency, defaultAgentConcurrency)
	set(&s.AgentRetries, defaultStageRetries)
	set(&s.PerChatCapacity, defaultPerChatCapacity)
	set(&s.GlobalCapacity, defaultGlobalCapacity)
	set(&s.IdleTTLSec, defaultIdleTTLSec)
	set(&s.DrainTimeoutSec, defaultDrainTimeoutSec)
}

func (s *pipelineSection) Validate() error {
	positive := []struct {
		name string
		val  *int
	}{
		{"pipeline.sync_batch", s.SyncBatch},
		{"pipeline.sync_concurrency", s.SyncConcurrency},
		{"pipeline.asset_batch", s.AssetBatch},
		{"pipeline.asset_concurrency", s.AssetConcurrency},
		{"pipeline.agent_batch", s.AgentBatch},
		{"pipeline.agent_concurrency", s.AgentConcurrency},
		{"pipeline.per_chat_capacity", s.PerChatCapacity},
		{"pipeline.global_capacity", s.GlobalCapacity},
	}
	for _, item := range positive {
		if *item.val <= 0 {
			return positiveInt(item.name, *item.val, "批量/并发/容量为 0 会让流水线停摆")
		}
	}
	nonNegative := []struct {
		name string
		val  *int
	}{
		{"pipeline.sync_linger_ms", s.SyncLingerMs},
		{"pipeline.sync_retries", s.SyncRetries},
		{"pipeline.asset_linger_ms", s.AssetLingerMs},
		{"pipeline.asset_retries", s.AssetRetries},
		{"pipeline.agent_linger_ms", s.AgentLingerMs},
		{"pipeline.agent_retries", s.AgentRetries},
		{"pipeline.idle_ttl_sec", s.IdleTTLSec},
		{"pipeline.drain_timeout_sec", s.DrainTimeoutSec},
	}
	for _, item := range nonNegative {
		if *item.val < 0 {
			return fmt.Errorf("%s 不能为负（当前 %d）：0 有明确含义（不等/不重试/不休眠/不排空）", item.name, *item.val)
		}
	}
	return nil
}

func (s *pipelineSection) convert() PipelineConf {
	ms := func(v *int) time.Duration { return time.Duration(*v) * time.Millisecond }
	sec := func(v *int) time.Duration { return time.Duration(*v) * time.Second }
	return PipelineConf{
		SyncBatch:        *s.SyncBatch,
		SyncLinger:       ms(s.SyncLingerMs),
		SyncConcurrency:  *s.SyncConcurrency,
		SyncRetries:      *s.SyncRetries,
		AssetBatch:       *s.AssetBatch,
		AssetLinger:      ms(s.AssetLingerMs),
		AssetConcurrency: *s.AssetConcurrency,
		AssetRetries:     *s.AssetRetries,
		AgentBatch:       *s.AgentBatch,
		AgentLinger:      ms(s.AgentLingerMs),
		AgentConcurrency: *s.AgentConcurrency,
		AgentRetries:     *s.AgentRetries,
		PerChatCapacity:  *s.PerChatCapacity,
		GlobalCapacity:   *s.GlobalCapacity,
		IdleTTL:          sec(s.IdleTTLSec),
		DrainTimeout:     sec(s.DrainTimeoutSec),
	}
}
