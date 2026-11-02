import type { AccountInfo, AlertItem, GroupInfo, BotStatus, Message, WorkspaceInfo, APIResponse } from './types'

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
  accounts: AccountInfo[]
  bot_info: BotStatus
}

export interface WorkspacesData {
  workspaces: WorkspaceInfo[]
  bot_info: BotStatus
  /** 带 ?with_groups=1 时才有：账号 id → 该账号的会话列表 */
  groups_by_workspace?: Record<string, GroupInfo[]>
}

export interface WorkspaceGroupsData {
  workspace: WorkspaceInfo
  groups: GroupInfo[]
  accounts: AccountInfo[]
  bot_info: BotStatus
}

/** 所有账号的会话平铺（「全部会话」视图） */
export interface SessionsData {
  sessions: GroupInfo[]
  bot_info: BotStatus
}

/** 最近的报警；total 是全库总条数，可能大于 alerts.length */
export interface AlertsData {
  alerts: AlertItem[]
  total: number
}

export interface MessagesData {
  group_id: string
  group_name: string
  workspace_id: string
  workspace_name: string
  self_id: string
  account_name: string
  /** 最近一批消息（后端封顶 500 条，时间升序） */
  messages: Message[]
  /** 库里这个群的消息总条数；messages 只给最近一部分 */
  total: number
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

// withGroups 为 true 时把每个账号的会话列表一并拉回来（groups_by_workspace）。
// 面板要同时显示账号汇总和账号下的群，带上它只需 1 个请求、每个账号查 1 次库。
export function fetchWorkspaces(withGroups = false): Promise<WorkspacesData> {
  return request<WorkspacesData>(withGroups ? '/workspaces?with_groups=1' : '/workspaces')
}

export function fetchWorkspaceGroups(workspaceId: string): Promise<WorkspaceGroupsData> {
  return request<WorkspaceGroupsData>(`/workspaces/${encodeURIComponent(workspaceId)}/groups`)
}

export function fetchSessions(): Promise<SessionsData> {
  return request<SessionsData>('/sessions')
}

export function fetchAlerts(limit = 50): Promise<AlertsData> {
  return request<AlertsData>(`/alerts?limit=${limit}`)
}

export function fetchWorkspaceMessages(workspaceId: string, groupId: string): Promise<MessagesData> {
  return request<MessagesData>(
    `/workspaces/${encodeURIComponent(workspaceId)}/groups/${encodeURIComponent(groupId)}`,
  )
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
