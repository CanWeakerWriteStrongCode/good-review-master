// 在 playwright test 之前同步准备测试环境（由 pnpm test 脚本调用）。
//
// 为什么必须放在这里而不是 Playwright 的 globalSetup：
//   Playwright 的 webServer 先于 globalSetup 启动。若在 globalSetup 里才构建二进制、
//   写 config.yaml，webServer 启动二进制时可能读到上一轮残留的旧二进制/旧配置
//   （本用例就踩过：残留 config 的 llm_send_count=200 被当作最新配置加载）。
//   把准备挪到 npm 脚本里，保证 playwright 启动前 .workdir 已是干净、确定的状态。
import { execSync } from 'node:child_process'
import { existsSync, mkdirSync, copyFileSync, rmSync } from 'node:fs'
import path from 'node:path'

const e2eDir = path.resolve(import.meta.dirname, '..')
const repoRoot = path.resolve(e2eDir, '../..')
const frontendDir = path.join(repoRoot, 'web/frontend')
const embedDir = path.join(repoRoot, 'web/server/static/frontend')
const workdir = path.join(e2eDir, '.workdir')
const exeName = process.platform === 'win32' ? 'good-review-test.exe' : 'good-review-test'
const exePath = path.join(workdir, exeName)

function copyDir(src, dest) {
  rmSync(dest, { recursive: true, force: true })
  mkdirSync(dest, { recursive: true })
  if (process.platform === 'win32') {
    execSync(`xcopy /e /i /y "${src}" "${dest}"`, { stdio: 'inherit' })
  } else {
    execSync(`cp -r "${src}/." "${dest}/"`, { stdio: 'inherit' })
  }
}

mkdirSync(workdir, { recursive: true })

// 1. 确保内嵌前端存在（缺失或 FORCE_FRONTEND_BUILD=1 时重建）——必须在 go build 之前
if (process.env.FORCE_FRONTEND_BUILD === '1' || !existsSync(path.join(embedDir, 'index.html'))) {
  execSync('pnpm run build:h5', { cwd: frontendDir, stdio: 'inherit' })
  copyDir(path.join(frontendDir, 'dist/build/h5'), embedDir)
}

// 2. 写入测试 config.yaml，覆盖任何残留旧配置
copyFileSync(path.join(e2eDir, 'fixtures/config.yaml'), path.join(workdir, 'config.yaml'))

// 3. 清理上次运行残留的 prompt_custom.yaml（避免脏关键字污染）
rmSync(path.join(workdir, 'prompt_custom.yaml'), { force: true })

// 4. 构建测试二进制（内嵌当前前端）
execSync(`go build -o "${exePath}" .`, { cwd: repoRoot, stdio: 'inherit' })

console.log(`[prep] binary ready at ${exePath}`)
