import { test, expect } from '@playwright/test'
import { authHeaders, loginToken } from './helpers'

// 运行时诊断接口。这组用例的核心是**鉴权边界**：
// 用户明确要求"pprof 之类的检测必须登录才能看"，所以无 token 必须一律 401。

test('diagnostics：无 token 返回 401', async ({ request }) => {
  const res = await request.get('/api/diagnostics')
  expect(res.status()).toBe(401)
  expect((await res.json()).code).toBe(401)
})

test('diagnostics：有 token 返回运行时概况与 goroutine 分组', async ({ request }) => {
  const token = await loginToken(request)
  const res = await request.get('/api/diagnostics', { headers: authHeaders(token) })
  expect(res.status()).toBe(200)

  const body = await res.json()
  expect(body.code).toBe(200)

  const runtime = body.data.runtime
  expect(runtime.goroutines).toBeGreaterThan(0)
  expect(runtime.go_version).toMatch(/^go1\./)
  expect(runtime.num_cpu).toBeGreaterThan(0)
  expect(typeof runtime.uptime_seconds).toBe('number')
  // 夹具里写了 enable_pprof: true，前端靠这个字段决定要不要显示 profile 区块
  expect(runtime.pprof_enabled).toBe(true)

  const goroutines = body.data.goroutines
  expect(goroutines.total).toBeGreaterThan(0)
  expect(goroutines.group_count).toBe(goroutines.groups.length)
  expect(goroutines.groups.length).toBeGreaterThan(0)

  // 分组要能看穿运行时帧：每个分类键都得是真函数名，不能是 runtime.gopark
  for (const group of goroutines.groups) {
    expect(group.frame).not.toMatch(/^runtime\./)
    expect(group.frame).not.toMatch(/^sync\./)
    expect(group.count).toBeGreaterThan(0)
  }
  // 降序
  for (let i = 1; i < goroutines.groups.length; i++) {
    expect(goroutines.groups[i - 1].count).toBeGreaterThanOrEqual(goroutines.groups[i].count)
  }

  // 不带 full 时不该回传几 MB 的栈
  expect(goroutines.stack_dump).toBeUndefined()
})

test('diagnostics：full=1 才返回栈全文', async ({ request }) => {
  const token = await loginToken(request)
  const res = await request.get('/api/diagnostics?full=1', { headers: authHeaders(token) })
  const body = await res.json()

  const dump = body.data.goroutines.stack_dump
  expect(typeof dump).toBe('string')
  expect(dump).toContain('goroutine ')
  // 服务端解析出的分组数应与全文对得上
  const headers = dump.split('\n').filter((line: string) => /^goroutine \d+ \[/.test(line))
  expect(headers.length).toBe(body.data.goroutines.total)
})

test('pprof 文本视图：无 token 401，有 token 返回明文', async ({ request }) => {
  const unauth = await request.get('/api/debug/pprof/goroutine?debug=1')
  expect(unauth.status()).toBe(401)

  const token = await loginToken(request)
  const res = await request.get('/api/debug/pprof/goroutine?debug=1', {
    headers: authHeaders(token),
  })
  expect(res.status()).toBe(200)
  expect(res.headers()['content-type']).toContain('text/plain')
  expect(await res.text()).toContain('goroutine')
})

// 下载链路：前端靠这两个响应头才知道拿到的是二进制附件
test('pprof 二进制 profile：无 token 401，有 token 返回附件', async ({ request }) => {
  const unauth = await request.get('/api/debug/pprof/heap')
  expect(unauth.status()).toBe(401)

  const token = await loginToken(request)
  const res = await request.get('/api/debug/pprof/heap', { headers: authHeaders(token) })
  expect(res.status()).toBe(200)
  expect(res.headers()['content-type']).toContain('application/octet-stream')
  expect(res.headers()['content-disposition']).toContain('attachment')
  // 二进制 profile 非空；有内容才谈得上能被 go tool pprof 解析
  expect((await res.body()).length).toBeGreaterThan(0)
})
