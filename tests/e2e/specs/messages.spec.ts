import { test, expect } from '@playwright/test'
import { loginToken, authHeaders, debugReset } from './helpers'

test.beforeEach(async ({ request }) => {
  await debugReset(request)
})

test('messages：注入的消息按序返回（昵称/内容）', async ({ request }) => {
  const token = await loginToken(request)
  await request.post('/api/debug/inject', {
    data: {
      group_id: '10001',
      messages: [
        { nick: '张三', content: '你好' },
        { nick: '李四', content: '在吗' },
      ],
    },
  })

  const res = await request.get('/api/groups/10001', { headers: authHeaders(token) })
  const body = await res.json()
  expect(body.code).toBe(200)
  expect(body.data.empty).toBe(false)
  expect(body.data.messages).toHaveLength(2)
  expect(body.data.messages[0].content).toBe('你好')
  expect(body.data.messages[0].nick).toBe('张三')
  expect(body.data.messages[1].content).toBe('在吗')
})

test('messages：未缓存的群返回空', async ({ request }) => {
  const token = await loginToken(request)
  const res = await request.get('/api/groups/99999', { headers: authHeaders(token) })
  const body = await res.json()
  expect(body.code).toBe(200)
  expect(body.data.empty).toBe(true)
  expect(body.data.messages).toEqual([])
})
