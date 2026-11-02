<script setup lang="ts">
import { onLaunch } from '@dcloudio/uni-app'
import { applyLocale, i18n, type AppLocale } from './locale'

onLaunch(() => {
  // uni 启动后 uni.setLocale 才可用：把首屏选定的语言同步给它自带组件
  // （内置弹窗的按钮），并设好 html lang。
  // 标签页标题不在这里管，由每个页面的 usePageTitle 跟着页名换。
  applyLocale(i18n.global.locale.value as AppLocale)
})
</script>

<style>
/* 全局样式 */
page {
  background-color: #f0f2f5;
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', 'Microsoft YaHei', sans-serif;
  color: #333;
  line-height: 1.6;
}
</style>
