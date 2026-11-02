import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vitest/config'

/**
 * 单独一份 vitest 配置。
 *
 * vite.config.ts 里挂着 uni 的插件，它要求依赖树里有 vue/compiler-sfc
 * （uni 自己管着这份依赖，pnpm 下外部拿不到），跑测试会直接启动失败。
 * 我们的测试只做纯 TS 比对和源码扫描、不需要编译单文件组件，
 * 而 vitest 优先读 vitest.config.ts，所以测试不会走到 uni 插件上。
 */
export default defineConfig({
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  test: {
    include: ['src/**/*.test.ts'],
    // 测的是语言包这类字符串，不碰 DOM
    environment: 'node',
  },
})
