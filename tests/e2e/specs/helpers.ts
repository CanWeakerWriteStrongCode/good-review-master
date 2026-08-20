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

// 清空服务端缓存/锚点/fake 状态（用例隔离）。
// 说明：debug/trigger 现在会同步等待 handler 收尾（锚点更新）才返回（见 web/server/debug.go），
// 因此异步副作用不会跨用例泄漏，这里只需简单清空即可。
export async function debugReset(request: APIRequestContext) {
  await request.post('/api/debug/reset')
}

// 读取 debug 状态快照（缓存/锚点/FakeLLM 调用）
export async function debugState(request: APIRequestContext): Promise<any> {
  const res = await request.get('/api/debug/state')
  return (await res.json()).data
}

// 轮询直到 FakeLLM 调用数达到 count（trigger 的 handler 异步执行），返回全部调用
export async function waitForLLMCalls(
  request: APIRequestContext,
  count: number,
  timeoutMs = 10_000,
): Promise<any[]> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const state = await debugState(request)
    const calls = state.llm_calls || []
    if (calls.length >= count) return calls
    await new Promise((r) => setTimeout(r, 200))
  }
  throw new Error(`等待 FakeLLM 调用超时：期望至少 ${count} 次`)
}

// 轮询直到群锚点满足 predicate（SetLLMAnchor 在 Review 返回后异步保存），返回该锚点。
// 必须先等锚点落库再注入/触发下一次，否则读到的空锚点会被误判为「无锚点→重置」。
export async function waitForAnchor(
  request: APIRequestContext,
  groupId: string,
  predicate: (anchor: any) => boolean,
  timeoutMs = 10_000,
): Promise<any> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const state = await debugState(request)
    const anchor = (state.anchors || {})[groupId]
    if (anchor && predicate(anchor)) return anchor
    await new Promise((r) => setTimeout(r, 200))
  }
  throw new Error(`等待群 ${groupId} 锚点超时`)
}
