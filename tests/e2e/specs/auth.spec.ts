import { test, expect } from '@playwright/test'
import { TEST_USER, TEST_PASS, loginToken } from './helpers'

test('登录：错误密码返回 401', async ({ request }) => {
  const res = await request.post('/api/login', {
    data: { username: TEST_USER, password: 'wrong-password' },
  })
  expect(res.status()).toBe(200)
  const body = await res.json()
  expect(body.code).toBe(401)
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
  expect((await unauth.json()).code).toBe(401)

  const token = await loginToken(request)
  const res = await request.get('/api/status', { headers: { Authorization: `Bearer ${token}` } })
  const body = await res.json()
  expect(body.code).toBe(200)
  expect(body.data.bot_qq).toBe('123456')
  expect(body.data.group_count).toBe(2)
})
