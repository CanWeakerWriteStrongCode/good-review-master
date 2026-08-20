import { test, expect } from '@playwright/test'
import { debugReset, waitForLLMCalls, waitForAnchor } from './helpers'

// 缓存命中成本：验证「按扩展/重置成本对比选择发送窗口」的决策真实生效。
// 可观测信号：
//   - 锚点（debug/state → anchors）：扩展 = Start 不变、LastSent 前移；重置 = Start 跳到新窗口首条。
//   - chat_log（debug/state → llm_calls[].chat_log）：扩展窗口 = msgs[anchorIndex:]（旧+新），
//     重置窗口 = 最近 llm_send_count=3 条（截断）——两路径发送范围不同，内容可区分。
// msg_id 由 debug inject 自动分配，从 1000000 递增。
test('cache-cost：无锚点走重置，锚点命中走扩展（Start 不变、窗口含旧+新）', async ({ request }) => {
  await debugReset(request)
  const groupId = '10001'

  // 1. 注入 5 条（id 1000001..1000005），超过重置窗口 llm_send_count=3
  await request.post('/api/debug/inject', {
    data: {
      group_id: groupId,
      messages: Array.from({ length: 5 }, (_, i) => ({ nick: '张三', content: `第一条消息${i}` })),
    },
  })

  // 2. 第一次触发：无锚点 → 重置，窗口 = 最近 3 条 = msgs[2:5]（第一条消息2/3/4）
  await request.post('/api/debug/trigger', {
    data: { group_id: groupId, content: '[CQ:at,qq=123456] 锐评下' },
  })
  const callsAfterFirst = await waitForLLMCalls(request, 1)
  const firstCall = callsAfterFirst[0]
  expect(firstCall.chat_log).toContain('第一条消息2')
  expect(firstCall.chat_log).toContain('第一条消息4')
  expect(firstCall.chat_log).not.toContain('第一条消息0')

  // 3. 读取重置后的锚点（trigger 已同步等待 handler 收尾，锚点已就绪）
  const firstAnchor = await waitForAnchor(request, groupId, () => true)

  // 4. 注入 2 条新消息（id 1000006..1000007）
  await request.post('/api/debug/inject', {
    data: {
      group_id: groupId,
      messages: Array.from({ length: 2 }, (_, i) => ({ nick: '李四', content: `新消息${i}` })),
    },
  })

  // 5. 第二次触发：锚点仍在缓存、扩展成本（命中0.033×旧窗口+1.0×新增）≪ 重置成本 → 扩展
  await request.post('/api/debug/trigger', {
    data: { group_id: groupId, content: '[CQ:at,qq=123456] 锐评下' },
  })
  const callsAfterSecond = await waitForLLMCalls(request, 2)
  const secondCall = callsAfterSecond[1]
  // 扩展核心证据：重置只发最近 3 条 = [4,5,6] 不含 第一条消息2；扩展窗口从锚点起保留旧窗口
  expect(secondCall.chat_log).toContain('第一条消息2')
  // 新增消息随窗口一起发送
  expect(secondCall.chat_log).toContain('新消息1')
  // 窗口从锚点开始，未回溯到最早
  expect(secondCall.chat_log).not.toContain('第一条消息0')

  // 6. 锚点：Start 不变 = 命中前缀复用（重置则 Start 会跳到新窗口首条）；LastSent 恰好前进注入的 2 条
  const secondAnchor = await waitForAnchor(request, groupId, (a) => a.LastSent > firstAnchor.LastSent)
  expect(secondAnchor.Start).toBe(firstAnchor.Start)
  expect(secondAnchor.LastSent - firstAnchor.LastSent).toBe(2)
})
