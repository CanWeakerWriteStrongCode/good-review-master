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

/*
 * uni 的 H5 基础样式把 html/body 设成 user-select: none，那是为原生 App 手感准备的。
 * 这套面板是在浏览器里读记录、复制群号和消息正文的，必须能选中。
 * 直接命中 page / text 的规则优先于 html/body 继承下来的 none，不用 !important。
 */
page,
text {
  -webkit-user-select: text;
  -moz-user-select: text;
  user-select: text;
}
</style>
