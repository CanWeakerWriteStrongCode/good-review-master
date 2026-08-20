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
  expect(systemPrompt).toBeTruthy()

  // persona 全字段必须进 system prompt
  expect(systemPrompt).toContain('【人格】')
  expect(systemPrompt).toContain('身份：混迹网络社区多年的毒舌点评人')
  expect(systemPrompt).toContain('性格特质：')
  expect(systemPrompt).toContain('说话风格：')
  expect(systemPrompt).toContain('与群友的关系：')
  expect(systemPrompt).toContain('开场白：')
  expect(systemPrompt).toContain('人格级系统指令：')
  expect(systemPrompt).toContain('情绪维度：')
  expect(systemPrompt).toContain('情绪反应性')
  expect(systemPrompt).toContain('示例对话：')

  // 授权指令（明确要求 LLM 推演各维度变化后再回答）
  expect(systemPrompt).toContain('推演每个维度应有的变化')
})
