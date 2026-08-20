import { test, expect } from '@playwright/test'
import { debugReset, waitForLLMCalls } from './helpers'

// 人格维度：验证触发指令时，persona（身份/性格/说话风格/关系/开场白/系统指令/情绪维度/示例）
// 与授权指令被完整渲染进 system_prompt，且 LLM 收到该人格。
// 可观测信号：debug/state → llm_calls[].system_prompt 含【人格】块与「推演每个维度应有的变化」授权句。
test('persona：触发指令时 system_prompt 携带人格维度与授权指令', async ({ request }) => {
  await debugReset(request)
  const groupId = '10001'

  // 注入消息供锐评
  await request.post('/api/debug/inject', {
    data: {
      group_id: groupId,
      messages: Array.from({ length: 3 }, (_, i) => ({ nick: '张三', content: `消息${i}` })),
    },
  })

  // 触发 锐评下（fixture 里该指令带 persona）
  await request.post('/api/debug/trigger', {
    data: { group_id: groupId, content: '[CQ:at,qq=123456] 锐评下' },
  })

  const calls = await waitForLLMCalls(request, 1)
  const systemPrompt = calls[0].system_prompt
  const chatLog = calls[0].chat_log
  expect(systemPrompt).toBeTruthy()
  expect(chatLog).toBeTruthy()

  // 人格放在 user 消息末尾（systemPrompt 保持稳定，跨指令前缀一致 → 缓存命中）
  expect(systemPrompt).not.toContain('【人格】')

  // persona 5 个 essence 字段必须进 user 消息
  expect(chatLog).toContain('【人格】')
  expect(chatLog).toContain('身份：混迹网络社区多年的毒舌点评人')
  expect(chatLog).toContain('性格特质：')
  expect(chatLog).toContain('与群友的关系：')
  expect(chatLog).toContain('人格级系统指令：')
  expect(chatLog).toContain('情绪维度：')
  expect(chatLog).toContain('情绪反应性')

  // 授权指令（要求 LLM 推演各维度变化后再回答；用户情绪过于强烈时缓解情绪、不升级冲突）
  expect(chatLog).toContain('推演每个维度应有的变化')
  expect(chatLog).toContain('不要升级冲突')
  expect(chatLog).toContain('缓解对方情绪')
})
