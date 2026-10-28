import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  fetchGroups,
  fetchMessages,
  fetchStatus,
  fetchWorkspaces,
  fetchWorkspaceGroups,
  fetchWorkspaceMessages,
} from '@/api'
import type { AccountInfo, GroupInfo, BotStatus, Message, WorkspaceInfo } from '@/api/types'

/** 群列表按账号分组后的一项 */
export interface AccountSection extends AccountInfo {
  displayName: string
  groups: GroupInfo[]
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

  const selectedWorkspace = computed<WorkspaceInfo | null>(() => {
    return workspaces.value.find((w) => w.id === selectedWorkspaceId.value) || null
  })

  const accountSections = computed<AccountSection[]>(() => {
    const sections = new Map<string, AccountSection>()
    for (const acc of accounts.value) {
      sections.set(acc.self_id, {
        ...acc,
        displayName: accountLabel(acc.self_id, acc.nickname),
        groups: [],
      })
    }
    for (const group of groups.value) {
      const key = group.self_id || ''
      let section = sections.get(key)
      if (!section) {
        section = {
          self_id: key,
          nickname: '',
          group_count: 0,
          cached_group_count: 0,
          message_count: 0,
          last_activity: '',
          displayName: accountLabel(key, group.account_name),
          groups: [],
        }
        sections.set(key, section)
      }
      section.groups.push(group)
      if (!section.nickname && group.account_name) {
        section.nickname = group.account_name
        section.displayName = accountLabel(key, group.account_name)
      }
    }

    const list = [...sections.values()]
    for (const section of list) {
      section.group_count = section.groups.length
      if (section.message_count === 0) {
        section.message_count = section.groups.reduce((sum, g) => sum + g.message_count, 0)
      }
    }
    // 已归属的账号在前，未归属的放最后；同组按显示名排序。
    list.sort((a, b) => {
      const aUnknown = !a.self_id
      const bUnknown = !b.self_id
      if (aUnknown !== bUnknown) return aUnknown ? 1 : -1
      return a.displayName.localeCompare(b.displayName, 'zh-CN')
    })
    return list
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
    loadStatus,
    loadGroups,
    selectWorkspace,
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
    loadMessages,
    formatTime,
  }
})
