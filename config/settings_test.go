package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestApplyRuntimeSettings覆盖与显式零(t *testing.T) {
	base := &Config{
		Pipeline: PipelineConf{
			SyncBatch: 20, SyncLinger: 20 * time.Millisecond, SyncConcurrency: 4,
			AgentRetries: 3, IdleTTL: 10 * time.Minute, DrainTimeout: 10 * time.Second,
		},
		Assets: AssetsConf{
			BackfillEnabled: true, BackfillInterval: 60 * time.Second,
			TagConcurrency: 3, MessageBudget: 30 * time.Second,
		},
	}
	raw := map[string]json.RawMessage{
		"pipeline": json.RawMessage(`{"sync_batch":7,"sync_linger_ms":0,"sync_concurrency":-1,"agent_retries":0,"idle_ttl_sec":0}`),
		"assets":   json.RawMessage(`{"backfill_enabled":false,"message_budget_sec":0,"tag_concurrency":5}`),
	}
	merged, err := ApplyRuntimeSettings(base, raw)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Pipeline.SyncBatch != 7 {
		t.Fatalf("sync_batch = %d", merged.Pipeline.SyncBatch)
	}
	if merged.Pipeline.SyncLinger != 0 {
		t.Fatalf("显式 0 应生效，实际 %v", merged.Pipeline.SyncLinger)
	}
	if merged.Pipeline.SyncConcurrency != 4 {
		t.Fatalf("负数应被忽略，实际 %d", merged.Pipeline.SyncConcurrency)
	}
	if merged.Pipeline.AgentRetries != 0 || merged.Pipeline.IdleTTL != 0 {
		t.Fatalf("显式 0 应生效：%+v", merged.Pipeline)
	}
	if merged.Pipeline.DrainTimeout != 10*time.Second {
		t.Fatalf("未覆盖的值应保留：%v", merged.Pipeline.DrainTimeout)
	}
	if merged.Assets.BackfillEnabled || merged.Assets.MessageBudget != 0 || merged.Assets.TagConcurrency != 5 {
		t.Fatalf("assets = %+v", merged.Assets)
	}
	if base.Pipeline.SyncBatch != 20 || base.Assets.BackfillEnabled != true {
		t.Fatal("ApplyRuntimeSettings 不能修改基础配置")
	}
}

func TestApplyRuntimeSettings非法JSON保留文件值(t *testing.T) {
	base := &Config{Pipeline: PipelineConf{SyncBatch: 20}}
	merged, err := ApplyRuntimeSettings(base, map[string]json.RawMessage{
		"pipeline": json.RawMessage(`not-json`),
	})
	if err == nil {
		t.Fatal("应返回解析错误")
	}
	if merged.Pipeline.SyncBatch != 20 {
		t.Fatalf("解析失败应保留原值，实际 %d", merged.Pipeline.SyncBatch)
	}
}
