import { test, expect } from '@playwright/test'
import { TEST_USER, TEST_PASS, loginToken } from './helpers'

test('登录：错误密码走业务码 40101（HTTP 仍 200）', async ({ request }) => {
  const res = await request.post('/api/login', {
    data: { username: TEST_USER, password: 'wrong-password' },
  })
  // 401 专留给“登录态失效”：密码输错是业务判断，用 401 会让前端
  // 把刚输错一次密码的人直接踢回登录页
  expect(res.status()).toBe(200)
  const body = await res.json()
  expect(body.code).toBe(40101)
  // 文案由后端翻好，前端直接弹
  expect(body.msg).toBe('账号或密码错误')
})

test('登录：带 X-Lang 时文案换成该语言', async ({ request }) => {
  const res = await request.post('/api/login', {
    data: { username: TEST_USER, password: 'wrong-password' },
    headers: { 'X-Lang': 'en-US' },
  })
  const body = await res.json()
  expect(body.code).toBe(40101)
  expect(body.msg).toBe('Incorrect username or password')
})

test('登录：正确账号密码返回 token', async ({ request }) => {
  const res = await request.post('/api/login', {
    data: { username: TEST_USER, password: TEST_PASS },
  })
  expect(res.status()).toBe(200)
  const body = await res.json()
  expect(body.code).toBe(200)
  expect(typeof body.data.token).toBe('string')
  expect(body.data.token.length).toBeGreaterThan(0)
})

test('status：无 token 401，有 token 返回机器人信息', async ({ request }) => {
  const unauth = await request.get('/api/status')
  expect(unauth.status()).toBe(401)
  expect((await unauth.json()).code).toBe(40100)

  const token = await loginToken(request)
  const res = await request.get('/api/status', { headers: { Authorization: `Bearer ${token}` } })
  const body = await res.json()
  expect(body.code).toBe(200)
  expect(typeof body.data.bot_nickname).toBe('string')
  expect(body.data.allow_all_groups).toBe(false)
  // group_count 是库里有记录的群数，跟着群列表走；这里不写死数字，
  // 只验证两个接口算的是同一件事。
  const groupsRes = await request.get('/api/groups', { headers: { Authorization: `Bearer ${token}` } })
  const groupsBody = await groupsRes.json()
  expect(body.data.group_count).toBe(groupsBody.data.groups.length)
})
