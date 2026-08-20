// Playwright globalSetup：启动测试服务前构建前端(如缺失)、构建测试二进制、写测试 config.yaml。
// webServer 随后直接以前台方式运行该二进制（cwd=.workdir），
// Playwright 终止时连坐杀掉的是 Go 进程本身，不会残留孤儿进程。
import { execSync } from 'node:child_process'
import { existsSync, mkdirSync, copyFileSync, rmSync } from 'node:fs'
import path from 'node:path'

const e2eDir = __dirname
const repoRoot = path.resolve(e2eDir, '../..')
const frontendDir = path.join(repoRoot, 'web/frontend')
const embedDir = path.join(repoRoot, 'web/server/static/frontend')
const workdir = path.join(e2eDir, '.workdir')
const exeName = process.platform === 'win32' ? 'good-review-test.exe' : 'good-review-test'

function copyDir(src, dest) {
  rmSync(dest, { recursive: true, force: true })
  mkdirSync(dest, { recursive: true })
  if (process.platform === 'win32') {
    execSync(`xcopy /e /i /y "${src}" "${dest}"`, { stdio: 'inherit' })
  } else {
    execSync(`cp -r "${src}/." "${dest}/"`, { stdio: 'inherit' })
  }
}

export default async function globalSetup() {
  // 1. 确保内嵌前端存在（缺失或 FORCE_FRONTEND_BUILD=1 时重建）
  if (process.env.FORCE_FRONTEND_BUILD === '1' || !existsSync(path.join(embedDir, 'index.html'))) {
    execSync('pnpm run build:h5', { cwd: frontendDir, stdio: 'inherit' })
    copyDir(path.join(frontendDir, 'dist/build/h5'), embedDir)
  }

  // 2. 构建测试二进制（内嵌当前前端）到 workdir
  mkdirSync(workdir, { recursive: true })
  execSync(`go build -o "${path.join(workdir, exeName)}" .`, { cwd: repoRoot, stdio: 'inherit' })

  // 3. 写入测试 config.yaml（必须先写，否则 InitDefaultFiles 会创建模板并退出）
  copyFileSync(path.join(e2eDir, 'fixtures/config.yaml'), path.join(workdir, 'config.yaml'))

  // 4. 清理上次运行残留的 prompt_custom.yaml
  rmSync(path.join(workdir, 'prompt_custom.yaml'), { force: true })

  console.log(`[global-setup] binary ready at ${path.join(workdir, exeName)}`)
}
