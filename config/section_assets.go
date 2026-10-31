package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// assetsSection 资产打标签与后台补标签（config.yaml 的 assets 段）。
//
// 这里的 0 都有明确含义，用指针区分“没写”与“显式写 0”。
type assetsSection struct {
	BackfillEnabled     *bool `yaml:"backfill_enabled"`
	BackfillIntervalSec *int  `yaml:"backfill_interval_sec"`
	BackfillBatch       *int  `yaml:"backfill_batch"`
	BackfillMaxAttempts *int  `yaml:"backfill_max_attempts"`
	JobRetentionDays    *int  `yaml:"job_retention_days"`

	MessageBudgetSec *int `yaml:"message_budget_sec"`
	TagConcurrency   *int `yaml:"tag_concurrency"`

	DeferCacheDir      string `yaml:"defer_cache_dir"`
	DeferCacheMaxBytes *int64 `yaml:"defer_cache_max_bytes"`
	DeferCacheTTLDays  *int   `yaml:"defer_cache_ttl_days"`

	PromptVersion string `yaml:"prompt_version"`
}

const (
	defaultBackfillEnabled     = true
	defaultBackfillIntervalSec = 60
	defaultBackfillBatch       = 20
	defaultBackfillMaxAttempts = 3
	defaultJobRetentionDays    = 30
	defaultMessageBudgetSec    = 30
	defaultTagConcurrency      = 3
	defaultDeferCacheDir       = "asset-cache"
	defaultDeferCacheMaxBytes  = 256 << 20
	defaultDeferCacheTTLDays   = 7
	defaultPromptVersion       = "1"
)

func (s *assetsSection) Name() string { return "assets" }

func (s *assetsSection) Unmarshal(document *yaml.Node) error {
	return decodeSection(document, s.Name(), s)
}

func (s *assetsSection) SetDefaults() {
	if s.BackfillEnabled == nil {
		v := defaultBackfillEnabled
		s.BackfillEnabled = &v
	}
	set := func(target **int, value int) {
		if *target == nil {
			v := value
			*target = &v
		}
	}
	set(&s.BackfillIntervalSec, defaultBackfillIntervalSec)
	set(&s.BackfillBatch, defaultBackfillBatch)
	set(&s.BackfillMaxAttempts, defaultBackfillMaxAttempts)
	set(&s.JobRetentionDays, defaultJobRetentionDays)
	set(&s.MessageBudgetSec, defaultMessageBudgetSec)
	set(&s.TagConcurrency, defaultTagConcurrency)
	set(&s.DeferCacheTTLDays, defaultDeferCacheTTLDays)
	if s.DeferCacheMaxBytes == nil {
		v := int64(defaultDeferCacheMaxBytes)
		s.DeferCacheMaxBytes = &v
	}
	s.DeferCacheDir = strings.TrimSpace(s.DeferCacheDir)
	if s.DeferCacheDir == "" {
		s.DeferCacheDir = defaultDeferCacheDir
	}
	s.PromptVersion = strings.TrimSpace(s.PromptVersion)
	if s.PromptVersion == "" {
		s.PromptVersion = defaultPromptVersion
	}
}

func (s *assetsSection) Validate() error {
	if *s.BackfillBatch <= 0 {
		return positiveInt("assets.backfill_batch", *s.BackfillBatch, "一次补多少张欠条")
	}
	if *s.TagConcurrency <= 0 {
		return positiveInt("assets.tag_concurrency", *s.TagConcurrency, "同一时刻最多描述几张图")
	}
	for _, item := range []struct {
		name string
		val  *int
	}{
		{"assets.backfill_interval_sec", s.BackfillIntervalSec},
		{"assets.backfill_max_attempts", s.BackfillMaxAttempts},
		{"assets.job_retention_days", s.JobRetentionDays},
		{"assets.message_budget_sec", s.MessageBudgetSec},
		{"assets.defer_cache_ttl_days", s.DeferCacheTTLDays},
	} {
		if *item.val < 0 {
			return fmt.Errorf("%s 不能为负（当前 %d）：0 有明确含义（关闭/不重试/永久保留/不限制/不过期）",
				item.name, *item.val)
		}
	}
	if *s.DeferCacheMaxBytes < 0 {
		return fmt.Errorf("assets.defer_cache_max_bytes 不能为负（当前 %d）：0 表示不保存原图副本",
			*s.DeferCacheMaxBytes)
	}
	return nil
}

func (s *assetsSection) convert() AssetsConf {
	return AssetsConf{
		BackfillEnabled:     *s.BackfillEnabled,
		BackfillInterval:    time.Duration(*s.BackfillIntervalSec) * time.Second,
		BackfillBatch:       *s.BackfillBatch,
		BackfillMaxAttempts: *s.BackfillMaxAttempts,
		JobRetention:        time.Duration(*s.JobRetentionDays) * 24 * time.Hour,
		MessageBudget:       time.Duration(*s.MessageBudgetSec) * time.Second,
		TagConcurrency:      *s.TagConcurrency,
		DeferCacheDir:       s.DeferCacheDir,
		DeferCacheMaxBytes:  *s.DeferCacheMaxBytes,
		DeferCacheTTL:       time.Duration(*s.DeferCacheTTLDays) * 24 * time.Hour,
		PromptVersion:       s.PromptVersion,
	}
}
