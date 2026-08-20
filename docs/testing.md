# 测试（Playwright API 测试）

本项目内置一套 **API 层自测**：用 Playwright 作为测试运行器（`request` fixture，**不启动浏览器**），直打真实二进制的 HTTP 接口。覆盖：登录鉴权、群数据、消息数据、bot 全流程（注入→触发锐评→断言回复）。

## 原理

- 测试模式：`GOOD_REVIEW_TEST=1` 启动真实二进制时，用 `FakeLLM`（固定回复并记录调用）替代真实大模型；NapCat 指向死地址（`127.0.0.1:1`），调用失败仅 warn，不致命。
- 测试模式额外注册 `/api/debug/*` 自测接口（**生产模式不注册**——gin 的 SPA fallback 会把这些路径回成页面 HTML，而非调试 JSON）：

| 接口 | 作用 |
|------|------|
| `POST /api/debug/inject` | 向指定群注入假消息填充缓存（可带 `group_name`） |
| `POST /api/debug/reset` | 清空缓存/锚点/fake 状态（用例隔离） |
| `GET /api/debug/state` | dump 缓存、锚点、FakeLLM 调用（供断言） |
| `POST /api/debug/trigger` | 模拟 @机器人 触发一次指令（走完整 bot 链路；**同步等待 handler 收尾再返回**，见「测试进程生命周期」） |

## 环境准备

```bash
# 1. 前端依赖 + 构建（内嵌到 Go 二进制的静态资源）
cd web/frontend && pnpm install && pnpm run build:h5

# 2. e2e 依赖
cd ../..  # 回到仓库根
cd tests/e2e && pnpm install
```

> 纯 API 测试**不需要**安装 Playwright 浏览器（`npx playwright install`）。

## 运行

```bash
cd tests/e2e
pnpm test           # 跑全部用例（自动：构建测试二进制 → 写测试 config → 启动 → 打接口）
pnpm test:headed    # 调试模式
```

`pnpm test` 会做：
1. `scripts/prep.mjs`（`package.json` 的 test 脚本里**先于 playwright 执行**）：若内嵌前端缺失则先 `pnpm run build:h5`（CI 可用 `FORCE_FRONTEND_BUILD=1` 强制重建）；写入 `fixtures/config.yaml` 覆盖残留旧配置；`go build` 测试二进制到 `tests/e2e/.workdir/`。
2. `webServer` 直接在 `.workdir` 里前台运行该二进制（`GOOD_REVIEW_TEST=1`），随后 Playwright 打接口。

> 为什么准备不能放 Playwright 的 `globalSetup`：实测 **webServer 先于 globalSetup 启动**。若在 globalSetup 里才构建二进制/写 config，webServer 启动时会读到 `.workdir` 里上一轮残留的旧二进制/旧配置（本项目的缓存成本用例就曾因此误加载旧 `llm_send_count`）。所以准备放在 npm script 里，保证 playwright 启动前 `.workdir` 已是干净、确定的状态。

测试二进制运行于 `http://localhost:9090`，日志输出到终端（由 Playwright 捕获）。

## 查看报告

跑完生成 HTML 报告：

```bash
cd tests/e2e
npx playwright show-report        # 打开 tests/e2e/playwright-report/index.html
```

## 用例一览

| 文件 | 覆盖 |
|------|------|
| `specs/auth.spec.ts` | 错误密码 401、正确登录拿 token、status 鉴权 |
| `specs/groups.spec.ts` | inject 后群列表显示已缓存/消息数/群名；未注入群无数据 |
| `specs/messages.spec.ts` | 注入消息按序返回；未缓存群返回空 |
| `specs/bot-flow.spec.ts` | inject → trigger 锐评 → 轮询 state → FakeLLM 被调用、回复正确、chat_log 含注入内容 |
| `specs/cache-cost.spec.ts` | 注入超重置窗口（llm_send_count=3）→ 触发：无锚点走**重置**（只发最近 3 条）；再注入再触发：锚点命中走**扩展**（Start 不变、窗口含旧+新消息） |

用例间通过 `POST /api/debug/reset` 隔离；缓存/锚点是进程内全局，因此用例**串行**执行（`workers: 1`）。`debug/trigger` 同步等待 handler 收尾（锚点更新）再返回，保证异步副作用不跨用例泄漏——否则上一用例 handler 的 `SetLLMAnchor` 可能落在下一次 `reset` 之后，污染锚点 `Start`。

## 测试进程生命周期（谁负责启停）

spec 用例只打 HTTP 接口，**不写任何进程管理代码**。测试二进制的启停由 Playwright 的 `webServer` 配置（`playwright.config.ts`）托管：

1. **启动**：`pnpm test` 先由 `scripts/prep.mjs` 构建二进制并写配置（见「运行」节，准备必须早于 playwright），随后 Playwright 直接 spawn 该二进制（`command` 指向 exe，`cwd=.workdir`），轮询 `url`（`http://localhost:9090/`）直到返回 200 视为就绪，才开始执行 spec。
2. **执行**：spec 只请求 `/api/debug/*` 与面板接口；进程内缓存/锚点是全局的，因此用例串行（`workers: 1`）且用 `/api/debug/reset` 隔离。`debug/trigger` 同步等待 handler 收尾，异步副作用（锚点写入）在用例内完成，不跨用例泄漏。
3. **收尾**：全部用例结束（无论成败），Playwright 自动 terminate 它自己启动的子进程并等待退出。Go 侧 `main.go` 的 `signal.NotifyContext` 收到终止信号走优雅关闭（web server 10s 超时、等 in-flight goroutine）；不退出则强制终止进程树（`taskkill /pid X /T /F`），确保 9090 释放。

**为什么不留孤儿进程**：`command` 直接指向 exe（而非经 shell/node 桥接），Playwright 持有子进程句柄，teardown 能直接终止它。若中间隔一层启动脚本，杀掉的只是脚本，Go 进程会变孤儿继续占端口。
