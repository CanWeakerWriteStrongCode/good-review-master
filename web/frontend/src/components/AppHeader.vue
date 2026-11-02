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
        <text class="brand-sub">海豚智章 · 监控面板</text>
      </view>
    </view>

    <text class="header-title" v-if="title">{{ title }}</text>
  </view>
</template>

<script setup lang="ts">
withDefaults(defineProps<{ title?: string; back?: boolean }>(), {
  title: '',
  back: false,
})

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
}

.header-title {
  font-size: 13px;
  color: #aab;
  flex-shrink: 0;
}
</style>
