import type { APIRequestContext } from '@playwright/test'

export const TEST_USER = 'admin'
export const TEST_PASS = '123456'

// login 并返回 JWT token
export async function loginToken(request: APIRequestContext): Promise<string> {
  const res = await request.post('/api/login', {
    data: { username: TEST_USER, password: TEST_PASS },
  })
  const body = await res.json()
  return body.data.token
}

// 带 token 的请求头
export function authHeaders(token: string): Record<string, string> {
  return { Authorization: `Bearer ${token}` }
}

// 清空服务端缓存/锚点/fake 状态（用例隔离）
export async function debugReset(request: APIRequestContext) {
  await request.post('/api/debug/reset')
}
