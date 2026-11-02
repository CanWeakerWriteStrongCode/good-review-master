<template>
  <view class="page">
    <AppHeader :title="t('nav.groups')" back />

    <!-- 本页选项：按监控账号分组看，或所有账号的会话平铺 -->
    <SegmentTabs v-model="viewMode" :options="modeOptions" />

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
          <text class="workspace-meta">{{ t('groups.workspace.meta', { groups: ws.group_count, messages: ws.message_count }) }}</text>
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
          <text class="account-meta">{{ t('groups.account.meta', { groups: section.group_count, messages: section.message_count }) }}</text>
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
                {{ group.has_messages ? t('common.hasRecords') : t('common.noData') }}
              </view>
            </view>
            <view class="group-card-body" v-if="group.has_messages">
              <view class="group-stat">
                <text class="stat-label">{{ t('common.stat.messages') }}</text>
                <text class="stat-value">{{ group.message_count }}</text>
              </view>
              <view class="group-stat">
                <text class="stat-label">{{ t('common.stat.members') }}</text>
                <text class="stat-value">{{ group.member_count }}</text>
              </view>
              <view class="group-stat">
                <text class="stat-label">{{ t('common.stat.lastActive') }}</text>
                <text class="stat-value stat-time">{{ formatDateTime(group.last_ts) }}</text>
              </view>
            </view>
            <view class="group-card-body" v-else>
              <text class="no-data">{{ t('common.waiting') }}</text>
            </view>
          </view>
        </view>

        <view class="account-empty" v-if="section.groups.length === 0">
          <text>{{ t('groups.empty.own') }}</text>
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
            {{ session.has_messages ? t('common.hasRecords') : t('common.noData') }}
          </view>
        </view>
        <view class="group-card-body" v-if="session.has_messages">
          <view class="group-stat">
            <text class="stat-label">{{ t('common.stat.messages') }}</text>
            <text class="stat-value">{{ session.message_count }}</text>
          </view>
          <view class="group-stat">
            <text class="stat-label">{{ t('common.stat.members') }}</text>
            <text class="stat-value">{{ session.member_count }}</text>
          </view>
          <view class="group-stat">
            <text class="stat-label">{{ t('common.stat.lastActive') }}</text>
            <text class="stat-value stat-time">{{ formatDateTime(session.last_ts) }}</text>
          </view>
        </view>
        <view class="group-card-body" v-else>
          <text class="no-data">{{ t('common.waiting') }}</text>
        </view>
        <view class="group-monitors">
          <text class="monitor-label">{{ t('common.stat.monitorAccount') }}</text>
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
      <text>{{ t('common.loading') }}</text>
    </view>
  </view>
</template>

<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { onLoad } from '@dcloudio/uni-app'
import { useGroupsStore, sessionLabel, type ViewMode } from '@/stores/groups'
import { useAuthStore } from '@/stores/auth'
import { useLocale } from '@/composables/useLocale'
import { formatDateTime } from '@/utils/format'
import AppHeader from '@/components/AppHeader.vue'
import SegmentTabs from '@/components/SegmentTabs.vue'
import type { SegmentOption } from '@/components/SegmentTabs.vue'

const authStore = useAuthStore()
const store = useGroupsStore()
const { t } = useLocale()

// v-model 用 computed 中转：读的是 store 里的当前视图，写入交给 switchMode，
// 它会顺手把该拉的列表拉一次。
const viewMode = computed<ViewMode>({
  get: () => store.viewMode,
  set: (next) => {
    store.switchMode(next)
  },
})

const modeOptions = computed<SegmentOption[]>(() => [
  { key: 'account', label: t('groups.mode.byAccount'), badge: store.workspaces.length || undefined },
  { key: 'all', label: t('groups.mode.all'), badge: store.sessions.length || undefined },
])

// 从监控页点某个账号跳进来时链接上带了 ?ws=，先把这个账号设成选中：
// loadGroups 会认出这个 id，直接拉它的群，不用先拉第一个再换一次。
// 同时把视图拨回「按监控账号」——上次可能停在全部列表，不拨回去
// 就会只显示全量列表，看不到刚点的那个账号。
onLoad((query) => {
  const ws = (query as Record<string, string> | undefined)?.ws
  if (!ws) return
  store.viewMode = 'account'
  store.selectedWorkspaceId = ws
})

// 两种视图各自空了才算真没数据。
const isEmpty = computed(() =>
  store.viewMode === 'account' ? store.accountSections.length === 0 : store.sessions.length === 0,
)
const emptyHint = computed(() =>
  store.viewMode === 'account'
    ? t('groups.empty.config')
    : t('groups.empty.all'),
)

onMounted(() => {
  if (!authStore.isAuthenticated()) {
    uni.reLaunch({ url: '/pages/login/index' })
    return
  }
  store.loadGroups()
})

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
</script>

<style scoped>
.page {
  min-height: 100vh;
  padding: 12px;
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
