import { readFileSync, readdirSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import zhCN from '../zh-CN'

/**
 * 两件运行期不会报错、但一定会翻车的事，在这里卡住：
 *
 * 1. 代码里 `t('a.b.c')` 的 key 在语言包里不存在 —— 页面直接显示 key 本身。
 * 2. 源码里还留着一句中文字面量 —— 切到英文后它纹丝不动。
 */

const SRC = fileURLToPath(new URL('../..', import.meta.url))
const LOCALE_DIR = join(SRC, 'locale')

type Tree = { [key: string]: string | Tree }

function flatten(node: Tree, prefix = ''): string[] {
  const out: string[] = []
  for (const [key, value] of Object.entries(node)) {
    const path = prefix ? `${prefix}.${key}` : key
    if (typeof value === 'string') out.push(path)
    else out.push(...flatten(value, path))
  }
  return out
}

/** 语言包里所有可用的 key */
const knownKeys = new Set(flatten(zhCN as unknown as Tree))

function sourceFiles(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name)
    if (entry.isDirectory()) {
      if (full === LOCALE_DIR || entry.name === 'node_modules') continue
      out.push(...sourceFiles(full))
    } else if (entry.name.endsWith('.ts') || entry.name.endsWith('.vue')) {
      out.push(full)
    }
  }
  return out
}

function posix(path: string): string {
  return relative(SRC, path).split(sep).join('/')
}

/** 去掉 JS 与 HTML 注释，只留会真正执行的代码；否则中文注释会被当成漏网文案 */
function stripComments(code: string): string {
  return code
    .replace(/<!--[\s\S]*?-->/g, '')
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|[^:])\/\/.*$/gm, '$1')
}

const files = sourceFiles(SRC).map((path) => ({ path: posix(path), code: stripComments(readFileSync(path, 'utf8')) }))

/**
 * 允许保留的中文。
 * 「中文」是语言切换按钮的字面。语言名按惯例写它自己那种文字、不翻译：
 * 英文界面下点「中文」、中文界面下点 EN，两边都看得懂。
 */
const ALLOWED = new Set(['中文'])

describe('语言包使用情况', () => {
  it('t() / tt() 用到的 key 都在语言包里', () => {
    // \b 保证不会把 split( clone( 这类名字里带 t 的函数算进来
    const used = /\b(?:tt|t)\(\s*(['"])([\w.]+)\1/g
    const missing: string[] = []
    for (const file of files) {
      for (const match of file.code.matchAll(used)) {
        if (!knownKeys.has(match[2])) missing.push(`${file.path}: ${match[2]}`)
      }
    }
    expect(missing).toEqual([])
  })

  it('语言包里没有用不到的 key', () => {
    // 反方向：页面改完忘了删旧文案，语言包会越积越脏，两种语言都得改两遍
    const used = new Set<string>()
    for (const file of files) {
      for (const match of file.code.matchAll(/\b(?:tt|t)\(\s*(['"])([\w.]+)\1/g)) used.add(match[2])
    }
    const unused = [...knownKeys].filter((key) => !used.has(key))
    expect(unused).toEqual([])
  })

  it('源码里没有残留中文字面量', () => {
    // 引号后面跟中文字符、再以同类引号收尾，就是一句没搬进语言包的文案
    const literal = /(['"`])([^'"`\n]*[\u4e00-\u9fff][^'"`\n]*)\1/g
    const leftovers: string[] = []
    for (const file of files) {
      // console.* 只给开发者看，不进界面，跟后端日志一样用中文
      const code = file.code
        .split('\n')
        .filter((line) => !line.includes('console.'))
        .join('\n')
      for (const match of code.matchAll(literal)) {
        if (ALLOWED.has(match[2])) continue
        leftovers.push(`${file.path}: ${match[2]}`)
      }
    }
    expect(leftovers).toEqual([])
  })
})
