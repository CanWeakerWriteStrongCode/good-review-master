# 工作空间自动开通设计

状态：已实现（2026-10-28，P0/P1）。配套：[im-protocol-standardization.md](im-protocol-standardization.md)。

**部署约束**：只走 QQ 官方机器人（NapCat 个人号已被封两次），没有数字 QQ 号；
账号键用 AppID，不用 QQ 号。

**本版变更（相对 OneBot 11 稿）**：

- 取消 `receiver_bot_account_id`：账号来自传输端点，平台来自标准字段 `self.platform`。
- 工作空间 ID 从 `{appid}` 改为 `{platform}_{account}`，如 `qq_1906xxxx`。
- 名单匹配完整工作空间 ID，而不是单独的 AppID。
- 未接管时的 HTTP 响应从「200 ignored」改为 4xx（OB12 只认 204/200 为成功）。
- 新增单个 SQLite 档案库（`state.db`），所有表带 `workspace_id` 列区分机器人账号，
  持久化消息、锚点、身份、人格、成员名（见 7.1）。

## 1. 目标

配置里不再声明工作空间。事件到达时按「接收账号 + 平台」自动创建或加载工作空间；
配置只保留一份「允许哪些工作空间自动开通」的名单。

## 2. 为什么删掉静态 `bot.workspaces`

- 账号身份来自运行期（传输端点的 account、事件里的 platform），配置再抄一遍
  就有两个事实来源，改一个忘一个。
- 新增账号要改配置 + 重启；自动开通只要求账号在允许名单里。
- 静态配置的 `id` 和传输层 `/im/event/{account}` 会重复维护，容易对不上。

## 3. 工作空间键

| 来源 | 值 | 说明 |
|---|---|---|
| 路径 `/im/event/{account}` | `account` | 每个机器人实例一个值；AstrBot 用 AppID |
| 事件 `self.platform` | 如 `qq` | OB12 标准字段，决定平台命名空间 |
| 组合 | `{platform}_{account}` | **工作空间 ID（数据库列 / 面板 URL）** |

- ID 约束 `^[A-Za-z0-9_-]{1,64}$`：平台名里的点号替换成下划线（`zulip.my-instance` → `zulip_my-instance`）。
- `account` 不合法（含 `/`、空格等）时返回 400，并写清楚怎么改：
  检查插件的 `go_event_url` / 平台实例的 AppID，或 sidecar 的账号配置。
- `self.user_id` 不参与工作空间键；它只用于 @ 判定（见协议文档 3.2）。

例：

| 平台 | account | 工作空间 ID | 档案 |
|---|---|---|---|
| `qq`（官方） | `1906xxxx` | `qq_1906xxxx` | `state.db` 中 `workspace_id = qq_1906xxxx` |
| `qq`（Koishi/Satori） | `123456789` | `qq_123456789` | `state.db` 中 `workspace_id = qq_123456789` |
| `telegram` | `foobar` | `telegram_foobar` | `state.db` 中 `workspace_id = telegram_foobar` |

## 4. 生命周期

```
POST /im/event/{account}  + 事件 self.platform
   │
   ├─ 拼出 id = platform_account
   │
   ├─ 已加载？──────────是──▶ 直接投递
   │
   ├─ 名单允许？────────否──▶ 404（推送失败，插件不接管，AstrBot 自己兜底）
   │
   └─ 是 ──▶ 建 Router + 写 meta(platform/account) + 注册 ──▶ 204
```

- 同步部分只做本地操作：取 DB Scope、建 Router。`AcceptEvent` 必须快速返回。
- 昵称来自 `get_self_info`（`user_name`），是网络调用，**异步**补，不阻塞入队。
- 并发：Manager 用互斥锁 + double-check，同账号两条事件同时到达只创建一次。

## 5. 启动时恢复已有工作空间

启动时从 `state.db` 的 `meta` 表读出 `(workspace_id, platform, account)`，逐个重建
Router 并挂上自己的 `Scope`。原因：

- 消息、锚点、人格、身份都在库里，重启后必须能继续用。
- 面板能直接看到全部已开通的工作空间，而不是等消息来了才出现。
- 允许名单只控制「新建」，不限制档案里已有的工作空间。

**`state.db` 是唯一持久化事实**：工作空间没有目录，也没有私有配置文件。
停服后按 `workspace_id` 删行（或删整个库）即可清理，v1 不做自动清理，也不做面板删除。

## 6. 配置

```yaml
im:
  token: ""
  accounts:
    - id: "qq_1906xxxx"                 # 出站连接：按工作空间 ID 选
      api_base: "http://127.0.0.1:6199"

bot:
  # 允许自动开通的工作空间 ID；"*" 表示全部。
  # 空列表 = 不允许自动开通（安全默认，启动时打一条提示）。
  workspace_accounts: ["qq_1906xxxx", "qq_1904xxxx"]
```

- 精确匹配工作空间 ID，不做前缀/昵称/账号部分匹配。
- 名单用完整 ID 的原因：不同平台可能出现相同的 `account`，只比 account 会一起放行。
- `*` 时任何能拿到共享 token 的调用方都能创建状态；监听非回环地址启动告警。
- 档案里已有工作空间不受名单影响：名单只管新建。
- **自动开通只建状态，不建连接**：新账号必须同时在 `im.accounts` 里配 `api_base`，
  否则能收事件、发不出消息（发送时记 warn 并按失败处理）。
- 删除 `bot.workspaces` 和 `bot.allow_self_ids`：范围统一由这份名单表达。

## 7. 每个工作空间的状态

| 状态 | 来源 | 重启后 |
|---|---|---|
| 平台 | 事件 `self.platform` | 保留（由 ID 隐含，同时写 `meta`） |
| 提示词 | 全局 `prompt_system.yaml` + `prompt_custom.yaml`（所有工作空间共享） | 保留（文件是唯一事实） |
| 消息缓存、选窗锚点 | 运行时 + `state.db` | 保留（从 DB 恢复最近 N 条与锚点） |
| 群人格（`#切换人格`） | Router + `state.db`（只存源关键字） | 保留（启动时按关键字重新绑定） |
| 群内 `self.user_id` | 事件学习 + `state.db` | 保留；按 `(工作空间, 群)` 存，不跨群复用 |
| 昵称 | `get_self_info` 的 `user_name` | 保留（`meta` 缓存），缺失时异步补 |
| 成员昵称目录 | `get_group_member_info` 的响应 + `members` 表 | 保留 |
| 群白名单 | v1 全部继承全局 `bot.allow_groups` | 保留（全局） |

工作空间**没有目录、没有私有配置文件**：提示词全局共享，运行期私有数据（消息、
锚点、人格、成员名、身份）全部写进共享 `state.db` 的 `workspace_id` 行。
以后新增工作空间级数据也走数据库，不再引入 `workspaces/<id>/*.yaml`。

### 7.1 本地持久化（SQLite）

**Go 没有自带 SQLite**：标准库只有 `database/sql` 接口，驱动要单独引入。
推荐 `modernc.org/sqlite`（纯 Go、不需要 CGO/gcc，Windows 下仍是单文件可执行）；
不选 `mattn/go-sqlite3`（需要 CGO，交叉编译和部署都更麻烦）。

**位置**：单个库 `state.db`，放在工作目录（`config.yaml` 同级），所有工作空间共用；
每张表用 `workspace_id` 列区分机器人账号。一个库文件更好备份、统计口径一致；
工作空间没有目录，清理某个账号就是按 `workspace_id` 删行，或停服删整个库。

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;

CREATE TABLE messages (
  workspace_id TEXT NOT NULL,
  group_id     TEXT NOT NULL,
  message_id   TEXT NOT NULL,    -- OB12 是字符串
  user_id      TEXT NOT NULL,
  content      TEXT NOT NULL,    -- 由消息段渲染出的文本
  images       TEXT NOT NULL,    -- JSON 数组
  reply_to     TEXT NOT NULL DEFAULT '',
  ts           INTEGER NOT NULL,
  PRIMARY KEY (workspace_id, group_id, message_id)
);
CREATE INDEX idx_messages_ts ON messages(workspace_id, group_id, ts);

CREATE TABLE anchors (
  workspace_id TEXT NOT NULL,
  group_id     TEXT NOT NULL,
  start_id     TEXT NOT NULL DEFAULT '',
  last_sent_id TEXT NOT NULL DEFAULT '',
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (workspace_id, group_id)
);

CREATE TABLE identities (          -- 本群学到的机器人 user_id
  workspace_id TEXT NOT NULL,
  group_id     TEXT NOT NULL,
  self_user_id TEXT NOT NULL,      -- 可能为哨兵 unknown_selfid
  learned_at   INTEGER NOT NULL,
  PRIMARY KEY (workspace_id, group_id)
);

CREATE TABLE members (
  workspace_id TEXT NOT NULL,
  group_id     TEXT NOT NULL,
  user_id      TEXT NOT NULL,
  name         TEXT NOT NULL DEFAULT '',
  display_name TEXT NOT NULL DEFAULT '',
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (workspace_id, group_id, user_id)
);

CREATE TABLE group_personas (
  workspace_id TEXT NOT NULL,
  group_id     TEXT NOT NULL,
  keyword      TEXT NOT NULL,
  PRIMARY KEY (workspace_id, group_id)
);

CREATE TABLE meta (                -- key/value：platform、account、name、created_at
  workspace_id TEXT NOT NULL,
  key          TEXT NOT NULL,
  value        TEXT NOT NULL,
  PRIMARY KEY (workspace_id, key)
);
```

每个工作空间通过 `Store.Scope(workspaceID)` 拿到一个视图，所有读写自动带上
`workspace_id`；同一个群、同一个 `message_id` 在不同账号下互不冲突。

- **写路径**：内存缓存仍是读路径；入站消息先入内存，再异步批量写库（有界 channel，
  满了丢 DB 写并记 warn，不影响回复）。锚点/人格/身份变更即时写。
- **保留策略**：**不自动清理**，消息全部留在库里；内存环形缓冲仍按 `runtime.max_cache_msg`
  封顶，两者互不影响（DB 是档案，内存是工作窗口）。
- **启动**：打开库 → `PRAGMA user_version` 做幂等建表/迁移 → 从 `meta` 读出
  `(workspace_id, platform, account)` 恢复工作空间（不依赖目录或第一条事件）→ 每个
  工作空间把各群最近 `max_cache_msg` 条读回内存 → 恢复锚点、身份、人格关键字
  （关键字在配置里找不到时降级为默认人格并告警）。同时统计一次「消息总条数 +
  库文件大小（含 `-wal`）」并按 `workspace_id` 打印各账号条数；超过
  **100,000 条或 100 MB** 时用 warn 提示「数据偏多，确认是否需要清理」。
  阈值先写成代码常量，不新增配置。
- **失败模式**：打不开或写失败只记 error 并继续用内存缓存（降级），不让进程退出；
  面板显示「持久化不可用」。
- **隐私**：消息原文落盘。库文件权限按 0600 创建，不放进版本库；文档里写明这一点。
- **`message_id` 类型**：OB12 是字符串，`cache.Message`、锚点、`reply_to` 一并从 int64 改成 string。

剩余取舍：

| 编号 | 决策 | 推荐默认 | 备选 |
|---|---|---|---|
| S1 | 驱动 | `modernc.org/sqlite`（纯 Go） | `mattn/go-sqlite3`（CGO） |
| S2 | 库的粒度 | 全局单库 + `workspace_id` 列（已采用） | 每工作空间一个 `state.db` |
| S3 | 写盘方式 | 异步批量写，失败降级 | 同步写（阻塞入站） |
| S4 | 清理策略 | 不自动删；启动打印条数/大小，超阈值 warn | 按条数或天数自动清理 |
| S5 | 群人格 | 存源关键字，启动重新绑定 | 不持久化（现状） |

## 8. 决策点

| 编号 | 决策 | 推荐默认 | 备选 |
|---|---|---|---|
| D1 | 工作空间键 | `{self.platform}_{路径 account}`；不合法拒绝 | 只用 account |
| D2 | 创建时机 | 第一条事件到达时同步创建（仅本地 IO），昵称异步 | 启动预创建 |
| D3 | 允许名单 | `bot.workspace_accounts` 存完整 ID，空 = 不创建，`*` = 全部 | 只存 account |
| D4 | 已有工作空间 | 启动从 `state.db` 的 `meta` 全部恢复，名单只管新建 | 纯懒加载 |
| D5 | 展示名 | 先用 ID；异步用 `get_self_info.user_name` 补，写入 `state.db` 的 `meta` | 只放内存 |
| D6 | 群白名单 | v1 全部继承全局；需要每工作空间覆盖再加状态文件 | 现在做覆盖 |
| D7 | 出站选账号 | 按工作空间 ID 找 `im.accounts[].api_base`，每账号一条连接 | 共用连接 + 动作请求带 `self` |
| D8 | 调试/兼容入口 | 不再有隐式 default；`workspace_id` 必填或唯一已加载时自动选 | 保留一个空壳 default |
| D9 | 本地持久化 | 单库 `state.db`（SQLite，WAL）+ `workspace_id` 列，见 7.1 | 每工作空间一库 / 不持久化 |

## 9. 连带改动

- `config`：删除 `Workspaces []WorkspaceConf`、`EffectiveWorkspaces`、`WorkspaceByID`、
  `allow_self_ids`、`astrbot.http_api`、`astrbot.access_token`；新增
  `im.token`、`im.accounts[{id, api_base}]`、`WorkspaceAccounts []string`（完整 ID）。
- `workspace.Manager`：新增 `byID` 索引、`ResolveOrCreate(platform, account)`、互斥锁、
  启动扫描；删除 `byPlatform` / `bySelfID` / `byNickname` / `single`。
- `onebot.Event`：换成 OB12 结构（`Self{Platform,UserID}`、字符串 `MessageID`、
  `AltMessage`）；不新增任何私有路由字段。
- `im`：路径参数 `account` + 事件 `self.platform` 拼 ID；未知/未授权账号返回 4xx；
  合法事件返回 204。
- AstrBot 插件：`go_event_url` 模板 `.../im/event`；`self.platform` 取平台实例的
  `meta().name`（`qq_official` → `qq`，`wecom` / `lark` 等原样）；`self.user_id`
  按群学习或发哨兵；实现 OB12 动作服务；删除 `platform_id` / `mentioned` /
  `display_message`。账号优先取事件所属平台实例的 AppID（`platform.appid`），
  这样一份插件配置能同时服务多个 QQ 官方账号；`account` 仅作其它平台的兜底。
- 提示词监听：只监听根目录固定的 `prompt_system.yaml` + `prompt_custom.yaml`；
  提示词变化后重读一次并重建每个工作空间的 Router。
- 面板/调试：`App.Router`、`App.Prompt` 不再保证非空；调试接口按 `workspace_id` 解析。
- 指标：`workspace_resolve_total{result=created|loaded|denied|invalid}`。
- 持久化：新增 `workspace/state` 包，封装单库 `state.db` 的打开、迁移、按
  `workspace_id` 隔离的视图、异步写队列与启动恢复；`cache.Message.MessageID`、
  锚点 ID 改成 string；`go.mod` 新增 `modernc.org/sqlite`。
- 人格：`router` 需要暴露「当前群人格关键字」的读写，用于持久化与启动重绑。

## 10. 迁移

1. 从 `config.yaml` 删除 `bot.workspaces`；在 `bot.workspace_accounts` 里填
   `qq_<A 的 AppID>`、`qq_<B 的 AppID>`。
2. 删除旧目录 `workspaces/haoping`、`workspaces/bushi`：工作空间不再使用目录或
   `prompt_system.yaml` / `prompt_custom.yaml` 私有副本；提示词统一用根目录那份。
   删除前先备份目录里的自定义指令，需要保留的内容手工合并进根 `prompt_custom.yaml`。
3. `astrbot.http_api` 迁到 `im.accounts[].api_base`，每个账号一条；
   `astrbot.access_token` 迁到 `im.token`。
4. 插件 `go_event_url` 保持 `http://127.0.0.1:18090/im/event`（账号自动拼到路径），
   同时升级到 1.7.1 的 OB12 事件格式；`account` 通常留空。
5. `bot.allow_self_ids` 删除；接管范围改由 `workspace_accounts` 判断。
6. `platform_id` / `mentioned` / `display_message` 在 Go 与插件两侧同步删除；
   两侧必须一起升级，否则旧插件发的事件无法被新 Go 解析。
7. `state.db` 不需要迁移脚本：首次启动在工作目录自动建库建表；旧的内存数据本来就不落盘，
   重启后从空库开始积累。此前版本遗留在工作空间目录下的 `state.db` 不再被读取，可随目录删除。

## 11. 风险

- **误开通**：路径写错（如 AppID 打错）会被名单挡住；名单用 `*` 时挡不住。
  创建时打一条 info 日志（平台、account），便于发现。
- **卡片基数**：工作空间 ID 会进指标/面板，数量由名单与档案约束；
  名单用 `*` 时建议同时在网关层限制来源。
- **哨兵 `self.user_id` 不影响归属**：路由用 `{platform}_{account}`，`self.user_id` 只用于 @ 判定；
  哨兵只导致「本条不触发回复」，事件仍入缓存。
- **`account` 必须稳定**：同一账号跨群、跨重启必须得到同一个值；不要用群内 openid
  或机器人昵称，优先 AppID。它变了会导致同一账号被开通成两个工作空间。
- **平台名变化**：`self.platform` 从 `qq` 改成别的值会变成另一个工作空间；
  适配器要固定映射，不要透传可能变化的适配器名（`qq_official` 也不合法）。
- **AppID 暴露**：AppID 会进 URL、日志、指标和工作空间 ID。它不是密钥；不想暴露就统一用短哈希，
  代价是排障时可读性差一点。
- **群内 openid 不能跨群**：本机已实测 B 在 8528 群的 openid 到 6CC6 群就失效；
  任何缓存都必须带上群维度，否则会把 A 群的身份用到 B 群。
- **响应语义依赖**：名单拒绝时返回 4xx 是「不接管」的约定，插件因此会让 AstrBot 自己回复；
  Go 与插件必须成对升级。
- **落盘无上限**：DB 不自动清理，长期运行会一直增长；启动日志会打印条数与大小并在超阈值时
  warn。清理方式：停服后删 `state.db`（连 `-wal`/`-shm`）重建，或按 `workspace_id`
  手工 `DELETE` 后 `VACUUM`。清理单个账号前必须先停服，避免删到一半还有写事务。
  磁盘写失败时按降级处理（继续用内存），不会拖垮回复。
- **SQLite 锁**：单进程单连接（`SetMaxOpenConns(1)`）+ WAL，避免 `database is locked`；
  不要把同一个 `state.db` 同时给别的程序打开。
- **隐私**：消息原文、成员昵称会落盘；文件权限 0600，备份/拷贝要按敏感数据处理。
- **驱动体积**：`modernc.org/sqlite` 是纯 Go，但会让可执行文件增大数 MB；
  换来的是不需要 CGO 和额外 DLL。

## 12. 验证

- 单测：`{platform}_{account}` 拼接与 ID 合法化（点号、非法字符）；同账号并发创建只发生一次；
  空名单拒绝创建；档案里已有的工作空间无需名单即可恢复；所有工作空间共享同一个 PromptConfig。
- E2E：向 `/im/event/<appid>` 推一条 OB12 群消息（`self.platform=qq`）→ 204、
  `state.db` 出现 `qq_<appid>` 行、面板出现该工作空间；再推未授权账号 → 4xx、不入库；
  重启后从档案恢复该工作空间。
- 真实端：A/B 各自一条真实 @ → 两个 `qq_<appid>` 路径分别命中自己的工作空间；
  B 收到 @A 的普通消息只入 B 的缓存，不触发回复；B 换群后仍归到 B 的工作空间。
- 持久化：
  - 单测：建表幂等（重复启动不报错）；DB 不删历史（插入超过 `max_cache_msg` 后库里仍全在，
    内存只留最近 N 条）；启动统计能拿到条数与文件大小、超阈值走 warn；启动只把尾部读回内存；
    异步队列满时不阻塞入站；DB 打不开/写失败时降级为纯内存且不 panic。
  - E2E：发 3 条消息 → 重启 Go → 同群缓存仍有 3 条、锚点与人格关键字恢复；
    两个账号向 `state.db` 写同一个群、同一个 `message_id` 时互不覆盖；
    清空允许名单后重启，档案里的工作空间仍按 `workspace_id` 恢复且不报错。
