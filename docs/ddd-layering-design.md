# DDD 分层重构方案：域划分、值对象与扩展方向

## 0. 这份文档说什么

把现在 16 个顶层包按「底层 / 中层 / 编排」三层摆开，中层划成 6 个域，值对象归到算得出它的那个域。同时给三条长期路线留位置：多渠道接入、信息收集与分析、多 Agent 协作。

**这一版只到"位置留好"，不实现多渠道、不实现分析、不实现多 Agent。** 理由在 §7 逐条写清楚。

规模控制在：新增 2 个包（`policy`、`conversation`），改 2 个包的内部结构（`state`、`speech`），搬 2 个包（`workspace/state` → `state`，`cache` 并入 `conversation`），重命名 1 个包（`router` → `speech`）。不引入 Repository 接口，不拆服务，不改数据库结构。

**这份方案已经落地了**：阶段 1–6 做完，第 7 阶段按计划没做。实际做法与方案的差异逐条记在 §13——§4 到 §9 保留原样，它们是判断依据，不是现状描述；§3 的“现有包”一列已按落地后更新。

---

## 1. 现在的问题

六条，都是可以在代码里指到具体行号的。

### 1.1 业务域反向依赖编排层

`workspace/sync.go` 第 18 行的 Stage 1 业务函数签名是：

```go
func (w *Workspace) PrepareBatch(ctx context.Context, events []pipeline.InboundEvent) []pipeline.SyncedMessage
```

`assets/tagger.go` 第 62 行的返回值里带着 `[]pipeline.Asset`：

```go
type TagResult struct {
    ImageIDs   []cache.ImageID
    ImageNotes []cache.ImageNote
    Assets     []pipeline.Asset
    Status     string
    Deferred   []DeferredAsset
}
```

`pipeline` 是编排层（它管"什么时候处理、批多大、重试几次"），业务域反过来 import 它，层次就翻过来了。

### 1.2 一条业务规则两份实现

| 规则 | 实现一 | 实现二 |
|---|---|---|
| 群在不在白名单 | `config.Config.HasGroup` | `workspace.Workspace.AllowsGroup` |
| 白名单有没有 `*` | `config.Config.AllowsAllGroups` | `workspace.Workspace.AllowsAllGroups` |

改一处漏一处的概率是 100%。

### 1.3 一个对象同时管六件事

`router.Router`（定义在 `router/command.go` 第 99-153 行）有 11 个依赖字段：`llmClient`、`obClient`、`promptCfg`、`appCfg`、`mcp`、`starter`、`store`、`personaStore`、`displayName`、`saveMessage`、`saveAnchor`、`ledger`。它同时负责：指令前缀树、锐评生成、主动接话裁判、分句排版、发送台账、群人格。

### 1.4 一个文件装 14 张表

`workspace/state/state.go` 1463 行，一个 `Scope` 门面覆盖 14 张表：`messages`、`anchors`、`identities`、`members`、`groups`、`group_personas`、`workspace_profile`、`member_info_history`、`group_info_history`、`image_notes`、`asset_tag_jobs`、`send_ledger`、`alerts`、`settings`。`workspace_id` 这个词在这个文件里出现 69 次。（这是拆文件之前的记录；现在已按表拆成 9 个文件，见 8.5。）

### 1.5 值对象被存储形状绑住

`cache.Message` 既是值对象又是环形缓冲区的一个槽位，还带一个 `ImageNotes []ImageNote` 字段打了 `json:"-"`——因为环缓存的 JSON 序列化路径不带它。结果是 `pipeline` 里又复制了一份 `EnrichedMessage`：

```go
type EnrichedMessage struct {
    SyncedMessage
    ImageIDs    []cache.ImageID   `json:"image_ids,omitempty"`
    ImageNotes  []cache.ImageNote `json:"image_notes,omitempty"`
    Assets      []Asset           `json:"assets,omitempty"`
    AssetStatus string            `json:"asset_status,omitempty"`
}
```

同一条消息的字段在两个包里各存一份，加字段要改两处。

### 1.6 入站模型是 QQ 专属

`onebot.Event.ChatID()` 返回群号，`bot.AcceptEvent(ctx, account, onebot.Event)` 只认这一种事件。`cache.Message` 的字段名是 `GroupID`、`SelfID`、`RefIdx`（QQ 官方的 REFIDX）。接论坛、接邮件时这套模型没法复用。

这一条现在不疼，但它决定了 §7 提到的长期路线走得顺不顺。

---

## 2. 划分原则

四条，后面所有归位都按它判。

1. **包里能不能出现业务词。** 出现 `group`、`nickname`、`@`、`锐评`、`persona` 的，只能是中层或以上。
2. **依赖只能朝下。** 底层不 import 中层任何包；中层不 import `pipeline`、`bot`、`agents`、`app`、`web`。
3. **域之间只传值对象，不互相持有对象。** 会话域不存 `*speech.Router`，发言域不存 `*conversation.Store`。需要谁，在编排层注入一个窄接口。
4. **值对象跟着"谁算出来的"走。** 流水线负责编码成 `Item.Body`，但载荷里的业务字段（消息、资产、判定结论）归各自的域。

还有一条这个项目特有的：**`telemetry` 是唯一允许被任何层直接 import 的包**。指标是进程级单例，为一个 `telemetry.CacheMessages.Set(...)` 去给十几个构造函数加参数换不来解耦。

---

## 3. 三层结构

```
编排   pipeline        agents(留位)      runtime         admin
            ↓              ↓                ↓              ↓
中层   conversation  →  asset  →  speech  →  account  →  policy
                                        knowledge(留位)
            ↓              ↓                ↓              ↓
底层      proc        protocol         storage          config        obs
```

| 层 | 域 | 一句话职责 | 现有包 |
|---|---|---|---|
| 底层 | proc 进程 | 路径解析、协程池、带 panic 恢复的 goroutine 组、日志、版本号 | `apppath` `async` `pool` `logutil` `version` |
| 底层 | protocol 协议 | 跟外部系统说话：OneBot 出站、入站 webhook、大模型、MCP 两侧 | `onebot` `im` `llm` `mcpclient` `mcpserver` |
| 底层 | storage 存储 | 按 `workspace_id` 隔离的 SQLite 读写、进程内 KV | `state`、`kv` |
| 底层 | config 配置 | 读三份 YAML、分域校验、发布不可变快照、监听文件变化 | `config` `config/store` |
| 底层 | obs 观测 | Prometheus 指标、`/metrics` 端点、OTLP 链路 | `telemetry` |
| 中层 | conversation 会话 | 消息值对象、群窗口、去重、补名、入库、锚点 | `conversation`（原 `cache` 并入） |
| 中层 | asset 资产 | 给图片生成描述、全局图库、打不完的欠条与后台补打 | `assets` |
| 中层 | speech 发言 | 谁在跟机器人说话、要不要回、怎么生成、怎么排版、怎么发 | `speech`（原 `router`） |
| 中层 | account 账号 | 账号身份（平台 + 账号类型 + 账号）、开通、恢复、昵称 | `workspace`（身份部分） |
| 中层 | policy 策略 | 群白名单、工作空间名单、只记录不回复这三类判断 | 从 `config` 和 `workspace` 抽出 |
| 中层 | knowledge 分析 | **留位，不建**。信息流 → 摘要/标签 | 无 |
| 编排 | pipeline 流水线 | 三段缓冲、协程池、每会话串行、批量/linger/重试/死信 | `pipeline` `bot` |
| 编排 | agents Agent | **留位，不建**。主聊天 Agent、工作 Agent 的调度 | 无（现在由 `speech` 顶替） |
| 编排 | runtime 运行时 | Wire 装配、启动顺序、优雅关闭、派生配置 | `app` `cmd/good-review` |
| 编排 | admin 管理面 | 登录、账号/群/诊断/调试接口、内嵌前端 | `web/server` |

---

## 4. 底层：5 个域

底层最重要的动作是**把业务判断踢出去**，不然中层永远会从底层借业务能力。

| 模块 | 具体做什么 | 动不动 |
|---|---|---|
| `onebot.Client` | 出站。一个共享 `resty.Client`，`For(workspaceID)` 派生出绑定不同账号地址的副本；所有动作走泛型 `call[T]` 拼 `ActionRequest` POST 到 `{base}/`；网络错误包成 `SendError{Uncertain:true}`，让上层知道"发出去了但没确认" | 不动 |
| `onebot.Event` | 入站事件模型。`Platform()`、`ChatID()`、`MentionsBot()`、`RenderText(nameOf)` 都是纯函数 | 不动（QQ 味的字段在 §7.1 标注） |
| `im.Server` | 入站。校验 `Authorization`、解事件、按路径 `{account}` 调 `EventConsumer.AcceptEvent`，`accepted=false` 就回 404 让实现端自己兜 | 不动 |
| `llm.Client` | 对话。`Chat` 走多轮与工具调用，`ChatJSON` 拿结构化输出；外面套令牌桶和熔断 | 不动 |
| `mcpclient.Manager` | 连 MCP 服务、列工具、调工具、断线重连、按服务熔断 | 不动 |
| `mcpserver.Server` | 内嵌 `view_image`：读本地目录的图，也抓 URL | 不动 |
| `state` | 14 张表的读写，`Scope(workspaceID)` 给每个工作空间一个隔离视图 | 拆类型 + 提路径，见 §9.2 |
| `config` | `Load(Sources)` 四步：读文档 → 遍历 `sections` 解码 → 各域 `SetDefaults()` → 各域 `Validate()`；再经 `assemble` 转成业务读的 `Config`。`store.Informer`/`Observer` 管热更新 | 减 6 个方法，见下 |
| `telemetry` | 指标注册表与 `/metrics`，OTLP 追踪初始化 | 不动 |

### 底层唯一要动的地方：`config` 身上的 6 个业务判断

`config.Config` 现在挂着这些方法：

| 方法 | 它判断什么 | 去哪 |
|---|---|---|
| `HasGroup(groupID)` | 群在不在白名单 | `policy.AllowsStream` |
| `AllowsAllGroups()` | 白名单有没有 `*` | `policy.AllowsAllStreams` |
| `HasWorkspaceAccount(id)` | 这个账号准不准开通 | `policy.AllowsAccount` |
| `AllowsAllWorkspaceAccounts()` | 账号名单有没有 `*` | `policy.AllowsAllAccounts` |
| `IsRecordOnly(id)` | 这个账号只记录不回复 | `policy.IsRecordOnly` |
| `AllowGroupsStr()` / `RecordOnlyAccountsStr()` / `WorkspaceAccountsStr()` | 拼成逗号串给启动日志 | 留在 `config`（纯格式化，没有判断） |

搬完之后 `config` 包里没有判断逻辑了，只剩字段和加载。配置对象该回答"配置值是什么"，不该回答"这个群放不放行"。

---

## 5. 中层：6 个域

### 5.1 conversation 会话域

管"这个群现在有哪些消息、说话的人叫什么、上次发到哪一条"。选窗和前缀缓存都建立在它上面，谁都不许绕过它直接读 `state`。

| 模块 | 具体做什么 |
|---|---|
| `Message` 值对象 | 一条消息。`MessageID` 保留平台原值，`RowID` 是 `messages.id` 自增主键（发给大模型看的 message_id 用它，跨窗口跨重启不变），加 group/self/user/nick/content/images/image_ids/reply_to/ref_idx/time |
| 环形缓存 | 按 `group_id` 一层键，固定大小 buffer 只分配一次，配 `seenSet` 做 O(1) 去重。`Add` 必须零分配（`BenchmarkAdd` 的 allocs/op 是门禁） |
| 窗口 | `FindMsgIndex` 找触发消息、`cutAtMessage` 截到触发消息为止、`Select` 按锚点决定这次发几条 |
| 锚点 | `Anchor{Start, LastSent, At}`，`Expired(ttl, now)` 判断服务端 KV 缓存还在不在。超过 TTL 再按"命中前缀"扩展，实际是全额重算 |
| 补名 | 一批消息先合并成一次 `ResolveNames`（内存 → `members` 表），miss 的再异步 `get_group_member_info`，学到后回填内存与库 |
| 入库 | 正文在 Stage 1 就同步写 `messages`，拿到 `RowID` 挂回 `Message`；图片标识等 Stage 2 再 `UpdateMessageImages` 补写 |
| 成员目录 | `Directory`：`names` 存展示名、`seen` 存"出现过的人"、`pending` 防重复查网络。两个 map 不能混用——统计人数用的是 `seen`，没名字的人也算 |
| 群档案 | `groups` 表 + 异步查群名。实现端拿不到名字时会把 `group_id` 原样返回，那不是群名，要拦住 |

**要动的**：把 `workspace/sync.go` 的 `PrepareBatch` 搬进来，签名从 `[]pipeline.InboundEvent → []pipeline.SyncedMessage` 换成 `[]Inbound → []Synced`。两个值对象定义在本域。

### 5.2 asset 资产域

| 模块 | 具体做什么 |
|---|---|
| `ImageTagger.TagURLs` | 一条消息的图挨个降采样 → 算 `ImageID{sha, size}` → 查全局图库 → 没有就让视觉模型写描述 → 写 `image_notes`。返回 `TagResult` |
| 图库 | `image_notes` 表按 `ImageID` 做键。描述只写图本身不写聊天语境，所以能跨群跨账号复用。**不存文件地址**——旧消息的图可能已经被清理，候选列表的 url 永远取当前消息自己的 |
| 欠条 | 这次打不完的（超时、并发满了）不阻塞，写一行 `AssetTagJob` 进 `asset_tag_jobs`，状态 `pending` 或 `failed` |
| 后台补打 | `Backfiller.Run(ctx)` 按 `BackfillInterval` 扫欠条：先选候选，再用条件 UPDATE 抢占（`pending/failed` 或租约过期的 `running` → `running`），`BackfillMaxAttempts` 次后放弃，结果回写图库 |

**要动的**：`TagResult.Assets` 现在是 `[]pipeline.Asset`，把 `Asset` 值对象定义挪进本域。

### 5.3 speech 发言域

现在 `router.Router` 一个对象六件事。拆成 5 个模块，`Router` 只剩一个薄的分发入口。

| 模块 | 具体做什么 | 现在在哪个文件 |
|---|---|---|
| 指令表 | 前缀树建路由表，`atomic.Pointer[routeTable]` 整体替换；内部指令单独存一份，`#切换人格`、`#取消人格` 走内部指令 | `command.go` `internal_cmd.go` |
| 判定 | 这条消息冲谁来：私聊、`mention` 段、适配器 `adapter_at_me` 标记、昵称开头。非点名时跑一次裁判 `Verdict{Reply, LongForm, Focus, Reason}`，`Reply=nil` 表示模型没给结论 | `proactive.go` |
| 生成 | 选窗 → `BuildChatLog` → 拼 `userMsg` → 调 `Chat`（带工具就进多轮）→ 拿回文本 | `chat_review.go` `mcp_loop.go` |
| 排版 | 句子数上限、`ReplySendInterval > 0` 就拆最多 5 条逐条发；知识型长篇跳过排版 | `reply_format.go` |
| 发送 | 句子转成 `ReplyAction`，`ID = workspace\|msg_id\|第几句`；先查台账 `Load`，已发就跳过；传输错误按"已发未确认"记 `Sent=true, SendOK=false` 不重发 | `actions.go` |
| 人格 | 按群绑 `keyword`，切换时把人格与共享规则**值拷贝**进来（配置重载会让旧指针失效），只持久化关键字不存渲染结果 | `persona_render.go` |

**要动的**：把"生成"和"发送"拆开，见 §7.3。这是为多 Agent 留的唯一一处结构性改动。

### 5.4 account 账号域

管"这是哪个机器人账号、它叫什么、要不要接管这个号"。包名从 `workspace` 改过来，见 §9.1。

| 模块 | 具体做什么 |
|---|---|
| 身份 | `ChannelID(platform, kind, account)` 拼成 `{platform}_{kind}_{account}`，三段各自正则校验。`self.user_id` 不参与归属——QQ 官方群内 openid 换群就变，用它做键会把一个号拆成好几个 |
| 开通 | `ResolveOrCreate`：`kind` 为空就只按 (platform, account) 找已有的；找不到返回 `ErrKindRequired` 不猜。名单里没有就返回 nil，入站层回 404 |
| 恢复 | 启动时按 `workspace_profile` 表恢复，不依赖第一条事件，面板一开就能看到 |
| 昵称 | `get_self_info` 结果进内存 + `workspace_profile.name`；`SyncSelfName` 每 10 分钟最多核对一次，改名后先改成员目录再镜像 meta |
| 群内身份 | `LearnIdentity(groupID, selfUserID)` 记下来，值没变不写库（每条消息都会调） |

### 5.5 policy 策略域

纯函数包，没有状态、没有字段、没有依赖。

```go
policy.AllowsStream(cfg, streamID)   // "*" 或群号命中
policy.AllowsAccount(cfg, chID)      // 只判"新建"，已有通道不受影响
policy.IsRecordOnly(cfg, chID)       // 不支持 "*"，全静默等于没有机器人
```

三个函数，每个 5 行。它存在的意义不是省代码，是让"这条规则只有一个实现"这件事有物理位置。

### 5.6 knowledge 分析域（留位，不建）

长期用来做"信息收集 → 分析总结"。现在不进代码，§7.2 说清楚什么时候建、建在哪。

---

## 6. 编排层：4 个域

编排层允许出现"什么时候做什么"的判断，但**不许出现业务规则**。

### 6.1 pipeline 流水线

| 模块 | 具体做什么 |
|---|---|
| `Item` | `{ID, Key, Stage, Body, Attempts}`。`Body` 是 JSON，所以以后换成消息队列时业务代码不用改 |
| `Buffer` | 每会话分区的内存缓冲，容量 `PerChatCapacity` / `GlobalCapacity`，满了 `Put` 返回错误让入站层回 503 |
| `Stage` | 固定协程池 + 就绪队列，批量 + linger + 重试。同一会话由 `inflight` 标记保证串行，不同会话并行；并发数就是池子大小 |
| 死信 | 超过重试上限的落 `alerts` 表，`Kind = "dead_letter_" + stage`，`Detail` 里带完整 item body |

**边界**：`pipeline` 只管"怎么排队、怎么重试、怎么隔离失败"。它不判断"这条消息要不要回"、"这个群放不放行"。现在 `bot/stages.go` 里那些判断要挪走：

| 现在在 `bot` 里的判断 | 挪到哪 |
|---|---|
| `addressed()`（私聊 / @ / 适配器标记 / 昵称开头） | `speech.IsAddressed` |
| `cfg.IsRecordOnly(ws.ID)` | `policy.IsRecordOnly` |
| `recordOnlyReply` 固定话术 | `speech` 的一个常量 |
| `recordAging`（触发消息已不在窗口） | 判定留 `conversation`，写 `alerts` 表留编排层 |

### 6.2 agents Agent 编排（留位，不建）

现在只有一个 Agent（就是 `speech` 里那套"选窗 → 生成 → 排版 → 发送"）。**不建接口，不建包。** 理由和 Trigger 见 §7.3。

### 6.3 runtime 运行时

`app` + `cmd`。组合根、Wire 图、启动顺序、优雅关闭、派生配置。这部分现在结构是对的（`providers.go` 说"我由什么造出来"、`build.go` 装 Wire 表达不了的东西），不用动。

### 6.4 admin 管理面

`web/server`。跨域读多个域的状态拼一个响应（账号列表、群列表、消息、诊断、调试），所以它在编排层。它只读不写业务数据，唯一的写操作是切人格、清缓存、改 settings。

---

## 7. 三条长期路线的扩展点

这一段回答"以后要接多个平台、要做分析、要多 Agent，现在该怎么准备"。

判断标准只有一条：**现在做的改动，是不是等需求真来了才做会付出更多代价。** 是就现在做，不是就先不做。

### 7.1 多渠道接入

**现状**：`im.Server` 只解 OneBot 12；`bot.AcceptEvent(ctx, account, onebot.Event)` 的第二个参数就是 OneBot 事件；`conversation` 里 `Message.GroupID` 就是群号。

**缺什么**：一个跟渠道无关的入站值对象。

**现在做吗：不做接口，只改两处命名。**

理由是"接论坛/邮件时需要什么字段"现在只能猜。猜出来的接口多半长这样：

```go
type Adapter interface {
    Parse([]byte) (Item, error)
}
```

然后接邮件时发现邮件要 `Subject`、要 `ThreadID`、要 `In-Reply-To` 头，接论坛发现要 `BoardID`、`FloorNo`、`QuoteOf`。接口得改。这时候接口带来的不是解耦，是一层要跟着改的壳。

**但有两件现在必须做，代价极小：**

1. **不要往 `conversation` 里继续加 QQ 特有字段。** 已经有的三个标出来，以后新渠道的信息一律走 `Extra string`（JSON）：

   | 字段 | 为什么是 QQ 特有 |
   |---|---|
   | `RefIdx` | QQ 官方的 `REFIDX`，其他平台没有这个概念 |
   | `SelfID` | QQ 官方是群作用域 openid，换群就变 |
   | `Message.Card` | QQ 的群名片，跟 `Nickname` 是两个东西 |

2. **把 `GroupID` 的语义在心里改成 `StreamID`。** 代码和库列名先不改（14 张表、`workspace_id` 出现 69 次、`config.yaml` 和前端都在用），但写新代码时想的是"一条信息流"而不是"一个群"。这样接论坛时 `StreamID` = 板块，接邮件时 = 邮件会话，不用改表。

**什么时候真做**：第二个渠道的接口文档拿到手之后。那时候 `ingest.Item` 的字段是从两个真实实现里取的，不是猜的。位置在 `protocol` 层加一个渠道子包，`conversation` 完全不用动。

### 7.2 信息收集与分析

**现状**：`messages` 表已经在收集了，但只有 QQ 群消息，没有主题、没有标签、没有摘要。

**缺什么**：一个只读的分析层。

**现在做吗：不做。只保证一件事——`messages` 是唯一的信息来源，分析不能改它。**

这一条现在已经满足（`messages` 只有 `SaveMessage` 和 `UpdateMessageImages` 两个写入口），但要守住：以后加分析结果时，**新开表，不要往 `messages` 加列**。原因很实在：`messages` 是热表，每条消息都写，加一列意味着 `messageRow` 结构、`SaveMessage` 的 upsert 冲突目标、`LoadRecent` 的扫描都要动，而且历史数据要回填。

**真做的时候长这样**（写在这里是为了让现在的字段设计不挡路）：

```
conversation 读 messages（按 StreamID + 时间窗）
    ↓
knowledge.Abstract(窗口) → Digest{Topics, Summary, KeyPoints, SourceRowIDs}
    ↓ 写新表 knowledge_digest，SourceRowIDs 指回 messages.id
```

`Digest` 是只读产物，谁都能读（Agent 读它当上下文、面板读它做展示），没人会改它。它天然是个值对象。

### 7.3 多 Agent 协作

**现状**：`router/chat_review.go` 第 41 行 `chatReplyNow` → 第 101 行 `replyWithWindow`，后者一路走到发送。生成和交付在同一个函数里，中间没有出口。

**缺什么**：一个"生成结果"的值对象，让生成方不用管结果去哪。

**现在做吗：做，这是三条路线里唯一现在就该动手的。**

理由：这一步的代价是**改一个函数的返回值**，收益是多 Agent 的前置条件。再往后拖，`replyWithWindow` 里的发送逻辑会越长，拆的成本越高。

具体改动：

| 现在 | 改成 |
|---|---|
| `chatReplyWithSnapshot(...) (string, bool)` | `Generate(...) (*Reply, error)` |
| `Reply{Sentences []string, LongForm bool, Anchor Anchor}` | 新值对象，只装结果 |
| `replyWithWindow` 里生成完直接 `sendAction` | 拆成 `Generate` + `Deliver(ctx, Reply, Sink)` |
| `Sink` 现在只有"聊天渠道"一种 | 枚举或接口，先只实现 chat |

`Sink` 先做成两三个常量就行，不要做接口：

```go
type Sink int
const (
    SinkChat  Sink = iota // 发到聊天渠道
    SinkPanel             // 交到管理面板
    SinkAgent             // 交给另一个 Agent
)
```

这样以后：

| Agent | 它干什么 | 输出到哪 |
|---|---|---|
| 主聊天 Agent | 跟人聊、传信息、确认需求 | `SinkChat` |
| 工作 Agent | 执行专业任务（查数据、跑工具、写报告） | `SinkPanel` 给人看，或 `SinkAgent` 回给主聊天 Agent 转述 |
| 分析任务 | 定期梳理信息流 | 写 `knowledge_digest`，不产生 `Reply` |

**关键原则：Agent 之间不直接调用，只通过 `Reply` 交接。** 主聊天 Agent 不知道有多少个工作 Agent，它只管把用户的话变成 `Reply`，把 `Reply` 交给 `Sink`。加第三个、第四个 Agent 不用改前两个。

**明确不做**：现在不建 `agents` 包、不建 `Agent` 接口、不做 Agent 间消息总线。等第二个 Agent 真出现，接口从两个实现里长出来。现在建的 `Reply` 和 `Sink` 是值对象和常量，不是抽象层，改起来不心疼。

---

## 8. 存储层：不引 Repository 接口，引 Dialect

以后可能接 MySQL 或 PostgreSQL。结论：**不要引 Repository 接口。**

### 8.1 现在跟 SQLite 绑死的有多少

`state.go` 208 行 + `schema.go` 317 行，按职责数：

| 位置 | 绑了什么 | 行数 |
|---|---|---|
| `Open()` state.go 75-112 | `sqlite.Open(dsn)`、DSN 里 `_pragma=journal_mode(WAL)&synchronous(NORMAL)&busy_timeout(5000)`、`SetMaxOpenConns(1)` | 38 |
| `migrate()` schema.go 48-261 | 12 条 `CREATE TABLE ... INTEGER PRIMARY KEY AUTOINCREMENT`、10 条索引、2 个群统计触发器（含 `strftime('%s','now')`、`MAX(last_ts, NEW.ts)`）、`PRAGMA user_version` 读写 | 214 |
| `ensureFile()` schema.go 28-46 | 建本地文件、`chmod 0600` | 19 |
| `Report()` state.go 136-156 | 库体积含 WAL，阈值 `warnBytes = 100<<20` | 21 |
| `tableExists()` schema.go 311-317 | `SELECT COUNT(*) FROM sqlite_master` | 7 |
| **合计** | | **299** |

`state` 包非测试代码共 2305 行。上面这些是纯 DDL 与驱动调用；剩下的既有 GORM 链式调用（`.Model(...).Where(...).Updates(...)`），也有各表写入的裸 SQL（`?` 占位、`ON CONFLICT(...) DO UPDATE SET`、`strftime`）——裸 SQL 这部分同样绑 SQLite 方言，换库时得另写。

证据：消息写入的并发兜底用的是（主路径是按三元组先 `UPDATE`，没命中才插入）

```go
clause.OnConflict{
    Columns:   []clause.Column{{Name: "workspace_id"}, {Name: "group_id"}, {Name: "message_id"}},
    UpdateAll: true,
}
```

`gorm.io/driver/postgres` 翻成 `ON CONFLICT (workspace_id,group_id,message_id) DO UPDATE SET ...`，`gorm.io/driver/mysql` 翻成 `ON DUPLICATE KEY UPDATE ...`。这些调用一行都不用改。

### 8.2 为什么接口解决不了这个问题

Repository 接口抽象的是方法签名——`SaveMessage`、`LoadRecent`、`LoadAnchors`……这些签名换库时一个都不变，因为 GORM 已经在做隔离了。要改的永远是接口**背后**那 299 行 DDL 加上各表的裸 SQL，而不管有没有接口，这部分都得另写一份。

加接口的实际代价：

1. **40 个方法被钉死。** 以后加一列、改个返回值，接口、实现、可能的假件要一起动。
2. **会把降级语义带歪。** 现在写失败 `return 0` + 一条 warn，读失败 `nil` + 一条 warn，调用方继续用内存缓存。写成接口后很难忍住不改成 `(int64, error)`，那就变成全仓调用点改动——而这条降级策略是 `state.go` 第 7-8 行刻意选的。
3. **假件测不出真问题。** 单测现在跑真 SQLite 临时文件。换内存假件，`OnConflict` 冲突目标写错、`RenameSelfMember` 的 WHERE 写错，假件永远发现不了。改主键那次 `OnConflict` 冲突目标失效，只有真库能暴露。

### 8.3 什么时候再引接口

满足任一条再动，不要现在拍：

- 同一份代码要在两个库上同时跑（本地 SQLite + 云端 MySQL）
- 同一种数据出现两个存储（SQLite 当缓存、远端当主库）
- 要给插件或 SDK 暴露存储能力，又不想让它 import gorm

真到那时候，接口形状从第二个实现里长出来，比现在猜准。

### 8.4 现在该做的三件小事

**第 1 步：`Open` 收成配置结构（约 40 行）**

```go
type Config struct {
    Driver       string // "sqlite" | "mysql" | "postgres"
    DSN          string
    MaxOpenConns int
}
func Open(cfg Config) (*Store, error)
```

SQLite 的 `_pragma` 串放到 `SQLiteDSN(path)` 由调用方拼。`ensureFile` 只在 `Driver == "sqlite"` 时跑。`SetMaxOpenConns(1)` 挪进来——这个值属于方言，不属于 state。

顺带解决一处路径耦合：`workspace.Manager.New` 现在有

```go
statePath := filepath.Join(filepath.Dir(systemPromptPath), state.FileName)
```

账号域不该知道库文件放哪。改成 `storage.dsn` 配置项，`Manager.New` 只收一个已经开好的 `*state.Store`。

**第 2 步：DDL 按方言分文件**

```
state/dialect/dialect.go   // 4 个方法
state/dialect/sqlite.go    // 现在的 schema.go 建库代码
state/dialect/mysql.go     // 以后加
```

```go
type Dialect interface {
    CreateTables(*gorm.DB) error       // 12 张表、10 条索引、2 个群统计触发器
    Version(*gorm.DB) (int, error)     // PRAGMA user_version ↔ SELECT version
    SetVersion(*gorm.DB, int) error
    TableExists(*gorm.DB, string) bool // sqlite_master ↔ information_schema.tables
}
```

**这个 4 方法接口才是真的移植边界**，它挡的是方言语法，而方言语法就是上面那 299 行 DDL 加 `tableExists` 那类查系统表的语句。

一个判断：**没有历史迁移要翻译。** 开发阶段已经把 v1→v16 的升级链整段删掉，建库只有一套语句，`migrate` 遇到版本对不上的库直接报错让人重建。换库时只要把 `CREATE TABLE`、10 条索引和那两个触发器按目标方言写一遍。

**第 3 步：连接池参数交给方言**

| 驱动 | MaxOpenConns | 理由 |
|---|---|---|
| sqlite | 1 | WAL 单写者，多连接只会换来 `database is locked` |
| mysql / pg | 20-25 | 网络库，要给并发查询留通道 |

### 8.5 换库时会撞上的两件跟语法无关的事

**关停顺序。** `app.Shutdown` 现在是 `Workspaces.Wait()` 排空在途任务 → `Close()` 关库。SQLite 写完就落地，这个顺序够了。换 MySQL 后 `Close()` 会关连接池，在途事务直接断——行为一样，但错误文本从"磁盘满"变成 `sql: database is closed`。代码不用改，但要知道会看到什么。

**`Report()` 的体积统计。** `warnBytes = 100<<20` 跟 `-wal` 文件是 SQLite 概念。MySQL 下要么返回 0（告警永远不触发），要么改成查 `information_schema.tables`。做成方言可选方法，别伪造一个假字节数。

### 8.6 一个 MySQL 特有的坑

真上 MySQL 之后，`OnConflict{Columns: {workspace_id, group_id, message_id}}` 里的 `Columns` 会被**忽略**——MySQL 不支持指定冲突列，它见到 `ON DUPLICATE KEY UPDATE` 就对**任何**唯一键冲突执行更新。

现在 `messages` 表上只有自增主键和那个三元组唯一键，自增主键不会冲突，所以安全。但以后要是在 `messages` 上加第二个唯一键，MySQL 下就会更新错行，而 SQLite/PG 下不会。这种差异接口层完全看不见，只能靠 DDL 分方言时写清楚。

---

## 9. 命名与目录调整

### 9.1 `workspace` → `account`（**这一步没做**，理由见 §13.4）

现在 `workspace` 包装了四件事：

| 职责 | 代码 | 行数 |
|---|---|---|
| 账号身份与开通 | `WorkspaceID()`、`ValidAccountKind`、`ResolveOrCreate`、`Find`、`restoreExisting`、`load` | 约 280 |
| 会话准备 | `sync.go` 的 `PrepareBatch` | 154 |
| 成员显示名目录 | `directory` + 9 个方法 | 约 244 |
| 机器人自身身份 | `name`、`nameSyncAt`、`identities`、`groups` 四个字段 + 三把锁 + `SyncSelfName` / `LearnIdentity` / `EnsureGroup` / `GroupName` | 约 200 |
| 面板视图 | `View`、`Views`、`ViewByID`、`displayName` | 约 60 |

拆法：

| 现在 | 目标 |
|---|---|
| 身份 + 开通 + 昵称 + 群内身份 + 面板视图 | `account` |
| 成员目录 + `PrepareBatch` | `conversation` |

改名清单：

| 现在 | 改成 | 说明 |
|---|---|---|
| 包 `workspace` | 包 `account` | 目录 `git mv` |
| `workspace.Workspace` | `account.Channel` | "一个机器人通道"，比"工作空间"准：有平台、账号类型、账号三段 |
| `workspace.Manager` | `account.Manager` | 名字不变 |
| `workspace.WorkspaceID()` | `account.ChannelID()` | 函数名跟类型 |
| `workspace.View` | `account.View` | |
| `workspace.directory` | `conversation.Directory` | |
| `Workspace.PrepareBatch` | `conversation.PrepareBatch` | 顺手换掉签名里的 `pipeline.*` |
| `workspace.ErrAccountKindRequired` | `account.ErrKindRequired` | |
| `AccountKindOfficialBot` 等三个常量 | `account.KindOfficialBot` | |

### 9.2 一个刻意不改的东西：`WorkspaceID` 这个标识符

类型叫 `Channel`，但标识符**仍然叫 `WorkspaceID`**：

- 14 张表的列名都是 `workspace_id`，`state.go` 里出现 69 次
- `config.yaml` 的键是 `bot.allow_workspace_accounts`
- 面板 URL 和内嵌前端字段里也是 workspace

改这个要动 DDL、迁移脚本、配置文件、前端四层，收益只是名字统一。留着当"通道标识符"，是一处刻意的名实不一致，比改一轮划算。

### 9.3 `workspace/state` → `state`

必须跟 9.1 捆绑做。`workspace` 改叫 `account` 之后它会变成 `account/state`，语义就错了——`state` 是所有域共用的存储，不属于账号域。

### 9.4 `account` 会不会跟面板的登录账号撞

不会，方向反而一致：`web/server/accounts.go` 里的 `AccountDetail` 注释写的是"面板上的机器人账号概览"，`SelfID` 是工作空间的接收账号——它说的就是机器人账号。登录用户走 `WebUsername` / `WebPassword` / `JWTSecret`，代码里没把登录用户叫 account。

唯一要守住的：以后别把登录用户塞进 `account` 包。

---

## 10. 迁移阶段

每个阶段单独能编译、能跑测试。顺序按"改动多机械决定风险低"排。

**阶段 1–6 已落地，第 7 阶段按计划没做。** 实际差异见 §13。

每阶段收尾统一验：

```bash
go build ./...
go vet ./...
go test ./...
cd tests/e2e && pnpm test     # 基线 12 过 / 2 既有失败（bot-flow、persona）
```

| 阶段 | 做什么 | 规模 | 风险 |
|---|---|---|---|
| 1 | 抽 `policy`：6 个判断从 `config` 和 `workspace` 搬出来，两份实现合成一份 | 新包约 60 行，删约 80 行 | 低，纯函数 |
| 2 | 值对象归位：`pipeline.InboundEvent` / `SyncedMessage` → `conversation`，`pipeline.Asset` → `asset`；删掉 `EnrichedMessage`；`PrepareBatch` 搬进 `conversation` | 改 6 个文件签名 | 中，会碰到 `bot/pipeline_test.go` |
| 3 | `state` 内部拆 7 个类型：`MessageRepo` / `AnchorRepo` / `DirectoryRepo` / `PersonaRepo` / `LedgerRepo` / `JobRepo` / `AdminRepo`。**同包内拆类型，不拆目录、不引接口** | 1463 行分文件 | 低，纯搬 |
| 4 | `workspace` → `account` 改名 + `workspace/state` 提到顶层 `state` | 约 10 个文件的 import | 中，机械但面广 |
| 5 | `router` 拆 `speech` 五个模块：指令表 / 判定 / 生成 / 排版 / 发送 | 约 1900 行分文件 | 中，会碰到 `persona_*_test.go` |
| 6 | 生成与交付解耦：`Reply` 值对象 + `Deliver(ctx, Reply, Sink)` | 改 1 个函数返回值 + 加 3 个常量 | 中，涉及行为，e2e 必跑 |
| 7 | **等真需求**：`Dialect` 4 方法 + `Open(Config)` | 方言文件约 261 行 | — |

阶段 4 的执行顺序（避免中途全红）：

1. 先在 `workspace` 包内加 `type Channel = Workspace`
2. 把调用方的 `workspace.Workspace` 批量改成 `workspace.Channel`，编译通过
3. `git mv workspace account`，全仓改 import 路径
4. 删掉类型别名，结构体本名改成 `Channel`
5. 同一提交里把 `state` 提到顶层

阶段 5 和阶段 6 建议合并看：拆 `speech` 的时候顺手把 `Generate` 和 `Deliver` 分开，比拆完再拆一次省事。

---

## 11. 刻意不做的事

| 不做 | 为什么 |
|---|---|
| Repository 接口 + 内存假件 | 只有一个实现；假件测不出 `OnConflict` 冲突目标写错这种真问题。见 §8 |
| 拆微服务、引 MQ | `Item` 已经是 `{ID, Key, Stage, Body}` 可序列化形状，好处留着。但现在只有一个进程，提前做是负债 |
| 多渠道 Adapter 接口 | "论坛/邮件要什么字段"现在只能猜，猜出来的接口接第一个新渠道就要改。见 §7.1 |
| `agents` 包、`Agent` 接口、Agent 间消息总线 | 只有一个 Agent，接口从第二个实现里长出来才准。现在只建 `Reply` 值对象和 `Sink` 常量。见 §7.3 |
| `knowledge` 包 | 没有真实分析需求。现在只守住"`messages` 是唯一信息来源、加分析结果要新开表"。见 §7.2 |
| GORM `AutoMigrate` | 它建出来的表跟现在手工 DDL 的默认值、唯一键、索引顺序有差异，14 张表逐列比对比重写 DDL 还费劲；且 AutoMigrate 不会删列 |
| 搬目录 | 除新增 `policy`、`conversation` 两个包和 `state` 提到顶层，其余包路径不动 |

---

## 12. 验收

架构上的约束没法靠编译器全查，但下面几条可以用命令查。

**1. 中层不许 import 编排层**

```bash
go list -deps ./conversation ./assets ./speech ./workspace ./policy \
  | Select-String 'good-review-master/(pipeline|bot|agents|app|web)$'
```

应该无输出。**`$` 不能省**：不加锚定的话 `app` 会匹配到 `apppath`，`bot` 会匹配到 `botXxx`，一跑就是一堆误报。

**2. 底层不许 import 中层**

```bash
go list -deps ./onebot ./im ./llm ./mcpclient ./mcpserver ./state ./config ./logutil \
  | Select-String 'good-review-master/(conversation|assets|speech|workspace|policy)$'
```

应该无输出。实际会报两条，改造前就有（那时包叫 `cache`）：

```
mcpclient -> conversation
state -> conversation
```

根因与处置见 §13.8。一句话是：`conversation` 里值对象那一半谁都能用，性质上等于底层。

**3. 业务规则一处实现**

```powershell
# 白名单通配符 "*" 应该只出现在 policy 包里
Get-ChildItem -Recurse -Filter *.go | Where-Object { $_.Name -notlike '*_test.go' } |
  Select-String -Pattern '== "\*"|g == "\*"' |
  ForEach-Object { $_.Path.Replace($PWD.Path + '\', '') } | Sort-Object -Unique

# @ 前缀判断应该只出现在 speech 里
Get-ChildItem -Recurse -Filter *.go | Where-Object { $_.Name -notlike '*_test.go' } |
  Select-String -Pattern 'HasPrefix\(.*"@"' |
  ForEach-Object { $_.Path.Replace($PWD.Path + '\', '') } | Sort-Object -Unique
```

两条命令的输出里，除 `policy/`、`speech/` 之外的路径都算回归。

一个例外要认下来：`config/section_bot.go` 里还会出现 `"*"`（第 157、191、206 行一带）。那是**配置校验**——判断"YAML 里这个值合法吗、`record_only_accounts` 不许写 `*`"，不是运行时策略。校验留在 config 是对的，别把它也搬进 policy。

**4. 性能门禁不能掉**

```bash
go test -bench . -benchmem ./conversation/ ./speech/
```

`BenchmarkAdd` 的 allocs/op 必须是 0。环形缓存的零分配是这条流水线能扛住消息量的前提，拆包时最容易顺手加个 `fmt.Sprintf` 就破了。

**5. 文档同步**

`docs/` 下其余专题文档（`three-stage-pipeline-design.md` 等）**没改**：它们是写作当日的设计记录，里面写 `router/`、`cache/` 是历史语境。读它们时以代码目录为准，本文 §13 是唯一的"落地后现状"。

顺序上要跟改代码同一批做，不然下一个人会照文档去 `router/` 里找文件。

一条实操坑记在这里：改包名不要直接用 PowerShell 的 `-replace` 做 `router.` → `speech.`。它默认大小写不敏感，`ws.Router.SetStore` 会被一起改成 `ws.speech.SetStore`——这个方向编译能抓出来，但要多跑一轮 `go build` 才发现。要么写 `-creplace`，要么先把 `.Router` 这种字段访问单独排除。

---

## 13. 执行记录：落地后与方案的差异

§1、§4 到 §10 写的是改造前的判断依据，不是现状描述（里面出现的 `router`、`cache`、`pipeline.InboundEvent` 都是当时的名字）。真实改完，下面几处与原文不一样，逐条记下来，免得下一个读文档的人按方案去代码里找不存在的东西。§11 的「刻意不做」清单仍然全部成立。

### 13.1 阶段 1：`policy` 按方案抽出来了

五个判断从 `config.Config` 与 `workspace.Workspace` 搬到 `policy`，两份实现合成一份：

| 原实现 | 现实现 |
|---|---|
| `config.Config.HasGroup` / `workspace.Workspace.AllowsGroup` | `policy.AllowsStream` |
| `config.Config.AllowsAllGroups` / `workspace.Workspace.AllowsAllGroups` | `policy.AllowsAllStreams` |
| `config.Config.HasWorkspaceAccount` | `policy.AllowsAccount` |
| `config.Config.AllowsAllWorkspaceAccounts` | `policy.AllowsAllAccounts` |
| `config.Config.IsRecordOnly` | `policy.IsRecordOnly` |

签名吃配置对象而不是裸 slice：`policy.AllowsStream(cfg *config.Config, streamID string) bool`。`*` 这个通配符落在「群白名单」还是「账号名单」上，是配置的事；policy 只管判。要是吃 `[]string`，每个调用方都得先知道该传哪个列表，选择逻辑又散出去了。

三个拼逗号串的格式化函数（`AllowGroupsStr` 等）按方案留在 `config`。

### 13.2 阶段 2：值对象归位，但 `Asset` 落在 `conversation`

方案 §5.1 说 `InboundEvent` / `SyncedMessage` → `conversation`，§5.2 说 `pipeline.Asset` → `assets`。前两件按方案做了；`Asset` 这一件换了落脚点：

**`Asset` 在 `conversation/asset.go`，不在 `assets`。** 理由写在文件头：打标签的动作在 `assets`，但结果要跟着消息一起走过 Stage 2 → Stage 3，而 `assets` 依赖 `conversation`（它要读 `Message` 和图注）。放 `assets` 的话 `conversation.Synced` 带不上 `Assets` 字段，只能再复制一份——正是 §1.5 要消掉的那个毛病。

**`EnrichedMessage` 是删掉，不是搬走。** 它存在的理由是 `cache.Message.ImageNotes` 打了 `json:"-"`：环缓存的序列化路径不带这个字段，于是流水线另立一个结构体装「消息 + 图注 + 资产」。但 `json:"-"` 只是个 tag，改它比多养一个结构体便宜——现在 `ImageNotes` 正常参与序列化，`EnrichedMessage` 随之消失，`conversation.Synced` 一个值对象从 Stage 1 走到 Stage 3。

**`PrepareBatch` 没搬进 `conversation`，但签名换了。** 方案 §5.1 说要搬。实际留在 `workspace`：

```go
func (w *Workspace) PrepareBatch(ctx context.Context, events []conversation.Inbound) []conversation.Synced
```

不搬的原因：它要缓存、成员目录、`state.Scope`、账号身份四样，三样在 `workspace`。搬过去等于把 `workspace` 搬空，还会让 `conversation` 反过来依赖 `workspace`——层次再翻一次。但它现在不再 import `pipeline`，§1.1 那条反向依赖已经消掉了，这才是当初要解决的问题。

### 13.3 阶段 3：`state` 按表分文件，没拆 7 个 Repo 类型

方案说拆 `MessageRepo` / `AnchorRepo` / ... 七个类型。**没拆**，只把 1463 行的 `state.go` 按表分成 9 个文件，`Scope` 门面保持原样。拆类型会更差：

- 七个 Repo 各持同一个 `*gorm.DB`，等于一个壳拆七份，`db.WithContext(ctx)` 一行没少，只是多了七次构造
- 要是不共享连接、各开一个——SQLite 单文件写锁，直接 `database is locked`

分文件已经拿到想要的东西：一个文件一件事，找 `send_ledger` 的读写不用在一个两千多行的文件里滚。

| 文件 | 表 |
|---|---|
| `state/state.go` | 包文档、`Scope` 门面、公共类型与错误 |
| `state/schema.go` | 建表（只负责把空库建出来） |
| `state/messages.go` | `messages` |
| `state/anchors.go` | `anchors` |
| `state/directory.go` | `members` `groups` `member_info_history` `group_info_history` |
| `state/persona.go` | `workspace_profile` |
| `state/images.go` | `image_notes` |
| `state/jobs.go` | `asset_tag_jobs` |
| `state/ops.go` | `send_ledger` `alerts` `settings`、`workspace_profile` 的列表查询 |

### 13.4 阶段 4：`state` 提上来了，`workspace` 没改名

`workspace/state` → `state` 做了（§9.3 的理由成立：`state` 是所有域共用的存储，不该挂在账号域下面）。

`workspace` → `account` **没做**，这是整轮唯一放弃的一步。把 §9.2 那笔账算完，只改 Go 包名是净负：包叫 `account`、类型叫 `Workspace`、字段叫 `WorkspaceID`，同一个概念三个名字。

§9.1 那份清单里真正有价值的不是「workspace 这个词不准」，而是「它装了四件事」。四件里 `state` 提走了，`policy` 提走了三个判断，剩下的 `PrepareBatch` 与成员目录是下一轮该拔的——拔完 §13.2 那点别扭（业务域还提着签名里的 `Workspace`）也就没了。届时一次改干净：包、类型、标识符同时改，而不是先在 Go 包里留个半成品。

### 13.5 阶段 5：`router` → `speech`，按角色拆文件

包改名做了（import 处只有 5 个文件，成本远低于收益）。`command.go` 从 619 行降到 138 行，`Router` 自己几乎没逻辑了：

| 文件 | 角色 | 行数 |
|---|---|---|
| `router.go` | 装配与生命周期：`Router`、注入 setter、`Go`/`Wait`、群人格存取 | 220 |
| `dispatch.go` | 薄入口：命中指令就分发，没命中走直答 | 116 |
| `command.go` | 指令表：前缀树 + 整表原子替换 + 热重载后重建 | 138 |
| `judge.go` | 判定：`IsAddressed`、`stripCQPrefix`、`trimLeadingMentionToken` | 60 |
| `prompt.go` | system prompt 拼装 | 87 |
| `generate.go` | 生成：选窗 → 拼 user 消息 → 调模型 | 344 |
| `reply_format.go` | 排版：长回复拆句 | 183 |
| `deliver.go` | 交付：句子 → 动作 → 台账 → 渠道 | 350 |
| `proactive.go` | 主动接话：裁判 + 每群串行 worker + 三重限流 | 494 |
| `persona_render.go` | 人格：按群绑定、渲染片段 | 98 |

方案说拆 5 个模块，实际多出 `router.go`（装配）、`dispatch.go`（入口）、`prompt.go`（提示词）三个。原因是原方案按「一件事一个模块」切，但 `Router` 那些注入字段和生命周期方法总得有个家，塞进任何一个业务文件都会让那个文件回到「一个文件两件事」。

**从 `bot` 搬下来的两个东西**（方案 §6.1 要求）：`isAtBot` 变成 `speech.IsAddressed`，`recordOnlyReply` 变成 `speech.RecordOnlyReply`。`bot.addressed` 现在只剩「昵称兜底能不能用」这个跟部署形态有关的输入：

```go
func (b *Bot) addressed(ws *workspace.Workspace, cfg *config.Config, synced conversation.Synced) bool {
	return speech.IsAddressed(synced.Event, synced.Message.Content, cfg,
		b.allowNicknameFallback(ws, cfg.BotNickname))
}
```

这是「同一问题只该有一份实现」最直接的收益：触发判定（Stage 3）与分发（`RouteMessage`）调同一个函数，不会再出现「Stage 3 认为是点名、分发认为不是」这种只有线上才看得见的错位。`allowNickname` 之所以是参数而不是从 `cfg` 里读：多账号且两号昵称相同时，`@昵称` 分不清是哪个号，只有调用方知道当前部署形态。

### 13.6 阶段 6：`Generate` 与 `Deliver` 分开

方案要求 `Reply` 值对象 + `Deliver(ctx, Reply, Sink)`。落了，三处细节值得写下来：

```go
// Reply 一次生成的产物：要发什么、发给谁、以及这次用掉了哪段窗口。
type Reply struct {
	Sentences []string
	LongForm  bool
	Event     onebot.Event
	ChatID    string
	Mention   bool
	Anchor    *conversation.LLMAnchor // nil 表示不推进锚点
}
```

**`Reply` 里带 `Event` / `ChatID` / `Mention`。** 方案只说「要发什么」。但 `Deliver` 不该回头再找这些——它拿到 `Reply` 就该能发，否则调用方得把上下文一路传下去，等于没解耦。

**`Anchor` 是指针，nil 表示这次不推进。** 兜底话术（「大师今天罢工了」）没有对应窗口，照常推进锚点会让下一轮以为那段前缀已经发过，白丢一次缓存命中。这是它用 `*LLMAnchor` 的唯一原因。

**失败兜底归交付侧，不归生成侧。** `Generate` 只返回 error，`replyWithWindow` 按错误类型决定发不发：`errLLMFailed` → 「今天罢工了」、`errNoHistory` → 「暂无群聊记录」、`errEmptyWindow` / `errTriggerGone` / 空回复 → 不发。群里主动接话没被点名时失败一律不发——当没听见比「罢工了」合适。

**`Sink` 用常量，没做接口。** 现在只有一个实现（发到聊天渠道）。`SinkPanel` / `SinkAgent` 是留给 §7.3 的常量，`Deliver` 遇到它们返回「尚未实现」而不是静默丢弃——静默丢弃会让人以为接上了。

**超时只包住调模型那一段。** `Generate` 里是 `genCtx, cancel := context.WithTimeout(ctx, cfg.LLMTimeout)`，只喂给 `runReviewLLM`；排版 `rewriteReply` 仍吃外层 `ctx`。如果整个 `Generate` 共用一个超时，主调用跑得久时排版只剩一点预算，而排版失败会退回原文——等于白白少排一次版。

### 13.7 阶段 7 没做

按方案（§8.3）等真需求。`Dialect` 4 方法 + `Open(Config)` 的前提是「第二个数据库真的要接」，现在只有一个 SQLite。

### 13.8 还剩的层次例外

`state` → `conversation`、`mcpclient` → `conversation` 这两条，§12 检查 2 会报出来。查证是改造前就有的（那时包叫 `cache`）：`state` 要持久化 `conversation.Message` / `ImageID` / `ImageNote`，`mcpclient` 用 `conversation.EstimateTokens` 估算工具结果有多大。

根因是 `conversation` 里混了两类东西：值对象（`Message`、`Synced`、`Asset`、`ImageNote`——谁都能用，性质上等于底层）与环形缓存 / 锚点 / 选窗（真正的中层）。要清零得把值对象拆到 `conversation/value` 之类的包，代价是每个调用方的类型名都要跟着改。

现在的判断是不划算：这两条是「底层用值对象」，不是「底层拿业务能力」，危害比 §1.1 那种反向依赖低一档。**这条债记在这里**——下一次动 `conversation` 时，如果拆包成本已经摊薄，就顺手把值对象分出去，§12 检查 2 才能做到零输出。

### 13.9 验收结果

```
go build ./...                       通过
go vet ./...                         通过
go test ./...                        19 个包 ok
BenchmarkAdd                        约 100 ns/op   7 B/op   0 allocs/op
cd tests/e2e && pnpm test            14 passed
```

e2e 比 §10 写的基线好：基线是 12 过 / 2 失败（bot-flow、persona），这次 14 个全过。那两个失败本轮没有专门修，通过原因没逐条追查（最可能是 `IsAddressed` 统一后判定口径一致），这条只当观察记下，不当正确性证据。
