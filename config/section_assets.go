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
	BackfillEnabled         *bool `yaml:"backfill_enabled"`
	BackfillIntervalSec     *int  `yaml:"backfill_interval_sec"`
	BackfillBatch           *int  `yaml:"backfill_batch"`
	BackfillMaxAttempts     *int  `yaml:"backfill_max_attempts"`
	BackfillClaimTimeoutSec *int  `yaml:"backfill_claim_timeout_sec"`
	JobRetentionDays        *int  `yaml:"job_retention_days"`

	MessageBudgetSec *int `yaml:"message_budget_sec"`
	TagConcurrency   *int `yaml:"tag_concurrency"`

	PromptVersion string `yaml:"prompt_version"`
}

const (
	defaultBackfillEnabled     = true
	defaultBackfillIntervalSec = 60
	defaultBackfillBatch       = 20
	defaultBackfillMaxAttempts = 3
	// 10 分钟：单条最坏耗时是取图 30 秒 + 一次图片描述超时，留足余量才不会被
	// 还在干活的副本抢走。它不决定“多久重试”，只决定“崩溃后多久有人接手”。
	defaultBackfillClaimTimeoutSec = 600
	defaultJobRetentionDays        = 30
	defaultMessageBudgetSec        = 30
	defaultTagConcurrency          = 3
	defaultPromptVersion           = "1"
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
	set(&s.BackfillClaimTimeoutSec, defaultBackfillClaimTimeoutSec)
	set(&s.JobRetentionDays, defaultJobRetentionDays)
	set(&s.MessageBudgetSec, defaultMessageBudgetSec)
	set(&s.TagConcurrency, defaultTagConcurrency)
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
	// 0 会让租约永不过期：进程崩在一次领取中途，那条欠条就永远停在 running 上。
	if *s.BackfillClaimTimeoutSec <= 0 {
		return positiveInt("assets.backfill_claim_timeout_sec", *s.BackfillClaimTimeoutSec, "领走后多久没动静算那个进程没了（秒）")
	}
	for _, item := range []struct {
		name string
		val  *int
	}{
		{"assets.backfill_interval_sec", s.BackfillIntervalSec},
		{"assets.backfill_max_attempts", s.BackfillMaxAttempts},
		{"assets.job_retention_days", s.JobRetentionDays},
		{"assets.message_budget_sec", s.MessageBudgetSec},
	} {
		if *item.val < 0 {
			return fmt.Errorf("%s 不能为负（当前 %d）：0 有明确含义（关闭/不重试/永久保留/不限制/不过期）",
				item.name, *item.val)
		}
	}
	return nil
}

func (s *assetsSection) convert() AssetsConf {
	return AssetsConf{
		BackfillEnabled:      *s.BackfillEnabled,
		BackfillInterval:     time.Duration(*s.BackfillIntervalSec) * time.Second,
		BackfillBatch:        *s.BackfillBatch,
		BackfillMaxAttempts:  *s.BackfillMaxAttempts,
		BackfillClaimTimeout: time.Duration(*s.BackfillClaimTimeoutSec) * time.Second,
		JobRetention:         time.Duration(*s.JobRetentionDays) * 24 * time.Hour,
		MessageBudget:        time.Duration(*s.MessageBudgetSec) * time.Second,
		TagConcurrency:       *s.TagConcurrency,
		PromptVersion:        s.PromptVersion,
	}
}
