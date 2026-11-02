import { describe, expect, it } from 'vitest'
import zhCN from '../zh-CN'
import enUS from '../en-US'

/**
 * 两份语言包必须长成一模一样的树。
 *
 * 少一个 key 的后果是运行期缺译文：vue-i18n 会 silently fallback 到中文，
 * 界面上就冒出一句中文夹在英文里，构建和类型检查都不会报错。所以用测试卡住。
 */

type Tree = { [key: string]: string | Tree }

/** 把嵌套对象压成 `a.b.c` 的平表，值都是最终文案 */
function flatten(node: Tree, prefix = ''): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [key, value] of Object.entries(node)) {
    const path = prefix ? `${prefix}.${key}` : key
    if (typeof value === 'string') out[path] = value
    else Object.assign(out, flatten(value, path))
  }
  return out
}

/** 取 `{name}` 这类占位符，去重排序，方便直接比数组 */
function placeholders(text: string): string[] {
  const found = [...text.matchAll(/\{(\w+)\}/g)].map((m) => m[1])
  return [...new Set(found)].sort()
}

const zh = flatten(zhCN as unknown as Tree)
const en = flatten(enUS as unknown as Tree)

describe('语言包对齐', () => {
  it('中英 key 完全一致', () => {
    const onlyZh = Object.keys(zh).filter((key) => !(key in en))
    const onlyEn = Object.keys(en).filter((key) => !(key in zh))
    expect({ onlyZh, onlyEn }).toEqual({ onlyZh: [], onlyEn: [] })
  })

  it('同一个 key 用的占位符一致', () => {
    // 中文写了 {count}、英文漏了，英文那句就会少一个数字，而且不报错
    const mismatched = Object.keys(zh)
      .filter((key) => key in en)
      .map((key) => ({ key, zh: placeholders(zh[key]).join(), en: placeholders(en[key]).join() }))
      .filter((row) => row.zh !== row.en)
    expect(mismatched).toEqual([])
  })

  it('没有空文案', () => {
    const empty = [...Object.keys(zh), ...Object.keys(en)].filter(
      (key) => (zh[key] ?? '').trim() === '' || (en[key] ?? '').trim() === '',
    )
    expect(empty).toEqual([])
  })
})
