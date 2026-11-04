# 管理指令权限与前移计划

> 状态：Implemented——主体是待办与实施顺序，已全部兑现，可删
> 日期：2026-11
> 结论落在：[ADR-0005](../adr/0005-orchestration-layer.md)、[domains/speech](../domains/speech.md)
> 范围：`config`、`policy`、`speech`、`bot`
> 前置讨论：指令系统本质是控制平面的硬编码调用，不是 Agent（见 [speech 域](../domains/speech.md)）

## 一、问题

指令（`#切换人格`、`#删除人格`、`#添加指令规则` …）是**控制平面**：确定性、改系统状态、不需要上下文。但当前它和数据平面（Agent 回复）挂在同一条链路上，由此带来三个后果：

1. **没有权限**：群里任何成员 @ 一下机器人就能执行 `#删除人格`、`#删除指令规则`，直接改写 `prompt_system.yaml` 并 `Rebuild()` 路由表。数据平面出错只是说错话，控制平面出错是配置被改坏。
2. **抢 Agent 的资源**：指令落在 Stage 3，与生成共享 `agent_concurrency=3` 和同群串行队列。一条 `#添加人格` 在 worker 里同步调 LLM（`LLMTimeout`），期间该群所有聊天都堵着。
3. **付了不该付的成本**：指令消息要走完 Stage 2 打标签（带图时下载 + 视觉调用），还可能占掉 `message_budget`。

本次先解决第 1 条（权限），第 2、3 条通过阶段二的前移解决。

## 二、目标与非目标

目标：

- 每个群都必须配自己的管理员 user ID；没有管理员的群不能使用管理指令。
- 权限判定只作用于 `Category == "internal"` 的管理指令；`chat_review` 这类功能指令（锐评）是数据平面，不加限制。
- 指令执行与 Agent 队列解耦（阶段二）：不再占用 Stage 3 的并发槽，不再走打标签。

非目标：

- 不做角色体系（管理员/普通成员/分组）、不做权限继承、不做面板上的权限编辑。
- 不做操作审计台账（`#删除人格` 改了什么、谁改的）。本次只做门禁，审计单独排期。
- 不做"自然语言操控系统"（那属于给 Agent 加工具，不是给指令加模糊匹配）。

## 三、关键决策

| 议题 | 决策 | 理由 |
| --- | --- | --- |
| 群是否可用指令 | **所有群都可**，不需要群白名单 | 指令系统对每个群开放，限制落在“谁能用” |
| 默认策略 | **拒绝**（fail-closed） | 没配管理员的群、名单外的用户，一律不能执行管理指令 |
| 哪些指令受限 | `Category == "internal"` 全部，`#帮助` 例外（未授权也可用） | 查看指令列表无副作用，放行便于排查 |
| 私聊 | **直接放行，不校验权限** | 私聊对话人只有账号主人自己，没有越权面 |
| 权限提示 | 回一句固定话术，**不回显名单** | 避免泄露管理员身份 |
| 配置位置 | `config.yaml` 的 `bot` 段（静态配置） | 简单；运行时改名单需要另一套权限，本期不做 |

**注意 user ID 的形态**：QQ 官方账号收到的是 **openid**（`Event.UserID`），不是 QQ 号。管理员必须从日志的 `user_id` 字段取值，填 QQ 号永远匹配不上。这一点要写进配置注释和本文档。

## 四、配置形态

```yaml
bot:
  # 管理指令管理员：每个群单独配，列出允许执行 # 开头管理指令（#切换人格、#删除人格…）的 user_id。
  # 没列出的群 = 该群没有管理员 = 没人能执行管理指令（#帮助 除外）。
  # user_id 取日志里的 user_id 字段（QQ 官方账号是 openid，不是 QQ 号）。
  # 群 ID 取日志里的 group 字段（QQ 官方账号是群 openid，不是群号）。
  command_admins:
    "群ID-A":
      - "0A1B2C3D4E5F60718293"
      - "5F6E7D8C9B0A19283746"
    "群ID-B":
      - "0A1B2C3D4E5F60718293"
  # 也支持逗号分隔写法：
  # command_admins:
  #   "群ID-A": "0A1B2C3D4E5F...,5F6E7D8C9B0A..."
```

类型与校验：

- `map[string]commandUserList`，`commandUserList` 接受 YAML 列表或逗号分隔标量（与 `groupList`、`workspaceIDList` 同一套写法）。
- 群键不能为空串。
- **每个群的名单为空只告警**：启动照常，但该群没人能执行管理指令（群级 fail-closed），告警文案指明是哪个群。
- 用户 ID 去空白、去空项、去重。
- **不支持 `"*"` 作为用户 ID**：管理员必须是具体的人，不能整个群放开。

## 五、阶段一：权限门禁

只在命中内部指令时多一道门禁：命中后由“这个群配没配管理员”决定放行。

### 1. `config` 层

| 文件 | 改动 |
| --- | --- |
| `config/section_bot.go` | 新增 `commandUserList` 解码类型 + `CommandAdmins map[string]commandUserList` 字段；`Validate` 里校验群键非空、用户 ID 非空、去重；群名单为空只告警 |
| `config/convert.go` | 解码成 `Config.CommandAdmins map[string][]string` |
| `config/config.go` | 新增字段 `CommandAdmins map[string][]string` |
| `config.yaml` / `config/config_example.yaml` | 补上面那段注释与示例（示例保持注释状态，默认不启用） |

### 2. `policy` 层

```go
// CanUseCommand 判断某个群成员能否使用管理指令。
//
// 只认精确的群 ID：没配这个群、或这个群里没有这个人 → 拒绝。
// 私聊（chat_id 以 "private:" 开头）直接放行：对话人只有账号主人自己。
func CanUseCommand(cfg *config.Config, chatID, userID string) bool
```

判定顺序：`cfg == nil` → false；`chatID` 以 `private:` 开头 → **true**；`CommandAdmins[chatID]` 不存在 → false；列表含 `userID` → true；否则 false。

### 升级须知

这是一次**不向后兼容**的变更：升级后没配 `command_admins` 的群，所有人（包括原来的使用者）都无法执行管理指令，只会收到拒绝提示。私聊不受影响。`#帮助` 仍可查看指令列表。

上线步骤：每个群都要配一遍。先从日志里拿到群 ID 与管理员 `user_id`（日志字段 `group` / `user_id`），只配一个群的最小写法：

```yaml
bot:
  command_admins:
    "群ID": ["管理员 openid"]
```

### 3. `speech` 层

`dispatch.go` 命中 route 之后、调用 `route.Handler` 之前插入：

```go
if route.Category == "internal" && route.Keyword != internalKeywordHelp &&
    !policy.CanUseCommand(cfg, groupID, event.UserID) {
    logutil.InfoCtx(ctx, "管理指令被拒绝", "group", groupID, "user_id", event.UserID, "keyword", route.Keyword)
    r.reply(event, groupID, "⛔ 你没有使用该管理指令的权限")
    return
}
```

- `#帮助` 用常量 `internalKeywordHelp` 从 `internal_cmd.go` 提出来，别写字面量。
- 日志留 `user_id`，方便管理员排查"为什么我被拒了"（提示语不回显名单，日志可以）。
- `speech` import `policy` 不产生循环依赖（`policy` 只 import `config`）。

### 4. 测试

| 位置 | 覆盖 |
| --- | --- |
| `config` 解码 | YAML 列表、逗号分隔、多群各自名单、空名单告警、群键为空报错 |
| `policy/policy_test.go` | 未配该群拒绝；配了但人不符拒绝；人在名单内放行；私聊直接放行；`cfg == nil` |
| `speech/command_test.go` | 名单外用户 `@bot #删除人格` → 收到拒绝提示，且 `promptCfg` 未被修改；名单内正常执行；`#帮助` 不受限 |

### 5. 验收标准

- 不配 `command_admins`：任何群、任何人的管理指令都收到 `⛔` 拒绝提示，`prompt_system.yaml` 不被改动；`#帮助` 可查看。
- 配了 A 群：A 群管理员正常执行，A 群其他人被拒；B 群（没配）所有人被拒。
- 名单外用户执行 `#删除人格` → 收到 `⛔` 提示，`prompt_system.yaml` 的 mtime 不变（副作用确实没发生）。
- 私聊：管理指令直接可用，不需要任何配置。

## 六、阶段二：独立指令阶段（Stage Command）

> 目标是把控制平面从数据平面里摘出来：自己的缓冲、自己的并发、自己的失败语义。
> 开发阶段不做兼容开关：前移直接生效，不保留旧路径。

### 1. 数据流

```
IM 入站 → b1 → Stage 1(sync) ─┬─ 命中内部指令 → bCmd → Stage Command → 执行完即止
                              └─ 其余        → b2 → Stage 2(assets) → b3 → Stage 3(agent)
```

Stage 1 只做“分流”，不执行副作用；执行全部发生在 Stage Command。这样慢指令（`#添加人格` 要调 LLM 生成提示词并 `Rebuild()`）既不堵该群的入库，也不占 Agent 的并发槽——之前“塞进 Stage 1”方案里那笔代价消失了。

### 2. 阶段定义

| 项 | 值 | 说明 |
| --- | --- | --- |
| 阶段名 | `command` | 指标自动按阶段名分标签（`pipeline_stage_*`） |
| 缓冲 | `bCmd` | 容量与 `b2`/`b3` 同源（`per_chat_capacity` / `global_capacity`） |
| 分区键 | `ChatKey(workspaceID, chatID)` | 同群指令之间串行 |
| `command_batch` / `command_linger_ms` | 1 / 0 | 指令低频，批量没有意义 |
| `command_concurrency` | 2 | 只影响不同群之间指令的并行度 |
| 重试 | **固定 0，不暴露配置** | 指令副作用不幂等，重试就是重复执行 |

`StageConfig.Retries` 直接传 0，不做成配置项——防的是有人照着 `sync_retries: 3` 的手感配出一个危险值。

### 3. 分流与接口

`handleSync` 在 `forward` 之前对每条 `Synced` 做一次**只读匹配**：

```go
for _, synced := range ws.PrepareBatch(ctx, inbounds) {
    nextKey := conversation.ChatKey(synced.WorkspaceID, synced.ChatID)
    if b.isInternalCommand(ws, synced) && !policy.IsRecordOnly(cfg, ws.ID) {
        b.forward(ctx, b.bCmd, pipeline.StageCommand, nextKey, synced.Message.MessageID, synced)
        b.stageCommand.Submit(nextKey)
        continue
    }
    // 其余照旧进 b2
}
```

`speech` 把“判”和“做”拆成两个入口：

```go
// MatchInternalCommand 只回答“这条消息是不是内部指令”，不执行、不改状态，供 Stage 1 分流。
func (r *Router) MatchInternalCommand(event onebot.Event, content string, allowNickname bool) (keyword string, ok bool)

// RunInternalCommand 真正执行；返回 false 表示“匹配不上了”，调用方丢弃即可。
func (r *Router) RunInternalCommand(ctx context.Context, event onebot.Event,
    groupID, content string, allowNickname bool) bool
```

两次匹配之间路由表可能被别的指令改掉（`#删除人格` 把关键字删了）。`RunInternalCommand` 匹配不上时返回 false，Stage Command 记一条 Warn 后丢弃——消息已在 Stage 1 入库，不会丢数据。

权限判定只在 `RunInternalCommand` 里做一次，不同入口不各自判一遍。`Handler` 的 `systemPrompt` / `keywordPrompt` 两个参数对内部指令无用（现有实现都不读），传空串，注释说明。

### 4. 失败与 record-only

- **指令执行永远不返回 `FailedItem`**：`Retries=0`，失败即丢弃、不重试、不进死信。失败只能在 handler 内部回一句错误给用户（现有 handler 就是这么做的）。
- **record-only 的指令不进 command 阶段**：备用号只记录不回复，这条消息按原路径走 b2/b3，由 Stage 3 既有的 record-only 分支回固定话术。这样“备用号不会执行 `#删除人格`”由分流条件保证，而不是靠 handler 自觉。
- 指令消息已由 Stage 1 入库，只是不再进 Stage 2/3，因此不会进群聊缓存（模型看不到“刚才有人切了人格”，已确认可接受）。

### 5. 顺序取舍

**指令与同群聊天之间的先后顺序不再保证**：这是两个独立队列，Stage Command 和 Stage 2/3 各跑各的。

- 窗口 = 那条聊天走完 Stage 1 → Stage 2 的时间。无图时几乎为 0；带图时最多到 `message_budget`。
- `#切换人格` 之后立刻提问，理论上可能用旧人格回复。
- `#添加人格` 要等 LLM 生成 + `Rebuild()`，窗口最大；但这类操作完成后会回一句回执，管理场景下用户自然会等确认再使用。

需要强顺序就必须加 per-chat 屏障（指令完成前让同群后续消息在 Stage 2 入口等待），那是另一个复杂度级别，本期不做。

### 6. 关停与观测

- `Run` / `closeBuffers` / `Close` / `Wait` 都要带上 `stageCommand` 与 `bCmd`，排空顺序与现有三个 stage 一致。
- 指标无需新增定义，按阶段名 `command` 分标签即可。
- 日志：分流命中打 Debug，执行结果由 handler 自己打，匹配漂移打 Warn。

### 7. 测试

- 内部指令命中 → 进 `bCmd`、不进 `b2`；Stage 3 指标不增长。
- 功能指令（`chat_review`）命中 → 仍走 `b2`，保持有窗口快照。
- 未命中指令 → 行为与现在完全一致。
- 备用号（record-only）发指令 → 不进 `bCmd`，只回固定话术。
- 指令 handler 内部失败（`#添加人格` 格式错）→ 不重试、不进死信、只回错误提示。
- 两次匹配漂移（第一次命中、第二次路由表已变）→ 丢弃并告警，不 panic。

## 七、改动清单汇总

| 阶段 | 文件 | 说明 |
| --- | --- | --- |
| 一 | `config/section_bot.go` | 解码类型 + 字段 + 校验 |
| 一 | `config/convert.go`、`config/config.go` | 转换与运行时字段 |
| 一 | `policy/policy.go` | `CanUseCommand` |
| 一 | `speech/dispatch.go`、`speech/internal_cmd.go` | 判定 + `#帮助` 常量 |
| 一 | `config.yaml`、`config/config_example.yaml` | 注释与示例 |
| 一 | `policy/policy_test.go`、`config/loader_test.go` | 测试（权限分支、`command_admins` 两种写法解码） |
| 二 | `pipeline/contracts.go` | 新增 `StageCommand` 常量 |
| 二 | `bot/handler.go` | `bCmd` 缓冲、`stageCommand` 注册与配置 |
| 二 | `bot/stages.go` | `handleSync` 分流 + 新增 `handleCommand` |
| 二 | `bot/ingest.go` | `Run` 里启动新阶段，排空与关闭 |
| 二 | `config/section_pipeline.go`、`config/config.go` | `command_batch` / `command_linger_ms` / `command_concurrency` |
| 二 | `speech/command_run.go` | `MatchInternalCommand` + `RunInternalCommand` + `commandText` |
| 二 | `speech/command_run_test.go`、`bot/pipeline_test.go` | 测试（匹配/权限/参数、独立阶段不调模型不进缓存） |
| 二 | `docs/domains/pipeline.md`、`docs/domains/speech.md` | 流程图与边界更新 |

## 八、风险

1. **漏配某个群**：这是有意为之的 fail-closed，但存量部署升级时忘了给某个群配管理员，会表现为“这个群的机器人突然不听口令了”。缓解：启动日志打印“管理指令已启用，覆盖 N 个群”并列出群 ID；完全没配时告警“未配置 bot.command_admins，所有管理指令已禁用”。
2. **openid / QQ 号混淆**：填错形态会导致永远无权限。缓解：配置注释、本文档、拒绝日志都带 `user_id` 实际值。
3. **指令与同群聊天的顺序不再保证**：独立阶段与 Stage 2/3 是两个队列，指令可能晚于紧随其后的聊天执行。窗口通常毫秒级（Stage 1 → Stage 2 无图时几乎为 0），带图时最多到 `message_budget`；要强顺序只能加 per-chat 屏障，本期不做。
4. **权限判定被绕过**：执行入口必须唯一。`MatchInternalCommand` 是只读的、不判权限；`RunInternalCommand` 是唯一能产生副作用的地方，权限判定只能写在它里面。以后再加入口（面板、定时任务）同样要收敛到它。
5. **没有配置级回滚**：前移不提供开关，出问题只能回滚二进制。开发阶段接受这个代价，正式上线前需要重新评估。

## 九、已确认

- `#帮助` 对未授权用户开放（含私聊），用于自助发现指令。
- 开发阶段不做兼容开关：指令前移直接生效，不保留旧路径。
- 指令前移用独立阶段（Stage Command），不塞进 Stage 1；接受指令与同群聊天可能乱序。

## 十、后续扩展：Agent 投递指令（本期不做）

指令独立成阶段后，入口从“IM 事件 + dispatch”变成了“谁能往 `bCmd` 投一条 Item”。IM 只是其中一个 producer，Agent、定时任务、将来的面板都可以是。这正好补上“自然语言操控系统”缺的那一环：不需要给 Agent 加一堆专用工具，只要让它把用户的话转译成一条指令消息投进流水线。

指令系统因此不止是“群里能敲 `#`”，而是一个内部命令总线。权限、参数解析、日志与指标全部复用，不用为 Agent 再写一套。

### 真要做时需补齐的四件事

1. **actor 语义（最重要）**：Item 要带 `actor`，区分“以某个用户身份代理”与“系统自己”。
   - 代理用户：权限按该用户判定（`CanUseCommand` 原逻辑），用户不是管理员就投不进去，不能提权。
   - 系统自己（主动发言、定时任务）：需要独立的系统身份 + 指令白名单，否则等于一个绕过权限的后门。
2. **回环防自激**：指令产生的回复会进 Stage 3，模型可能再投一条。需要“Agent 投递的指令，其回复不再作为 Agent 输入”或“每 turn 最多投递 N 条”。
3. **ID 与追踪**：伪消息没有 `message_id`，需生成稳定 ID；日志要能回答“这条指令是哪个 turn 投的”。`Retries=0` 让幂等压力很小，但追踪不能省。
4. **统一入口**：不许在 Agent 侧直接 `bCmd.Put`。封一个 `CommandBus.Submit(ctx, source, event)`，内部管 Item 构造、actor 注入与阶段提交；以后新增 producer（面板、定时任务）都收敛到它，才能守住“只有一个执行入口”。

### 不该越过的边界

不要拿这条路替代工具调用。MCP 工具是结构化参数 + 有返回值；投递指令是文本协议，参数靠字符串解析，脆弱且无返回。适合“用户说要切人格，转译一下”；不适合“Agent 需要知道操作结果再决定下一步”——那种必须同步返回。

本期只把执行入口收敛到 `RunInternalCommand`，不实现 `CommandBus`。

## 十一、实测记录（2026-11-04，米哈游玩家茶话会）

群里用真实桥接跑了三条：`#帮助`（管理员）、`#切换人格`（名单外成员）、`#切换人格`（管理员）。结果：前两条分别是帮助列表与“⛔ 你没有使用该管理指令的权限”，都**没有**调用大模型、**没有**进群聊缓存；第三条走到 handler。

实测发现并修掉一个参数解析 bug：匹配时用的是渲染后的正文，而渲染会把 `@昵称(ID)` 按原位置拼进去，于是「`#切换人格` + 正文后面还有 @机器人」这种消息里，mention 被当成人格名，回复「未找到人格：@昵称(ID)」。修复是新增 `commandText`，指令一律按**文本段**解析（`event.PlainText()` 再剥手打的 `@昵称` 前缀）；同时把 `dispatch.go` 里重复的那份权限判定与参数提取改为直接委托 `RunInternalCommand`，保证面板/调试触发与流水线两条路径行为一致。

