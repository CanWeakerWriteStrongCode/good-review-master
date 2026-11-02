<template>
  <view class="app-header">
    <view class="header-left">
      <view class="back-btn" v-if="back" @click="goBack">
        <text class="back-arrow">‹</text>
      </view>

      <view class="brand-badge">
        <text class="brand-initials">DM</text>
      </view>

      <view class="brand-text">
        <text class="brand-name">Dolphin Mind</text>
        <text class="brand-sub">{{ t('nav.brandSub') }}</text>
      </view>
    </view>

    <view class="header-right">
      <text class="header-title" v-if="title">{{ title }}</text>
      <!-- 按钮显示的是「点下去会变成哪种语言」，当前语言从副标题就能看出来 -->
      <view class="locale-btn" @click="toggle">
        <text class="locale-text">{{ isZh ? 'EN' : '中文' }}</text>
      </view>
    </view>
  </view>
</template>

<script setup lang="ts">
import { toRef } from 'vue'
import { useLocale, usePageTitle } from '@/composables/useLocale'

const props = withDefaults(defineProps<{ title?: string; back?: boolean }>(), {
  title: '',
  back: false,
})

const { t, isZh, toggle } = useLocale()

// 顶栏显示页名，顺便把浏览器标签页也对齐；没传 title 的页面就只切语言
const pageTitle = toRef(props, 'title')
usePageTitle(() => pageTitle.value)

function goBack() {
  uni.navigateBack({
    // 直接打开这一页时栈里只有它自己，退回监控页兜底
    fail: () => uni.reLaunch({ url: '/pages/monitor/index' }),
  })
}
</script>

<style scoped>
/* 通栏深色条：用负 margin 抵消父页面 .page 的 12px 内边距，
   页面内容滚动时靠 sticky 吸顶 */
.app-header {
  position: sticky;
  top: 0;
  z-index: 50;
  margin: -12px -12px 12px;
  padding: 10px 14px;
  padding-top: calc(10px + var(--status-bar-height, 0px));
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  background: linear-gradient(135deg, #1a1a2e 0%, #16213e 55%, #1d3557 100%);
}

.header-left {
  display: flex;
  align-items: center;
  gap: 9px;
  min-width: 0;
}
.back-btn {
  width: 26px;
  height: 26px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}
.back-arrow {
  color: #fff;
  font-size: 26px;
  line-height: 1;
  margin-top: -4px;
}

/* 左侧徽标：小方块里放斜体 DM，和右边字体呼应 */
.brand-badge {
  width: 30px;
  height: 30px;
  border-radius: 9px;
  background: linear-gradient(135deg, #7ec8e3, #4a9fd4);
  box-shadow: 0 2px 8px rgba(74, 159, 212, 0.45);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}
.brand-initials {
  color: #fff;
  font-size: 12px;
  font-weight: 700;
  letter-spacing: 0.5px;
  font-family: Georgia, 'Times New Roman', serif;
  font-style: italic;
}

.brand-text {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
/* 艺术字：衬线 + 斜体 + 斜向渐变（白→浅蓝）。
   不支持 background-clip:text 的环境退回纯白，不然会整行看不见。 */
.brand-name {
  color: #fff;
  font-family: Georgia, 'Times New Roman', 'Songti SC', serif;
  font-style: italic;
  font-weight: 700;
  font-size: 16px;
  letter-spacing: 0.7px;
  line-height: 1.15;
  white-space: nowrap;
}
@supports (-webkit-background-clip: text) or (background-clip: text) {
  .brand-name {
    background: linear-gradient(100deg, #ffffff 0%, #e8f8ff 35%, #7ec8e3 100%);
    -webkit-background-clip: text;
    background-clip: text;
    color: transparent;
  }
}
.brand-sub {
  font-size: 10px;
  color: #8fa0b4;
  letter-spacing: 0.4px;
  margin-top: 2px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* 右侧：当前页名 + 语言切换。能压则压，不能让品牌名被挤掉 */
.header-right {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}
.header-title {
  font-size: 13px;
  color: #aab;
  flex-shrink: 1;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.locale-btn {
  min-width: 30px;
  height: 22px;
  padding: 0 7px;
  border-radius: 11px;
  border: 1px solid rgba(255, 255, 255, 0.28);
  background: rgba(255, 255, 255, 0.08);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}
.locale-btn:active {
  background: rgba(255, 255, 255, 0.2);
}
.locale-text {
  font-size: 11px;
  font-weight: 600;
  color: #dbe7f2;
  letter-spacing: 0.3px;
}
</style>
