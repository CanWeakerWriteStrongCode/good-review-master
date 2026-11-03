# 文档

这套文档给改代码的人看，不替代根目录的 [README.md](../README.md)（那是给使用者的）。

## 按目的找

| 你想干什么 | 看这篇 |
| --- | --- |
| 五分钟搞清这个进程在跑什么 | [architecture/overview.md](architecture/overview.md) |
| 部署到一台新机器、查端口和文件 | [architecture/deployment.md](architecture/deployment.md) |
| 知道代码分几层、哪个包管什么 | [architecture/domains.md](architecture/domains.md) |
| 改 IM 接入 | [domains/ingest.md](domains/ingest.md) |
| 改三阶段流水线 | [domains/pipeline.md](domains/pipeline.md) |
| 改回复生成、指令、主动插话 | [domains/speech.md](domains/speech.md) |
| 改多账号、工作空间 | [domains/workspace.md](domains/workspace.md) |
| 改表结构、写入、查询 | [domains/state.md](domains/state.md) |
| 改模型调用、缓存、限速熔断 | [domains/llm.md](domains/llm.md) |
| 改管理面板和 API | [domains/panel.md](domains/panel.md) |
| 搞清某处为什么这样写 | [adr/](adr/) 按主题翻 |
| 对接 OneBot 12、查字段含义 | [reference/im-protocol.md](reference/im-protocol.md) |
| 看指标、跑测试 | [ops/monitoring.md](ops/monitoring.md)、[ops/testing.md](ops/testing.md) |

## 目录

```
docs/
├── architecture/     全局视角：跑起来是什么样、部署在哪、怎么分层
│   ├── overview.md
│   ├── deployment.md
│   └── domains.md
├── domains/          每个域一篇：职责、数据、主流程、失败模式
├── adr/              每个决策一篇：当时的选择、代价、被否的方案
├── reference/        查表用的：协议字段、动作清单
├── ops/              运维手册：指标、测试
└── analysis/         一次性的测算与结论，带日期
```

部署用的抓取配置和面板定义放在仓库根的 `deploy/`，不放文档目录：
[prometheus-scrape.yml](../deploy/prometheus-scrape.yml)、[grafana-dashboard.json](../deploy/grafana-dashboard.json)。

## 写法约定

1. **决策与现状分开**。域文档只写"现在是什么样"，"为什么不是另一种"放 ADR。同一句话不写两遍。
2. **不写行号、不数行数**。指到文件和方法名，例如 `state/schema.go` 的 `migrate`。行数一周就烂。
3. **图跟代码一起改**。时序图和泳道图用 mermaid，放在流程图该在的位置，不单独堆图册。
4. **过程不进文档**。实施顺序、待办清单、验证记录属于 commit message 和 issue，写完就删。
5. **一篇一个主题**。写不下就说明这个域该拆了，不是该写更长。

## ADR 索引

| 编号 | 决策 |
| --- | --- |
| [0001](adr/0001-single-shared-db.md) | 所有账号共用一个 state.db，按 workspace_id 分区 |
| [0002](adr/0002-dialect-not-repository.md) | 不为 MySQL 抽 Repository，只抽方言差异 |
| [0003](adr/0003-no-migration-chain.md) | 建库只建最新版，不做旧库升级链 |
| [0004](adr/0004-update-before-insert.md) | 单行写入先 UPDATE，没命中才 INSERT |
| [0005](adr/0005-three-stage-pipeline.md) | 三阶段流水线 + 内存缓冲，不引消息队列 |
| [0006](adr/0006-token-cost-cache-window.md) | 缓存窗口按 token 成本扩展，不按固定条数 |
