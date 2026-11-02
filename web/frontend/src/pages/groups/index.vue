<template>
  <view class="page">
    <!-- Bot 状态栏 -->
    <view class="status-bar" v-if="store.botStatus">
      <text class="status-text">
        {{ store.botStatus.account_count }} 个账号 |
        监控 {{ store.botStatus.group_count }} 个群
      </text>
      <view class="status-actions">
        <text class="diag-btn" @click="goDiagnostics">诊断</text>
        <text class="logout-btn" @click="doLogout">退出</text>
      </view>
    </view>

    <!-- 视图切换：只有一个账号时两种看法没区别，不显示 -->
    <view class="view-switch" v-if="store.workspaces.length > 1">
      <view
        class="view-tab"
        :class="{ active: store.viewMode === 'account' }"
        @click="store.switchMode('account')"
      >
        <text>按账号</text>
      </view>
      <view
        class="view-tab"
        :class="{ active: store.viewMode === 'all' }"
        @click="store.switchMode('all')"
      >
        <text>全部会话</text>
      </view>
    </view>

    <!-- 按账号：先选账号，再看它下面的会话 -->
    <template v-if="store.viewMode === 'account'">
      <!-- 工作空间切换：每个机器人账号一套独立数据 -->
      <view class="workspace-tabs" v-if="store.workspaces.length > 1">
        <view
          v-for="ws in store.workspaces"
          :key="ws.id"
          class="workspace-tab"
          :class="{ active: ws.id === store.selectedWorkspaceId }"
          @click="store.selectWorkspace(ws.id)"
        >
          <text class="workspace-name">{{ ws.name }}</text>
          <text class="workspace-meta">{{ ws.group_count }} 群 · {{ ws.message_count }} 条</text>
        </view>
      </view>

      <!-- 群列表：一个工作空间一组 -->
      <view
        class="account-section"
        v-for="section in store.accountSections"
        :key="section.self_id || '__unassigned__'"
      >
        <view class="account-header">
          <view class="account-title">
            <text class="account-name">{{ section.displayName }}</text>
            <text
              class="account-id"
              v-if="section.self_id && section.self_id !== 'qq_official' && section.self_id !== 'unknown_selfid'"
            >{{ section.self_id }}</text>
          </view>
          <text class="account-meta">{{ section.group_count }} 群 · {{ section.message_count }} 条</text>
        </view>

        <view class="group-list">
          <view
            class="group-card"
            v-for="group in section.groups"
            :key="group.group_id"
            @click="goMessages(group.group_id)"
          >
            <view class="group-card-header">
              <view class="group-title">
                <text class="group-name">{{ group.group_name || sessionLabel(group.group_id, group.type) }}</text>
                <text class="group-id-sub">{{ group.group_id }}</text>
              </view>
              <view class="group-badge" :class="group.has_messages ? 'badge-active' : 'badge-empty'">
                {{ group.has_messages ? '有记录' : '无数据' }}
              </view>
            </view>
            <view class="group-card-body" v-if="group.has_messages">
              <view class="group-stat">
                <text class="stat-label">消息数</text>
                <text class="stat-value">{{ group.message_count }}</text>
              </view>
              <view class="group-stat">
                <text class="stat-label">已收录成员</text>
                <text class="stat-value">{{ group.member_count }}</text>
              </view>
              <view class="group-stat">
                <text class="stat-label">最近活动</text>
                <text class="stat-value stat-time">{{ group.last_activity || '-' }}</text>
              </view>
            </view>
            <view class="group-card-body" v-else>
              <text class="no-data">等待接收消息...</text>
            </view>
          </view>
        </view>

        <view class="account-empty" v-if="section.groups.length === 0">
          <text>该账号暂无群数据</text>
        </view>
      </view>
    </template>

    <!-- 全部会话：所有账号的群/私聊平铺，卡片上标出归谁监控 -->
    <view class="group-list" v-else>
      <view
        class="group-card"
        v-for="session in store.sessions"
        :key="(session.workspace_id || '') + '|' + session.group_id"
        @click="goMessages(session.group_id, session.workspace_id)"
      >
        <view class="group-card-header">
          <view class="group-title">
            <text class="group-name">{{ session.group_name || sessionLabel(session.group_id, session.type) }}</text>
            <text class="group-id-sub">{{ session.group_id }}</text>
          </view>
          <view class="group-badge" :class="session.has_messages ? 'badge-active' : 'badge-empty'">
            {{ session.has_messages ? '有记录' : '无数据' }}
          </view>
        </view>
        <view class="group-card-body" v-if="session.has_messages">
          <view class="group-stat">
            <text class="stat-label">消息数</text>
            <text class="stat-value">{{ session.message_count }}</text>
          </view>
          <view class="group-stat">
            <text class="stat-label">已收录成员</text>
            <text class="stat-value">{{ session.member_count }}</text>
          </view>
          <view class="group-stat">
            <text class="stat-label">最近活动</text>
            <text class="stat-value stat-time">{{ session.last_activity || '-' }}</text>
          </view>
        </view>
        <view class="group-card-body" v-else>
          <text class="no-data">等待接收消息...</text>
        </view>
        <view class="group-monitors">
          <text class="monitor-label">监控账号</text>
          <text class="monitor-name">{{
            session.workspace_name || session.account_name || session.workspace_id
          }}</text>
        </view>
      </view>
    </view>

    <!-- 空态 -->
    <view class="empty" v-if="!store.loading && isEmpty">
      <text>{{ emptyHint }}</text>
    </view>

    <!-- 加载中 -->
    <view class="loading" v-if="store.loading">
      <text>加载中...</text>
    </view>
  </view>
</template>

<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useGroupsStore, sessionLabel } from '@/stores/groups'
import { useAuthStore } from '@/stores/auth'
import { logout as apiLogout } from '@/api'

const authStore = useAuthStore()
const store = useGroupsStore()

// 两种视图各自空了才算真没数据。
const isEmpty = computed(() =>
  store.viewMode === 'account' ? store.accountSections.length === 0 : store.sessions.length === 0,
)
const emptyHint = computed(() =>
  store.viewMode === 'account'
    ? '暂未配置群组，请在 config.yaml 中设置 allow_groups'
    : '还没有任何会话记录',
)

onMounted(() => {
  if (!authStore.isAuthenticated()) {
    uni.reLaunch({ url: '/pages/login/index' })
    return
  }
  store.loadGroups()
})

async function doLogout() {
  await apiLogout()
  authStore.logout()
  uni.reLaunch({ url: '/pages/login/index' })
}

function goMessages(groupId: string, workspaceId?: string) {
  // 全部会话视图里的卡片来自不同账号，链接要带各自的 wid；
  // 按账号视图里只有当前选中的那个工作空间。
  const wid = workspaceId || store.selectedWorkspaceId
  // 这里不能再 encodeURIComponent：uni.navigateTo 在 h5 下会对 query 值
  // 再编码一次（encodeQueryString），%3A 会变成 %253A，私聊会话
  // （private:<openid>）因此查不到。原样拼接，编码交给 uni。
  uni.navigateTo({
    url: `/pages/messages/index?wid=${wid}&id=${groupId}`,
  })
}

function goDiagnostics() {
  uni.navigateTo({ url: '/pages/diagnostics/index' })
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
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.status-text {
  color: #aab;
  font-size: 13px;
  flex: 1;
}
.status-actions {
  display: flex;
  gap: 8px;
  flex-shrink: 0;
}
.diag-btn {
  color: #7ec8e3;
  font-size: 13px;
  padding: 4px 12px;
  border: 1px solid #7ec8e3;
  border-radius: 6px;
}
.logout-btn {
  color: #e74c3c;
  font-size: 13px;
  padding: 4px 12px;
  border: 1px solid #e74c3c;
  border-radius: 6px;
}

/* 视图切换：按账号 / 全部会话 */
.view-switch {
  display: flex;
  gap: 4px;
  background: #eef0f4;
  border-radius: 10px;
  padding: 4px;
  margin-bottom: 14px;
}
.view-tab {
  flex: 1;
  text-align: center;
  padding: 8px 0;
  border-radius: 8px;
  font-size: 13px;
  color: #666;
}
.view-tab.active {
  background: #fff;
  color: #1a1a2e;
  font-weight: 600;
  box-shadow: 0 1px 3px rgba(0,0,0,0.08);
}

/* 工作空间切换 */
.workspace-tabs {
  display: flex;
  gap: 8px;
  margin-bottom: 16px;
  overflow-x: auto;
}
.workspace-tab {
  flex: 1;
  min-width: 132px;
  padding: 10px 12px;
  border-radius: 10px;
  background: #fff;
  border: 1px solid #e8e8ee;
  display: flex;
  flex-direction: column;
}
.workspace-tab.active {
  border-color: #7ec8e3;
  background: #f2fbff;
}
.workspace-name {
  font-size: 14px;
  font-weight: 600;
  color: #1a1a2e;
}
.workspace-meta {
  font-size: 11px;
  color: #888;
  margin-top: 2px;
}

/* 账号分组 */
.account-section {
  margin-bottom: 20px;
}
.account-header {
  display: flex;
  justify-content: space-between;
  align-items: flex-end;
  padding: 2px 4px 8px;
  margin-bottom: 10px;
  border-bottom: 1px solid #e8e8ee;
}
.account-title {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.account-name {
  font-size: 15px;
  font-weight: 600;
  color: #1a1a2e;
}
.account-id {
  font-size: 11px;
  color: #aaa;
  word-break: break-all;
}
.account-meta {
  font-size: 12px;
  color: #888;
  flex-shrink: 0;
  margin-left: 12px;
}
.account-empty {
  text-align: center;
  padding: 18px 0;
  color: #bbb;
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
.group-title {
  display: flex;
  flex-direction: column;
}
.group-name {
  font-size: 16px;
  font-weight: 600;
  color: #1a1a2e;
}
.group-id-sub {
  font-size: 12px;
  color: #999;
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

/* 全部会话视图：这张卡片归哪个账号监控 */
.group-monitors {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: 12px;
  padding-top: 10px;
  border-top: 1px dashed #eee;
}
.monitor-label {
  font-size: 12px;
  color: #999;
}
.monitor-name {
  font-size: 13px;
  color: #1a1a2e;
  font-weight: 500;
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
