# 2026-11 指令闸门：指令模块前移进 Agent 工作流

> 状态：已实施（2026-11-05）
> 日期：2026-11
> 结论落在：代码见「逐文件改动」；文档已更新 [message-flow](../architecture/message-flow.md)、[architecture/overview](../architecture/overview.md)、[architecture/domains](../architecture/domains.md)、[domains/speech](../domains/speech.md)、[domains/pipeline](../domains/pipeline.md)、[domains/state](../domains/state.md)；ADR-0007 待补（它与 ADR-0005 第 5、11 条的关系要在 ADR 里写清）

一次改造方案，不是决策记录。要回答的是：把「指令」和「Agent」接成一条链之后，怎么让某几条指令执行完还能继续往下走、被刚启动的 Agent 正常回复，同时不打破「指令不占生成并发槽」这条现有约束。

## 一句话

删掉独立的指令工作流，把 `command` 域挪到聊天工作流的第一站。指令执行完默认到此为止，只有 `#启动` / `#关闭` 这类**门控指令**执行完继续往下走——开关打开的那一刻就在本条消息之前生效，所以 `@机器人 #启动！ 鲸鱼娘` 会被刚加载的人格正常回复。

## 现状

```
IM 事件 → 前置 sync（补名 + 入库）
             ├─ chat    工作流：assets（打标签）→ agent（生成）
             └─ command 工作流：command（管理指令）→ ✂
```

分流点在前置：`bot/router.go` 的 `workflowOf` 用 `speech.MatchInternalCommand` 判一次，命中内部指令就送去 `WorkflowCommand`。指令工作流的终点写在 `NewBot` 的 `exit(WorkflowCommand, EventCommandDone)` 上。

这条结构现在卡住三件事：

1. **「执行完要不要继续」是拓扑决定的，不是指令自己的属性。** 想让某条指令继续，只能在前置里加特例——而特例加在前置，就等于把「指令的语义」写进了「择路」。
2. **指令域看不见别的域的状态，别的域也看不见指令改过的状态。** 门控指令要开关的正是打标签与 Agent，它们与指令域平行，没有先后关系。
3. **「关掉某个域」现在没有地方安放。** `record_only` 是账号级的配置项，会话级的开关不属于任何一条工作流。

## 目标拓扑

```
IM 事件 → 前置 sync（补名 + 入库）
             └─ chat 工作流
                  command（指令 / 闸门）
                     ├─ 终止型指令、#关闭  → command_done ✂
                     └─ 其余（#启动 / 非指令）→ command_pass
                          → assets（打标签；开关关则跳过）
                          → agent（生成；开关关则静默）
```

连线（`NewBot`）：

```go
b.orch.workflow(WorkflowChat, pipeline.NodeCommand)              // 指令在入口
b.orch.connect(WorkflowChat, EventCommandPass, pipeline.NodeAssets)
b.orch.connect(WorkflowChat, EventTagged, pipeline.NodeAgent)
b.orch.exit(WorkflowChat, EventCommandDone)                      // 终止型指令到此为止
b.orch.exit(WorkflowChat, EventReplied)
```

### 多工作流机制原样保留

本次只是把**指令工作流**合并进 chat 链，`orchestrator` 那一层一行不动：`Pick.Workflow`、`workflows` 表、`mount` / `workflow` / `connect` / `exit`、`enter` / `advance` / `isEnd` 全部留着。前置的择路也留着，`route` 与 `workflowOf` 退化成单分支而不是被删掉：

```go
// bot/router.go —— 仍然是唯一的择路点。
// 现在只有一条：指令是聊天工作流的第一个域，不再在这里分流。
// 加一条工作流 = 这里加一个分支 + NewBot 里 workflow/mount/connect/exit，别的都不用动。
func (b *Bot) workflowOf(_ *workspace.Workspace, _ *config.Config, _ conversation.Synced) string {
	return WorkflowChat
}
```

参数形状故意留着（改名的 `_`），目的就是将来加分支时不用连签名和调用点一起改。

真正删掉的只有两处：

| 删什么 | 为什么 |
| --- | --- |
| `WorkflowCommand` 常量 + `workflow(WorkflowCommand…)` + `exit(WorkflowCommand…)` | 它描述的那条工作流不存在了；常量留着就是一句没有对应拓扑的空话，比删掉更误导 |
| `isInternalCommand`（`bot/router.go` 里择路用的只读匹配） | 择路不再依赖「是不是指令」，这个判断下移到 command 域；同一个判断留两份就会漂 |

`assets → agent` 这一跳仍然用 `EventTagged`：事件名描述「这一步发生了什么」（打完标签），与现在一致。

## 契约变化

```go
// speech/command.go
type Command struct {
    Keyword string
    Help    string
    Prompt  string
    SharedRules string
    Category    string
    Handler     HandlerFunc
    Persona     *config.Persona

    // ContinueAfterRun 执行完不停工作流，继续交给下游（打标签 → 生成）。
    // 零值 false = 到此为止。只有门控指令会把它置 true。
    ContinueAfterRun bool
}

// conversation/types.go
type Synced struct {
    ...
    // CommandRan 本条消息的门控指令已由 command 域执行过，值为命中的关键字。
    // agent 域见到非空就不再二次匹配内部指令，直接按当前人格直答（见「两个容易踩的点」）。
    CommandRan string `json:"command_ran,omitempty"`
}
```

`MatchInternalCommand` 的返回值要带上这个标志：

```go
func (r *Router) MatchInternalCommand(event onebot.Event, content string, allowNickname bool) (keyword string, continues bool, ok bool)
```

## 指令分类

| 关键字 | 类型 | 执行完 | 说明 |
| --- | --- | --- | --- |
| `#帮助` | 管理 | ✂ 终止 | 列表无副作用，对未授权用户也开放（不变） |
| `#切换人格` / `#取消人格` | 管理 | ✂ 终止 | 只改人格，不回复这句话（不变） |
| `#添加人格` / `#删除人格` | 管理 | ✂ 终止 | 会调模型生成提示词，副作用不幂等（不变） |
| `#添加指令规则` / `#删除指令规则` | 管理 | ✂ 终止 | 同上（不变） |
| `#启动` | 门控·开 | → 继续 | 打开会话开关；带参数时同时加载人格 |
| `#关闭` | 门控·关 | ✂ 终止 | 关掉开关，由 handler 自己回一句回执 |

`#关闭` 声明为终止，理由是它的语义就是「到此为止，别再往后跑」：继续走的话 assets 与 agent 都会因为开关已关而什么都不做，白走两跳。`ContinueAfterRun` 让这个判断落在指令自己身上，拓扑只照抄。

## 会话开关（一个开关，管打标签 + Agent）

**三态**，不是 bool：

| 状态 | 含义 |
| --- | --- |
| 未设置 | 跟随配置 `bot.agent_default_enabled`（默认 `true`，即保持现有行为；已确认放 `bot` 段——它是会话策略，不是性能参数） |
| 开 | 本会话打标签 + Agent 都跑 |
| 关 | 本会话不打标签、不回复、不主动接话，但**照常入库、照常进群聊缓存、照常学身份** |

用三态而不是 bool：改配置默认值时，显式关掉的群不该被悄悄打开，反之亦然。

**内存**（`speech/router.go`，与 `groupPersona` 同款）：

```go
agentGateMu sync.RWMutex
agentGate   map[string]bool // chatID → true 开 / false 关；缺失 = 未设置

func (r *Router) AgentEnabled(chatID string) bool          // 缺失时回落到 cfg.AgentDefaultEnabled
func (r *Router) SetAgentEnabled(chatID string, enabled bool)
func (r *Router) RestoreAgentGates(stored map[string]bool) // 启动时从档案恢复
```

**持久化**：借 `settings` 表，key = `group_gate:<workspaceID>|<chatID>`，value 是 JSON bool。

为什么不给 `groups` 加列：`migrate()` 只支持全新库（ADR-0003），加列意味着 `schemaVersion` +1，然后**必须删掉 state.db 重建**，历史消息一起没。`settings` 表任何版本的库里都有；`ApplyRuntimeSettings` 只认 `pipeline` / `assets` 两个 key，多出来的键不会被误解析。

```go
// state/ops.go（或新增 state/gate.go，与 persona.go 同款写法）
func (s *Store) LoadGroupGates() (map[string]bool, error)          // 全表前缀扫描
func (s *Scope) LoadGroupGates() (map[string]bool, error)           // 本工作空间
func (s *Scope) SaveGroupGate(chatID string, enabled bool) error    // 走 SaveSetting
```

为什么不复用 `groups.extra`：`SaveGroupInfo` 会用事件里解析出来的 extra **整体覆盖**那一列，门控状态迟早被下一次群名同步冲掉。

注入与恢复（`workspace/workspace.go`，紧挨 `SetPersonaStore` / `RestorePersonas`）：

```go
ws.Router.SetAgentGateStore(st)   // 与 SetPersonaStore 同一处
w.Router.RestoreAgentGates(gates) // 与 RestorePersonas 同一处
```

## 两个容易踩的点

### 1. 双重执行：一条 `#启动` 会被执行两次

`command` 域执行完，消息继续往下走到 `agent`；`handleAgent` 的 `addressed` 分支调 `RouteMessageNow` → `dispatch.routeMessage` → 前缀树命中 `#启动`（`Category == "internal"`）→ **再执行一遍** `RunInternalCommand` 并 `return`，于是这句话既被执行两次、又不会被回复——正好和需求相反。

解法：执行过的门控指令在 payload 上留痕。

```
command 域：RunInternalCommand 成功 → synced.CommandRan = keyword → 发 command_pass
agent  域：synced.CommandRan != "" → ReplyAddressedNow(...) → continue
```

`ReplyAddressedNow` 就是现在的 `replyDefaultNow`（渲染本群人格 + 焦点提示 + 生成 + 发送），只是导出成一个语义明确的入口：**指令已执行完，这句话当作普通 @ 聊天回应**。`#启动` 刚把人格写进 `groupPersona`，这里渲染出来的就是新人格。

不选「内容里带标记」：正文要进缓存、要喂模型，不该混入内部状态。不选「再匹配一次、比对关键字」：那是拿路由表猜历史。

### 2. 开关必须在「本条消息」之前生效

`command` 是第一条，所以 `#启动` 打开开关时，同一条消息后面的 assets / agent 已经能看到新状态。反过来说，如果把指令放在 assets 之后，`#关闭` 之后的那一条仍会被打一次标签（无害但白花钱），`#启动` 的那一条也不会被打标签。

## 门控指令的实现细节

```go
// speech/agent_gate.go（新文件）
const (
    keywordStart = "#启动"
    keywordStop  = "#关闭"
)
```

- **关键字注册成 `#启动` / `#关闭`，不带标点。** 前缀树是最长前缀匹配，所以 `#启动`、`#启动！`、`#启动!`、`#启动！鲸鱼娘`、`#启动！ HRBP`、`#启动 HRBP` 全部命中同一条路由。
- **参数清洗**：`extra := strings.TrimSpace(text[len(keyword):])` 之后去掉前导的 `！!：:，,、~` 与空白，剩下的就是人格名。全角半角都要认，否则「#启动！ 鲸鱼娘」拿到的是「！ 鲸鱼娘」。
- **人格加载**：复用 `#切换人格` 的机制 `r.personaBinding(name)` + `r.setGroupPersona(...)`（值拷贝 + 写 `groups.persona_keyword`）。不是新实现，只是换个入口。
  - 找不到该人格 → **跳过加载**，开关照开，回执里说明。
  - **不带参数 → 只开开关，保留本群当前人格**（已确认，不隐式清空）。配合「`#关闭` 不清人格」，不带名字的 `#启动` 就等于「把上次的人格接着用」。
- **`#关闭` 不清人格**：只关开关。下次 `#启动` 不写人格名，就用回原人格。
- **权限**：`policy.CanUseCommand`，与其它 `#` 指令一致（`command_admins` 名单；私聊放行）。
- **record-only 账号**：账号级策略盖过会话级，`command` 域直接跳过所有指令（现状语义），消息继续走 record-only 分支回固定话术。

回执文案（`r.reply`）：

```
✅ 已启动：打标签与自动回复已开启（人格：鲸鱼娘）
✅ 已启动：打标签与自动回复已开启（未找到人格「鲸鱼娘」，沿用当前人格）
✅ 已关闭：本群暂停打标签与自动回复，发 #启动 可重新开启
```

## 逐文件改动

| 文件 | 改什么 |
| --- | --- |
| `bot/orchestrator.go` | 删 `WorkflowCommand`；新增 `EventCommandPass = "command_pass"`；顶部注释改成「一个前置 + 一条工作流」，但机制与字段全部保留 |
| `bot/handler.go` | 删 command 工作流声明；改成 `workflow(WorkflowChat, NodeCommand)` + 两条 `connect` + 两条 `exit` |
| `bot/domains.go` | `handleCommand` 改成闸门（见下）；`handleAssets` 加开关判断；`handleAgent` 加「静默」与「CommandRan 直答」两个分支 |
| `bot/router.go` | 文件保留；只删 `isInternalCommand`，`route` / `workflowOf` 改成单分支（参数形状留着，见「多工作流机制原样保留」） |
| `bot/orchestrator_test.go`、`bot/pipeline_test.go` | 测试里拿 `WorkflowCommand` 当示例工作流名的地方，换成测试自造的工作流名（实现时用的是 `testWorkflowAlt = "alt"`）——那几则测的是多工作流机制，不能跟着删 |
| `speech/command.go` | `Command` 加 `ContinueAfterRun` |
| `speech/command_run.go` | `MatchInternalCommand` 多回一个 `continues` |
| `speech/internal_cmd.go` | 注册 `#启动` / `#关闭`，加 `helpStart` / `helpStop`（`#帮助` 自动带出） |
| `speech/agent_gate.go` | 新增：两个 handler、参数清洗、开关读写 |
| `speech/dispatch.go` | 新增 `ReplyAddressedNow`（包内就是 `replyDefaultNow`） |
| `speech/router.go` | `agentGate` 状态 + `SetAgentGateStore` + `RestoreAgentGates` + `AgentEnabled` 默认值回落 |
| `conversation/types.go` | `Synced.CommandRan` |
| `state/gate.go` | 新增：`LoadGroupGates` / `SaveGroupGate`（前缀 `group_gate:`，落在 `settings` 表） |
| `workspace/workspace.go` | `SetAgentGateStore` + 启动时 `RestoreAgentGates` |
| `config/section_bot.go` + `config.go` + `convert.go` + `config_example.yaml` | `bot.agent_default_enabled`（`*bool`，默认 true）→ `Config.AgentDefaultEnabled` |

`handleCommand` 的新形状：

```go
for _, item := range items {
    synced := decode(item)
    // record-only 账号：指令一律不执行，交给下游走固定话术（现状语义）
    if policy.IsRecordOnly(cfg, synced.WorkspaceID) {
        out.Signals = append(out.Signals, pass(synced))
        continue
    }
    keyword, continues, ok := ws.Router.MatchInternalCommand(...)
    if !ok {                                     // 不是指令：透传
        out.Signals = append(out.Signals, pass(synced))
        continue
    }
    if !ws.Router.RunInternalCommand(...) {      // 分流之后关键字被别处删掉
        log…                                      // 消息已入库，让它正常走一次聊天
        out.Signals = append(out.Signals, pass(synced))
        continue
    }
    if !continues {                              // 终止型：#帮助 / #切换人格 / #关闭 …
        out.Signals = append(out.Signals, done(synced))
        continue
    }
    synced.CommandRan = keyword                  // 门控指令：#启动
    out.Signals = append(out.Signals, pass(synced))
}
```

`handleAssets` / `handleAgent` 的开关判断：

```go
// assets：开关关 → 不打标签，但仍然回 EventTagged（下游自己决定要不要干活）
if tagger != nil && len(synced.Message.Images) > 0 && ws.Router.AgentEnabled(synced.ChatID) { ... }

// agent：record-only 优先，其次才是会话开关
if policy.IsRecordOnly(cfg, ws.ID) { …回固定话术… continue }
if !ws.Router.AgentEnabled(synced.ChatID) { continue }   // 静默：只入库、只进缓存
if synced.CommandRan != "" {
    ws.Router.ReplyAddressedNow(ctx, synced.Message.Content, synced.Event, synced.ChatID, snapshot)
    continue
}
if b.addressed(...) { ws.Router.RouteMessageNow(...) ; continue }
```

## 边界条件

1. **关闭期间**：入库、进群聊缓存、学身份照旧（与 record-only 同款），只是不调模型、不打标签、不主动接话。**不发**「本群已关闭」这类被动提示——`@` 它也不理，避免变成刷屏源。
2. **record-only 优先于门控**：账号级配置盖过会话级；`#启动` 在 record-only 账号上不生效。
3. **一批里有混合条目**：闸门域逐条处理，终止型只让那一条止步，同批其它消息照常往下走。
4. **重试语义不变**：`command` 域的 `Retries` 仍固定 0——`#启动` 重复执行虽然幂等，但 `#添加人格` 不是，不能靠重试。
5. **投递失败不回滚**：`#启动` 已经把开关与人格写进内存、写进档案；之后 `command_pass` 投递失败只会让这条消息停在这一跳（记死信），状态保持不变——与「副作用已发生不重试」一致。
6. **重启**：从档案恢复开关；人格关键字在提示词里已不存在时，开关照旧、人格按默认处理（不删记录，提示词补回来后自动生效）。
7. **热重载**：默认值每次 `appCfg()` 现取，改 `agent_default_enabled` 立即对「未设置」的会话生效，不影响显式设置过的会话。
8. **关键字冲突**：`#添加人格` / `#删除人格` 已有 `isInternalKeyword` 守卫，`#启动` / `#关闭` 同样受保护。
9. **多进程共用一个 state.db**：本期单进程，内存副本即真相；跨进程同步不在范围内（同现有 `groupPersona`）。
10. **`assertEventsCovered`**：新增的 `EventCommandPass` 必须出现在某条工作流的 `next` 里，否则那个测试会红——这是有意留的装配检查。
11. **多工作流能力不减**：本期是「少了一条工作流」，不是「只剩一条工作流的特例」。`orch.workflows` 仍是个表、`workflowOf` 仍是唯一择路点；以后再加工作流（审计、问答、定时任务）仍是「`workflowOf` 加一条分支 + `mount` + `workflow`/`connect`/`exit`」，不需要先恢复任何东西。

## 测试计划

下表是计划；实际落地时的用例名与拆分见本节末尾的「实施记录」。

| 层 | 用例 |
| --- | --- |
| `speech` | `MatchInternalCommand` 对 `#启动` / `#启动！` / `#启动！鲸鱼娘` / `#启动! HRBP` 都命中且 `continues == true`；`#帮助` 的 `continues == false` |
| `speech` | 参数清洗表驱动：`#启动！ 鲸鱼娘` → `鲸鱼娘`；`#启动 HRBP` → `HRBP`；`#启动` → 空 |
| `speech` | `#启动！不存在` 仍开开关、不报错、不改人格；`#启动！鲸鱼娘` 之后 `AgentEnabled` 为真且 `groupPersona` 是鲸鱼娘 |
| `state` | 三态存取：未设置 / 显式 true / 显式 false，重启后一致 |
| `bot` | 改写 `Test管理指令走独立工作流` → `Test指令在聊天工作流入口终止`（`#帮助` 不进缓存、不触发模型） |
| `bot` | `#启动！<人格名>` → 模型被调、人格块出现、开关落库、本条进缓存 |
| `bot` | `#关闭！` → 不调模型、开关落库为关、本条不进缓存（回执本身会进） |
| `bot` | 未启动会话静默：`agent_default_enabled=false` 且没 `#启动` → 入库、进缓存、不调模型 |
| `bot` | 非管理员的 `#启动` 不生效：开关不动、不调模型 |

### 实施记录（2026-11-05）

- 实际新增：`speech/agent_gate_test.go`（关键字变体、参数清洗、开关与人格绑定、默认值回落、落库与恢复）、`state/gate_test.go`（三态、持久化、空键与 nil Scope）、`bot/gate_test.go`（端到端：`#启动` 继续并回复 / `#关闭` 只回执 / 关着时静默 / 非管理员无效）。
- “模型调用恰好一次”寓在 `#启动` 那条端到端用例里：`len(calls) != 1` 就红；它同时证明指令没有被吞、也没有被二次执行（少了要数回执的假包装）。
- `#启动` 的收尾与计划一致：关键字只注册到 `#启动`，`！` / `!` / 空格与人格名都落进 extra，由 `personaNameFromExtra` 洗掉。
- `go test ./...` 全绿；`go vet ./...` 无输出。
- 遗留：ADR-0007 未写。它与 [ADR-0005](../adr/0005-orchestration-layer.md) 第 5 条（数据平面/控制平面分开）和第 11 条（工作流 = tag）的关系需要单独记一页：本次是「指令仍占自己的节点与并发槽，但不再独占一条工作流」。

## 明确不做

| 不做 | 原因 |
| --- | --- |
| 打标签与 Agent 拆成两个开关 | 用户确认一个开关同时管；两个开关会让指令语法变成 `#启动！标签 HRBP`，收益不明显 |
| 面板上的群开关 UI | 本期只做指令 + 档案；`settings` 里的键以后面板直接读即可，不用改存储 |
| 给 `groups` 加列 | 会触发 schema 版本 +1 → 必须删 state.db（ADR-0003），历史消息一起没 |
| 关闭期间不回任何话术 | 用户确认 `#关闭` 的回执由指令域直接发；之后再 `@` 不该有回应 |
| 拓扑热替换 / 多进程状态同步 | 见 [orchestrator-workflow-2026-11.md](orchestrator-workflow-2026-11.md)、[mq-readiness-2026-11.md](mq-readiness-2026-11.md) |

## 验收

1. 群里 `@机器人 #启动！ 鲸鱼娘` → 一句回执之后，紧接着一条用「鲸鱼娘」人格生成的回复。
2. `@机器人 #启动！ 不存在的人格` → 开关打开、回执说明跳过人格、本条照样被回复（当前人格）。
3. `@机器人 #关闭！` → 一句回执；本条与之后的消息都不再调模型，但聊天记录里还在。
4. `@机器人 #帮助` → 与现在完全一致，且**不**进群聊缓存。
5. 重启进程后，关掉的群仍是关的，开着的群仍是开的。
6. `grep -r "WorkflowCommand" .` 只剩注释与测试里自造的示例名；`orchestrator` 那套多工作流能力未动：`bot/orchestrator.go` 的 `workflows` / `connect` / `exit` 与 `bot/router.go` 的 `route` / `workflowOf` 都还在。
7. `go test ./...` 全绿。
