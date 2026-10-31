import { test, expect } from '@playwright/test'
import { debugReset, waitForLLMCalls } from './helpers'

// 人格维度：验证触发指令时，persona（身份/性格/关系/系统指令/情绪维度）
// 被完整渲染进 system_prompt，且 LLM 收到该人格。
// 可观测信号：debug/state → llm_calls[].system_prompt 含【人格】块。
test('persona：触发指令时 system_prompt 携带人格维度', async ({ request }) => {
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
    data: { group_id: groupId, content: '@锐评大师(test_self) 锐评下' },
  })

  const calls = await waitForLLMCalls(request, 1)
  const systemPrompt = calls[0].system_prompt
  const chatLog = calls[0].chat_log
  expect(systemPrompt).toBeTruthy()
  expect(chatLog).toBeTruthy()

  // 人格块拼在 systemPrompt 末尾；user 消息只放聊天记录与该指令提示词
  expect(systemPrompt).toContain('【人格】')
  expect(chatLog).not.toContain('【人格】')

  // persona 5 个 essence 字段必须进 system prompt
  expect(systemPrompt).toContain('身份：混迹网络社区多年的毒舌点评人')
  expect(systemPrompt).toContain('性格特质：')
  expect(systemPrompt).toContain('与群友的关系：')
  expect(systemPrompt).toContain('人格级系统指令：')
  expect(systemPrompt).toContain('情绪维度：')
  expect(systemPrompt).toContain('情绪反应性')
})
