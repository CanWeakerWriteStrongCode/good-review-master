package state

// gate.go：会话级的 Agent 开关（#启动 / #关闭）。
//
// 存在 settings 表里，不是 groups 的一列：
//
//   - 加列要 bump schemaVersion，而 migrate() 只支持全新库（ADR-0003），
//     代价是让用户删掉整个 state.db —— 历史消息一起没。
//   - settings 表的 key 本来就是开放集合，任何版本的库里都有这张表。
//   - 它不会被 ApplyRuntimeSettings 误读：那里只认 "pipeline" / "assets" 两个键，
//     多出来的键原样忽略。
//
// 为什么不用 groups.extra：SaveGroupInfo 会用事件里解析出的 extra 整体覆盖那一列，
// 开关写进去迟早被下一次群名同步冲掉。
//
// 三态靠"键在不在"表达：没有这个键 = 从未显式设置过，交给
// bot.agent_default_enabled 决定；有键 = 用户明确 #启动 或 #关闭 过。

import (
	"encoding/json"
	"strings"
)

// gateKeyPrefix 所有会话开关共用一个前缀，便于一次扫出本工作空间的全部开关。
const gateKeyPrefix = "group_gate:"

func groupGateKey(workspaceID, chatID string) string {
	return gateKeyPrefix + workspaceID + "|" + chatID
}

// SaveGroupGate 记录某会话的开关。写入走 SaveSetting（先 UPDATE 再 INSERT）。
func (s *Scope) SaveGroupGate(chatID string, enabled bool) error {
	if s == nil || s.store == nil || s.store.closed.Load() || chatID == "" {
		return nil
	}
	return s.store.SaveSetting(groupGateKey(s.workspaceID, chatID), enabled)
}

// LoadGroupGates 返回本工作空间所有显式设置过的会话开关（chatID → 开关）。
// 一条都没设置过时返回空 map，不是错误。
//
// 只按前缀扫本工作空间：同一个 state.db 里可能有多个账号，
// 不按 workspace_id 过滤会把别人的开关读到自己的群里。
func (s *Scope) LoadGroupGates() (map[string]bool, error) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return nil, nil
	}
	prefix := gateKeyPrefix + s.workspaceID + "|"
	var rows []Setting
	if err := s.store.db.Where("key LIKE ?", prefix+"%").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		chatID := strings.TrimPrefix(row.Key, prefix)
		if chatID == "" {
			continue
		}
		var enabled bool
		if err := json.Unmarshal([]byte(row.Value), &enabled); err != nil {
			// 值坏了就当没设置过：宁可回落到配置默认值，也不要把群卡在打不开的状态。
			s.store.noteFailure("解析会话开关", err)
			continue
		}
		out[chatID] = enabled
	}
	return out, nil
}
