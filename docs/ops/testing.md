# 测试

三层，各管各的问题，不要混着用：

| 层 | 在哪 | 回答什么问题 | 能不能进 CI 的必过项 |
| --- | --- | --- | --- |
| 单元测试 | 各包的 `*_test.go` | 这段逻辑对不对 | 能 |
| 端到端 | `tests/e2e` | 整套跑起来，接口还通不通 | 能（需要 Node） |
| 提示词评测 | `tests/eval` | 改了提示词或换了模型，回复质量有没有退化 | 不能（要真实模型，结果只报告） |

## 单元测试

```bash
go test ./...          # 全部
go test ./state/ -v    # 一个包
go vet ./...           # 编译期以外的问题
gofmt -l .             # 格式
```

约定：

- **用真 SQLite**，不 mock 库。每个用例开一个临时目录里的 `state.db`，跑完删掉。mock 出来的库不会暴露"自增号被烧"这类问题。
- **模型用假实现**：`internal/testutil/fake_llm.go`，可以脚本化返回内容或错误。
- **不起真实 HTTP 服务**的地方用 `Handler()` / `newRouter` 拿 `http.Handler` 直接打。
- 时间相关的逻辑尽量注入时钟，不要 `time.Sleep` 等真实时间。

改 `state` 包时，这三个测试必须绿——它们守的是"自增号不白烧"这条规则：

```
Test群统计不再白烧自增号
Test重复写入不再白烧其余表的自增号
Test单行写入不再白烧自增号
```

建库只支持全新库以后，原来那些"从 vN 升级到最新"的测试都删了。新增表结构不需要写升级测试，但要确认 `migrate` 在旧库上仍然报错而不是静默建表。

## 端到端

`tests/e2e` 是 Playwright，但**不启浏览器**：只用 `request` fixture 打 API。

```bash
cd tests/e2e
pnpm install
pnpm test            # 见 package.json 的 test 脚本
```

跑之前会自动做两件事：

1. `scripts/prep.mjs` 编出测试二进制 `good-review-test(.exe)`，写测试用的 `config.yaml` / `prompt_system.yaml` / `secret.yaml` 到 `.workdir`。
2. Playwright 用 `webServer` 直接前台启动这个二进制，`cwd=.workdir`，带 `GOOD_REVIEW_TEST=1`，端口 9090。

几条固定约束：

- `workers: 1`。所有用例共享同一个进程，进程内的缓存和锚点是全局的，并发跑会互相干扰。
- `reuseExistingServer: false`。残留进程可能加载旧配置，宁可多花几十秒重新起。
- 用例之间用 `/api/debug/reset` 隔离，`scripts/global-setup.ts` 先推一条事件把工作空间开通，后续 debug 接口才有对象可操作。
- 这些 `/api/debug/*` 接口只在测试模式下存在，正常运行时没有。

覆盖的用例：登录与鉴权、群列表、消息、人格切换、诊断信息、缓存成本曲线、完整机器人流程。

## 提示词评测

```bash
go run ./tests/eval            # 规则检查 + 与基线比对
go run ./tests/eval -update    # 确认变化就是要的，写新基线
go run ./tests/eval -dry       # 只打印将要发送的提示词，不调模型
```

在仓库根目录跑，复用主程序的 `config.yaml` / `secret.yaml` / `prompt_system.yaml`（模型、采样参数、API Key 都从这里读）。

判定方式分两半：

- **规则检查是硬失败**：字数上下限、必须出现、不许出现。退出码 1 就说明明显退化了。
- **与 `baseline.json` 的差异只报告，不判失败**。模型输出每次都不一样，把差异当失败只会让人关掉这项检查。

基线里记了 `prompt_sha256`、`model`、真实 token 用量和记录时间。有了提示词指纹才能分清"输出变了是因为我改了提示词"和"上游模型漂了"——这两种情况的处置完全相反。

用例通过 `category` + `keyword` 从 `prompt_system.yaml` 里取提示词，走的是线上同一对组装函数（`ComposeSystemPrompt`、`BuildUserMsg`）。所以别在用例里内联提示词：那样会出现"评测全绿、线上回归"。

已知边界：只覆盖命中关键字的指令路由；未命中关键字时的纯 @ 聊天没有 category/keyword 可引用，不在评测范围内。也刻意不做 LLM-as-judge——没有标注样本时，那只是把不确定性从一个模型搬到另一个模型。

## 加测试时的判断

| 你想验证的 | 放哪 | 不该做什么 |
| --- | --- | --- |
| 一个函数的边界条件 | 同包的 `*_test.go` | 不要为此起整个 app |
| 一条消息从入站到回复 | `tests/e2e` | 不要在单测里手搓三段流水线 |
| 回复读起来对不对 | `tests/eval` | 不要用 `assert.Contains` 断言文风 |
| 表结构建得对不对 | `state` 包单测 | 不要写"从旧版升级"的测试（没有升级） |

## 代码位置

| 路径 | 看什么 |
| --- | --- |
| `state/state_test.go` / `features_test.go` | 建库、写入规则、缓存与统计 |
| `internal/testutil/fake_llm.go` | 假模型的可控行为 |
| `tests/e2e/scripts/prep.mjs` | 构建测试二进制与 fixture |
| `tests/e2e/playwright.config.ts` | 串行、端口、启动方式 |
| `tests/e2e/specs/` | 各接口的用例 |
| `tests/eval/main.go` | 用例加载、规则检查、基线比对 |
| `tests/eval/cases/` | 用例定义 |
