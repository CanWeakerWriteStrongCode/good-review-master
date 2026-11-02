<template>
  <view class="page">
    <AppHeader title="报警" />

    <!-- 汇总：总数 + 刷新。报警只增不删，没有分页，只能看最近一批 -->
    <view class="summary">
      <view class="summary-nums">
        <text class="summary-num">{{ store.alertTotal }}</text>
        <text class="summary-unit">条报警</text>
        <text class="summary-hint" v-if="store.alertTotal > store.alerts.length">
          显示最近 {{ store.alerts.length }} 条
        </text>
      </view>
      <text class="summary-refresh" @click="reload">{{ loading ? '刷新中' : '刷新' }}</text>
    </view>

    <view class="empty" v-if="!loading && store.alertTotal === 0">
      <view class="empty-icon">
        <text class="empty-icon-text">✓</text>
      </view>
      <text class="empty-title">暂无报警</text>
      <text class="empty-sub">窗口太小、阶段重试超限这类异常会记在这里</text>
    </view>

    <view class="list" v-else-if="store.alerts.length > 0">
      <view
        class="card"
        :class="'card-' + alertSeverity(a.kind)"
        v-for="a in store.alerts"
        :key="a.id"
      >
        <view class="card-head">
          <text class="kind">{{ alertKindLabel(a.kind) }}</text>
          <text class="time">{{ a.created_at_text }}</text>
        </view>
        <view class="card-meta">
          <text class="owner">{{ a.workspace_name || a.workspace_id || '全局' }}</text>
          <text class="toggle" v-if="a.detail" @click="toggle(a.id)">
            {{ open === a.id ? '收起现场' : '看现场' }}
          </text>
        </view>
        <!-- 现场是完整 JSON，保留换行 + 强制断行，不撑破卡片 -->
        <text class="detail" v-if="open === a.id">{{ a.detail }}</text>
      </view>
    </view>

    <view class="loading" v-if="loading">
      <text>加载中...</text>
    </view>

    <TabBar current="alerts" />
  </view>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useGroupsStore, alertKindLabel, alertSeverity } from '@/stores/groups'
import { useAuthStore } from '@/stores/auth'
import AppHeader from '@/components/AppHeader.vue'
import TabBar from '@/components/TabBar.vue'

const store = useGroupsStore()
const authStore = useAuthStore()

/** 展开了「现场」的那条报警 id；null = 都收起 */
const open = ref<number | null>(null)
const loading = ref(false)

// 本页要一整屏：监控页顶卡只取 50 条够用，这里翻到后端允许的上限 200。
const PAGE_LIMIT = 200

function toggle(id: number) {
  open.value = open.value === id ? null : id
}

async function reload() {
  loading.value = true
  try {
    await store.loadAlerts(PAGE_LIMIT)
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  if (!authStore.isAuthenticated()) {
    uni.reLaunch({ url: '/pages/login/index' })
    return
  }
  await reload()
})
</script>

<style scoped>
.page {
  min-height: 100vh;
  background: #f0f2f5;
  padding: 12px;
  padding-bottom: calc(76px + env(safe-area-inset-bottom));
  box-sizing: border-box;
}

/* 汇总条 */
.summary {
  background: #fff;
  border-radius: 12px;
  padding: 12px 14px;
  margin-bottom: 12px;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.04);
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}
.summary-nums {
  display: flex;
  align-items: baseline;
  gap: 5px;
  flex-wrap: wrap;
  min-width: 0;
}
.summary-num {
  font-size: 22px;
  font-weight: 700;
  color: #1a1a2e;
  line-height: 1;
}
.summary-unit {
  font-size: 13px;
  color: #6b7280;
}
.summary-hint {
  font-size: 11px;
  color: #a0a5ad;
  margin-left: 2px;
}
.summary-refresh {
  font-size: 13px;
  color: #4a9fd4;
  flex-shrink: 0;
}

/* 空态 */
.empty {
  background: #fff;
  border-radius: 12px;
  padding: 40px 20px;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.04);
}
.empty-icon {
  width: 46px;
  height: 46px;
  border-radius: 50%;
  background: #eaf8f0;
  display: flex;
  align-items: center;
  justify-content: center;
}
.empty-icon-text {
  font-size: 22px;
  color: #2fb37a;
}
.empty-title {
  font-size: 15px;
  color: #1a1a2e;
}
.empty-sub {
  font-size: 12px;
  color: #a0a5ad;
  text-align: center;
}

/* 报警卡片：左边一条竖色带区分严重度 */
.card {
  background: #fff;
  border-radius: 12px;
  padding: 12px 14px;
  margin-bottom: 10px;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.04);
  border-left: 3px solid #e0e3e8;
}
.card-warn {
  border-left-color: #e6a23c;
}
.card-error {
  border-left-color: #e05b4a;
}
.card-head {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  gap: 10px;
}
.kind {
  font-size: 14px;
  font-weight: 500;
  color: #1a1a2e;
  word-break: break-all;
}
.time {
  font-size: 11px;
  color: #b0b5bd;
  flex-shrink: 0;
}
.card-meta {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 10px;
  margin-top: 5px;
}
.owner {
  font-size: 12px;
  color: #8a9099;
  word-break: break-all;
}
.toggle {
  font-size: 12px;
  color: #4a9fd4;
  flex-shrink: 0;
}
.detail {
  display: block;
  margin-top: 8px;
  padding: 10px;
  background: #f8f9fb;
  border: 1px solid #eceef3;
  border-radius: 8px;
  font-size: 11px;
  line-height: 1.55;
  color: #5b6470;
  white-space: pre-wrap;
  word-break: break-all;
}

.loading {
  padding: 20px;
  text-align: center;
  font-size: 13px;
  color: #999;
}
</style>
