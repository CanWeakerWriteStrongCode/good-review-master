<template>
  <view class="page">
    <!-- 群信息头 -->
    <view class="msg-header">
      <text class="header-group-id">群 {{ store.groupId }}</text>
      <text class="header-count" v-if="!store.empty">{{ store.messages.length }} 条消息</text>
    </view>

    <!-- 消息列表 -->
    <view class="msg-list" v-if="!store.empty">
      <view class="msg-item" v-for="msg in store.messages" :key="msg.msg_id">
        <view class="msg-meta">
          <text class="msg-sender">{{ msg.nick }}</text>
          <text class="msg-time">{{ store.formatTime(msg.time) }}</text>
        </view>
        <text class="msg-content">{{ msg.content }}</text>
      </view>
    </view>

    <!-- 空态 -->
    <view class="empty" v-else-if="!store.loading">
      <text>该群暂无缓存消息</text>
    </view>

    <!-- 加载中 -->
    <view class="loading" v-if="store.loading">
      <text>加载中...</text>
    </view>
  </view>
</template>

<script setup lang="ts">
import { onLoad } from '@dcloudio/uni-app'
import { useMessagesStore } from '@/stores/groups'

const store = useMessagesStore()

onLoad((options?: AnyObject) => {
  if (options?.id) {
    store.loadMessages(options.id as string)
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
.header-group-id {
  color: #fff;
  font-size: 16px;
  font-weight: 600;
}
.header-count {
  color: #aab;
  font-size: 13px;
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
.msg-sender {
  font-size: 14px;
  font-weight: 600;
  color: #1a1a2e;
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
