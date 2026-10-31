import type { FullConfig } from '@playwright/test'

// 工作空间是收到第一条事件才开通的，而整套用例都依赖 /api/debug/* 指向唯一工作空间。
// 这里在 webServer 就绪后推一条 OB12 群消息，把 qq_official_bot_1906xxxx 建出来。
export default async function globalSetup(_config: FullConfig): Promise<void> {
  const body = JSON.stringify({
    id: 'e2e-warmup',
    time: Math.floor(Date.now() / 1000),
    type: 'message',
    detail_type: 'group',
    sub_type: 'normal',
    self: { platform: 'qq', user_id: 'unknown_selfid' },
    account_kind: 'official_bot',
    message_id: 'e2e-warmup',
    message: [{ type: 'text', data: { text: 'e2e warmup' } }],
    alt_message: 'e2e warmup',
    group_id: '10001',
    user_id: '1',
  })

  let lastError: unknown
  for (let attempt = 0; attempt < 50; attempt++) {
    try {
      const res = await fetch('http://127.0.0.1:18090/im/event/1906xxxx', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-OneBot-Version': '12' },
        body,
      })
      if (res.status === 204) return
      lastError = new Error(`warmup status ${res.status}`)
    } catch (err) {
      lastError = err
    }
    await new Promise((resolve) => setTimeout(resolve, 200))
  }
  throw new Error(`无法开通测试工作空间：${lastError}`)
}
