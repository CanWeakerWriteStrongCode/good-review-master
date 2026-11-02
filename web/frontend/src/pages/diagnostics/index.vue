<template>
  <view class="page">
    <AppHeader :title="t('nav.diagnostics')" />

    <!-- 概览卡片 -->
    <view class="cards" v-if="data">
      <view class="card">
        <text class="card-num">{{ data.runtime.goroutines }}</text>
        <text class="card-label">{{ t('diagnostics.card.goroutines') }}</text>
      </view>
      <view class="card">
        <text class="card-num">{{ formatBytes(data.runtime.heap_inuse_bytes) }}</text>
        <text class="card-label">{{ t('diagnostics.card.heap') }}</text>
      </view>
      <view class="card">
        <text class="card-num">{{ data.runtime.num_gc }}</text>
        <text class="card-label">{{ t('diagnostics.card.gc') }}</text>
      </view>
      <view class="card">
        <text class="card-num">{{ formatDuration(data.runtime.uptime_seconds) }}</text>
        <text class="card-label">{{ t('diagnostics.card.uptime') }}</text>
      </view>
    </view>

    <view class="meta" v-if="data">
      {{ data.runtime.go_version }} · {{ data.runtime.num_cpu }} CPU ·
      {{ data.runtime.version }}
    </view>

    <!-- 操作栏 -->
    <view class="toolbar">
      <text class="btn" @click="reload">{{ t('common.refresh') }}</text>
      <text class="btn" :class="{ 'btn-on': autoRefresh }" @click="toggleAuto">
        {{ autoRefresh ? t('diagnostics.toolbar.stopAuto') : t('diagnostics.toolbar.auto') }}
      </text>
    </view>

    <!-- goroutine 分组：找泄漏的关键视图 -->
    <view class="panel" v-if="data">
      <view class="panel-head">
        <text class="panel-title">{{ t('diagnostics.goroutines.title') }}</text>
        <text class="panel-sub">
          {{ t('diagnostics.goroutines.count', { total: data.goroutines.total, groups: data.goroutines.group_count }) }}
        </text>
      </view>
      <view class="group-row" v-for="group in data.goroutines.groups" :key="group.frame">
        <text class="group-count">{{ group.count }}</text>
        <text class="group-frame">{{ shortenFrame(group.frame) }}</text>
        <text class="group-state">{{ group.state }}</text>
      </view>
      <view class="tabs" v-if="data.goroutines.group_count > 0">
        <text class="tab" @click="toggleDump">
          {{ showDump ? t('diagnostics.dump.collapse') : t('diagnostics.dump.expand') }}
        </text>
        <text class="tab" v-if="showDump" @click="copyDump">{{ t('diagnostics.dump.copy') }}</text>
      </view>
      <scroll-view class="dump" scroll-y v-if="showDump && dumpText">
        <text class="dump-text">{{ dumpText }}</text>
      </scroll-view>
      <view class="note" v-if="dumpTruncated">
        {{ t('diagnostics.dump.truncated', { size: formatBytes(DUMP_DISPLAY_LIMIT) }) }}
      </view>
    </view>

    <!-- pprof 未开启 -->
    <view class="panel hint" v-if="data && !data.runtime.pprof_enabled">
      <text class="hint-title">{{ t('diagnostics.pprofOff.title') }}</text>
      <text class="hint-body">{{ t('diagnostics.pprofOff.body') }}</text>
    </view>

    <!-- pprof 已开启 -->
    <view v-if="data && data.runtime.pprof_enabled">
      <view class="panel">
        <view class="panel-head">
          <text class="panel-title">{{ t('diagnostics.profile.title') }}</text>
        </view>
        <view class="tabs">
          <text
            v-for="item in PROFILES"
            :key="item.name"
            class="tab"
            :class="{ 'tab-on': currentProfile === item.name }"
            @click="selectProfile(item.name)"
          >
            {{ item.label }}
          </text>
        </view>
        <scroll-view class="dump" scroll-y>
          <text class="dump-text">{{ profileText || t('common.loading') }}</text>
        </scroll-view>
      </view>

      <view class="panel">
        <view class="panel-head">
          <text class="panel-title">{{ t('diagnostics.download.title') }}</text>
          <text class="panel-sub">{{ t('diagnostics.download.hint') }}</text>
        </view>
        <view class="tabs">
          <text
            v-for="item in DOWNLOADS"
            :key="item.name + (item.seconds || '')"
            class="tab"
            @click="download(item)"
          >
            {{ item.label }}
          </text>
        </view>
      </view>
    </view>

    <view class="error" v-if="error">
      <text>{{ error }}</text>
    </view>

    <TabBar current="diagnostics" />
  </view>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useAuthStore } from '@/stores/auth'
import { fetchDiagnostics, fetchPprofBinary, fetchPprofText } from '@/api/diagnostics'
import type { DiagnosticsData } from '@/api/types'
import AppHeader from '@/components/AppHeader.vue'
import TabBar from '@/components/TabBar.vue'
import { useLocale } from '@/composables/useLocale'

interface ProfileSpec {
  name: string
  label: string
}

interface DownloadSpec extends ProfileSpec {
  seconds?: number
}

/** 可看文本的命名 profile（debug=1 即明文） */
const PROFILES: ProfileSpec[] = [
  { name: 'heap', label: 'heap' },
  { name: 'allocs', label: 'allocs' },
  { name: 'block', label: 'block' },
  { name: 'mutex', label: 'mutex' },
  { name: 'threadcreate', label: 'threadcreate' },
]

/** 可下载的二进制 profile；profile/trace 是流式采集，必须带 seconds */
const DOWNLOADS: DownloadSpec[] = [
  { name: 'heap', label: 'heap' },
  { name: 'goroutine', label: 'goroutine' },
  { name: 'allocs', label: 'allocs' },
  { name: 'block', label: 'block' },
  { name: 'mutex', label: 'mutex' },
  { name: 'profile', label: 'CPU 30s', seconds: 30 },
  { name: 'trace', label: 'trace 5s', seconds: 5 },
]

/**
 * 栈全文在页面里的显示上限。
 * 泄漏时栈可能到几 MB，塞进 DOM 会把浏览器卡死；完整内容仍可复制。
 */
const DUMP_DISPLAY_LIMIT = 256 * 1024

const authStore = useAuthStore()
const { t } = useLocale()

const data = ref<DiagnosticsData | null>(null)
const error = ref('')
const showDump = ref(false)
const dumpText = ref('')
const dumpTruncated = ref(false)
const currentProfile = ref('heap')
const profileText = ref('')
const autoRefresh = ref(false)

let timer: ReturnType<typeof setInterval> | null = null

onMounted(() => {
  if (!authStore.isAuthenticated()) {
    uni.reLaunch({ url: '/pages/login/index' })
    return
  }
  reload()
})

// 必须清掉定时器：否则离开页面后它继续打接口，
// 而每次诊断都要 stop-the-world 抓一次全量栈。
onUnmounted(stopAuto)

async function reload() {
  try {
    error.value = ''
    data.value = await fetchDiagnostics()
    // 展开状态要跟着刷新走，否则每次刷新都得重新点一次
    if (showDump.value) {
      await loadDump()
    }
    if (data.value.runtime.pprof_enabled) {
      await loadProfile()
    }
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

async function loadDump() {
  try {
    const full = await fetchDiagnostics(true)
    const dump = full.goroutines.stack_dump || ''
    dumpTruncated.value = dump.length > DUMP_DISPLAY_LIMIT
    dumpText.value = dumpTruncated.value ? dump.slice(0, DUMP_DISPLAY_LIMIT) : dump
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

async function toggleDump() {
  showDump.value = !showDump.value
  if (showDump.value && !dumpText.value) {
    await loadDump()
  }
}

async function copyDump() {
  const full = await fetchDiagnostics(true)
  uni.setClipboardData({
    data: full.goroutines.stack_dump || '',
    success: () => uni.showToast({ title: t('diagnostics.dump.copied'), icon: 'none' }),
  })
}

async function selectProfile(name: string) {
  currentProfile.value = name
  await loadProfile()
}

async function loadProfile() {
  try {
    profileText.value = await fetchPprofText(currentProfile.value, 1)
  } catch (e) {
    profileText.value = ''
    error.value = e instanceof Error ? e.message : String(e)
  }
}

async function download(item: DownloadSpec) {
  // #ifdef H5
  try {
    const buffer = await fetchPprofBinary(item.name, item.seconds)
    const blob = new Blob([buffer], { type: 'application/octet-stream' })
    const url = URL.createObjectURL(blob)
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = `${item.name}.pprof`
    document.body.appendChild(anchor)
    anchor.click()
    document.body.removeChild(anchor)
    URL.revokeObjectURL(url)
    uni.showToast({ title: t('diagnostics.download.done', { file: `${item.name}.pprof` }), icon: 'none' })
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
  // #endif
  // #ifndef H5
  uni.showToast({ title: t('diagnostics.download.h5Only'), icon: 'none' })
  // #endif
}

function toggleAuto() {
  if (autoRefresh.value) {
    stopAuto()
    return
  }
  autoRefresh.value = true
  timer = setInterval(reload, 10000)
}

function stopAuto() {
  autoRefresh.value = false
  if (timer) {
    clearInterval(timer)
    timer = null
  }
}

/**
 * 只压缩**显示**，不改数据。
 * 帧名形如 good-review-master/pool.(*Pool).worker，模块前缀在窄表格里是纯噪音；
 * 分组用的键仍是服务端给的完整名，压缩显示不会导致错误合并。
 */
function shortenFrame(frame: string): string {
  const parenIndex = frame.indexOf('(')
  const head = parenIndex === -1 ? frame : frame.slice(0, parenIndex)
  const slash = head.lastIndexOf('/')
  return slash === -1 ? frame : frame.slice(slash + 1)
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
}

function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  return `${Math.floor(seconds / 3600)}h${Math.floor((seconds % 3600) / 60)}m`
}
</script>

<style scoped>
.page {
  min-height: 100vh;
  padding: 12px;
  /* 底部给固定 tabbar 留位 */
  padding-bottom: calc(76px + env(safe-area-inset-bottom));
}

/* 概览卡片 */
.cards {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  margin-bottom: 12px;
}
.card {
  flex: 1 1 40%;
  background: #fff;
  border-radius: 10px;
  padding: 14px;
  box-shadow: 0 1px 4px rgba(0, 0, 0, 0.06);
  display: flex;
  flex-direction: column;
}
.card-num {
  font-size: 20px;
  font-weight: 600;
  color: #1a1a2e;
}
.card-label {
  font-size: 12px;
  color: #999;
  margin-top: 2px;
}

.meta {
  font-size: 12px;
  color: #888;
  margin-bottom: 12px;
  word-break: break-all;
}

/* 操作栏 */
.toolbar {
  display: flex;
  gap: 8px;
  margin-bottom: 12px;
}
.btn {
  font-size: 13px;
  padding: 6px 14px;
  border-radius: 6px;
  background: linear-gradient(135deg, #1a1a2e, #16213e);
  color: #fff;
}
.btn-on {
  background: #e74c3c;
}

/* 面板 */
.panel {
  background: #fff;
  border-radius: 10px;
  padding: 14px;
  margin-bottom: 12px;
  box-shadow: 0 1px 4px rgba(0, 0, 0, 0.06);
}
.panel-head {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  margin-bottom: 10px;
}
.panel-title {
  font-size: 15px;
  font-weight: 600;
  color: #1a1a2e;
}
.panel-sub {
  font-size: 12px;
  color: #999;
  margin-left: 8px;
}

/* 分组表格 */
.group-row {
  display: flex;
  align-items: center;
  padding: 6px 0;
  border-bottom: 1px solid #f2f3f5;
}
.group-count {
  width: 46px;
  font-size: 14px;
  font-weight: 600;
  color: #e74c3c;
  flex-shrink: 0;
}
.group-frame {
  flex: 1;
  font-size: 12px;
  color: #333;
  word-break: break-all;
}
.group-state {
  font-size: 11px;
  color: #999;
  margin-left: 8px;
  flex-shrink: 0;
}

/* 标签 */
.tabs {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 10px;
}
.tab {
  font-size: 12px;
  padding: 4px 10px;
  border-radius: 6px;
  background: #f0f2f5;
  color: #555;
}
.tab-on {
  background: #1a1a2e;
  color: #fff;
}

/* 长文本 */
.dump {
  max-height: 320px;
  margin-top: 10px;
  background: #1a1a2e;
  border-radius: 8px;
  padding: 10px;
}
.dump-text {
  font-size: 11px;
  color: #c8d0e0;
  /* pre-wrap：保留缩进与空白（栈帧的 file:line 靠 tab 对齐），同时允许折行 */
  white-space: pre-wrap;
  word-break: break-all;
}

.note {
  font-size: 11px;
  color: #b8860b;
  margin-top: 6px;
}

/* 提示与错误 */
.hint {
  border-left: 3px solid #f0ad4e;
}
.hint-title {
  font-size: 14px;
  font-weight: 600;
  color: #b8860b;
}
.hint-body {
  display: block;
  font-size: 12px;
  color: #666;
  margin-top: 6px;
  line-height: 1.7;
}
.error {
  color: #e74c3c;
  font-size: 13px;
  padding: 10px;
  word-break: break-all;
}
</style>
