import { computed, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { applyLocale, LOCALES, type AppLocale } from '@/locale'

/**
 * 页面取文案、切语言的统一入口。
 *
 * 不在各处直接调 useI18n()：以后换库或者加第三种语言只改这里；
 * 「切语言时还要顺手拨 uni 自己的 locale」这件事也收在 applyLocale 里，
 * 省得每个调用点各写一遍，漏一个就半中半英。
 */
export function useLocale() {
  const { t, locale } = useI18n()

  const current = computed(() => locale.value as AppLocale)
  const isZh = computed(() => current.value.startsWith('zh'))

  /** 中英来回切：顶栏那个按钮用 */
  function toggle() {
    applyLocale(isZh.value ? 'en-US' : 'zh-CN')
  }

  function setLocale(next: AppLocale) {
    applyLocale(next)
  }

  return { t, locale: current, isZh, toggle, setLocale, LOCALES }
}

/**
 * 把浏览器标签页的标题跟当前页名对齐。
 *
 * uni 每次激活页面都会按 pages.json 里的 navigationBarTitleText 改写 document.title，
 * 那是构建期写死的中文 —— 英文模式下标签页会莫名其妙跳回中文。
 * setNavigationBarTitle 会同时写回 pageMeta，所以再次激活也不会被盖回去。
 *
 * 传函数而不是字符串：里面读到的 locale 是 ref，才能跟着切语言重算。
 */
export function usePageTitle(title: () => string) {
  watchEffect(() => {
    const text = title()
    if (text) uni.setNavigationBarTitle({ title: text })
  })
}
