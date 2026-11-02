import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { i18n } from '@/locale'
import { getLangHeader, login, request } from '../index'

/**
 * 接口层那两条判断的守门测试。
 *
 * 这层的输出就是用户看到的那句弹窗：后端把文案翻好了，前端只负责原样抛。
 * 一旦有人在这里“顺手加个错误码分支”，或者把 msg 换成自己的固定文案，
 * 界面上就会出现“中文界面弹英文”或者“后端改了措辞前端没生效”。
 */

/** node 环境没有 localStorage，而 getAuthHeader / handleUnauthorized 都要用 */
function stubLocalStorage(seed: Record<string, string> = {}) {
  const store = new Map(Object.entries(seed))
  vi.stubGlobal('localStorage', {
    getItem: (key: string) => store.get(key) ?? null,
    setItem: (key: string, value: string) => void store.set(key, value),
    removeItem: (key: string) => void store.delete(key),
  })
  return store
}

/** 假的 uni：request 按用例回放响应，reLaunch 只记录被调了什么 */
function stubUni(handler: (opts: any) => any) {
  const req = vi.fn(handler)
  const reLaunch = vi.fn()
  vi.stubGlobal('uni', { request: req, reLaunch })
  return { req, reLaunch }
}

/** 一次成功的 HTTP 往返：statusCode 是传输层，data 是后端写的信封 */
function reply(statusCode: number, data: unknown) {
  return () => Promise.resolve({ statusCode, data })
}

describe('接口层：业务错误直接弹后端文案', () => {
  beforeEach(() => {
    stubLocalStorage()
    i18n.global.locale.value = 'zh-CN'
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('成功时拆掉信封，只返回 data', async () => {
    stubUni(reply(200, { code: 200, data: { bot_nickname: '锐评大师' } }))
    await expect(request('/status')).resolves.toEqual({ bot_nickname: '锐评大师' })
  })

  it('HTTP 200 + 业务码：抛出后端翻好的 msg，前端不自己拼文案', async () => {
    stubUni(reply(200, { code: 40101, msg: '账号或密码错误' }))
    await expect(request('/status')).rejects.toThrow('账号或密码错误')
  })

  it('换语言只是换个 X-Lang，前端代码一行不改', async () => {
    i18n.global.locale.value = 'en-US'
    expect(getLangHeader()).toEqual({ 'X-Lang': 'en-US' })

    const { req } = stubUni(reply(200, { code: 40101, msg: 'Incorrect username or password' }))
    await expect(request('/status')).rejects.toThrow('Incorrect username or password')
    expect(req.mock.calls[0][0].header['X-Lang']).toBe('en-US')
  })

  it('后端漏带 msg 时兜底成本地文案，而不是弹一个空框', async () => {
    stubUni(reply(200, { code: 60001 }))
    await expect(request('/status')).rejects.toThrow('未知错误，请稍后重试')
  })

  it('HTTP 401：清 token 并回登录页（这条是唯一还认状态码的地方）', async () => {
    const store = stubLocalStorage({ good_review_token: 'stale-token' })
    const { reLaunch } = stubUni(reply(401, {}))

    await expect(request('/status')).rejects.toThrow('未授权，请重新登录')
    expect(store.get('good_review_token')).toBeUndefined()
    expect(reLaunch).toHaveBeenCalledWith({ url: '/pages/login/index' })
  })

  it('200 但拿到的不是信封（反代回了 HTML）：不当成成功', async () => {
    stubUni(reply(200, '<html>502 Bad Gateway</html>'))
    await expect(request('/status')).rejects.toThrow('未知错误，请稍后重试')
  })

  it('请求没到服务器（断网/超时）：没有 msg 可弹，用本地网络文案', async () => {
    stubUni(() => Promise.reject(new Error('request:fail')))
    await expect(request('/status')).rejects.toThrow('网络连接失败，请检查网络后重试')
  })

  it('登录密码错也是业务码：抛后端的 msg，不是固定的“登录失败”', async () => {
    stubUni(reply(200, { code: 40101, msg: '账号或密码错误' }))
    await expect(login('admin', 'wrong')).rejects.toThrow('账号或密码错误')
  })
})
