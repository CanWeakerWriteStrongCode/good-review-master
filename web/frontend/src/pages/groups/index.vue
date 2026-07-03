<template>
  <view class="page">
    <!-- Bot 状态栏 -->
    <view class="status-bar" v-if="store.botStatus">
      <text class="status-text">
        {{ store.botStatus.bot_nickname }}（{{ store.botStatus.bot_qq }}） |
        监控 {{ store.botStatus.group_count }} 个群 |
        API: {{ store.botStatus.api_key }}
      </text>
    </view>

    <!-- 群列表 -->
    <view class="group-list">
      <view
        class="group-card"
        v-for="group in store.groups"
        :key="group.group_id"
        @click="goMessages(group.group_id)"
      >
        <view class="group-card-header">
          <text class="group-id">{{ group.group_id }}</text>
          <view class="group-badge" :class="group.cached ? 'badge-active' : 'badge-empty'">
            {{ group.cached ? '已缓存' : '无数据' }}
          </view>
        </view>
        <view class="group-card-body" v-if="group.cached">
          <view class="group-stat">
            <text class="stat-label">消息数</text>
            <text class="stat-value">{{ group.message_count }}</text>
          </view>
          <view class="group-stat">
            <text class="stat-label">最近活动</text>
            <text class="stat-value stat-time">{{ group.last_activity || '-' }}</text>
          </view>
        </view>
        <view class="group-card-body" v-else>
          <text class="no-data">等待轮询获取数据...</text>
        </view>
      </view>
    </view>

    <!-- 空态 -->
    <view class="empty" v-if="!store.loading && store.groups.length === 0">
      <text>暂未配置群组，请在 config.yaml 中设置 allow_groups</text>
    </view>

    <!-- 加载中 -->
    <view class="loading" v-if="store.loading">
      <text>加载中...</text>
    </view>
  </view>
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import { useGroupsStore } from '@/stores/groups'
import { useAuthStore } from '@/stores/auth'

const authStore = useAuthStore()
const store = useGroupsStore()

onMounted(() => {
  if (!authStore.isAuthenticated()) {
    uni.reLaunch({ url: '/pages/login/index' })
    return
  }
  store.loadGroups()
})

function goMessages(groupId: string) {
  uni.navigateTo({
    url: `/pages/messages/index?id=${groupId}`,
  })
}
</script>

<style scoped>
.page {
  min-height: 100vh;
  padding: 12px;
}

/* 状态栏 */
.status-bar {
  background: linear-gradient(135deg, #1a1a2e, #16213e);
  border-radius: 10px;
  padding: 14px 16px;
  margin-bottom: 16px;
}
.status-text {
  color: #aab;
  font-size: 13px;
}

/* 群卡片 */
.group-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.group-card {
  background: #fff;
  border-radius: 10px;
  padding: 16px;
  box-shadow: 0 1px 4px rgba(0,0,0,0.06);
}
.group-card:active {
  background: #f8f9fb;
}
.group-card-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 10px;
}
.group-id {
  font-size: 16px;
  font-weight: 600;
  color: #1a1a2e;
}
.group-badge {
  font-size: 12px;
  padding: 3px 10px;
  border-radius: 12px;
}
.badge-active {
  background: #e6f7ee;
  color: #22a06b;
}
.badge-empty {
  background: #f0f0f0;
  color: #999;
}

/* 卡片内容 */
.group-card-body {
  display: flex;
  gap: 24px;
}
.group-stat {
  display: flex;
  flex-direction: column;
}
.stat-label {
  font-size: 12px;
  color: #999;
  margin-bottom: 2px;
}
.stat-value {
  font-size: 15px;
  font-weight: 500;
  color: #333;
}
.stat-time {
  font-size: 13px;
}
.no-data {
  font-size: 13px;
  color: #bbb;
}

/* 空态 / 加载 */
.empty, .loading {
  text-align: center;
  padding: 60px 0;
  color: #999;
  font-size: 14px;
}
</style>
