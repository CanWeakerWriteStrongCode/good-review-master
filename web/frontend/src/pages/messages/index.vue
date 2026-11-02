<template>
  <view class="page">
    <AppHeader :title="t('nav.messages')" back />

    <!-- 群信息头 -->
    <view class="msg-header">
      <view class="header-main">
        <text class="header-group-id">
          {{ t('messages.header.labeled', { name: store.groupName || sessionLabel(store.groupId, store.chatType), id: store.groupId }) }}
        </text>
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
        <text class="msg-content">{{ msg.content }}</text>
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
import { useMessagesStore, sessionLabel } from '@/stores/groups'
import { useAuthStore } from '@/stores/auth'
import { useLocale } from '@/composables/useLocale'
import AppHeader from '@/components/AppHeader.vue'

const authStore = useAuthStore()
const store = useMessagesStore()
const { t } = useLocale()

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
.header-group-id {
  color: #fff;
  font-size: 16px;
  font-weight: 600;
  word-break: break-all;
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
