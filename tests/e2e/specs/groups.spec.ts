import { test, expect } from '@playwright/test'
import { loginToken, authHeaders, debugReset } from './helpers'

test.beforeEach(async ({ request }) => {
  await debugReset(request)
})

test('groups：注入后群显示已缓存 + 消息数 + 群名，未注入群显示无数据', async ({ request }) => {
  const token = await loginToken(request)
  await request.post('/api/debug/inject', {
    data: {
      group_id: '10001',
      group_name: '测试群A',
      messages: [
        { nick: '张三', content: '第一条' },
        { nick: '李四', content: '第二条' },
      ],
    },
  })

  const res = await request.get('/api/groups', { headers: authHeaders(token) })
  const body = await res.json()
  expect(body.code).toBe(200)

  const groups: any[] = body.data.groups
  const g1 = groups.find((g) => g.group_id === '10001')
  expect(g1).toBeTruthy()
  expect(g1.cached).toBe(true)
  expect(g1.message_count).toBe(2)
  expect(g1.group_name).toBe('测试群A')

  const g2 = groups.find((g) => g.group_id === '10002')
  expect(g2.cached).toBe(false)

  expect(body.data.bot_info.group_count).toBe(2)
})
