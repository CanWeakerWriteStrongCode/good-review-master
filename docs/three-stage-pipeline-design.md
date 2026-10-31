# 三段式消息处理流水线设计

> 状态：方案待评审（2026-10-31）
> 范围：IM 入站 → 信息同步 → 资产打标签 → Agent，以及各段之间的缓冲层（含入站缓冲）
> 相关文档：[缓存方案技术决策](cache-design-decisions.md)、[多机器人账号工作空间隔离设计](multi-account-workspace-design.md)、[IM 协议标准化](im-protocol-standardization.md)

## 1. 目标

把一条 IM 消息从入站到 Agent 回复拆成三个职责清晰的阶段，并把阶段之间的衔接抽象成可替换的缓冲层：

```
IM(OneBot 12 → AstrBot / 未来的 Koishi)
   ↓  B1 入站缓冲（批量）
Stage 1 同步与补全（按 workspace，批量）
   ↓  B2
Stage 2 资产打标签（图片/视频/文件，批量）
   ↓  B3
Stage 3 Agent（批量出队，当前逐条执行 → 回复动作）
   ↓
IM 发送；机器人回复写回缓存/DB
```

设计要求：

1. **三个阶段独立**：每个阶段有明确的输入/输出契约，不共享内部对象。
2. **所有阶段批量执行**：Stage 1/2/3 都是按 `Key` 批量 `Take` 的消费者，没有哪个阶段是逐条同步执行的；批量大小和凑批等待时间可配。
3. **缓冲层可替换**：现在用进程内缓存实现，以后同一接口换成消息队列（Redis / NATS / Kafka / RabbitMQ），实现削峰填谷。
4. **消息窗口缓存不动**：`cache.GroupMsgCache` 是为 prompt 命中设计的专用结构（按群、按时间序、环形缓冲），不属于通用缓存抽象，也不参与 MQ 替换。
5. **每条消息的 `message_id` 全程保留**：Agent 以后可以对每条 @ 单独生成回复动作，当前不需要实现。
6. **数据库与缓存底座可替换**：GORM 先接 SQLite，保留换 MySQL 的接口边界；通用 KV 缓存用 Ristretto 实现，未来换 Redis。
7. **消息本体不丢，但回复任务可以放弃**：消息正文与 `message_id` 在 Stage 1 同步入库，任何阶段都不删消息；容量不足时只跳过可选的补全/打标签，并通过回压向上游暴露压力。只有当触发消息已经不在窗口里（窗口配小了）时才放弃这条回复任务，并调用报警记录“窗口小了，要调大”和完整现场。

## 2. 现状与改造点

当前一条消息的主链路：

| 位置 | 现在做什么 | 改造后归属 |
|---|---|---|
| `im/server.go` | OneBot 12 webhook 解码、鉴权、投递 | 不变 |
| `bot/ingest.go` | 全局队列（容量 1024）、工作空间归属、白名单、去重 | B1 入站缓冲（按 `workspace` + `chat` 分区）+ 入口过滤 |
| `bot/handler.go:ProcessMessage` | ensure 群/成员、学名字、补昵称、图片描述、写缓存/DB、路由 | 拆分到 Stage 1 + Stage 2 |
| `router/image_note.go:EnsureImageNotes` | 图片入库描述（同步） | 移到 `assets` 包（Stage 2） |
| `router/chat_review.go:chatReplyNow` | 读取缓存窗口、选窗、调 LLM、排版、发送 | Stage 3 的 Agent 执行体 |
| `router/proactive.go` | 非 @ 消息的裁判与每群串行 pending | 合并进 Stage 3 的 Agent 队列 |
| `router/reply_format.go` | 回复拆成 ≤5 句逐条发送 | 改为产出 `ReplyAction` 列表 |
| `workspace/state` | SQLite 全局库 + `workspace_id` 视图 | 换 GORM（SQLite），新增历史表与通用异步写队列 |
| `cache/cache.go` | 消息窗口 + 锚点 | **保持不变** |

## 3. 阶段契约

阶段之间的数据必须可序列化（只放 ID 与值，不放 `*Workspace` 之类指针），这样以后可以直接作为 MQ 消息体。

### 3.0 B1 输入：`InboundEvent`

IM 入口投递到 B1 的载荷：

```go
type InboundEvent struct {
    WorkspaceID string
    Event       onebot.Event // message_id、user_id、self_id、mention、图片 url 等
}
```

工作空间归属与白名单在入口判定，判定通过后才 `Put`；重复消息的去重也在入口完成，避免同一条消息进入 B1 两次。

### 3.1 Stage 1 输出：`SyncedMessage`

一条已经补全身份信息的消息：

```go
type SyncedMessage struct {
    WorkspaceID string
    ChatID      string
    Private     bool
    Event       onebot.Event  // 原始事件：Stage 3 的路由/引用/@ 判定要用
    Message     cache.Message // content、reply_to、images、time 等
}
```

`message_id` 必须保留；后续每条 @ 的单独回复都依赖它。

### 3.2 Stage 2 输出：`EnrichedMessage`

```go
type EnrichedMessage struct {
    SyncedMessage
    ImageIDs    []cache.ImageID   // sha256 + size
    ImageNotes  []cache.ImageNote // 全局图库里的描述（可能为空）
    Assets      []Asset           // 每个资产：kind、url、id、标签、状态
    AssetStatus string            // ok | degraded | deferred | failed
}
```

`cache.Message.ImageNotes` 带 `json:"-"`（只在本进程窗口里用），所以要单独放一份才能过缓冲。

Stage 2 完成标签后，把资产结果更新到该消息（消息本体在 Stage 1 已同步入库），再提交窗口缓存并投递到 B3，保证 Agent 看不到“半成品”消息。标签失败时仍然提交，并带 `AssetStatus`。

### 3.3 Stage 3 输入：`EnrichedMessage` 与冻结快照

Stage 3 的缓冲载荷就是 `EnrichedMessage`（每条消息一个 `Item`），没有单独的 `AgentTask` 结构：批量 `Take` 只用于出队效率，处理器逐条执行。

```go
// 处理一条消息时在进程内构造，不进入缓冲/消息队列。
type Snapshot struct {
    Messages     []cache.Message // 已冻结的窗口
    Participants map[string]Participant
    Group        GroupInfo
    Assets       []Asset
    SelfID       string
    FreezeID     string
}
```

快照在 **Agent 出队后、逐条执行前** 冻结：每条消息以自己的 `message_id` 为边界从窗口缓存复制；运行期间新到的消息继续进入窗口缓存与队列，但不会改变本次快照。以后按批执行时，才改成整批最后一条为边界。

出队后先确认该消息仍在窗口缓存里：找不到（环形缓冲已淘汰、窗口配置偏小）就放弃这条回复任务，写 `alerts` 表并记错误日志“窗口小了，要调大”，同时保存原始事件、窗口首尾与当前配置。消息本身早已同步入库，不会因为放弃回复而丢失。

“只回最后一条点名消息”也在这一层实现：同一会话的主动任务按 latest-wins 覆盖，队列空时才执行最新任务，旧任务直接丢弃。

裁判（是否值得主动接话）**不是独立阶段**，它是 Stage 3 内部的一步：同一条消息先由裁判判定，再由同一个 Agent 执行体决定是否生成回复，共用同一份快照与队列。

### 3.4 账号类型扩展（OneBot 12 扩展字段）

`self.platform` 只表示 IM 软件（`qq` / `telegram` / …），**不要**把“官方机器人 / 个人号”塞进 platform：platform 参与 workspace ID、指标标签和扩展命名，改了会拆出新的工作空间。

做法是加一个顶层扩展字段，不放进 platform：

```json
{
  "self": {"platform": "qq", "user_id": "..."},
  "account_kind": "official_bot"
}
```

- 取值：`official_bot` / `personal_account` / `app`；`unknown` 只用于读取历史档案，不用于新开通；
- **工作空间键 = `{platform}_{account_kind}_{account}`**：三元组一起决定归属，ID 如
  `qq_official_bot_1906xxxx`、`qq_personal_account_123456789`。只拼 platform+account
  不够：官方 AppID 和个人 uin 可能撞号，撞上会让消息、缓存、人格和成员身份串台；
- 账号类型在首次开通时必须知道，且此后不变；缺失时拒绝新开通，不猜 `unknown`。
  画像表同时存 `platform` / `account_kind` / `account`，并对三者建唯一索引；代码不解析 ID 反推；
- **platform 归一**：转换层应把个人 QQ（AstrBot 的 `aiocqhttp`）也映射成 `platform=qq`，用 `account_kind=personal_account` 区分；不要用 `aiocqhttp` 当平台名，否则同一个 IM 会拆成两个平台、workspace ID 和指标都对不上；
- Go 在首次事件或开通工作空间时把它写入 `workspace_profile.account_kind`，并作为工作空间键的一部分；
- `identity_scheme` 可由它推导（`official_bot` → `qq_openid`，`personal_account` → `qq_uin`），但正式的 `capabilities` / `limits` 建议由转换层通过一次性 `POST /im/profile/{account}` 上报，事件里只带 `account_kind` 作为兜底；
- 转换层（AstrBot、以后的 Koishi）都填同一个字段，业务层不按平台名或实现名硬编码。

## 4. 缓冲层抽象

### 4.1 接口

```go
type Item struct {
    ID       string // 幂等键：stage + workspace + chat + message_id
    Key      string // 分区键：workspace|chat
    Stage    string // ingest | sync | assets | agent
    Body     []byte // JSON 编码的阶段数据（强制可序列化）
    Attempts int
}

type Buffer interface {
    Put(ctx context.Context, item Item) error
    Take(ctx context.Context, key string, max int) ([]Item, error) // 批量取
    Ack(ctx context.Context, items []Item) error
    Close() error
}
```

- `Body` 用 `[]byte` 而不是泛型对象：保证契约天然可序列化，换成 MQ 时不需要改业务代码；聊天消息量下 JSON 开销可忽略。
- `Key` 统一为 `workspaceID + "|" + chatID`，同一群的顺序由分区键保证；
  `workspaceID` 本身是 `{platform}_{account_kind}_{account}`，账号类型不同天然分到不同分区。
- **分区单位是会话，不是工作空间**：一个工作空间（机器人账号）下面有多个群和私聊，每个 `Key` 有自己的有序队列和按需 worker；工作空间只是共享缓存、Router、数据库视图和配置的容器。如果按工作空间只开一个 worker，一个群的大模型调用会把同账号下所有群都堵住。
- `Take` 是批量语义：**Stage 1 / 2 / 3 的消费者都按批取数**，一次取走当前待处理项，即“从缓存区拿下一批”。
- `Ack` 现在可以是空操作，但接口保留；MQ 实现用提交位点/可见性超时，配合 `Item.ID` 做幂等。

### 4.2 三个衔接点

| 缓冲 | 生产者 | 消费者 | 载荷 |
|---|---|---|---|
| B1 | IM 入口（`AcceptEvent`） | Stage 1 | `InboundEvent`（原始事件 + workspaceID） |
| B2 | Stage 1 | Stage 2 | `SyncedMessage` |
| B3 | Stage 2（提交后） | Stage 3 | `EnrichedMessage` / `AgentTask` |

B1 取代现在的全局事件 channel：HTTP 入站 `Put`，Stage 1 按 `workspace` + `chat` 批量 `Take`。工作空间归属与白名单仍在入口判定；重复消息的去重建议放在入口，避免重复项进入 B1。

### 4.3 内存实现（当前）

- 每个 `Key` 一个 FIFO + 条件变量；`Take` 阻塞到有数据或 ctx 取消。
- 容量、等待时长、重试次数、回压/跳过标签次数全部出指标；**消息本体不丢弃**。
- `Put` 保持现有背压语义：队列满时最多等待 `enqueueTimeout`，超时返回错误（IM 层回 503），不静默丢消息。
- **确认与重试**：`Take` 只是借出，`Ack` 才划掉；处理失败按条重试，超过上限的“问题件”落库并报警（保留原始载荷与错误），不静默丢弃，也不无限重试。
- **全局并发上限**：每段一个独立的全局信号量，限制同一时刻最多几个会话在处理。M0 设为 1 且只保留一个全局消费循环，行为与现在完全一致；M4 再打开每群并行。没有这道闸，群一多就会同时发起大量模型调用。
- **同群顺序**：并行只发生在不同 `Key` 之间，以及同一条消息的多个资产之间；同一个 `Key` 的多条消息按输入顺序处理、按输入顺序提交到下一段。批量只减少查库/调用次数，不并行处理同群的多条消息。
- **同一会话在每个阶段最多一个 worker**：Stage 1/2/3 可以同时处理同一个会话的不同消息（流水线），但同一阶段内严格按输入顺序，不会两条消息同时处理。一个会话忙碌时可能同时有 1~3 个协程，分别停在三个工位上。
- **空闲回收（让处理任务休眠，不是卸载工作空间）**：某个 `Key` 连续空闲超过 `idle_ttl` 后，它的消费 worker 退出、释放协程；工作空间目录、成员信息、窗口缓存、数据库记录全部保留，队列里未完成的项也保留，下条消息到来时按需重新拉起。回收的只是“处理者”，不是数据，更不是工作空间本身。
- 这里的 worker 在 Go 里是 goroutine（可类比 Java 的虚拟线程/任务），不是操作系统线程；运行时线程池始终存在并被复用，“回收”只结束这个会话的 goroutine，不销毁线程池。
- **关停**：先停止接收新项，给存量队列 `drain_timeout` 排空；到期仍未完成的落库为待恢复记录并报警。进程内实现崩溃后无法重投，接 MQ 后由至少一次投递补齐。
- Stage 1 / 2 / 3 的消费 worker 都按 `Key` 跑（每群一个串行处理流），不同 `Key` 之间并行；这也是之前暂缓的“每群独立处理流”的落地时机。
- 进程内实现天然不保证崩溃后重投；MQ 实现再补上。

### 4.4 MQ 实现（以后）

- `Put` = publish（按 `Key` 分区），`Take` = 批量拉取，`Ack` = commit。
- 至少一次投递 + 幂等；消费失败重试，超过次数进死信。
- 批量消费：同一分区一次 drain 多条，对应各阶段的批量语义，实现削峰填谷。

### 4.5 批量语义

- 每个阶段都是一个（或一组）消费者：`Take(key, max)`；`max` 与“凑批等待”（linger，默认 0~50ms）可配。
- **Stage 1**：同一批的发送者/@ 对象合并成一次批量查库；身份变更按批去重后再写当前值与历史。
- **Stage 2**：一批消息的资产一起打标签，便于统一限流与并发；单条失败不影响整批其他消息。
- **Stage 3**：批量 `Take` 只用于出队效率；当前按 `Item` **逐条执行**（每条独立路由、快照和回复），以后再把一批合并成一次 Agent 运行。运行期间新到的 `Item` 留在 B3，等本轮结束再取。
- 重试以 `Item` 为单位，`Ack` 只对成功项；内存实现可以先整批 Ack（进程内本来就没有重投）。

### 4.6 配置面（M0 前必须定）

缓冲层的旋钮分三层作用范围，不要混：

- **每会话**（`workspace|chat`）：单群/单私聊的积压上限、顺序、空闲回收；天然按群隔离，不需要按工作空间单独配。
- **全局**（整个进程）：总积压上限、关停排空时间、后台补标签参数；保护内存与数据库，按进程一份。
- **每工作空间**（一个机器人账号）：只有“这个账号整体配额/限速”才需要；M0 不做覆盖，保留以后用 `workspace_profile.extra` 覆盖的口子。

配置优先级：内置默认 → `config.yaml` → 全局 `settings` 表（运行时可调）。以后需要时再加“工作空间覆盖 > 全局设置 > 文件 > 默认”。

实际字段名（`config.yaml` 的 `pipeline:` / `assets:` 段，`settings` 表用同名 JSON 键）：

| 旋钮 | 文件键 | 运行时可覆盖 |
|---|---|---|
| 各段批量 | `sync_batch` / `asset_batch` / `agent_batch` | 是（仅正数） |
| 各段凑批等待 | `sync_linger_ms` / `asset_linger_ms` / `agent_linger_ms` | 是 |
| 各段并发 | `sync_concurrency` / `asset_concurrency` / `agent_concurrency` | 是（仅正数） |
| 各段重试 | `sync_retries` / `asset_retries` / `agent_retries` | 是 |
| 单会话容量 | `per_chat_capacity` | 否（结构性参数，重启生效） |
| 全局容量 | `global_capacity` | 否（结构性参数，重启生效） |
| 空闲回收 | `idle_ttl_sec` | 是 |
| 关停排空 | `drain_timeout_sec` | 是 |
| 补标签 | `assets.backfill_*`、`job_retention_days` | 是 |
| 消息描述预算 | `assets.message_budget_sec` | 是 |
| 描述并发 | `assets.tag_concurrency` | 是（仅正数） |
| 描述提示词版本 | `assets.prompt_version` | 是 |

| 旋钮 | 人话 | 范围 | 建议默认 | 显式 0 的含义 |
|---|---|---|---|---|
| 批量大小（每段） | 一次取几条 | 每段 | 同步 20 / 资产 4 / Agent 1 | 非法（至少 1） |
| 凑批等待（每段） | 为凑一批最多等多久 | 每段 | 同步/资产 20ms，Agent 0 | 0=不等，来一条处理一条 |
| 单会话容量 | 一个群最多积压多少条 | 每会话 | 500 | 非法（至少 1） |
| 全局容量 | 整个进程最多积压多少条 | 全局 | 5000 | 非法（至少 1） |
| 失败重试次数（每段） | 失败后最多再试几次 | 每段 | 3 | 0=不重试，直接问题件 |
| 每段全局并发 | 同一时刻最多几个会话在跑 | 每段 | 同步 4 / 资产 2 / Agent 2 | 非法（至少 1） |
| 处理线程空闲回收 | 一个群多久没消息就让它的处理线程休眠 | 每会话 | 10 分钟 | 0=不休眠（一直保留，不推荐） |
| 关停排空时长 | 关停时最多等多久处理存量 | 全局 | 10 秒 | 0=不等待，直接落库 |
| 补标签开关 | 是否启用后台补 | 全局 | 开 | 关=不补（欠条保留） |
| 补标签扫描间隔 | 多久扫一次欠条本 | 全局 | 60 秒 | 0=关闭（等同开关关） |
| 补标签每批条数 | 一次补多少 | 全局 | 20 | 非法（至少 1） |
| 补标签最多尝试 | 一张欠条最多补几次 | 全局 | 3 | 0=不重试 |
| 完成记录保留天数 | 完成的欠条留多久 | 全局 | 30 天 | 0=永久保留 |

规则：

- **“没写”和“写了 0”必须区分**：外部配置用指针/可选值，未写才套默认；写了 0 就按上表含义生效，不再被默认值覆盖。
- **负数一律启动失败**，并给出具体字段和允许范围，不静默纠正。
- 批量大小、容量这类“0 会让流水线停摆”的旋钮，0 视为非法；等待、重试、保留天数这类“0 有明确业务含义”的旋钮，0 必须生效。
- M0 曾用“保持现状”的一组值跑（批量 1、全局并发 1、凑批等待 0、容量 1024、不重试、空闲不回收）；M4 起已切到上表的生产默认值，上面的文件键与 settings 覆盖都已生效。

必须在 M0 前定的原因：缓冲接口的 `Take(key, max)`、内存实现的容量/等待/回收/排空都要读这些值；先写死再改，等于把配置语义和测试一起返工。

## 5. Stage 1：同步与补全

### 5.1 职责

Stage 1 按批消费，但对每条消息分别补全；同一批内的成员名合并成一次批量查库。

处理顺序：

1. 消费 B1 的 `InboundEvent`（工作空间归属、白名单、去重已在入口完成）；
2. ensure 群档案、成员目录；
3. 名字解析：sender → mention → 内存目录 → **按群批量查库**（缓存优先，miss 回源）；
4. **变更检测**：拿到的成员/群信息与当前快照比较（名字、群名及 `extra` 字段），任一变化就更新当前值并追加历史；
5. 组装 `cache.Message`（content、reply_to、ref_idx、time、images），返回 `SyncedMessage`。

### 5.2 成员/群信息历史表

当前值仍放 `members` / `groups`；历史只在信息确实变化时追加，查询不扫历史。表结构采用“少量通用列 + `extra` JSON”：以后要记录成员的角色、头衔、等级、头像等信息时，不需要再加列。

```sql
CREATE TABLE IF NOT EXISTS member_info_history (
    id           INTEGER PRIMARY KEY AUTOINCREMENT, -- 变更序号（同一秒内多次变更也能排序）
    workspace_id TEXT NOT NULL,
    group_id     TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '', -- 账号名（常用查询列）
    display_name TEXT NOT NULL DEFAULT '', -- 群名片（常用查询列）
    source       TEXT NOT NULL DEFAULT '', -- sender / member_info / db / self_name
    source_message_id TEXT NOT NULL DEFAULT '', -- 触发本次变化的入站消息；重试幂等
    extra        TEXT NOT NULL DEFAULT '{}', -- 其他成员信息 JSON：角色、头衔、等级、头像等
    changed_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_member_info_history
    ON member_info_history(workspace_id, group_id, user_id, changed_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_member_info_history_source
    ON member_info_history(workspace_id, group_id, user_id, source_message_id)
    WHERE source_message_id <> '';

CREATE TABLE IF NOT EXISTS group_info_history (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id TEXT NOT NULL,
    group_id     TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '', -- 群名（常用查询列）
    source       TEXT NOT NULL DEFAULT '',
    source_message_id TEXT NOT NULL DEFAULT '',
    extra        TEXT NOT NULL DEFAULT '{}', -- 群人数、群主等 JSON
    changed_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_group_info_history
    ON group_info_history(workspace_id, group_id, changed_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_group_info_history_source
    ON group_info_history(workspace_id, group_id, source_message_id)
    WHERE source_message_id <> '';
```

约定：

- 通用列只放经常查询、需要索引的字段（名字、群名）；其余全部进 `extra` JSON。JSON 只用于展示与回溯，不参与高频过滤。
- 是否追加按“完整信息快照”比较：常用列或 `extra` 任一变化才 append，避免每条消息写一行历史。
- 机器人自身昵称变化追加到 `member_info_history`（source=`self_name`），与群成员统一回溯。
- **崩溃边界**：不引入事务，改为“先写历史、再更新当前值”，并用触发消息编号做唯一键。重试最多多写一条重复历史，不会漏记一次改名；这符合“信息记录优先”的原则。
- MySQL 下自增主键对应 `BIGINT AUTO_INCREMENT`，由方言层处理，不影响表的逻辑结构。

### 5.3 读走缓存，写同步入库

- **消息同步入库**：带 `message_id` 的消息正文在 Stage 1 处理时同步写入 `messages`，写完才算这条消息处理完成（`Ack`）。去掉“写队列满了就丢库写”的旧语义，避免出现“内存有、库里没有”。
- **成员/群信息也在 Stage 1 同步更新**：`members` / `groups` 的当前值只在信息确实变化时更新并追加历史，写入频率很低，不引入异步队列，也不需要跨表事务。
- **读走缓存**：成员名、群名、当前身份优先读 KV 缓存（Ristretto），miss 再查 SQLite；批量补名仍用现有的 `ResolveNames` / `LookupMemberNames`，查到的名字回填缓存与目录。之前说的“异步”只指这里的缓存查询路径，不指消息入库。
- **唯一键兜底，不依赖事务**：`messages`、`members`、`groups` 已是 `(workspace_id, group_id, ...)` 复合主键；`workspace_profile` 以 `workspace_id` 为主键；`asset_tag_jobs` 以资产位置为主键。新成员并发首次出现时由主键 + upsert 收敛成一行，历史表用自增序号区分同秒多次变更。

### 5.4 信息完整性

Stage 1 + Stage 2 结束后，进入 Agent 的信息应满足：

- 窗口内每条消息都有昵称（实在拿不到时显式标记为未知，不阻塞）；
- 群名、self_id、reply_to 已解析；
- 资产有标签或明确状态（`ok` / `degraded` / `deferred` / `failed`）；
- 缺失项写入指标（按群/按来源），便于排查，不阻塞回复。

## 6. Stage 2：资产打标签

打标签要下载资产、算内容指纹、调视觉模型，慢且会失败；如果让消息等它，@ 回复会被拖住，还可能把整个流水线堵死。所以这一段的定位是“能做就做，做不完先欠着”：描述写进全局图库，欠的账写进 `asset_tag_jobs`，消息照常往下走，后台再补。欠条按“某条消息里的第几个资产”记，描述按“图片内容指纹”存——同一张图出现在十个群里，只写一次描述，但每次出现都可能各欠一张账。

- 输入：`SyncedMessage` 中的图片/视频/文件引用。
- 把 `router/image_note.go` 的入库描述逻辑拆到独立的 `assets` 包，Router 不再负责入库描述；`view_image` 仍是纯 MCP 工具。
- 接口：

```go
type Tagger interface {
    Tag(ctx context.Context, assets []Asset) ([]Asset, error)
}
```

- 先实现 `ImageTagger`（沿用现有：sha256 + size 去重、低清描述、全局图库 `image_notes`、失败记录与重试上限）；`video`/`file` 预留 `kind`，未实现时返回 unsupported，不阻塞消息。
- 目前 Stage 2 按批消费、标签在阶段内同步完成（有总时间预算）；以后可以换成更重的异步实现。
- 标签回填到 `EnrichedMessage.Assets`，再提交窗口缓存与 DB。
- **超时/容量满：先落库，异步补标签，不丢消息**。B2/Stage 2 队列满或时间预算耗尽时：
  1. 把这条消息里还没处理的资产写入 `asset_tag_jobs`（`status=pending`，带原始 url、message_id、group_id、asset_index）；如果已经取到字节，顺手复制到本地临时缓存并记 `local_path`；
  2. 调用 `Alerter.Record` 记录完整原始事件/消息 JSON + 指标；
  3. 消息带 `AssetStatus = deferred` 照常进入 B3/Stage 3；B3 满时向上游回压，不丢消息。

  后台 backfill worker 定期扫描 `pending/failed` 任务补打标签，成功写 `image_notes` 并标记 `done`。
- **标签失败：继续进入 Stage 3**。`asset_tag_jobs` 保留记录（`status=failed`，`attempts+1`）交给后台重试；`EnrichedMessage.AssetStatus = degraded|failed`，Stage 3 据此做降级处理（见 7.4）。

```sql
CREATE TABLE IF NOT EXISTS asset_tag_jobs (
    workspace_id TEXT NOT NULL,
    group_id     TEXT NOT NULL,
    message_id   TEXT NOT NULL,
    asset_index  INTEGER NOT NULL DEFAULT 0, -- 消息里第几个资产
    kind         TEXT NOT NULL DEFAULT 'image',
    url          TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending', -- pending | failed | missing | done
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (workspace_id, group_id, message_id, asset_index)
);
CREATE INDEX IF NOT EXISTS idx_asset_tag_jobs_pending
    ON asset_tag_jobs(status, updated_at);
```

- 配置：单条消息的标签时间预算、打标签并发上限、待办队列上限、`assets.backfill_enabled`、`assets.backfill_interval_sec`、`assets.backfill_batch`、`assets.backfill_max_attempts`、`assets.job_retention_days`、当前 `prompt_version`；这些阈值统一放全局配置表，运行时可调，沿用显式 0 语义（缺省才用默认，负数非法）。
- **欠条只记原始地址**：QQ 官方的图常是本地临时文件，欠条就把这个地址记下来，后台补标签时按它重新读一次；读不到直接把欠条记成 `missing`，不另存原图副本。补不到就等同一张图下次出现时重新描述。
- **回填只写全局图库，不改已经进入窗口的旧消息**：窗口里的消息在 Stage 2 提交时冻结“图片描述快照”；否则后台补好描述后再塞回旧消息，会改变 prompt 前缀、破坏缓存命中，省下的调用费又被抵消。
- 后台任务只补标签，不重放消息。**文件消失是终态**：后台发现原始地址取不到（链接过期、本地临时文件被清），直接标记 `missing`、记录“文件已消失”，不再重试、不再管它；等同一内容以后再次出现时，按新消息/新资产重新建欠条并正常描述。
- 同一内容再次出现时，全局图库里的 `failed` 记录**不永久阻止**重新描述：每次出现都有独立的欠条和尝试次数，由本次出现的重试上限兜住成本。
- 后台 worker：启动后常驻，按间隔扫描 `status in (pending, failed)` 且 `attempts < max` 的任务，批量调用 `Tagger`；成功 upsert `image_notes` 并把任务标记 `done`（已有成功记录也直接 done），失败 `attempts+1` 并记录 `last_error`，超过上限标记 failed 并报警。指标包括 pending 数、最老任务年龄、补标签成功率。
- **生命周期与清理**：
  - `pending`：这次没来得及打标签，绝不因此丢消息，交给后台补；
  - `failed`：尝试过但出错（模型超时等临时问题），`attempts` 累加，到上限停止并报警；
  - `missing`：文件已经没了（链接过期、本地临时文件被清），终态，不重试、不报警，只计数；
  - `done`：描述已写进 `image_notes`，台账保留 `assets.job_retention_days` 后清理；
  - 待补任务不清理（还没完成），到上限的失败任务保留供排查；`done` 和 `missing` 按保留天数清理，不动全局图库。
- **描述版本**：`image_notes` 现有的 `model` 列记录生成模型，再新增 `prompt_version` 记录提示词版本；换模型/提示词后默认不自动重刷（费钱且可能大面积改写），需要时按版本号手动触发批量重刷。同一张图按 `(sha256, byte_size)` 去重，重刷只更新这一条。

## 7. Stage 3：Agent 与冻结快照

### 7.1 队列语义

- 每个 `workspace|chat` 一个串行 worker，从 B3 批量 `Take`。worker 是按需启动的 goroutine（类虚拟线程），空闲超时后自行退出；不是常驻操作系统线程。
- **当前阶段逐条执行**：每条 `Item` 独立路由、独立快照、独立回复；批量 `Take` 只减少出队开销。以后再把一批合并成一次 Agent 运行。
- 运行期间新消息继续进入 B2/B3 和窗口缓存；本次执行不受影响。
- 一次执行结束（发送完成并 `Ack`）后，继续取下一条/下一批。
- **裁判属于 Stage 3**：是否值得主动接话由 Stage 3 内的裁判判定，与回复执行共用同一份快照/队列，不单独成段。
- **触发消息已不在窗口**：出队后在快照里找不到触发消息（环形缓冲已淘汰、窗口配置偏小）时，放弃这条回复任务，调用报警记录“窗口小了，要调大”，并保存原始事件、窗口首尾、配置和完整快照；消息本体已同步入库，不会丢。
- **不做 @ 优先级**，严格按该 chat 的 FIFO 顺序执行；主动接话保持 latest-wins（最多一条待处理），避免大量主动任务把 @ 拖慢。
- 消息本体不丢弃；B3 满时向上游回压，而不是丢消息。
- 现有 `router/chat_review.go` 与 `router/proactive.go` 的 `running/pending` 合并到统一的 Agent 队列。
- worker 需要空闲回收、最大并发限制和关停取消，避免每群一个 goroutine 泄漏。

### 7.2 快照冻结

- 当前逐条执行：每个 `Item` 以自己的消息 ID 作为 `FreezeID`，从窗口缓存复制快照；
- 以后按批执行时，才用整批最后一条消息作为 `FreezeID`；
- 选窗、拼 prompt、图片候选、工具调用全部基于快照；
- 这样运行期间的新消息不会“插进”当前上下文，也保证前缀稳定、利于 prompt 缓存命中。

### 7.3 回复动作化

把现在的“一次回复文本 → 排版成 ≤5 句 → 逐条发送”抽象成动作列表：

```go
type ReplyAction struct {
    ID               string // 幂等键，重投递不重复发送
    ReplyToMessageID string // 引用/被动回复哪条入站消息
    RefIdx           string // QQ 官方 REFIDX
    Text             string
}

type AgentResult struct {
    Actions []ReplyAction
}

// SendResult 是发送层对单个动作的结果。Sent=true 表示已发（含结果不确定）；
// SendOK=false 表示发送接口没有明确返回成功，需要后台查询和排查。
type SendResult struct {
    ActionID string
    Sent     bool
    SendOK   bool
}
```

- 当前逐条执行：每条入站消息一次运行，产出同一目标的 N 个动作（N ≤ 5 句），发送层逐条发送，等价于现在的排版功能。
- 以后按批执行：Agent 可以从批次里拿到多条 @ 的 `message_id`，为每条各生成动作，实现“逐个 @ 回复”。
- 每条动作用自己的入站消息 ID，QQ 被动回复配额（每条 ≤5）按各自目标计算。
- **当前不做多 @ 合并回复**；先保持“每条入站消息一次运行”，后续在 Agent 内部完善。
- **发送台账与幂等**：动作编号由“入站消息 + 第几句”确定；发送层为每个编号记录“未发 / 已发”，另带一个“发送是否明确返回成功”的字段。任务重试只补“未发”，已发跳过，避免整段重发。
- **部分发送失败**：前面的句子成功、后面的失败时，重试只继续发剩下的句子。
- **发送结果不确定**（超时但可能已到达）：按“已发”记录，但“发送是否返回成功”填否；不盲目重发，记录现场并报警。宁可少一句，也不重复刷屏，用户可以再次 @。
- **对话历史按已发处理**：不确定的句子也进入机器人回复历史，模型看到的是“我说过”；后台可以按“发送是否返回成功=否”查出所有不确定的句子。这样模型不会把同一句再补一遍，排查时也能看见风险。
- **锚点随台账推进**：已发（含成功和不确定）都推进窗口锚点，未发出的不推进，避免下一轮把同一句再喂给模型。
- 台账当前在内存；跨重启/多实例时落库，键就是动作编号；入口的消息去重继续保留。

### 7.4 资产降级

`EnrichedMessage.AssetStatus` 不是 `ok` 时，Stage 3 在 prompt 里追加对应的降级提示：

- `deferred`：**> 【资产状态】资产标签稍后补全，本次先基于文字内容作答；除非确实必要，不要读取资产。**
- `degraded|failed`：**> 【资产状态】本次资产打标签失败或系统繁忙，除非回答确实需要，否则不要读取图片/视频/文件，优先基于文字内容作答。**

- 不因为标签缺失而拒绝回复；
- 每个 `Asset` 也保留自己的状态，Agent 可以选择只读成功的资产；
- 降级处理属于 Agent 策略，Stage 2 只负责标记状态和落库。

## 8. 存储与缓存底座

### 8.1 数据库（GORM，先 SQLite）

- `state.Store` 是**全局单例**：一个 `*gorm.DB`；消息与身份信息同步写，不再保留“消息异步写队列”。`state.Scope(workspaceID)` 只是带 `workspace_id` 条件的视图。
- GORM 使用 `github.com/glebarez/sqlite`（底层仍是 `modernc.org/sqlite`），保持纯 Go 构建；不使用官方 CGO 驱动。
- **迁移继续使用现有版本化 DDL**（`PRAGMA user_version`），不启用 `AutoMigrate`，避免改坏已有 `state.db`。
- 复杂查询保留 Raw：`LoadRecent` 的 `ROW_NUMBER() OVER`；方言敏感的 upsert 用 `clause.OnConflict`，MySQL 的 `VALUES()` 差异后续在方言层处理。
- 表边界：
  - 全局表（无 `workspace_id`）：`image_notes`、未来的全局配置/统计；
  - 工作空间表（有 `workspace_id`）：`messages`、`members`、`member_info_history`、`groups`、`group_info_history`、`anchors`、`group_personas`、`workspace_profile`、`asset_tag_jobs`；
  - 不使用空 `workspace_id` 表示全局。

### 8.2 workspace 画像表（替换 meta）

现有 `meta` 是 `(workspace_id, key, value)` 的纯 KV，字段一多就变成字符串拼装。改成**常用字段建列、其余进 `extra` JSON** 的 typed 表：

```sql
CREATE TABLE IF NOT EXISTS workspace_profile (
    workspace_id    TEXT PRIMARY KEY,
    platform        TEXT NOT NULL DEFAULT '',
    account         TEXT NOT NULL DEFAULT '',
    name            TEXT NOT NULL DEFAULT '', -- 机器人展示名
    impl            TEXT NOT NULL DEFAULT '', -- astrbot-bridge / koishi-onebot / ...
    account_kind    TEXT NOT NULL DEFAULT '', -- official_bot / personal_account / app
    identity_scheme TEXT NOT NULL DEFAULT '', -- qq_uin / qq_openid / telegram_user_id
    identity_scope  TEXT NOT NULL DEFAULT '', -- global / per_group / per_bot_group
    extra           TEXT NOT NULL DEFAULT '{}', -- capabilities / limits / 其他画像字段（JSON）
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_workspace_profile_identity
    ON workspace_profile(platform, account_kind, account);
```

- `account_kind` 不只是画像字段，而是**工作空间键的一段**：ID 为
  `{platform}_{account_kind}_{account}`，三元组唯一；代码不解析 ID 反推三段。
- 常用/需要查询的字段（`platform`、`account`、`name`、`impl`、`account_kind`、`identity_scheme`、`identity_scope`）做成列；
- 能力、限制及其他低频画像字段放 `extra` JSON，增加字段不用改表；
- 不再使用 `meta` 这种任意 key-value；以后如果出现零散配置，也进 `extra` 或独立的全局 `settings` 表，不回到 KV 形式。

**迁移（schema v6）**：把旧 `meta` 的 `platform/account/name` 透视进新表，再删除旧表：

```sql
INSERT INTO workspace_profile (workspace_id, platform, account, name, created_at, updated_at)
SELECT workspace_id,
       COALESCE(MAX(CASE WHEN key = 'platform' THEN value END), ''),
       COALESCE(MAX(CASE WHEN key = 'account'  THEN value END), ''),
       COALESCE(MAX(CASE WHEN key = 'name'     THEN value END), ''),
       strftime('%s', 'now'),
       strftime('%s', 'now')
FROM meta
GROUP BY workspace_id;

DROP TABLE meta;
```

代码影响：

- `Scope.Meta` / `SetMeta` 删除，改为 typed 的 `LoadWorkspaceProfile()` / `SaveWorkspaceProfile()`；
- `Store.Workspaces()` 直接查新表，不再透视 `key/value`；
- `Workspace` 启动时读一次画像并缓存到内存，改名/能力变化时先比对再写，避免每条消息写库；
- MySQL 方言下把 `strftime('%s','now')` 换成 `UNIX_TIMESTAMP()`，其余结构不变。
- **开发阶段允许直接重建 `state.db`**（旧数据不保留），上面的数据透视迁移可以等正式上线前再补；迁移逻辑届时用旧库副本验证。
- 旧 ID `qq_<appid>` 迁移时按当前部署全部是官方机器人，改写为
  `qq_official_bot_<appid>`，并同步写入 `account_kind=official_bot`；所有带
  `workspace_id` 的表一起改。改完后 `(platform, account_kind, account)` 唯一索引应无冲突；
  若出现冲突，停下来人工确认，不要自动合并两个账号的档案。

### 8.3 通用 KV 缓存（Ristretto）

```go
type KV interface {
    Get(ctx context.Context, key string) ([]byte, bool, error)
    GetMany(ctx context.Context, keys []string) (map[string][]byte, error)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
    Delete(ctx context.Context, key string) error
    Close() error
}
```

- 后端用 `github.com/dgraph-io/ristretto/v2`（v2.4+ 支持 `SetWithTTL` / `GetTTL`）。
- 值统一 `[]byte`，TTL 是一等公民，所有方法返回 `error`；消费者必须能接受缓存 miss/失败并回源。
- Ristretto 的 `Set` 是缓冲写（最终可见），测试里用 `Wait()`；业务按“可能 miss”处理。
- 键带命名空间：`ws:<id>:member:<uid>`、`ws:<id>:group:<gid>`、`img:<sha>:<size>` 等。
- 首个消费方建议：图片描述读缓存（包一层现有 `ImageNoteStore`），验证抽象可用。

### 8.4 全局配置入库

- 启动配置（监听地址、DB DSN、日志级别）必须留在 `config.yaml`/环境变量，避免“读库配置需要先连库”的循环。
- 业务全局参数放全局 `settings` 表（`key / value(JSON) / updated_at`），`config.yaml` 作为默认值，数据库作为动态覆盖（库 > 文件）。
- 当前支持两个键：`pipeline`、`assets`，值是与 YAML 段同名的 JSON 对象（字段名见 [4.6](#46-配置面m0-前必须定)）。
- 覆盖在 `Workspace.Manager` 侧每 30 秒拉一次 `settings` 表并重算快照；非法 JSON/负数保留文件值并记 warn，不中断运行。
- 批量、并发、容量这类“0 会停摆”的字段不接受运行时 0；其余字段显式 0 在库里同样生效。
- `settings` 保留 key-value 是因为它的键是开放集合；workspace 画像是固定字段集合，所以用列 + `extra` JSON，两者不混用。

## 9. 代码结构

```
bot/                 入口：IM 事件 → B1；Stage 1/2/3 的处理器与生命周期
  ingest.go          接收、去重、全局背压、Run/Shutdown/排空
  stages.go          三段处理器：同步 → 资产 → Agent（含 latest-wins 与老化报警）
workspace/           工作空间、身份目录、Stage 1 同步与历史
  sync.go            Stage 1：批量补名、变更检测、同步入库、引用补全
  settings.go        settings 表覆盖：30 秒轮询重算快照
  ledger.go          发送台账适配（router.LedgerEntry ↔ state.SendRecord）
assets/              Stage 2：Tagger、图片标签（从 router 拆出）
  image.go           sha256 标识、探测/降采样/描述解析
  tagger.go          单条消息内并发描述、预算、欠条
  jobs.go            后台补标签 worker、台账清理
pipeline/            阶段契约、缓冲层接口、每会话 worker
  contracts.go       阶段载荷（InboundEvent / SyncedMessage / EnrichedMessage）
  buffer.go          进程内 Buffer 实现
  worker.go          每 key 串行 worker：批量、凑批、重试、死信、空闲回收、排空
router/              Agent 执行体：选窗、prompt、LLM、工具、回复动作
  actions.go         回复动作化 + 发送台账
cache/               消息窗口缓存（不动）
cache/kv/            通用 KV 接口 + Ristretto 实现
workspace/state/     全局 DB、Scope、GORM、迁移、同步写路径、KV
```

依赖方向保持单向：`bot` → `pipeline` / `workspace` / `assets` → `router` / `state` / `cache`。
`agentqueue` 没有再单独建包：Stage 3 的“每 chat 串行 + 冻结快照”由 `pipeline.Stage` 的
worker 加 `bot/stages.go` 的 latest-wins 逻辑承担，避免只包一层的空壳。

## 10. 演进到消息队列

| 现在（进程内） | 以后（MQ） |
|---|---|
| `Buffer.Put` | publish，key 作为分区键 |
| `Buffer.Take` | 批量 fetch/consume |
| `Buffer.Ack` | commit / ack |
| `Item.Body` (JSON) | 消息体，原样传输 |
| 进程内 FIFO | 按 `workspace` + `chat` 分区，保证同群顺序 |
| 失败重试（内存） | 重试 + 退避 + 死信 |
| 无崩溃重投 | 至少一次投递 + `Item.ID` 幂等 |

按边界拆 topic：B1 入站（IM → Stage 1）、B2 同步（Stage 1 → Stage 2）、B3 资产（Stage 2 → Stage 3）；三个 stage 各自是消费组。也可以一个 topic + `stage` 字段。分区键统一为 `workspace` + `chat`，所有阶段批量消费。

## 11. 实施顺序

| 里程碑 | 内容 | 验收 | 状态 |
|---|---|---|---|
| **M0 缓冲层** | `pipeline` 契约、`Buffer` 接口、内存实现、把现有链路改成 IM→B1→Stage1→B2→Stage2→B3→Stage3 | 行为不变，现有 e2e 测试全绿 | 已完成 |
| **M1 存储底座** | GORM(SQLite)、消息同步写路径、`cache/kv` + Ristretto、契约测试 | `state` 测试全绿；开发期可重建 `state.db`，正式迁移用旧库副本验证 | 已完成（schema v8） |
| **M2 阶段一** | 抽 `workspace/sync.go`、变更检测、成员/群信息历史表（通用列 + `extra` JSON）、缓存读、同步写 | 改名/信息变化产生历史记录；缺名能批量补全；消息与身份信息落库可验证 | 已完成 |
| **M3 阶段二** | `assets` 包、`Tagger`、图片实现、标签回填、`asset_tag_jobs` + 后台补标签 worker | 超时/容量满落库不丢消息；后台能补标签、失败可重试 | 已完成 |
| **M4 阶段三** | `pipeline.Stage` 每会话 worker、**逐条执行**、冻结快照、动作化回复；按批执行留待后续 | 每条消息独立路由/快照/回复；运行期间新消息进入下一轮；无重复回复 | 已完成 |
| **M5（可选，未做）** | MQ 适配器 | 同一套缓冲契约测试在内存与 MQ 上通过 | 按需求暂缓 |

M0 与 M1 相互独立，可以并行；M2 依赖 M1，M3/M4 依赖 M0。

## 12. 风险与决策记录

| 项 | 决策 / 风险 | 处理 |
|---|---|---|
| 消息窗口缓存 | **不动**，它服务于 prompt 命中 | 新缓冲层是独立结构 |
| @ 批次策略 | 当前 Stage 3 **逐条执行**，每条独立路由/快照/回复；以后再做按批执行和多 @ 动作化 | 批次只做出队单位；不做 @ 优先级，严格 FIFO |
| Stage 2 容量满 | 报警 + 记录原始信息 + 写 `asset_tag_jobs`（pending），消息带 `deferred` 继续进入 Stage 3；B3 满时回压上游 | 后台 backfill worker 异步补标签，不重放消息 |
| 消息本体 | **消息与 `message_id` 同步入库，任何阶段都不删消息**；只有回复任务可能被放弃 | 容量不足只跳过可选补全/标签；回压向上游暴露压力 |
| 触发消息老化 | 窗口配小、触发消息已被环形缓冲淘汰 | 放弃回复任务 + 报警“窗口小了，要调大” + 记录完整现场；消息仍在库 |
| Stage 2 标签失败 | 不阻塞、不丢弃，带 `AssetStatus` 进入 Stage 3 | Agent 注入降级提示，按需读资产 |
| 标签任务生命周期 | pending/failed 可能累积，旧描述可能过时 | 文件消失记 `missing` 终态、不重试；完成/消失台账按保留天数清理；失败到上限报警保留；记录 model + prompt_version，手动按版本重刷 |
| 每群并发 | 现在全局单消费者；改造后按 `workspace` + `chat` 串行 | M0 全局并发 1，行为不变；M4 再打开每群并行 |
| 全局并发 | 每群并行会同时发起大量模型调用 | 每段全局信号量；默认同步 4 / 资产 2 / Agent 2 |
| 同群顺序 | 批量并行可能打乱消息顺序 | 同群按输入顺序处理/提交；只跨群或同消息内并行 |
| 写入顺序 | 消息与成员/群信息在 Stage 1 同步写；不引入跨表事务 | 每表复合主键 + upsert；历史带触发消息编号，唯一键防重试写两条 |
| 欠条原图 | 临时地址过期后补不到 | 延后时尽量复制原图到本地缓存；失败退回 URL，等下次出现重描述 |
| 回填与窗口 | 补标签改写旧消息会破坏 prompt 缓存 | 窗口冻结描述快照，回填只写全局图库 |
| 崩溃重放 | 重启后内存缓冲丢失 | 消息已同步入库；不自动补回复，用户可再 @；MQ 后再补 |
| 多实例 | 进程内缓冲无法跨实例协调 | 接 MQ 前单实例部署；滚动发布短暂停机，不抢同一群 |
| 数据保留 | 消息/历史表长期增长 | 保留容量告警 + 人工清理；自动清理默认关闭，可配 |
| 发送幂等 | 重试/部分发送可能重复 | 动作编号 + 发送台账（未发/已发）；已发带“发送是否返回成功”，不确定按已发记录、不重发，后台可查 |
| Ristretto | `Set` 最终可见 | 消费者按 miss 处理；测试用 `Wait()` |
| GORM | 反射开销、复杂 SQL 仍需 Raw | 复杂查询集中 `state` 包；双库契约测试 |
| MySQL | upsert/窗口函数/迁移有方言差异 | 方言层 + 同一套契约测试；本次先 SQLite |
| MQ | 至少一次投递、顺序、幂等 | `Item.ID` 去重；按 chat 分区；不建议在 M2/M3 前引入 |

## 13. 待办

- [x] M0：缓冲层（B1/B2/B3）接口与内存实现，三段拆分
- [x] 4.6 配置面：`pipeline` / `assets` 段 + `settings` 表运行时覆盖（30 秒轮询）
- [x] M1：GORM + Ristretto + 同步写路径（消息/身份信息；不再有通用异步写队列）
  - schema v8：`workspace_profile`、成员/群信息历史、`asset_tag_jobs`、`send_ledger`、`alerts`、`settings`
  - 消息与身份信息按表 upsert 同步入库；缓存只做读走加速，miss 回源
- [x] M2：Stage 1 同步、历史表、变更检测、按群批量补名
- [x] M3：Stage 2 `assets` 抽取、欠条落库、后台补标签
- [x] M4：Stage 3 每会话 worker、冻结快照、动作化回复与发送台账
- [ ] M5：MQ 适配（按需，本次不做）

已知边界：

- 旧库（v5 之前）里的 `qq_<appid>` 工作空间记录没有 `account_kind`，新键 `platform_kind_account`
  不会自动认领它们；需要人工确认后再迁移/清理，程序不会自动删除旧记录。
- Stage 3 目前仍是“逐条执行”，批量只用于出队；按批合并成一次 Agent 运行留待以后。
- `per_chat_capacity` / `global_capacity` 是结构性参数，只认文件，运行时改库不生效。
