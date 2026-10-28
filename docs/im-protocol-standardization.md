# IM 协议标准化设计（对外契约 = OneBot 12）

状态：P0（Go 契约 + `state.db`）与 P1（AstrBot 插件）已实现（2026-10-28）；P2 Satori sidecar 待做。sidecar 只做「平台消息 ⇄ OneBot 12」翻译，
不携带私有扩展字段、不做业务判断；账号路由与业务逻辑都在 Go。

**部署约束**：小号用 NapCat 已被封两次，不再使用个人号协议，只走 QQ 官方机器人。
官方 Bot API 不提供数字 QQ 号/群号，identity 只有 AppID、机器人全局 ID 和群内 openid。

**本版变更（相对 OneBot 11 稿）**：

- 契约整体换成 **OneBot 12**：`self = {platform, user_id}`、ID 全字符串、事件/动作/消息段统一。
- 取消扩展字段 `receiver_bot_account_id`：**平台取标准字段 `self.platform`，账号取传输端点**，
  路由键在 Go 内部拼成 `{platform}_{account}`。
- `self.user_id` 只放真实可比值；拿不到就写哨兵 `unknown_selfid`，Go 见到哨兵一律不触发。
- 入站只做 OB12 webhook：`204` 即成功；取消 202/200「接管 / 不接管」语义。
- 出站改标准动作（`send_message`、`get_self_info`、`get_group_info`、`get_group_member_info`），
  默认共用一个 `im.api_base`（多实现端时可按账号覆盖），不再传扩展参数 `self_id`。
- 删除 `platform_id` / `mentioned` / `display_message`；`message_sent` 归一逻辑取消
  （OB12 没有该事件）。

## 1. 为什么是 OneBot 12

### 1.1 OneBot 11 的问题

| 问题 | 后果 |
|---|---|
| 事件里没有「IM 平台」维度 | 多平台时账号 ID 可能重名，只能靠私有字段或 URL 约定 |
| `self_id` 是 int64 标准 | QQ 官方只有 openid 字符串，必须做宽类型或哈希，偏离标准 |
| `post_type` / `message_type` 两套分类 | 平台差异只能靠适配器归一，容易丢语义 |
| 没有标准「我是否被 @」 | 只能让插件算好塞进 `mentioned` 这类私有字段 |

### 1.2 OneBot 12 的收益

- 每个非元事件都带 `self: {platform, user_id}`，**平台是协议原生字段**；
- 所有 ID 都是 `string`，QQ 官方 openid 直接可用，不需要宽类型；
- 事件只有 `type` / `detail_type` / `sub_type`，群消息字段稳定；
- 动作与响应结构统一（`action` / `params` / `echo` → `status` / `retcode` / `data` / `message`），
  多账号可以用「共用地址 + 按账号覆盖」表达，不需要私有选路参数；
- 允许带前缀的扩展字段（`qq.xxx`），媒体这类标准表达不了的信息有正规出口。

### 1.3 换 12 的代价（已接受）

| 项 | 影响 |
|---|---|
| Go 契约层 | 事件模型、动作客户端、消息段、响应外壳全部重写 |
| AstrBot 插件 | 从「补字段」升级为「完整的 OB12 实现端」：发事件 + 提供动作服务 |
| Koishi 侧 | 没有现成 OB11 参考，要写一个 Satori 双向 sidecar（见 4.2） |
| 交叉验证 | 生态里现成 OB12 实现端很少；`astrbot_plugin_onebot12` 是应用端，只能参考协议细节 |
| 能力缺口 | OB12 没有标准的群历史消息动作；历史改由本地 SQLite 消息库提供（见工作空间文档 7.1） |

**不选 OB11 的理由**：平台维度只能靠传输约定或私有字段，且 ID 类型与 QQ 官方天然冲突；
既然两边都要写适配器，就选一个身份模型正确的协议。

## 2. 契约要点

### 2.1 事件

基本字段（所有非元事件必备）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | 事件唯一标识（UUID） |
| `time` | float64 | 发生时间，Unix 秒（注意不是毫秒/整数） |
| `type` | string | `meta` / `message` / `notice` / `request` |
| `detail_type` | string | 如 `group` / `private` |
| `sub_type` | string | 可空字符串 |
| `self` | object | `{platform, user_id}`，机器人自身标识 |

群消息（`type=message`、`detail_type=group`）另有：

| 字段 | 类型 | 说明 |
|---|---|---|
| `message_id` | string | 平台消息 ID |
| `message` | segment[] | 消息段数组 |
| `alt_message` | string | 纯文本替代表示，可为空 |
| `group_id` | string | 群 ID |
| `user_id` | string | 发送者 ID |

注意：**OB12 群消息事件没有 `sender` 对象**，也没有群角色字段。昵称/群名片只能走
`get_group_member_info` / `get_user_info` 动作（插件实现见 4.1，Satori 见 4.2）。

### 2.2 消息段

| 段类型 | 参数 | 用途 |
|---|---|---|
| `text` | `text` | 纯文本 |
| `mention` | `user_id` | @ 某人 |
| `mention_all` | 无 | @ 全体 |
| `image` / `voice` / `audio` / `video` / `file` | `file_id` | 媒体，按平台提供 |
| `location` | `latitude` `longitude` `title` `content` | 位置 |
| `reply` | `message_id`、可选 `user_id` | 引用回复 |

QQ 官方只给 URL、不给 `file_id`，按扩展规则补 `data["qq.url"]`（见 4.3）。

### 2.3 动作与响应

| 用途 | 动作 | 关键参数 / 响应 |
|---|---|---|
| 发消息 | `send_message` | `detail_type=group` + `group_id` + `message`；响应 `{message_id, time}` |
| 撤回 | `delete_message` | `message_id` |
| 机器人自身 | `get_self_info` | `{user_id, user_name, user_displayname}` |
| 群信息 | `get_group_info` | `{group_id, group_name}` |
| 群成员 | `get_group_member_info` | `{user_id, user_name, user_displayname}` |
| 用户信息 | `get_user_info` | `{user_id, user_name, user_displayname, user_remark}` |
| 诊断 | `get_version` / `get_status` / `get_supported_actions` | 实现名、在线状态、能力列表 |

响应外壳：`{status: "ok"|"failed", retcode, data, message, echo}`。本项目只依赖
`status`、`retcode`、`data`；`retcode` 非 0 一律按失败处理并记日志。

### 2.4 传输

- **入站（webhook）**：实现端 `POST <url>`，请求头 `X-OneBot-Version: 12`、`X-Impl: <name>`，
  可选 `Authorization: Bearer <token>`；响应 `204`（或 `200` + 动作列表，本项目不用）。
- **出站（HTTP API）**：Go `POST <base_url>/`，body `{action, params, echo}`，
  响应体为动作响应；鉴权同上。
- **一条连接一个账号**：每个账号一个 `api_base`，动作请求不需要 `self` 字段；
  若将来共用一条连接，才必须按标准带 `self`。

## 3. 身份与路由

### 3.1 四个身份

| 概念 | 字段 | 说明 |
|---|---|---|
| 接收账号（路由） | 传输端点里的 `{account}` | 跨群、跨重启稳定，实例间唯一 |
| 平台 | 事件 `self.platform` | 标准字段，如 `qq` |
| @ 比较值 | 事件 `self.user_id` | 只与 `mention.user_id` 比较 |
| @ 目标 | `mention` 段的 `user_id` | 决定是否触发回复 |
| 发送者 | 事件 `user_id` | 不是接收账号 |

**路由键 = `{platform}_{account}`**，同时是工作空间 ID（会进数据库列与面板 URL）：

- `platform` 直接取 `self.platform`，按 OB12 术语表只能是小写字母、数字、连字符和点号
  （如 `qq`、`telegram`）；**下划线不合法**（`qq_official` 不行），
  QQ 官方与将来的 OneBot 实现统一用 `qq`。
- ID 要把平台名里的点号替换成下划线，再套 `^[A-Za-z0-9_-]{1,64}$`（如 `qq_1906xxxx`）。
- 同一账号在 QQ 官方和 OneBot 实现下都是 `qq`，靠 `account` 区分。
  若将来同时接 QQ 官方与个人号，AppID 和 QQ 号仍可能撞号，届时用 `qq.official` / `qq`
  两个平台名隔离（点号分段是术语表允许的）；当前只接官方，统一用 `qq`。

### 3.2 为什么账号必须来自端点

`self.user_id` 在标准实现里是机器人账号，但在 QQ 官方里是**群作用域 openid**：

| 观测（本机 log/bot.log） | 值 | 结论 |
|---|---|---|
| B 真实 @（16:59:16） | `self.user_id = mention.user_id = 0C2F9A2A…`，`is_you=true` | @ 判定成立 |
| A 真实 @（17:00:01） | 平台只给占位值，没有 mentions | 没有可比值 → 哨兵、不触发 |
| B 收到 @A 的普通消息（16:55:32） | `is_you=false`，无 mention | 不会误触发 |
| B 换群（17:48:02，群 6CC6…） | 群内 openid 变了 | 群作用域，不能跨群复用 |

所以 `self.user_id` 只能做 @ 比较，不能当账号键；账号由端点说明「这条事件是哪个实例收的」。

### 3.3 哨兵 `unknown_selfid`

拿不到本群真实 openid 时，`self.user_id` 写 `unknown_selfid`：

- Go 见到哨兵直接判定「不可比较、不触发」，消息仍然入缓存；
- 不用 `account`、AppID、`client.robot.id` 去填充 `self.user_id`：
  它们和 `mention.user_id` 不同命名空间，填进去再补一个 `mention` 只是伪造匹配；
- 该账号的群 @ 事件因此**不会回复**，这是「宁可漏回，不伪造匹配」的选择。
  要恢复回复只能从平台原始 `content` 解析真实 `<@!id>`（待确认，见 4.1 第 9 条）。

### 3.4 触发判定

```
self.user_id == unknown_selfid        → 不触发（只入缓存）
存在 mention.user_id == self.user_id  → 触发
否则                                   → 不触发（只入缓存）
```

`mention_all` 不算触发；收到事件但没被 @ 是合法事件。

`self.user_id` 的来源是平台给的「这条 @ 的是不是我」标记：QQ 官方在消息的 `mentions[]`
里用 `is_you=true` 指出机器人自己（AstrBot 的 `qqofficial_platform_adapter.py:857`
就是筛 `is_you is True`）。插件据此学到本群 openid，并把它同时写进 `self.user_id` 和
对应的 `mention` 段。**`is_you` 只存在于适配器内部**，不是事件字段——标准端
（Koishi/OneBot 实现）本来就有稳定的 `self.user_id`，直接比较即可。

兜底规则（保留现有行为）：仅当该昵称在部署内**唯一归属一个工作空间**时，
渲染后的 `content` 以 `@昵称` 开头也算触发。昵称可变、可重名、也可以被手打成普通文本，
所以多账号同名时必须禁用（现有 `allowNicknameFallback` 就是这个判断）；有可比较的
`mention` 段时以段为准，兜底只在没有段命中时生效。

## 4. 适配器

### 4.1 AstrBot 插件（OB12 实现端）

1. **`self.platform` 从平台实例自动识别**：取适配器的 `meta().name`
   （`qq_official` → `qq`，`wecom` / `lark` / `telegram` 等原样使用）；
   配置里的 `platform` 只在识别不到时兜底。
2. **上报路径的 account 取事件所属平台实例的 AppID**（`platform.appid`）：
   一个 AstrBot 进程挂多个 QQ 官方账号时，各事件才会进各自的工作空间。
   `account` 配置只作其它平台的兜底，QQ 官方通常留空。
3. 每个事件必须带 `id`（UUID）、`time`（秒，float）、`type`、`detail_type`、`sub_type`、`self`。
3. `self.user_id` 按 `(platform, group_id)` 缓存：只存 `mentions[].is_you` 学到的本群 openid；
   拿不到就写哨兵 `unknown_selfid`，**不跨群复用**。
4. AstrBot 为群 @ 事件补的占位 `At(qq=qq_official)` **直接丢弃**，不改写成 `mention` 段：
   它的目标值不是任何真实账号 ID。
5. `message` 段用真实 ID：`mention.user_id` = 被 @ 成员的 openid；
   `@全体` 输出 `mention_all`；引用输出 `reply` 段。
6. `alt_message` 由插件生成（`@昵称(ID)`、`[图片]` 这类可读形式），只作纯文本替代。
7. `message_id` 用平台原始 ID（字符串），不做哈希，出站 `reply` 段能原样回传。
8. 媒体：`image` / `file` 等只带 `data["qq.url"]`（QQ 官方给的是 URL，没有 file_id）。
9. 待确认：A 的原始 `content` 是否含真实 `<@!member_openid>`。若有，可填进 `self.user_id`
   与 `mention.user_id`，A 就能正常触发；这是唯一能解除「A 不回」的正当途径。
10. 无论是否被 @ 都上报；是否回复由 Go 判断。`notice` / `request` 等事件可以不上报（v1 只消费消息）。

插件同时提供 OB12 动作服务（`POST /`）：

| 动作 | 数据来源 |
|---|---|
| `send_message` | AstrBot 发送消息；`reply` 段映射回 QQ 被动回复的 msg_id |
| `get_self_info` | `client.robot.name` + AppID |
| `get_group_info` | AstrBot 群信息 |
| `get_group_member_info` / `get_user_info` | 插件自己维护的成员目录（来源是 AstrBot 事件的 sender/mentions；OB12 事件里没有这些字段） |
| `get_version` / `get_supported_actions` / `get_status` | 静态信息 |

`get_self_info.user_id` 用 AppID（账号级身份）。它与群内 `self.user_id` 不是同一个值，
这是 QQ 官方的平台限制，文档里要写明；昵称以 `user_name` 为准。

### 4.2 Koishi / Satori sidecar

Koishi 自身就是 Satori 服务端，不为它写 Koishi 插件，写一个独立双向 sidecar：

**入站**：连 Satori WS，收 `message-created` → 转 OB12 群消息 → `POST /im/event/{account}`。

| Satori | OB12 |
|---|---|
| `login.platform`（如 `qq`） | `self.platform` |
| `login.user.id` | `self.user_id` |
| `login.user.name` | `get_self_info.user_name` |
| `message.id` | `message_id` |
| `message.channel.id`（群） | `group_id` |
| `message.user.id` | `user_id` |
| `message.content`（元素数组） | `message` 段数组 |
| 纯文本拼接 | `alt_message` |

**出站**：sidecar 暴露 OB12 HTTP API，`send_message` → Satori `message.create(channel_id, content)`；
`get_group_info` → channel/guild 查询；`get_group_member_info` → guild member / user 查询。

- 每个 Satori 登录对应一条 `account`（配置别名，缺省用 `login.user.id`）；
- Satori 的 `platform` 值必须符合 OB12 命名规则，不合法时按映射表转换；
- Koishi 把 QQ 群映射成 guild/channel 的具体形状以实现时实测为准（见 9 风险）。

### 4.3 扩展字段边界

**事件与消息段默认只用标准字段**。唯一例外是标准表达不了的信息，按 OB12 扩展规则加前缀：

| 扩展 | 位置 | 原因 |
|---|---|---|
| `qq.url` | `image` / `file` / `voice` / `video` 的 `data` | QQ 官方给 URL，不给 file_id |

前缀必须是 `[_a-z]+`，所以是 `qq.url` 而不是 `qq_official.url`。

## 5. 传输、配置、工作空间

### 5.1 入站

`POST /im/event/{account}`，`account` 是部署级账号标识：

- AstrBot：插件配置 `go_event_url = http://127.0.0.1:18090/im/event`（前缀），
  每个事件按所属平台实例的 AppID 追加 `/{account}`；多账号共用一个插件配置；
- Satori sidecar：每个登录一个 `account`（别名或 `login.user.id`）。

校验：`account` 必须匹配 `^[A-Za-z0-9_-]{1,64}$`，否则 400；Bearer 不匹配返回 401；
版本头存在但不是 `12` 返回 400 并记日志。合法事件一律 **204**。

### 5.2 出站

出站默认用一个全局地址 `im.api_base`，Go 用 `POST <base>/` 发动作；账号选择由
`workspace_id` 决定，动作请求不带 `self`。一个实现端服务所有账号时（AstrBot 插件
就是这种），配一行地址即可。多套实现端/多端口时，才用 `im.accounts[].api_base`
按账号覆盖，留空表示沿用全局。

为什么用 `workspace_id` 而不是请求里带 `self`：`self` 是 `{platform, user_id}`，
QQ 官方的 `user_id` 是群作用域 openid，拿它选账号会在换群或未学到时失效；用 AppID
顶替又会让「动作身份」和「事件身份」不一致。账号选择留在 Go，实现端只负责把动作
交给正确的平台实例。

### 5.3 配置

```yaml
im:
  token: ""                      # 入站/出站共用，可空
  api_base: "http://127.0.0.1:6199"  # 所有账号默认的出站地址
  # 可选：多套实现端时按账号覆盖；不写 api_base 表示沿用全局
  # accounts:
  #   - id: "qq_1904xxxx"        # 工作空间 ID = platform_account
  #     api_base: "http://127.0.0.1:6200"

bot:
  # 允许自动开通工作空间的工作空间 ID（platform_account）
  workspace_accounts: ["qq_1906xxxx", "qq_1904xxxx"]
```

- `bot.workspaces`、`bot.allow_self_ids` 删除；旧 `astrbot.http_api` / `astrbot.access_token`
  迁移为 `im.api_base` / `im.token`。
- 名单匹配完整工作空间 ID，避免两个平台用同一个 account 时被一起放行。
- **自动开通只建状态，不建连接**：没有 `im.api_base` 且该账号没有覆盖地址时，
  能收事件、发不出消息（发送时记一条 warn 并按失败处理）。

### 5.4 响应语义（部署约定）

OB12 只定义「204/200 = 推送成功，其它 = 失败」。本项目在此之上约定：

- **2xx**：Go 已接管，插件 `stop_event()`，不再让 AstrBot 自己的 Agent 回复；
- **401 / 400 / 404**（未被接管：鉴权失败、账号非法、名单拒绝）：按推送失败处理，
  插件不 `stop_event()`，AstrBot 可以走自己的 Agent 兜底。

这不是 OB12 标准的一部分，插件与 Go 必须成对升级。

### 5.5 工作空间

- 事件到达时按 `{platform}_{account}` 自动创建/加载；
- 启动从 `state.db` 的 `meta` 恢复已有工作空间（不依赖目录或配置文件）；
- 允许名单只控制「新建」，档案里已有的工作空间不受影响；
- 所有工作空间共用一个 `state.db`（SQLite），每张表带 `workspace_id` 列区分账号，
  保存消息、锚点、身份、人格与成员名；
- 详细设计见 [workspace-auto-provisioning-design.md](workspace-auto-provisioning-design.md)。

## 6. 迁移对照（OneBot 11 → 12）

| 用途 | OneBot 11 | OneBot 12 |
|---|---|---|
| 事件类型 | `post_type` + `message_type` | `type` + `detail_type` + `sub_type` |
| 机器人身份 | `self_id` | `self: {platform, user_id}` |
| ID 类型 | int64 | string |
| 发送者 | `sender` 对象 | 只有 `user_id`，昵称查 `get_group_member_info` |
| 消息文本 | `raw_message` / `display_message` | `alt_message` + 段数组 |
| 发群消息 | `send_group_msg` | `send_message{detail_type=group}` |
| 回复 | 参数 `message_id` / 段 | `reply` 段 |
| 昵称 | `get_login_info` | `get_self_info` |
| 历史 | `get_group_msg_history` | 无标准动作，功能下线/扩展 |
| 多账号选路 | 扩展参数 `self_id` | 每账号独立 `api_base`（或共享连接时标准 `self`） |
| 上报成功 | 202 / 200（自定义） | 204 / 200（标准） |

## 7. 分阶段实施

### P0 Go 契约层（先做）

1. `onebot` 包换成 OB12 类型：`Event`（含 `self`、`message_id` 字符串、`alt_message`）、
   `Segment{Type, Data}`、`ActionRequest{Action, Params, Echo}`、`ActionResponse{Status, Retcode, Data, Message, Echo}`。
2. `im/server.go`：路径 `{account}`、Bearer、`X-OneBot-Version` 校验、204；
   非 `message` 事件直接 ack 丢弃。
3. 工作空间：ID = `{platform}_{account}`（点号替换为下划线）；名单匹配 ID；
   删除 `platform_id` / `self_id` / `nickname` 归属逻辑。
4. 触发：`self.user_id` 为哨兵 → 不触发；否则比较 `mention.user_id`。
5. 文本：从段渲染；`mention` 渲染成 `@昵称(ID)`，名称由成员目录补（异步查
   `get_group_member_info` 并缓存），没有名称时用 `@用户(ID)`；`alt_message` 仅兜底。
6. 出站：按账号 `api_base` 调 `POST /`；`send_message`、`get_self_info`、`get_group_info`、
   `get_group_member_info`、`get_user_info`、`get_version`、`get_supported_actions`；
   删除 `get_login_info` / `get_group_msg_history` 调用：历史与回填改读本地共享 `state.db`
   （见 [workspace-auto-provisioning-design.md](workspace-auto-provisioning-design.md) 7.1）。
7. 删除 `display_message` / `mentioned` / `platform_id` 相关字段与测试。
8. 接入单个 SQLite `state.db`（纯 Go 驱动 `modernc.org/sqlite`），按 `workspace_id`
   持久化消息、锚点、身份、人格关键字与成员名，详见工作空间文档 7.1。

### P1 AstrBot 插件（OB12 实现端）

8. 事件改写：`id/time/type/detail_type/sub_type/self` + 群消息字段；
   `self.platform` 取平台实例的 `meta().name`（`qq_official` 映射为 `qq`）。
   上报账号取平台实例的 AppID，避免一份插件配置只能服务一个机器人。
9. `self.user_id` 按 `(platform, group)` 学 openid，学不到发哨兵；占位 At 丢弃。
10. 消息段映射：`text` / `mention` / `mention_all` / `reply` / `image(qq.url)`；
    `alt_message` 生成；`message_id` 原样透传。
11. 动作服务：按 4.1 的表实现 `POST /`，响应外壳与 `retcode` 按标准；`echo` 原样回传。
12. 删除 `platform_id` / `mentioned` / `display_message`；`message_sent` 归一取消。
13. README 改为「OneBot 12 适配器」，写清 AppID / `robot.id` / 群内 openid 的关系与哨兵规则。

### P2 Satori sidecar

14. 入站：Satori WS → OB12 → `POST /im/event/{account}`（见 4.2 映射）。
15. 出站：实现 OB12 动作 → Satori API；元素数组与 OB12 段互转。
16. 文档 + 配置示例：Koishi 开 Satori 服务，sidecar 配 token 与账号映射。

### P3 可选

17. WebSocket 传输、`get_latest_events` 轮询、`notice` 事件消费、能力探测
    （按 `get_supported_actions` 跳过不支持的段/动作）。

## 8. 验证

- 单测：`{platform}_{account}` 拼接与 ID 合法化；哨兵不触发；`mention.user_id == self.user_id`
  才触发；`mention_all` 不触发；动作响应 `status/retcode` 解析；`echo` 回传；4xx 视为失败。
- E2E（mock OB12 实现端）：向 `/im/event/1906xxxx` 推两条事件——
  1. `self={qq, openid}` + `mention={openid}` → 204、工作空间入库、Go 调 mock 的 `POST /`
     发 `send_message{detail_type=group}`；
  2. `self={qq, openid}` + 无 mention → 204、入缓存、不发送。
- E2E（QQ 官方，按本机日志复现）：
  1. B 的真实 @：`self.user_id == mention.user_id` → 触发；
  2. A 的真实 @：`self.user_id = unknown_selfid`、无 mention → 不触发（预期）；
  3. B 收到 @A 的普通消息：无 mention → 入 B 的缓存，不触发。
- 稳定性：同一账号跨两个群时 `account` 不变、`self.user_id` 可以不同；
  归属始终按 `{platform}_{account}` 走。
- 真实端：B 换群后仍归到 B 的工作空间；A 的哨兵事件不丢缓存。
- Satori：mock 一条 `message-created` → OB12 事件 → 回复经 sidecar 回到 Satori。

## 9. 风险

- **`self.user_id` 群作用域**：QQ 官方换群后 openid 变化（6CC6 群已复现），
  只能做 @ 比较；账号必须来自端点，否则会出现「换群后归属丢失」。
- **A 类账号不回复**：没有 mentions 的群 @ 事件只能发哨兵。需要先抓原始 `content`，
  确认是否有真实 `<@!id>` 可解析；没有的话这是平台限制，不要用 AppID 伪造。
- **平台命名**：必须符合 `[a-z][-a-z0-9]*(\.[-a-z0-9]+)*`，`qq_official` 这类下划线非法；
  统一用 `qq` 需要和其它实现作者保持一致。
- **生态薄**：现成 OB12 实现端少，AstrBot 没有官方支持，排障只能靠本地 mock；
  `astrbot_plugin_onebot12` 是应用端，只能参考字段细节（且 AGPL-3.0，不能抄代码）。
- **事件信息变少**：群消息没有 `sender`/角色，昵称要额外调 `get_group_member_info`；
  首次见到的用户可能只有 ID，权限类功能暂时没有角色字段。
- **媒体**：标准只有 `file_id`，QQ 官方只有 URL；用 `qq.url` 扩展，严格实现可能不认，
  需要在 `get_supported_actions` 之外再做能力协商。
- **历史消息**：OB12 没有标准动作，历史由本地 `state.db` 按 `workspace_id` 提供；
  换机器、删库或新部署时没有历史，属预期行为，不要指望平台回填。
- **2xx/4xx 语义**：这是本项目的部署约定，不是标准；插件与 Go 必须成对升级，
  否则会出现「Go 已接管但插件还让 AstrBot 回复」或反过来。
- **同连接多账号**：账号选择在 Go（`platform + account → workspace`），投递地址是
  `im.api_base`（可按账号覆盖）。不要把账号选择塞进动作请求靠 `self`：QQ 官方的
  `self.user_id` 是群作用域 openid，无法稳定选账号。
- **迁移期**：旧 OB11 事件结构与新插件不兼容，两侧要同时升级；Go 可短暂保留旧解码器
  并打 warn，但不能长期双协议并存。
