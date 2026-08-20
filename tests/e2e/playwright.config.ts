import { defineConfig } from '@playwright/test'
import path from 'node:path'

// 纯 API 测试：不启动浏览器，通过 request fixture 直打 HTTP 接口。
// 所有用例共享同一个运行中的测试二进制（进程内缓存/锚点是全局的），
// 因此必须串行执行（workers: 1），用例间用 /api/debug/reset 隔离。

const isWin = process.platform === 'win32'
const exeName = isWin ? 'good-review-test.exe' : 'good-review-test'
const workdir = path.join(__dirname, '.workdir')
// 用绝对路径避免 Windows cmd 解析裸文件名的坑
const exePath = path.join(workdir, exeName)

export default defineConfig({
  // 准备（构建二进制 + 写 config.yaml）由 scripts/prep.mjs 在 playwright 启动前完成——
  // 见 package.json 的 test 脚本。webServer 先于 globalSetup 启动，准备不能放 globalSetup。
  testDir: './specs',
  timeout: 30_000,
  workers: 1,
  fullyParallel: false,
  use: {
    baseURL: 'http://localhost:9090',
  },
  reporter: [
    ['list'],
    ['html', { outputFolder: 'playwright-report' }],
  ],
  webServer: {
    // 直接用绝对路径前台运行二进制；cwd=.workdir 让 Go 进程按 cwd 解析 config.yaml。
    // Playwright 直接管理 Go 进程，teardown 连坐杀死，不留孤儿。
    // reuseExistingServer 固定 false：残留进程可能加载旧配置，必须每次全新启动。
    command: exePath,
    cwd: workdir,
    url: 'http://localhost:9090/',
    reuseExistingServer: false,
    timeout: 60_000,
    env: {
      GOOD_REVIEW_TEST: '1',
    },
  },
})
