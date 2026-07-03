import { defineStore } from 'pinia'
import { ref } from 'vue'
import { fetchGroups, fetchMessages, fetchStatus } from '@/api'
import type { GroupInfo, BotStatus, Message } from '@/api/types'

export const useGroupsStore = defineStore('groups', () => {
  const botStatus = ref<BotStatus | null>(null)
  const groups = ref<GroupInfo[]>([])
  const loading = ref(false)

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
      const data = await fetchGroups()
      botStatus.value = data.bot_info
      groups.value = data.groups
    } catch (e) {
      console.error('加载群列表失败', e)
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
    loading,
    loadStatus,
    loadGroups,
    formatTime,
  }
})

export const useMessagesStore = defineStore('messages', () => {
  const messages = ref<Message[]>([])
  const groupId = ref('')
  const loading = ref(false)
  const empty = ref(true)

  async function loadMessages(id: string) {
    groupId.value = id
    loading.value = true
    try {
      const data = await fetchMessages(id)
      messages.value = data.messages
      empty.value = data.empty
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
    return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
  }

  return {
    messages,
    groupId,
    loading,
    empty,
    loadMessages,
    formatTime,
  }
})
