import { test, expect } from '@playwright/test'
import { debugReset, waitForLLMCalls } from './helpers'

test('bot-flow：注入消息后触发锐评，FakeLLM 被调用并返回固定回复', async ({ request }) => {
  await debugReset(request)
  await request.post('/api/debug/inject', {
    data: { group_id: '10001', messages: [{ nick: '张三', content: '今天好开心' }] },
  })

  const triggerRes = await request.post('/api/debug/trigger', {
    data: { group_id: '10001', content: '[CQ:at,qq=123456] 锐评下' },
  })
  expect(triggerRes.status()).toBe(200)

  // 轮询 debug state 直到 FakeLLM 被调用（handler 异步执行）
  const calls = await waitForLLMCalls(request, 1)

  expect(calls[0].reply).toBe('【测试回复】锐评完毕')
  // chat_log = 发给 LLM 的完整 userMsg，应包含注入的群聊内容与固定前缀
  expect(calls[0].chat_log).toContain('今天好开心')
  expect(calls[0].chat_log).toContain('以下是群聊记录')
})
