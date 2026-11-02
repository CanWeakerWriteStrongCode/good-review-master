<template>
  <view class="tabbar">
    <view
      v-for="item in items"
      :key="item.key"
      class="tabbar-item"
      :class="{ active: item.key === current }"
      @click="switchTo(item)"
    >
      <image class="tabbar-icon" :src="iconOf(item.key, item.key === current)" />
      <text class="tabbar-label">{{ t(item.labelKey) }}</text>
    </view>
  </view>
</template>

<script setup lang="ts">
import { useLocale } from '@/composables/useLocale'

interface TabItem {
  key: string
  // 存 key 而不是译文：文案跟着语言切换，数组里的字符串只能是 key
  labelKey: string
  url: string
}

const props = defineProps<{ current: string }>()
const { t } = useLocale()

const items: TabItem[] = [
  { key: 'monitor', labelKey: 'nav.monitor', url: '/pages/monitor/index' },
  { key: 'alerts', labelKey: 'nav.alerts', url: '/pages/alerts/index' },
  { key: 'diagnostics', labelKey: 'nav.diagnostics', url: '/pages/diagnostics/index' },
]

// 图标走内联 SVG 的 data URI：不用引图标库、也不用往 static 里塞图片，
// 选中与否只换描边颜色，跟下面的文字同时变色。
const PATHS: Record<string, string> = {
  // 带折线的屏幕：监控
  monitor:
    'M3 17V7a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2zM6 14l3.5-4 3 2.5L18 7',
  // 三角感叹号：报警
  alerts: 'M10.3 4.1a2 2 0 0 1 3.4 0l7.5 13a2 2 0 0 1-1.7 3H4.5a2 2 0 0 1-1.7-3l7.5-13zM12 9.5v4M12 17.2v.1',
  // 心跳线：诊断
  diagnostics: 'M3 12h3.5l2.2-5 3.1 10 2.4-6.5 1.4 1.5H21',
}

function iconOf(key: string, active: boolean): string {
  // 选中是蓝底白字，图标也得跟着变白，否则蓝底上看不见。
  const color = active ? '#ffffff' : '#6b7280'
  const path = PATHS[key] || PATHS.diagnostics
  const svg =
    `<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 24 24' width='24' height='24' fill='none' stroke='${color}' stroke-width='1.9' stroke-linecap='round' stroke-linejoin='round'><path d='${path}'/></svg>`
  // 必须 encode：不编码的话 # 会被当成 URL 片段，颜色直接失效。
  return `data:image/svg+xml;utf8,${encodeURIComponent(svg)}`
}

function switchTo(item: TabItem) {
  if (item.key === props.current) return
  // 三个主页面是并列关系，reLaunch 把页面栈清干净，
  // 免得从监控页点进群列表、再点进消息详情后，切回底部 tab 还能看到返回箭头。
  uni.reLaunch({ url: item.url })
}
</script>

<style scoped>
/* 整体是一条浅灰的底条，两个按钮各自是一块白底圆角块，
   中间留缝 + 边框，边界一眼能看出来；选中那块反过来变成蓝底白字。 */
.tabbar {
  position: fixed;
  left: 0;
  right: 0;
  bottom: 0;
  display: flex;
  gap: 8px;
  background: #eef0f4;
  border-top: 1px solid #e3e6ec;
  box-shadow: 0 -2px 10px rgba(0, 0, 0, 0.05);
  padding: 8px 12px;
  padding-bottom: calc(8px + env(safe-area-inset-bottom));
  z-index: 100;
}
.tabbar-item {
  flex: 1;
  display: flex;
  flex-direction: row;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: 9px 0;
  background: #fff;
  border: 1px solid #dde1e8;
  border-radius: 10px;
  box-shadow: 0 1px 2px rgba(0, 0, 0, 0.03);
}
.tabbar-item:active {
  background: #f6f8fb;
}
.tabbar-item.active {
  background: #4a9fd4;
  border-color: #3d8cbd;
  box-shadow: 0 2px 8px rgba(74, 159, 212, 0.35);
}
.tabbar-item.active:active {
  background: #3d8cbd;
}
.tabbar-icon {
  width: 18px;
  height: 18px;
}
.tabbar-label {
  font-size: 13px;
  color: #6b7280;
}
.tabbar-item.active .tabbar-label {
  color: #fff;
  font-weight: 600;
}
</style>
