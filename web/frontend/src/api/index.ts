import type { GroupInfo, BotStatus, Message, APIResponse } from './types'

export const BASE_URL = '/api'

export function getAuthHeader(): Record<string, string> {
  const token = localStorage.getItem('good_review_token')
  if (token) {
    return { Authorization: `Bearer ${token}` }
  }
  return {}
}

/**
 * 401 的统一出口：清 token 并回登录页。
 *
 * 抽出来是因为还有第二个调用面——pprof 那几个端点返回的不是 {code,data} 信封
 * （有的是明文、有的是二进制），走不了 request<T>，只能自己调 uni.request。
 * 那种地方必须复用这里，否则 token 过期后不同入口的行为会不一致。
 */
export function handleUnauthorized(): never {
  localStorage.removeItem('good_review_token')
  uni.reLaunch({ url: '/pages/login/index' })
  throw new Error('未授权，请重新登录')
}

/** 发一个 GET 并拆掉 {code,data} 信封。非信封响应（pprof）见 diagnostics.ts。 */
export async function request<T>(url: string): Promise<T> {
  const res = await uni.request({
    url: BASE_URL + url,
    method: 'GET',
    header: getAuthHeader(),
  })
  const body = res.data as APIResponse<T>
  if (body.code === 401) {
    handleUnauthorized()
  }
  if (body.code !== 200) {
    throw new Error(`API error: ${res.statusCode}`)
  }
  return body.data
}

export interface GroupsData {
  groups: GroupInfo[]
  bot_info: BotStatus
}

export interface MessagesData {
  group_id: string
  group_name: string
  messages: Message[]
  empty: boolean
}

export interface LoginResult {
  token?: string
}

export function fetchStatus(): Promise<BotStatus> {
  return request<BotStatus>('/status')
}

export function fetchGroups(): Promise<GroupsData> {
  return request<GroupsData>('/groups')
}

export function fetchMessages(groupId: string): Promise<MessagesData> {
  return request<MessagesData>(`/groups/${groupId}`)
}

export async function login(username: string, password: string): Promise<LoginResult> {
  const res = await uni.request({
    url: BASE_URL + '/login',
    method: 'POST',
    header: { 'Content-Type': 'application/json' },
    data: { username, password },
  })
  const body = res.data as APIResponse<LoginResult>
  if (body.code !== 200) {
    throw new Error((body.data as any)?.msg || '登录失败')
  }
  return body.data
}

export async function logout() {
  await uni.request({
    url: BASE_URL + '/logout',
    method: 'POST',
    header: getAuthHeader(),
  })
}
