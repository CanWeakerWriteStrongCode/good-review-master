import { i18n } from '@/locale'

/**
 * 时间戳 → 当前语言的日期时间。
 *
 * 后端只给 Unix 秒，格式化放在前端：中文要「2026-11-02 16:28:31」，
 * 英文要「Nov 2, 2026, 16:28」，同一份数据两种写法。
 * 让后端按 Accept-Language 自己拼字符串，等于把语言逻辑摊在两个进程里，
 * 改一处要动两处，所以接口只传原始值。
 */

type Style = 'full' | 'short'

const OPTIONS: Record<Style, Intl.DateTimeFormatOptions> = {
  full: {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  },
  short: {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  },
}

// Intl.DateTimeFormat 构造一次不便宜，列表里一屏几十条，按「语言+样式」缓存
const formatters = new Map<string, Intl.DateTimeFormat>()

function formatter(locale: string, style: Style): Intl.DateTimeFormat {
  const key = `${locale}:${style}`
  const hit = formatters.get(key)
  if (hit) return hit
  const built = new Intl.DateTimeFormat(locale, OPTIONS[style])
  formatters.set(key, built)
  return built
}

/**
 * 只取各段的值，分隔符自己拼。
 * 中文 Intl 默认把日期写成 2026/11/02，和页面其它地方的横杠不一致；
 * 拼 parts 能既用 Intl 的数字规则，又保持标点统一。
 */
function segments(locale: string, date: Date, style: Style): Record<string, string> {
  const out: Record<string, string> = {}
  for (const part of formatter(locale, style).formatToParts(date)) {
    if (part.type !== 'literal') out[part.type] = part.value
  }
  return out
}

// 读的是 i18n 的 locale ref，所以渲染里调用它会建立依赖：切语言自动重算
function currentLocale(): string {
  return (i18n.global.locale.value as string) || 'zh-CN'
}

/** 2026-11-02 16:28:31 / Nov 2, 2026, 16:28；0 表示没有记录 */
export function formatDateTime(ts: number): string {
  if (!ts) return '-'
  const date = new Date(ts * 1000)
  if (currentLocale().startsWith('zh')) {
    const p = segments('zh-CN', date, 'full')
    return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}`
  }
  const p = segments('en-US', date, 'short')
  return `${p.month} ${p.day}, ${p.year}, ${p.hour}:${p.minute}`
}
