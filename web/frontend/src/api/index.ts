import type { GroupInfo, BotStatus, Message, APIResponse } from './types'

const BASE_URL = '/api'

function getAuthHeader(): Record<string, string> {
  const token = localStorage.getItem('good_review_token')
  if (token) {
    return { Authorization: `Bearer ${token}` }
  }
  return {}
}

async function request<T>(url: string): Promise<T> {
  const res = await uni.request({
    url: BASE_URL + url,
    method: 'GET',
    header: getAuthHeader(),
  })
  const body = res.data as APIResponse<T>
  if (body.code === 401) {
    // token 失效，跳转登录
    localStorage.removeItem('good_review_token')
    uni.reLaunch({ url: '/pages/login/index' })
    throw new Error('未授权，请重新登录')
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
  messages: Message[]
  empty: boolean
}

export interface LoginResult {
  token?: string
  need_password?: boolean
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
