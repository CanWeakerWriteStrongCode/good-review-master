import { createI18n } from 'vue-i18n'
import zhCN from './zh-CN'
import enUS from './en-US'

/** 支持的语言。加语言时这里、messages、AppHeader 的按钮一起改。 */
export const LOCALES = ['zh-CN', 'en-US'] as const
export type AppLocale = (typeof LOCALES)[number]

export const DEFAULT_LOCALE: AppLocale = 'zh-CN'

/** 用户手动选过的语言存在这里；没选过才去猜浏览器 */
const STORAGE_KEY = 'good_review_locale'

/**
 * 初始语言：用户选过 > 浏览器语言 > 中文。
 *
 * 浏览器只认前缀：zh、zh-TW、zh-HK 都算中文，其余一律英文。
 * 这样海外的机器打开面板直接是英文，不用先点一下切换。
 */
export function resolveInitialLocale(): AppLocale {
  try {
    const saved = localStorage.getItem(STORAGE_KEY)
    if (saved && (LOCALES as readonly string[]).includes(saved)) return saved as AppLocale
  } catch {
    // 隐私模式下 localStorage 会抛异常，忽略即可，落到浏览器语言
  }
  if (typeof navigator === 'undefined') return DEFAULT_LOCALE
  const lang = (navigator.language || '').toLowerCase()
  return lang.startsWith('zh') ? 'zh-CN' : 'en-US'
}

export const i18n = createI18n({
  // legacy: false 才是 Composition API 模式：模板里 $t 和 setup 里 useI18n() 都能用
  legacy: false,
  locale: resolveInitialLocale(),
  // 漏翻的文字回落到中文，页面上至少是正常句子而不是 alerts.card.expand 这种 key
  fallbackLocale: DEFAULT_LOCALE,
  messages: {
    'zh-CN': zhCN,
    'en-US': enUS,
  },
  // 开发期把缺 key 打到控制台；生产构建里这两个开关不生效
  missingWarn: true,
  fallbackWarn: true,
})

/**
 * 切语言。
 *
 * 三件事一件都不能少：
 * 1. 改 vue-i18n 的 locale —— 业务文案立刻变（响应式，不用刷新页面）
 * 2. 调 uni.setLocale —— uni 自带组件（内置弹窗的按钮）是另一套 locale，不跟着改会半中半英
 * 3. 写 localStorage —— 下次打开还是这个语言
 *
 * 浏览器标签页标题不在这里改：它跟着当前页名走（见 usePageTitle），
 * 这里换成总标题反而会被页面自己的标题盖掉，白跳一下。
 */
export function applyLocale(next: AppLocale) {
  i18n.global.locale.value = next
  try {
    localStorage.setItem(STORAGE_KEY, next)
  } catch {
    // 存不了就算了，不影响本次切换
  }
  try {
    uni.setLocale(next)
  } catch {
    // 启动早期 uni 还没就绪，正常；首屏业务文案已经由 i18n.locale 决定了
  }
  if (typeof document !== 'undefined') {
    // html lang 影响断行、字体回退和无障碍朗读
    document.documentElement.lang = next
  }
}
