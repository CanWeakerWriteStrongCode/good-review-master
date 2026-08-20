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
| `POST /api/debug/trigger` | 模拟 @机器人 触发一次指令（走完整 bot 链路） |

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
1. `globalSetup`（`global-setup.ts`）：若内嵌前端缺失则先 `pnpm run build:h5`（CI 可用 `FORCE_FRONTEND_BUILD=1` 强制重建）；`go build` 测试二进制到 `tests/e2e/.workdir/`；写入 `fixtures/config.yaml`。
2. `webServer` 直接在 `.workdir` 里前台运行该二进制（`GOOD_REVIEW_TEST=1`），随后 Playwright 打接口。

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

用例间通过 `POST /api/debug/reset` 隔离；缓存/锚点是进程内全局，因此用例**串行**执行（`workers: 1`）。
