import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  fetchAlerts,
  fetchGroups,
  fetchMessages,
  fetchSessions,
  fetchStatus,
  fetchWorkspaces,
  fetchWorkspaceGroups,
  fetchWorkspaceMessages,
} from '@/api'
import type { AccountInfo, AlertItem, GroupInfo, BotStatus, Message, WorkspaceInfo } from '@/api/types'
import { i18n } from '@/locale'
import { formatDateTime } from '@/utils/format'

/**
 * store 不是组件，用不了 useI18n()，直接问全局实例。
 * 读到的是同一个 locale ref，所以在模板里调这些函数照样会跟着切语言重算。
 */
function tt(key: string, named?: Record<string, unknown>): string {
  return i18n.global.t(key, named || {}) as string
}

/** 群列表的两种看法：按账号分、所有账号的会话平铺 */
export type ViewMode = 'account' | 'all'

/** 群列表按账号分组后的一项 */
export interface AccountSection extends AccountInfo {
  displayName: string
  groups: GroupInfo[]
}

// 会话类型只看 groups 表的 type 列：group = 群聊，private = 私聊。
// 群列表接口和消息接口都把这个字段带回来了，卡片和顶栏的标记就认它，
// 不去猜 group_id 前缀（那样私聊改键名就全错）。
export function isPrivateChat(type?: string): boolean {
  return type === 'private'
}

// 标记文案：监控页卡片、详情页顶栏共用这一份，两边措辞不会跑偏。
export function chatKindLabel(type?: string): string {
  return isPrivateChat(type) ? tt('common.chatKind.private') : tt('common.chatKind.group')
}

// 账号显示名：昵称优先；占位值是适配器还没学到身份，显示「未知账号」。
function accountLabel(selfID: string, nickname: string): string {
  if (nickname) return nickname
  if (!selfID) return tt('common.unassignedAccount')
  if (selfID === 'qq_official' || selfID === 'unknown_selfid') return tt('common.unknownAccount')
  return selfID
}

/**
 * 报警分类翻成人话。
 * kind 是写库时定的分类（window_too_small、dead_letter_<stage>），
 * 直接摆在页面上没人看得懂。监控页和报警页共用这一份，两边措辞不会跑偏。
 */
export function alertKindLabel(kind: string): string {
  if (kind === 'window_too_small') return tt('alerts.kind.windowTooSmall')
  if (kind.startsWith('dead_letter_')) {
    return tt('alerts.kind.deadLetter', { stage: kind.slice('dead_letter_'.length) })
  }
  return kind
}

/** 报警严重度：warn = 丢一条回复，error = 某个阶段彻底失败 */
export function alertSeverity(kind: string): 'warn' | 'error' {
  return kind === 'window_too_small' ? 'warn' : 'error'
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
  /** 各账号的会话列表（账号 id → 会话）：进页面时跟汇总一次拉回来，切账号不再发请求 */
  const groupsByWorkspace = ref<Record<string, GroupInfo[]>>({})
  /** 最近的报警（alerts 表），按时间倒序 */
  const alerts = ref<AlertItem[]>([])
  /** 报警总条数，可能大于 alerts.length（只读了最近一批） */
  const alertTotal = ref(0)

  const selectedWorkspace = computed<WorkspaceInfo | null>(() => {
    return workspaces.value.find((w) => w.id === selectedWorkspaceId.value) || null
  })

  const accountSections = computed<AccountSection[]>(() => {
    // 一个页面只对应一个工作空间，群全部挂在这个账号下。
    // 不能按 group.self_id 分组：那是机器人 openid（v15 起账号级，各群同一个值），
    // 账号 ID 是 qq_official_bot_<AppID>，两者不是一个东西；拿 openid 当 key
    // 既分不出账号，库里没学到身份的群还会落到「未归属账号」。
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
        last_ts: 0,
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

  // loadAlerts 拉最近一批报警。失败只记日志：报警区块坏了不该把整页拖黄。
  async function loadAlerts(limit = 50) {
    try {
      const data = await fetchAlerts(limit)
      alerts.value = data.alerts || []
      alertTotal.value = data.total || 0
    } catch (e) {
      console.error('加载报警失败', e)
    }
  }

  async function loadGroups() {
    loading.value = true
    try {
      // 带上 with_groups：账号汇总和每个账号下的会话一起回来，
      // 避免再对每个账号单发一次 /workspaces/{id}/groups。
      const data = await fetchWorkspaces(true)
      botStatus.value = data.bot_info
      workspaces.value = data.workspaces || []
      groupsByWorkspace.value = data.groups_by_workspace || {}
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
    // 各账号的会话在 loadGroups 时已经进缓存了，本地拼一下就行。
    // 再打 /sessions 会把每个账号的 groups 表又读一遍，数字还跟卡片上的对不上。
    if (Object.keys(groupsByWorkspace.value).length > 0) {
      sessions.value = collectAllSessions()
      return
    }
    const data = await fetchSessions()
    botStatus.value = data.bot_info
    sessions.value = data.sessions || []
  }

  // collectAllSessions 把各账号的会话拼成一张表，排序规则跟后端 collectSessions 一致：
  // 最近消息时间倒序，同一时间按账号、再按群号，保证每次打开顺序一样。
  function collectAllSessions(): GroupInfo[] {
    const all: GroupInfo[] = []
    for (const list of Object.values(groupsByWorkspace.value)) {
      all.push(...list)
    }
    all.sort((a, b) => {
      const ta = a.last_ts || 0
      const tb = b.last_ts || 0
      if (ta !== tb) return tb - ta
      const wa = a.workspace_id || ''
      const wb = b.workspace_id || ''
      if (wa !== wb) return wa < wb ? -1 : 1
      return a.group_id < b.group_id ? -1 : a.group_id > b.group_id ? 1 : 0
    })
    return all
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

  // accountFromWorkspace 把账号汇总转成群列表页用的 AccountInfo。
  // 数值就是同一个 workspaceInfo，跟后端 /workspaces/{id}/groups 的 accounts 一致。
  function accountFromWorkspace(ws: WorkspaceInfo): AccountInfo {
    return {
      self_id: ws.self_ids?.[0] || '',
      nickname: ws.name || '',
      group_count: ws.group_count,
      active_group_count: ws.active_group_count,
      message_count: ws.message_count,
      last_ts: ws.last_ts,
    }
  }

  async function loadWorkspace(id: string) {
    const ws = workspaces.value.find((w) => w.id === id)
    const cached = groupsByWorkspace.value[id]
    if (ws && cached) {
      // 缓存命中：会话列表和汇总都是 loadGroups 一次拿回来的，这里只切换，不发请求。
      selectedWorkspaceId.value = id
      groups.value = cached
      accounts.value = [accountFromWorkspace(ws)]
      return
    }
    const data = await fetchWorkspaceGroups(id)
    selectedWorkspaceId.value = id
    botStatus.value = data.bot_info
    groups.value = data.groups
    groupsByWorkspace.value[id] = data.groups
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
    return formatDateTime(ts)
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
    groupsByWorkspace,
    alerts,
    alertTotal,
    loadStatus,
    loadAlerts,
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
  /** 会话类型（groups.type）：详情页顶栏靠它标「群聊 / 私聊」 */
  const chatType = ref('')
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
      chatType.value = data.type || ''
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
    return formatDateTime(ts)
  }

  return {
    messages,
    groupId,
    groupName,
    chatType,
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
