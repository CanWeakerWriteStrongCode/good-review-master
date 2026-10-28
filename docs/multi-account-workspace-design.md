# 多机器人账号工作空间隔离设计

> 状态：P0–P2 已实施（2026-10-28），P3 待定
> 适用版本：当前 `main`（AstrBot sidecar + 多工作空间架构）
> **注意：本文描述的是 `platform_id` + OneBot 11 时代的实现，正在被
> [im-protocol-standardization.md](im-protocol-standardization.md) 与
> [workspace-auto-provisioning-design.md](workspace-auto-provisioning-design.md) 取代；
> 冲突时以后者为准（契约改 OneBot 12，账号键为 `{self.platform}_{路径 account}`，
> `self.user_id` 只做 @ 判定，工作空间自动开通；**提示词改为全局共享，工作空间不再有
> 目录或私有配置文件，私有数据统一进 `state.db`**）。**

## 1. 目标与边界

### 1.1 目标

一个 Go 进程同时接管多个机器人账号时，**每个账号是一个独立工作空间**：

- 自己的聊天上下文与缓存锚点；
- 自己的群人格（`#切换人格` 只影响当前账号）；
- 自己的指令表与提示词（在 A 账号下 `#添加关键字` 不会出现在 B 账号）；
- 自己的身份（昵称、模型名、@ 识别）；
- 管理与监控页面按工作空间查看，不把两个账号的数据混在一个列表里。

判定标准：**A 账号的任何一次操作（发消息、切人格、加指令、看上下文）都不改变 B 账号的任何可观察状态。**

### 1.2 非目标（明确不隔离）

| 资源 | 原因 |
| --- | --- |
| OneBot 动作代理客户端 | 无状态，每次调用都显式带 `self_id` |
| IM 入站监听端口 / token | 进程级入口，按事件内容再分派 |
| Web 登录、JWT、指标端口 | 进程级基础设施 |
| MCP 服务连接（P1 阶段） | 外部工具会话，与账号身份无关；按工作空间注入工具列为 P3 可选项 |
| 大模型客户端（P1 阶段） | 默认共享；按工作空间覆盖模型/Key 列为 P3 可选项 |

> 后两项如果一开始就按账号各建一套，会让连接数、限速器和熔断器按账号翻倍，收益小于复杂度。先把"状态与提示词"隔离干净，再决定要不要拆外部资源。

## 2. 现状：共享状态清单

| # | 状态 | 当前键 | 多账号下的后果 | 目标键 |
| --- | --- | --- | --- | --- |
| 1 | 群消息缓存 `cacheMap`（`cache/cache.go:54`） | `groupID` | 通用 OneBot 下两个账号同群共用一份上下文 | `workspace + groupID` |
| 2 | 选窗锚点 `anchorMap`（`cache/cache.go:57`） | `groupID` | A 的发送窗口影响 B 的缓存成本决策 | `workspace + groupID` |
| 3 | 群人格 `groupPersona`（`router/command.go:119`） | `groupID` | **A 切人格，B 也变**（已实际发生） | `workspace + groupID` |
| 4 | 指令/路由表（`router/command.go:166`） | 全局单份 | A 添加的指令 B 也能命中 | 每个工作空间一份 |
| 5 | 提示词文件（`config/prompt.go:111`） | 全局单份 | 同上，且 `#删除关键字` 会误删对方指令 | 每个工作空间一份 |
| 6 | 机器人昵称 `cfg.BotNickname`（`config/config.go:18`） | 全局单值，取自 `GetLoginInfo()` 第一个适配器 | 提示词身份与实际账号不符；@ 兜底误判 | 每个工作空间一份 |
| 7 | 模型名 / 温度 / 看图开关（`config/config.go:38`） | 全局 | 两个账号无法用不同模型 | P3 可选覆盖 |
| 8 | `allow_groups`（`config/config.go:19`） | 全局 | 无法让 A 只进 1 群、B 只进 2 群 | 全局默认 + 工作空间覆盖 |
| 9 | `allow_self_ids`（`config/config.go:20`） | 全局 | 只是接管开关，可保留为总闸 | 保留，但由工作空间声明派生 |
| 10 | 跨账号去重 `isCrossAccountDuplicate`（`cache/cache.go:122`） | 同群缓存内 | 隔离后同一缓存内不再出现别的账号，逻辑自然失效 | 删除或降级为同账号去重 |

关键判断：**QQ 官方机器人的 `group_openid` 是按 bot 分配的**，所以现状下"同群不同 ID"偶然地把缓存分开了；但通用 OneBot / aiocqhttp 下 `group_id` 是相同的，缓存、锚点、人格会直接串台。不能依赖平台碰巧隔离。

## 3. 工作空间身份：用 `platform_id`，不用 `self_id`

### 3.1 问题

`self_id` 在 QQ 官方群消息里可能是占位值 `qq_official` / `unknown_selfid`（`onebot/types.go:65`）。它来自 mention 元数据：QQ 群设置里没给机器人「查看群所有信息」权限时 QQ 不下发 `mentions`，补上权限后才会带 `is_you` 和真实 openid（2026-10-28 实测）。拿不到时无法解析；用它做工作空间主键会出现"同一条消息归属不明"。

### 3.2 方案

AstrBot 的平台实例 ID（插件里的 `platform_meta.id`，即配置里的 `id`）**每个实例唯一且恒定**，与是否被 @ 无关。把它作为工作空间主键。

- 桥接插件在 payload 中新增 `platform_id`（`sidecar/astrbot_plugin_onebot_bridge/main.py` 的 `_build_payload`，在 `self_id` 归一化附近写入）。
- Go 的 `onebot.Event` 新增 `PlatformID string \`json:"platform_id"\``。
- 工作空间声明里同时列出该账号的 `platform_id` 与已知 `self_id`（openid / QQ 号），二者都映射到同一个工作空间：
  - `platform_id` → 主键，精确匹配；
  - `self_id` → 别名，用于老插件、aiocqhttp 或手工注入事件。

### 3.3 归属解析顺序（建议默认）

1. `platform_id` 命中工作空间 → 采用；
2. `self_id`（非占位）命中工作空间别名 → 采用；
3. 都没命中且**只配置了一个工作空间** → 归入该工作空间（兼容单账号）；
4. 其余情况 → **拒绝接管**（返回 `accepted=false`，交给 AstrBot 自己的 Agent），并打一条 warn 日志 + 指标，绝不猜。

第 4 条是硬规则：宁可这条消息不回复，也不能让错误的账号用错误的人格回复。

### 3.4 校验

启动时校验并直接失败的情况：

- 两个工作空间声明了同一个 `platform_id`；
- 两个工作空间声明了同一个 `self_id`；
- 工作空间 `id` 重复或为空。

只在日志告警的情况：某个 `self_id` 既不在任何工作空间别名里，也不在 `allow_self_ids` 中（意为 Go 不接管）。

## 4. 目标架构

### 4.1 运行时对象

新增 `workspace` 包（或 `app/workspace.go`，视最终体量）：

```go
type Workspace struct {
    ID       string   // 稳定标识（配置里的 id，用于目录/日志/URL）
    Name     string   // 展示名（默认取昵称）
    Platform string   // AstrBot 平台实例 ID
    SelfIDs  []string // 该账号可能出现的 self_id 别名

    Router *router.Router          // 独立路由表 + 独立 groupPersona
    Prompt *config.PromptConfig    // 独立提示词文件
    LLM    llm.Client              // P1 共享同一实例
    MCP    router.MCPProvider      // P1 共享同一实例
    Groups []string                // 生效的群白名单
}
```

`Bot` 从"持有一个 Router"变成"持有 `Resolver` + 多个工作空间"：

```
AcceptEvent(event)
  └─ ws := resolver.Resolve(event)     // platform_id → self_id → 单工作空间兜底
     if ws == nil: return false        // 交还 AstrBot Agent
     enqueue(event, ws)

dispatch(event, ws)
  └─ ws.Bot 的缓存/人格/路由全部走 ws 命名空间
```

### 4.2 缓存与锚点

`cache` 包从"全局 map + groupID"改为"按工作空间持有"：

```go
type Store struct {
    caches  map[string]*GroupMsgCache
    anchors map[string]*LLMAnchor
}

func (s *Store) GetGroupCache(groupID string, maxSize int) *GroupMsgCache
func (s *Store) GetAnchor(groupID string) *LLMAnchor
func (s *Store) SetAnchor(groupID string, a LLMAnchor)
```

每个 `Workspace` 持有一个 `*cache.Store`。`cache.ListGroupIDs()` 等全局函数改为 Store 方法；Web 面板遍历工作空间取数据。

`Message.SelfID` 保留（面板展示、排障需要），但不再承担隔离职责。`seenSet` 可简化为 `map[int64]struct{}`，因为一个 Store 内只有同一个账号的消息；`isCrossAccountDuplicate` 删除。

### 4.3 人格

`groupPersona` 从 `Router` 的字段变为每个 `Router` 各自持有（每个工作空间一个 Router，天然隔离）。`#切换人格` / `#取消人格` 的语义不变，只是作用域变成"当前工作空间 + 当前群"。

### 4.4 指令与提示词

每个工作空间一个 `config.PromptConfig`，文件按以下顺序解析（逐文件回退）：

1. 工作空间目录下的 `prompt_system.yaml`（存在则用，否则用全局）；
2. 工作空间目录下的 `prompt_custom.yaml`（不存在则创建到工作空间目录，**不回退写全局**）。

效果：

- `#添加关键字` / `#删除关键字` / `#添加指令规则` 只改当前工作空间的自定义文件；
- 命令热重载监听所有工作空间的提示词文件；
- `#帮助` 只列当前工作空间的指令。

### 4.5 身份与 @ 识别

- `cfg.BotNickname` 改为工作空间级字段，取值顺序：工作空间配置 `nickname` → 桥接 `get_login_info(self_id)` → 空。
- `ComposeSystemPrompt(cfg, ...)` 接收工作空间的配置快照，模型名、昵称都来自该工作空间。
- `isAtBot(event, content, ws)`：
  - ID 命中（`self_id` / `platform_id`）始终可信；
  - 昵称前缀兜底只在**该昵称唯一归属一个工作空间**时启用。多账号且昵称重复时禁用兜底（保留当前 `isMultiAccount` 的思路，但判定依据从"全局账号数"变成"昵称归属是否唯一"）。
- `stripCQPrefix` 使用工作空间昵称。

## 5. 配置设计

### 5.1 `config.yaml`

```yaml
bot:
  # 兼容保留：没有 workspaces 时，它仍是唯一总闸（现状行为不变）
  allow_self_ids: "*"

  # 新增：多账号工作空间。不写 = 隐式单工作空间 "default"（完整兼容旧配置）
  workspaces:
    - id: "haoping"                       # 必填：稳定标识，用于目录/URL/日志/指标
      name: "好评大师的小助理"              # 可选：展示名，默认取 nickname
      platform_id: "好评大师的小助理"        # 必填：AstrBot 平台实例 ID（唯一）
      self_ids:                           # 可选：该账号的 openid / QQ 号别名
        - "0C2F9A2A..."
      nickname: "好评大师的小助理"          # 可选：覆盖桥接取到的昵称
      allow_groups: "group_openid_A,..."  # 可选：覆盖全局 bot.allow_groups
      prompt_dir: "workspaces/haoping"    # 可选：默认 workspaces/<id>

    - id: "bushi"
      name: "不是好评大师"
      platform_id: "不是好评大师"
      self_ids:
        - "3319D7..."
      prompt_dir: "workspaces/bushi"
```

### 5.2 合并规则（建议默认）

| 配置项 | 规则 |
| --- | --- |
| `allow_groups` | 工作空间写了就完全替换全局，不合并 |
| `prompt_custom.yaml` | 每个工作空间独立；首次创建时若全局文件存在，复制一份作为起点（避免升级后指令消失） |
| `nickname` | 工作空间 > 桥接 `get_login_info` |
| `prompt_dir` | 默认 `workspaces/<id>`，目录不存在时首次写自定义文件会创建 |
| `llm` / `mcp` | P1 不支持覆盖，全部继承全局（改了要重启的提示保持现状）；P3 再加整段覆盖 |
| `allow_self_ids` | 有 `workspaces` 时，实际接管集合 = 各工作空间 `self_ids` 的并集；`allow_self_ids` 只作为额外总闸（为空不额外限制） |

### 5.3 热更新

| 变化 | 生效方式 |
| --- | --- |
| 工作空间提示词文件 | 热重载（重建该工作空间路由表） |
| `allow_groups` / `nickname` | 热重载（下一次消息处理生效） |
| 新增/删除工作空间、改 `platform_id` | **重启生效**，并打 warn（与 `web_port` 同类） |
| `llm` / `mcp` | 维持现状的"限速/熔断需重启"提示 |

## 6. 面板与可观测性

### 6.1 API

新增工作空间维度，保留旧路由映射到 `default` 工作空间：

```
GET /api/workspaces
    → [{ id, name, platform_id, account_count, group_count, cached_group_count, message_count, last_activity }]

GET /api/workspaces/:wid/groups
    → { workspace, accounts, groups, bot_info }

GET /api/workspaces/:wid/groups/:gid
    → { workspace, group_id, group_name, self_id, account_name, messages, empty }
```

`GroupDetail` 增加 `workspace_id` / `workspace_name`；旧 `/api/groups`、`/api/groups/:id` 仅在只有一个工作空间时保留，否则返回 400 提示选择工作空间（避免两账号数据混排）。

### 6.2 前端

- 群列表页顶部增加工作空间切换（工作空间数量 > 1 时显示）；当前"按 self_id 分组"降级为工作空间内部的二级分组；
- 消息页标题显示"工作空间 / 账号 / 群名"；
- `stores/groups.ts` 的 `accountSections` 改为 `workspaceSections`，账号分组保留在内部。

### 6.3 指标与日志

- 所有按消息处理的日志增加 `workspace` 字段；
- 给 `llm_requests_total`、`llm_duration_seconds`、`llm_tokens_total`、`llm_window_mode_total`、`llm_cost_total`、`llm_cache_hit_total`、`cache_messages` 增加 `workspace` 标签。基数由配置里的工作空间数量决定（有界），符合现有 `CacheMessages` 的注释约定；
- 新增 `workspace_resolve_total{result="platform|self_id|single|unresolved"}`，用于观察归属兜底是否频繁触发。

## 7. 迁移与兼容

1. 旧配置（没有 `bot.workspaces`）→ 自动生成一个 `id: "default"` 的隐式工作空间，`platform_id` 为空，`self_ids` 取 `allow_self_ids`，提示词仍用全局文件。行为与现状逐字一致。
2. 面板旧 URL 继续可用（仅单工作空间时）。
3. 已有缓存/锚点/人格都是内存态，重启即清空，无需数据迁移。
4. 桥接插件新增 `platform_id` 字段对旧 Go 版本是无害的（Go 忽略未知 JSON 字段）；旧插件对新增 Go 版本会走 `self_id` 别名解析。

## 8. 安全与边界

- 工作空间 `id` 会拼进目录与 URL：只允许 `[a-zA-Z0-9_-]`，启动时校验，防止路径穿越；
- `platform_id` 来自桥接插件，属于信任边界内，但仍做长度与空值校验；
- 未归属事件一律不接管，避免"猜错账号"造成越权回复。

## 9. 分阶段实施

### P0：归属与状态隔离（必须，修复现有串台）

1. 桥接插件 payload 增加 `platform_id`；`onebot.Event` 增加字段。
2. `config` 增加 `bot.workspaces` 解析与校验；无配置时生成隐式 `default`。
3. 新增 `workspace.Resolver`（`platform_id` → `self_id` → 单工作空间兜底），带单元测试（命中/别名/占位/冲突/未归属）。
4. `cache` 改为实例化 `Store`，按工作空间持有缓存与锚点；删除跨账号去重。
5. `Bot` 持有 Resolver 与工作空间列表；`AcceptEvent`、`dispatch`、`ProcessMessage` 全链路携带工作空间。
6. 每个工作空间一个 `Router`，`groupPersona` 随之隔离。
7. `isAtBot`、`stripCQPrefix`、`ComposeSystemPrompt` 改用工作空间昵称。
8. 测试：缓存隔离、锚点隔离、人格隔离、A 账号 @ 不触发 B、未归属事件不接管。

### P1：提示词与指令隔离

9. 每个工作空间一个 `PromptConfig`，按工作空间目录解析/创建 `prompt_custom.yaml`。
10. 提示词监听器监听所有工作空间的提示词文件；`#添加/删除` 只写当前工作空间。
11. 测试：A 加指令 B 不可见；A 删指令不影响 B；`#帮助` 内容按工作空间区分。

### P2：面板与可观测性

12. 新增 `/api/workspaces` 及工作空间作用域的群/消息接口；旧接口单工作空间兼容。
13. 前端增加工作空间切换与面包屑；账号分组保留在工作空间内部。
14. 指标/日志增加 `workspace` 维度。
15. 浏览器验证两个账号的群列表、消息页、账号归属均正确。

### P3（可选，待确认）

16. 工作空间级 `llm` 整段覆盖（模型、Key、温度、上下文上限）。
17. 工作空间级 MCP 工具注入选择（共享连接，按工作空间决定 inject 集合）。

## 10. 需要拍板的决策

| # | 决策 | 建议默认 |
| --- | --- | --- |
| D1 | 工作空间主键 | `platform_id`（插件新增字段），`self_id` 仅作别名 |
| D2 | 未归属事件策略 | 拒绝接管，绝不猜测；仅单工作空间时兜底 |
| D3 | 提示词隔离程度 | 每个工作空间独立 `prompt_custom.yaml`；`prompt_system.yaml` 可覆盖、默认全局 |
| D4 | LLM/MCP 是否按账号拆分 | P1 共享，P3 再做可选覆盖 |
| D5 | `allow_self_ids` 去留 | 保留为总闸；有 `workspaces` 时以各工作空间声明的 `self_ids` 并集为准 |
| D6 | 面板旧接口 | 单工作空间保留，多工作空间返回 400 要求指定 |
| D7 | 目录布局 | `workspaces/<id>/prompt_custom.yaml`，可在配置里用 `prompt_dir` 覆盖 |

## 11. 风险与缓解

| 风险 | 缓解 |
| --- | --- |
| 桥接插件未升级，没有 `platform_id` | 走 `self_id` 别名；仍无法归属时按 D2 拒绝，日志明确提示升级插件 |
| 两个账号昵称相同导致 @ 兜底误判 | 昵称兜底只在唯一归属时启用；ID 匹配优先 |
| 每个工作空间一个 Router 增加内存 | Router 只持有路由表与人格 map，量级为 KB；工作空间数量是个位数 |
| 指标增加 `workspace` 标签导致曲线变化 | 在文档与 Grafana 面板中同步更新；标签值有界 |
| 热更新边界变复杂 | 明确"结构性变更需重启"并在 `warnRestartOnlyChanges` 中补充提示 |

---

## 12. 实施记录（P0–P2）

| 层 | 落地位置 | 说明 |
| --- | --- | --- |
| 事件归属 | `sidecar/.../main.py`、`onebot/types.go` | payload 新增 `platform_id`；`workspace.Manager.Resolve` 按 platform → self_id → 单工作空间兜底，解析不到直接拒绝接管 |
| 配置 | `config/section_bot.go`、`config/config.go` | `bot.workspaces` 解析与冲突校验；无配置时 `EffectiveWorkspaces()` 生成隐式 default |
| 状态隔离 | `cache`（新增 `Store`）、`router`（`SetStore`/`SetWorkspace`） | 每个工作空间一套缓存、锚点与 Router；`groupPersona` 随 Router 隔离 |
| 提示词 | `workspace/workspace.go` | `<prompt_dir>/prompt_system.yaml` 存在则覆盖，否则用全局；`prompt_custom.yaml` 每空间独立；隐式 default 继续沿用全局自定义文件 |
| Bot 分发 | `bot/handler.go`、`bot/ingest.go` | 队列元素是 `(event, workspace)`，归属只解析一次 |
| 面板 | `web/server/accounts.go`、`handlers.go` | `/api/workspaces/{id}/groups[/{gid}]`；旧 `/api/groups` 仅单工作空间可用，多工作空间返回 400 |
| 前端 | `web/frontend` | 工作空间页签；消息页显示工作空间与接收账号 |
| 指标 | `telemetry/metrics.go` | 选窗/成本/缓存命中/CacheMessages 增加 `workspace` 标签；新增 `workspace_resolve_total` |

与第 6.3 节的差异：`llm_requests_total` / `llm_duration_seconds` / `llm_tokens_total`
仍只有 `model` 标签。原因是 P1 阶段大模型客户端跨工作空间共享，客户端本身不知道
当前是哪个账号；等 P3 做工作空间级 LLM 覆盖时再补，避免现在为打标签把 workspace
塞进 context 或包一层代理客户端。
