import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  fetchGroups,
  fetchMessages,
  fetchSessions,
  fetchStatus,
  fetchWorkspaces,
  fetchWorkspaceGroups,
  fetchWorkspaceMessages,
} from '@/api'
import type { AccountInfo, GroupInfo, BotStatus, Message, WorkspaceInfo } from '@/api/types'

/** 群列表的两种看法：按账号分、所有账号的会话平铺 */
export type ViewMode = 'account' | 'all'

/** 群列表按账号分组后的一项 */
export interface AccountSection extends AccountInfo {
  displayName: string
  groups: GroupInfo[]
}

// 会话标题：群聊直接用群号（有群名时外面会优先用群名），
// 私聊在库里的键是 private:<openid>，直接显示没人看得懂。
// 后端现在会给出 type（groups 表的会话类型），有就用它；
// 消息页拿不到 type，退回看前缀，两边结果一致。
export function sessionLabel(chatID: string, type?: string): string {
  const prefix = 'private:'
  const isPrivate = type === 'private' || chatID.startsWith(prefix)
  if (!isPrivate) return chatID
  return `私聊 ${chatID.slice(prefix.length, prefix.length + 8)}`
}

// 账号显示名：昵称优先；占位值是适配器还没学到身份，显示「未知账号」。
function accountLabel(selfID: string, nickname: string): string {
  if (nickname) return nickname
  if (!selfID) return '未归属账号'
  if (selfID === 'qq_official' || selfID === 'unknown_selfid') return '未知账号'
  return selfID
}

export const useGroupsStore = defineStore('groups', () => {
  const botStatus = ref<BotStatus | null>(null)
  const groups = ref<GroupInfo[]>([])
  const accounts = ref<AccountInfo[]>([])
  const workspaces = ref<WorkspaceInfo[]>([])
  const selectedWorkspaceId = ref('')
  const loading = ref(false)
  /** 当前看的是「按账号」还是「全部会话」 */
  const viewMode = ref<ViewMode>('account')
  /** 全部会话视图的数据：所有账号的群/私聊，已按最近消息时间倒序 */
  const sessions = ref<GroupInfo[]>([])

  const selectedWorkspace = computed<WorkspaceInfo | null>(() => {
    return workspaces.value.find((w) => w.id === selectedWorkspaceId.value) || null
  })

  const accountSections = computed<AccountSection[]>(() => {
    // 一个页面只对应一个工作空间，群全部挂在这个账号下。
    // 不能按 group.self_id 分组：那是群作用域的机器人 openid，账号 ID 是
    // qq_official_bot_<AppID>，两者不是一个东西；拿它当 key 会把一个账号的群
    // 拆成一堆重复卡片，库里没记到的群还会落到「未归属账号」。
    let section: AccountSection | null = null
    if (accounts.value.length > 0) {
      const acc = accounts.value[0]
      section = {
        ...acc,
        displayName: accountLabel(acc.self_id, acc.nickname),
        groups: [...groups.value],
      }
    } else if (groups.value.length > 0) {
      // 没有账号汇总（工作空间还没学到接收账号）时只有群，用群上的信息兜底。
      const first = groups.value[0]
      section = {
        self_id: first.self_id || '',
        nickname: first.account_name || '',
        group_count: 0,
        active_group_count: 0,
        message_count: 0,
        last_activity: '',
        displayName: accountLabel(first.self_id || '', first.account_name || ''),
        groups: [...groups.value],
      }
    }
    if (!section) return []
    section.group_count = section.groups.length
    return [section]
  })

  async function loadStatus() {
    try {
      botStatus.value = await fetchStatus()
    } catch (e) {
      console.error('加载机器人状态失败', e)
    }
  }

  async function loadGroups() {
    loading.value = true
    try {
      const data = await fetchWorkspaces()
      botStatus.value = data.bot_info
      workspaces.value = data.workspaces || []
      if (workspaces.value.length > 0) {
        // 停在「全部会话」时刷新要回到同一个视图，不能把用户拽回按账号。
        if (viewMode.value === 'all') {
          await refreshSessions()
          return
        }
        const keep = workspaces.value.some((w) => w.id === selectedWorkspaceId.value)
        await loadWorkspace(keep ? selectedWorkspaceId.value : workspaces.value[0].id)
        return
      }
      // 后端未启用工作空间（旧版本）：退回全局接口
      const legacy = await fetchGroups()
      botStatus.value = legacy.bot_info
      groups.value = legacy.groups
      accounts.value = legacy.accounts || []
    } catch (e) {
      console.error('加载群列表失败', e)
    } finally {
      loading.value = false
    }
  }

  // refreshSessions 拉一次全部会话，不碰 loading（调用方负责）。
  async function refreshSessions() {
    const data = await fetchSessions()
    botStatus.value = data.bot_info
    sessions.value = data.sessions || []
  }

  async function loadSessions() {
    loading.value = true
    try {
      await refreshSessions()
    } catch (e) {
      console.error('加载会话列表失败', e)
    } finally {
      loading.value = false
    }
  }

  // switchMode 在「按账号」与「全部会话」之间切。
  // 切回按账号时不能走 selectWorkspace：它在 id 没变时直接 return，
  // 从全部会话切回来就会留一屏旧数据。
  async function switchMode(mode: ViewMode) {
    if (viewMode.value === mode) return
    viewMode.value = mode
    loading.value = true
    try {
      if (mode === 'all') {
        await refreshSessions()
        return
      }
      const id = selectedWorkspaceId.value || workspaces.value[0]?.id
      if (id) await loadWorkspace(id)
    } catch (e) {
      console.error('切换视图失败', e)
    } finally {
      loading.value = false
    }
  }

  async function loadWorkspace(id: string) {
    const data = await fetchWorkspaceGroups(id)
    selectedWorkspaceId.value = id
    botStatus.value = data.bot_info
    groups.value = data.groups
    accounts.value = data.accounts || []
    const index = workspaces.value.findIndex((w) => w.id === id)
    if (index >= 0) workspaces.value[index] = data.workspace
  }

  async function selectWorkspace(id: string) {
    if (id === selectedWorkspaceId.value) return
    loading.value = true
    try {
      await loadWorkspace(id)
    } catch (e) {
      console.error('切换工作空间失败', e)
    } finally {
      loading.value = false
    }
  }

  function formatTime(ts: number): string {
    if (!ts) return '-'
    const d = new Date(ts * 1000)
    const pad = (n: number) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
  }

  return {
    botStatus,
    groups,
    accounts,
    workspaces,
    selectedWorkspaceId,
    selectedWorkspace,
    accountSections,
    loading,
    viewMode,
    sessions,
    loadStatus,
    loadGroups,
    loadSessions,
    selectWorkspace,
    switchMode,
    formatTime,
  }
})

export const useMessagesStore = defineStore('messages', () => {
  const messages = ref<Message[]>([])
  const groupId = ref('')
  const groupName = ref('')
  const workspaceId = ref('')
  const workspaceName = ref('')
  const selfId = ref('')
  const accountName = ref('')
  const loading = ref(false)
  const empty = ref(true)
  /** 库里这个群的消息总条数；messages 只有最近一批 */
  const total = ref(0)

  async function loadMessages(wid: string, id: string) {
    groupId.value = id
    workspaceId.value = wid
    loading.value = true
    try {
      const data = wid
        ? await fetchWorkspaceMessages(wid, id)
        : await fetchMessages(id)
      messages.value = data.messages
      empty.value = data.empty
      total.value = data.total ?? data.messages.length
      groupName.value = data.group_name || ''
      workspaceId.value = data.workspace_id || wid
      workspaceName.value = data.workspace_name || ''
      selfId.value = data.self_id || ''
      accountName.value = data.account_name || ''
    } catch (e) {
      console.error('加载消息失败', e)
    } finally {
      loading.value = false
    }
  }

  function formatTime(ts: number): string {
    if (!ts) return '-'
    const d = new Date(ts * 1000)
    const pad = (n: number) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
  }

  return {
    messages,
    groupId,
    groupName,
    workspaceId,
    workspaceName,
    selfId,
    accountName,
    loading,
    empty,
    total,
    loadMessages,
    formatTime,
  }
})
