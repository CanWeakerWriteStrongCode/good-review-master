import { test, expect } from '@playwright/test'
import { loginToken, authHeaders, debugReset } from './helpers'

test.beforeEach(async ({ request }) => {
  await debugReset(request)
})

test('groups：注入后群显示有记录 + 消息数 + 群名，别的账号的群不出现', async ({ request }) => {
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
  expect(g1.has_messages).toBe(true)
  expect(g1.message_count).toBe(2)
  expect(g1.group_name).toBe('测试群A')

  // 10002 写在全局 allow_groups 里，但这个账号库里没有它的记录，
  // 说明它是另一个账号看的群（group_openid 按机器人分配），不该出现。
  expect(groups.find((g) => g.group_id === '10002')).toBeFalsy()
  expect(groups).toHaveLength(1)
  expect(body.data.bot_info.group_count).toBe(1)
})
