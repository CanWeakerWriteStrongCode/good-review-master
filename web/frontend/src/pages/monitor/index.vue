<template>
  <view class="page">
    <AppHeader :title="t('nav.monitor')" />

    <!-- 报警：一进页面就能看到有没有出事（alerts 表） -->
    <view class="alerts" :class="store.alertTotal > 0 ? 'alerts-bad' : 'alerts-ok'">
      <view class="alerts-head">
        <view class="alerts-title-wrap">
          <view class="alerts-dot"></view>
          <text class="alerts-title">
            {{ store.alertTotal > 0 ? t('monitor.alerts.count', { count: store.alertTotal }) : t('monitor.alerts.none') }}
          </text>
        </view>
        <text
          class="alerts-toggle"
          v-if="store.alerts.length > ALERT_PREVIEW"
          @click="alertsOpen = !alertsOpen"
        >
          {{ alertsOpen ? t('common.collapse') : t('monitor.alerts.expandMore', { count: store.alerts.length }) }}
        </text>
      </view>

      <view class="alerts-list" v-if="store.alertTotal > 0">
        <view class="alert-row" v-for="a in visibleAlerts" :key="a.id" @click="toggleAlert(a.id)">
          <view class="alert-row-top">
            <text class="alert-kind">{{ alertKindLabel(a.kind) }}</text>
            <text class="alert-time">{{ formatDateTime(a.created_at) }}</text>
          </view>
          <view class="alert-row-bottom">
            <text class="alert-owner">{{ a.workspace_name || a.workspace_id || t('monitor.alerts.global') }}</text>
            <text class="alert-more" v-if="a.detail">{{ openAlert === a.id ? t('monitor.alerts.hideDetail') : t('monitor.alerts.showDetail') }}</text>
          </view>
          <text class="alert-detail" v-if="openAlert === a.id">{{ a.detail }}</text>
        </view>
      </view>
    </view>

    <!-- 顶部概览：几个账号、几个会话 -->
    <view class="overview">
      <view class="overview-nums">
        <text class="overview-num">{{ store.botStatus?.account_count ?? 0 }}</text>
        <text class="overview-unit">{{ t('monitor.overview.accountUnit', store.botStatus?.account_count ?? 0) }}</text>
        <text class="overview-sep">·</text>
        <text class="overview-num">{{ store.botStatus?.group_count ?? 0 }}</text>
        <text class="overview-unit">{{ t('monitor.overview.convUnit', store.botStatus?.group_count ?? 0) }}</text>
      </view>
      <text class="logout" @click="doLogout">{{ t('monitor.logout') }}</text>
    </view>

    <!-- 本页选项 -->
    <SegmentTabs v-model="mode" :options="options" />

    <!-- 按账号：一个机器人账号一张卡，点进去看它下面有哪些群 -->
    <view class="list" v-if="mode === 'account'">
      <view class="ws-card" v-for="ws in store.workspaces" :key="ws.id" @click="openWorkspace(ws)">
        <view class="ws-head">
          <text class="ws-name">{{ ws.name || ws.id }}</text>
          <view class="ws-badge" :class="ws.message_count > 0 ? 'badge-active' : 'badge-empty'">
            {{ ws.message_count > 0 ? t('monitor.ws.live') : t('monitor.ws.idle') }}
          </view>
        </view>
        <text class="ws-id">{{ ws.id }}</text>
        <view class="ws-stats">
          <view class="ws-stat">
            <text class="stat-label">{{ t('monitor.ws.statChats') }}</text>
            <text class="stat-value">{{ ws.group_count }}</text>
          </view>
          <view class="ws-stat">
            <text class="stat-label">{{ t('monitor.ws.statActive') }}</text>
            <text class="stat-value">{{ ws.active_group_count }}</text>
          </view>
          <view class="ws-stat">
            <text class="stat-label">{{ t('monitor.ws.statMessages') }}</text>
            <text class="stat-value">{{ ws.message_count }}</text>
          </view>
        </view>

        <!-- 这个账号下的群，直接列在卡片里，不用再跳进二级页 -->
        <view class="ws-groups">
          <view class="ws-groups-head">
            <text class="ws-groups-title">{{ t('monitor.ws.chatList') }}</text>
            <text class="ws-groups-count">{{ t('monitor.ws.chatCount', { count: groupList(ws.id).length }) }}</text>
          </view>

          <view
            class="ws-group-row"
            v-for="g in visibleGroups(ws.id)"
            :key="g.group_id"
            @click.stop="openGroup(g.group_id, ws.id)"
          >
            <view class="ws-group-head">
              <view class="ws-group-title">
                <text class="ws-group-name">{{ g.group_name || sessionLabel(g.group_id, g.type) }}</text>
                <text class="ws-group-id">{{ g.group_id }}</text>
              </view>
              <view class="ws-group-badge" :class="g.has_messages ? 'badge-active' : 'badge-empty'">
                {{ g.has_messages ? t('common.hasRecords') : t('common.noData') }}
              </view>
            </view>

            <!-- 字段跟「按群」卡片对齐：消息数、已收录成员、最近活动 -->
            <view class="ws-group-stats" v-if="g.has_messages">
              <text class="ws-group-stat">
                {{ t('monitor.group.messages') }} <text class="ws-group-num">{{ g.message_count }}</text>
              </text>
              <text class="ws-group-stat">
                {{ t('monitor.group.members') }} <text class="ws-group-num">{{ g.member_count }}</text>
              </text>
              <text class="ws-group-time">{{ formatDateTime(g.last_ts) }}</text>
            </view>
            <text class="ws-group-no-data" v-else>{{ t('common.waiting') }}</text>
          </view>

          <view class="ws-group-empty" v-if="groupList(ws.id).length === 0">
            <text>{{ store.groupsByWorkspace[ws.id] ? t('monitor.group.emptyOwn') : t('monitor.group.reading') }}</text>
          </view>

          <text
            class="ws-group-more"
            v-if="groupList(ws.id).length > PREVIEW"
            @click.stop="toggleMore(ws.id)"
          >
            {{ expanded[ws.id] ? t('common.collapse') : t('monitor.group.expandAll', { count: groupList(ws.id).length }) }}
          </text>
        </view>

        <view class="ws-foot">
          <text class="ws-foot-label">{{ t('monitor.foot.lastActive', { time: formatDateTime(ws.last_ts) }) }}</text>
          <text class="ws-foot-go">{{ t('monitor.foot.fullList') }}</text>
        </view>
      </view>

      <view class="empty" v-if="!store.loading && store.workspaces.length === 0">
        <text>{{ t('monitor.empty.accounts') }}</text>
      </view>
    </view>

    <!-- 按群：所有账号下的会话平铺，卡片上标出归谁监控 -->
    <view class="list" v-else>
      <view
        class="ws-card"
        v-for="s in store.sessions"
        :key="(s.workspace_id || '') + '|' + s.group_id"
        @click="openGroup(s.group_id, s.workspace_id)"
      >
        <view class="ws-head">
          <view class="ws-title">
            <text class="ws-name">{{ s.group_name || sessionLabel(s.group_id, s.type) }}</text>
            <text class="ws-id">{{ s.group_id }}</text>
          </view>
          <view class="ws-badge" :class="s.has_messages ? 'badge-active' : 'badge-empty'">
            {{ s.has_messages ? t('common.hasRecords') : t('common.noData') }}
          </view>
        </view>
        <view class="ws-stats" v-if="s.has_messages">
          <view class="ws-stat">
            <text class="stat-label">{{ t('common.stat.messages') }}</text>
            <text class="stat-value">{{ s.message_count }}</text>
          </view>
          <view class="ws-stat">
            <text class="stat-label">{{ t('common.stat.members') }}</text>
            <text class="stat-value">{{ s.member_count }}</text>
          </view>
          <view class="ws-stat ws-stat-wide">
            <text class="stat-label">{{ t('common.stat.lastActive') }}</text>
            <text class="stat-value stat-time">{{ formatDateTime(s.last_ts) }}</text>
          </view>
        </view>
        <view class="ws-stats" v-else>
          <text class="no-data">{{ t('common.waiting') }}</text>
        </view>
        <view class="ws-foot">
          <text class="ws-foot-label">{{ t('common.stat.monitorAccount') }}</text>
          <text class="ws-foot-owner">{{ s.workspace_name || s.account_name || s.workspace_id }}</text>
        </view>
      </view>

      <view class="empty" v-if="!store.loading && store.sessions.length === 0">
        <text>{{ t('monitor.empty.chats') }}</text>
      </view>
    </view>

    <view class="loading" v-if="store.loading">
      <text>{{ t('common.loading') }}</text>
    </view>

    <TabBar current="monitor" />
  </view>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useGroupsStore, sessionLabel, alertKindLabel } from '@/stores/groups'
import { useAuthStore } from '@/stores/auth'
import { useLocale } from '@/composables/useLocale'
import { formatDateTime } from '@/utils/format'
import { logout as apiLogout } from '@/api'
import AppHeader from '@/components/AppHeader.vue'
import TabBar from '@/components/TabBar.vue'
import SegmentTabs from '@/components/SegmentTabs.vue'
import type { SegmentOption } from '@/components/SegmentTabs.vue'
import type { GroupInfo, WorkspaceInfo } from '@/api/types'

const authStore = useAuthStore()
const store = useGroupsStore()
const { t } = useLocale()

// 本页自己的两种看法，跟群列表页的 viewMode 互不干扰：
// 那边关心「一个账号里有哪些群」，这边关心「有几个账号在跑」。
const mode = ref<'account' | 'group'>('account')

const options = computed<SegmentOption[]>(() => [
  { key: 'account', label: t('monitor.mode.byAccount'), badge: store.workspaces.length || undefined },
  { key: 'group', label: t('monitor.mode.byChat'), badge: store.sessions.length || undefined },
])

// 每个账号卡片里默认先摆几个群，剩下的点「展开全部」再看。
// 一个账号几十上百个群，全铺开页面会没边。
const PREVIEW = 4

// 报警区默认摆几条，多了点「展开」看。
const ALERT_PREVIEW = 3

/** 哪些账号点了「展开全部」 */
const expanded = ref<Record<string, boolean>>({})

// 会话列表直接从 store 取：loadGroups 已经把每个账号的群一起拉回来了，
// 本页不再为每个账号单发一次请求。
function groupList(workspaceId: string): GroupInfo[] {
  return store.groupsByWorkspace[workspaceId] || []
}

function visibleGroups(workspaceId: string): GroupInfo[] {
  const list = groupList(workspaceId)
  return expanded.value[workspaceId] ? list : list.slice(0, PREVIEW)
}

function toggleMore(workspaceId: string) {
  expanded.value[workspaceId] = !expanded.value[workspaceId]
}

const alertsOpen = ref(false)
/** 展开“看现场”的那条报警 id；null = 都收起 */
const openAlert = ref<number | null>(null)

const visibleAlerts = computed(() => {
  return alertsOpen.value ? store.alerts : store.alerts.slice(0, ALERT_PREVIEW)
})

function toggleAlert(id: number) {
  openAlert.value = openAlert.value === id ? null : id
}

// 「按群」要跨账号拿全部会话；store 里已经有各账号的会话，直接用，不再发请求。
watch(mode, async (next) => {
  if (next === 'group') await store.loadSessions()
})

onMounted(async () => {
  if (!authStore.isAuthenticated()) {
    uni.reLaunch({ url: '/pages/login/index' })
    return
  }
  await Promise.all([store.loadGroups(), store.loadAlerts()])
})

// 带着账号 id 进群列表页（二级页，导航栏有返回箭头），
// 那边 onLoad 会先选中这个账号再加载。
function openWorkspace(ws: WorkspaceInfo) {
  uni.navigateTo({ url: `/pages/groups/index?ws=${ws.id}` })
}

// 账号卡片下面那一行群点进去看消息（不冒泡到卡片上，否则会先跳去群列表）
function openGroup(groupId: string, workspaceId?: string) {
  const wid = workspaceId || store.selectedWorkspaceId
  // 不 encode：uni.navigateTo 在 h5 下会自己再编一次（见群列表页的说明）。
  uni.navigateTo({ url: `/pages/messages/index?wid=${wid}&id=${groupId}` })
}

async function doLogout() {
  await apiLogout()
  authStore.logout()
  uni.reLaunch({ url: '/pages/login/index' })
}
</script>

<style scoped>
.page {
  min-height: 100vh;
  padding: 12px;
  /* 底部给固定 tabbar 留位，最后一个卡片不会被挡住 */
  padding-bottom: calc(76px + env(safe-area-inset-bottom));
}

/* 报警区：没报警时是一条绿边细卡，有报警换成橙红，列表才会展开 */
.alerts {
  margin-bottom: 12px;
  padding: 10px 12px;
  border-radius: 12px;
  border: 1px solid;
}
.alerts-ok {
  background: #f2fbf5;
  border-color: #cfe9d8;
}
.alerts-bad {
  background: #fff5f2;
  border-color: #f7d4c8;
  box-shadow: 0 2px 10px rgba(217, 83, 79, 0.08);
}
.alerts-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}
.alerts-title-wrap {
  display: flex;
  align-items: center;
  gap: 7px;
}
.alerts-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: #2fb37a;
  flex-shrink: 0;
}
.alerts-bad .alerts-dot {
  background: #e05b4a;
}
.alerts-title {
  font-size: 14px;
  font-weight: 600;
  color: #22a06b;
}
.alerts-bad .alerts-title {
  color: #c94f3d;
}
.alerts-toggle {
  font-size: 12px;
  color: #c94f3d;
  flex-shrink: 0;
}
.alerts-list {
  margin-top: 6px;
}
.alert-row {
  padding: 8px 0;
  border-top: 1px dashed #f2d9d2;
}
.alert-row:active {
  background: #fff9f7;
}
.alert-row-top {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  gap: 10px;
}
.alert-kind {
  font-size: 13px;
  font-weight: 500;
  color: #4a3b38;
  word-break: break-all;
}
.alert-time {
  font-size: 11px;
  color: #b09b96;
  flex-shrink: 0;
}
.alert-row-bottom {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 10px;
  margin-top: 3px;
}
.alert-owner {
  font-size: 11px;
  color: #a08f8a;
  word-break: break-all;
}
.alert-more {
  font-size: 11px;
  color: #c94f3d;
  flex-shrink: 0;
}
/* 现场是一整段 JSON，保留换行 + 强制断行，不撑破卡片 */
.alert-detail {
  display: block;
  margin-top: 6px;
  padding: 8px;
  background: #fffdfc;
  border: 1px solid #f3e2dc;
  border-radius: 8px;
  font-size: 11px;
  line-height: 1.5;
  color: #6b5a55;
  white-space: pre-wrap;
  word-break: break-all;
}

/* 概览条 */
.overview {
  background: linear-gradient(135deg, #1a1a2e, #16213e);
  border-radius: 10px;
  padding: 14px 16px;
  margin-bottom: 14px;
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.overview-nums {
  display: flex;
  align-items: baseline;
  gap: 4px;
  flex-wrap: wrap;
}
.overview-num {
  color: #fff;
  font-size: 20px;
  font-weight: 600;
}
.overview-unit {
  color: #aab;
  font-size: 13px;
}
.overview-sep {
  color: #556;
  margin: 0 4px;
}
.logout {
  color: #e74c3c;
  font-size: 13px;
  padding: 4px 12px;
  border: 1px solid #e74c3c;
  border-radius: 6px;
  flex-shrink: 0;
  margin-left: 12px;
}

.list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

/* 账号卡片 / 会话卡片共用一套壳 */
.ws-card {
  background: #fff;
  border-radius: 10px;
  padding: 16px;
  box-shadow: 0 1px 4px rgba(0, 0, 0, 0.06);
}
.ws-card:active {
  background: #f8f9fb;
}
.ws-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  margin-bottom: 8px;
}
.ws-title {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.ws-name {
  font-size: 16px;
  font-weight: 600;
  color: #1a1a2e;
  word-break: break-all;
}
.ws-id {
  font-size: 12px;
  color: #999;
  word-break: break-all;
}
.ws-badge {
  font-size: 12px;
  padding: 3px 10px;
  border-radius: 12px;
  flex-shrink: 0;
  margin-left: 10px;
}
.badge-active {
  background: #e6f7ee;
  color: #22a06b;
}
.badge-empty {
  background: #f0f0f0;
  color: #999;
}

.ws-stats {
  display: flex;
  gap: 24px;
  margin-top: 10px;
}
.ws-stat {
  display: flex;
  flex-direction: column;
}
.ws-stat-wide {
  flex: 1;
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

/* 账号卡片里的群列表 */
.ws-groups {
  margin-top: 12px;
  padding-top: 10px;
  border-top: 1px dashed #eee;
}
.ws-groups-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 2px;
}
.ws-groups-title {
  font-size: 12px;
  color: #999;
}
.ws-groups-count {
  font-size: 12px;
  color: #b0b5bd;
}
.ws-group-row {
  padding: 10px 0;
  border-bottom: 1px solid #f5f6f8;
}
.ws-group-row:active {
  background: #f8f9fb;
}
.ws-group-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 10px;
}
.ws-group-title {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.ws-group-name {
  font-size: 14px;
  color: #1a1a2e;
  word-break: break-all;
}
.ws-group-id {
  font-size: 11px;
  color: #b0b5bd;
  word-break: break-all;
}
.ws-group-badge {
  font-size: 11px;
  padding: 2px 8px;
  border-radius: 10px;
  flex-shrink: 0;
}
.ws-group-stats {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 4px 14px;
  margin-top: 6px;
}
.ws-group-stat {
  font-size: 12px;
  color: #888;
}
.ws-group-num {
  font-size: 13px;
  color: #333;
  font-weight: 500;
}
/* 时间是长串（2026-11-02 16:28:31），单独占一行不跟数字挤 */
.ws-group-time {
  font-size: 12px;
  color: #a0a5ad;
  flex-basis: 100%;
}
.ws-group-no-data {
  display: block;
  font-size: 12px;
  color: #bbb;
  margin-top: 6px;
}
.ws-group-empty {
  font-size: 13px;
  color: #bbb;
  padding: 10px 0;
}
.ws-group-more {
  display: block;
  font-size: 13px;
  color: #4a9fd4;
  padding: 10px 0 2px;
}

.ws-foot {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
  margin-top: 12px;
  padding-top: 10px;
  border-top: 1px dashed #eee;
}
.ws-foot-label {
  font-size: 12px;
  color: #999;
}
.ws-foot-go {
  font-size: 13px;
  color: #4a9fd4;
  font-weight: 500;
  flex-shrink: 0;
}
.ws-foot-owner {
  font-size: 13px;
  color: #1a1a2e;
  font-weight: 500;
  word-break: break-all;
  text-align: right;
}

.empty,
.loading {
  text-align: center;
  padding: 60px 0;
  color: #999;
  font-size: 14px;
}
</style>
