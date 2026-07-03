import type { GroupInfo, BotStatus, Message, APIResponse } from './types'

const BASE_URL = '/api'

async function request<T>(url: string): Promise<T> {
  const res = await uni.request({
    url: BASE_URL + url,
    method: 'GET',
  })
  const body = res.data as APIResponse<T>
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

export function fetchStatus(): Promise<BotStatus> {
  return request<BotStatus>('/status')
}

export function fetchGroups(): Promise<GroupsData> {
  return request<GroupsData>('/groups')
}

export function fetchMessages(groupId: string): Promise<MessagesData> {
  return request<MessagesData>(`/groups/${groupId}`)
}
