<template>
  <view class="page">
    <AppHeader :title="t('nav.messages')" back />

    <!-- 聊天信息栏：会话类型标在群名前面，类型只认 groups.type -->
    <view class="msg-header">
      <view class="header-main">
        <view class="header-title-line">
          <text
            v-if="store.chatType"
            class="header-kind"
            :class="isPrivateChat(store.chatType) ? 'header-kind-private' : 'header-kind-group'"
          >
            {{ chatKindLabel(store.chatType) }}
          </text>
          <text class="header-group-id">
            {{ store.groupName ? t('messages.header.labeled', { name: store.groupName, id: store.groupId }) : store.groupId }}
          </text>
        </view>
        <text class="header-account" v-if="store.workspaceName">
          {{ t('messages.header.workspace', { name: store.workspaceName }) }}
        </text>
        <text class="header-account">
          {{ t('messages.header.account', { name: store.accountName || store.selfId || t('common.unknown') }) }}
        </text>
      </view>
      <text class="header-count" v-if="!store.empty">
        {{ store.total > store.messages.length
          ? t('messages.header.partial', { total: store.total, shown: store.messages.length })
          : t('messages.header.total', store.total, { count: store.total }) }}
      </text>
    </view>

    <!-- 消息列表 -->
    <view class="msg-list" v-if="!store.empty">
      <view class="msg-item" v-for="msg in store.messages" :key="msg.msg_id">
        <view class="msg-meta">
          <view class="msg-sender-col">
            <view class="msg-sender-line">
              <text class="msg-sender">{{ msg.card || msg.nick || msg.user_id || t('messages.senderBot') }}</text>
              <text class="msg-user-id" v-if="msg.user_id">{{ msg.user_id }}</text>
            </view>
            <text class="msg-card" v-if="msg.card && msg.card !== msg.nick">{{ msg.nick }}</text>
          </view>
          <text class="msg-time">{{ store.formatTime(msg.time) }}</text>
        </view>
        <text class="msg-content">{{ contentWithImageNotes(msg) }}</text>
      </view>
    </view>

    <!-- 空态 -->
    <view class="empty" v-else-if="!store.loading">
      <text>{{ t('messages.empty') }}</text>
    </view>

    <!-- 加载中 -->
    <view class="loading" v-if="store.loading">
      <text>{{ t('common.loading') }}</text>
    </view>
  </view>
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import { useMessagesStore, isPrivateChat, chatKindLabel } from '@/stores/groups'
import { useAuthStore } from '@/stores/auth'
import { useLocale } from '@/composables/useLocale'
import type { Message } from '@/api/types'
import AppHeader from '@/components/AppHeader.vue'

const authStore = useAuthStore()
const store = useMessagesStore()
const { t } = useLocale()

// 正文里的 [图片] 是入站时后端拼的占位（图没法直接显示），
// 图库里有描述就把它换成 [图片：描述]，一条消息里多张图按顺序各配各的。
// 描述可能为空（还没跑评估或评估失败），那种情况保持原样。
function imagePlaceholder(note: string | undefined): string {
  const text = (note || '').trim()
  return text ? t('messages.imageWithNote', { note: text }) : t('messages.imagePlaceholder')
}

function contentWithImageNotes(msg: Message): string {
  const notes = (msg.image_notes || []).map((item) => item.note)
  const placeholder = t('messages.imagePlaceholder')
  if (!notes.some((note) => (note || '').trim()) || !msg.content.includes(placeholder)) {
    return msg.content
  }
  const parts = msg.content.split(placeholder)
  let index = 0
  let text = parts[0]
  for (let i = 1; i < parts.length; i++) {
    text += imagePlaceholder(notes[index++]) + parts[i]
  }
  // 占位符比图片少（比如图在正文里没有对应段）时，剩下的说明补在末尾，别丢。
  const rest = notes.slice(index).filter((note) => (note || '').trim())
  return rest.length ? `${text} ${rest.map(imagePlaceholder).join(' ')}` : text
}

// uni.navigateTo 在 h5 下会把 query 值再 encodeURIComponent 一次，
// 拼好的链接里 %3A 会变成 %253A，URLSearchParams 解一次后还会剩一层。
// 这里最多再解两轮；普通群号是纯十六进制，不含 %，不会受影响。
function decodeParam(value: string | null): string {
  let out = value || ''
  for (let i = 0; i < 2 && /%[0-9a-fA-F]{2}/.test(out); i++) {
    try {
      out = decodeURIComponent(out)
    } catch {
      break
    }
  }
  return out
}

onMounted(() => {
  if (!authStore.isAuthenticated()) {
    uni.reLaunch({ url: '/pages/login/index' })
    return
  }
  const hash = window.location.hash
  const queryIndex = hash.indexOf('?')
  if (queryIndex !== -1) {
    const params = new URLSearchParams(hash.slice(queryIndex))
    const id = decodeParam(params.get('id'))
    const wid = decodeParam(params.get('wid'))
    if (id) {
      store.loadMessages(wid, id)
    }
  }
})
</script>

<style scoped>
.page {
  min-height: 100vh;
  padding: 12px;
}

/* 聊天信息栏里的会话类型标记：这块也是深色底，用半透明底 + 浅色字 */
.header-kind {
  flex-shrink: 0;
  font-size: 11px;
  line-height: 1;
  padding: 4px 7px;
  border-radius: 5px;
  border: 1px solid rgba(255, 255, 255, 0.22);
  color: #dbe7f2;
}
.header-kind-private {
  background: rgba(126, 200, 227, 0.22);
  border-color: rgba(126, 200, 227, 0.5);
  color: #c9ecfa;
}
.header-kind-group {
  background: rgba(255, 255, 255, 0.1);
}

/* 头部 */
.msg-header {
  background: linear-gradient(135deg, #1a1a2e, #16213e);
  border-radius: 10px;
  padding: 14px 16px;
  margin-bottom: 16px;
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.header-main {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
/* 类型标记和群名同一行，名字太长就在标记后面换行 */
.header-title-line {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.header-group-id {
  color: #fff;
  font-size: 16px;
  font-weight: 600;
  word-break: break-all;
  min-width: 0;
}
.header-account {
  color: #aab;
  font-size: 12px;
  margin-top: 2px;
  word-break: break-all;
}
.header-count {
  color: #aab;
  font-size: 13px;
  flex-shrink: 0;
  margin-left: 12px;
}

/* 消息列表 */
.msg-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.msg-item {
  background: #fff;
  border-radius: 10px;
  padding: 14px 16px;
  box-shadow: 0 1px 3px rgba(0,0,0,0.05);
}
.msg-meta {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 8px;
}
.msg-sender-col {
  display: flex;
  flex-direction: column;
}
.msg-sender-line {
  display: flex;
  align-items: baseline;
  gap: 8px;
}
.msg-sender {
  font-size: 14px;
  font-weight: 600;
  color: #1a1a2e;
}
.msg-card {
  font-size: 12px;
  color: #aaa;
  margin-top: 1px;
}
.msg-user-id {
  font-size: 12px;
  color: #bbb;
}
.msg-time {
  font-size: 12px;
  color: #999;
}
.msg-content {
  font-size: 14px;
  color: #444;
  line-height: 1.5;
  word-break: break-all;
}

/* 空态 / 加载 */
.empty, .loading {
  text-align: center;
  padding: 60px 0;
  color: #999;
  font-size: 14px;
}
</style>
