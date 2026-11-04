# OneBot 12 对接

本进程不直接连 QQ：它跟 AstrBot 的桥接插件说 OneBot 12。这一页只记我们实际用到的那部分，字段的完整定义以 `onebot/types.go` 为准。

## 为什么是 12

- `message_id` 是字符串。QQ 官方的消息 ID 是长字符串，OneBot 11 的数字 ID 装不下。
- 动作和事件是标准 JSON，不用 CQ 码拼字符串，带引用、带 @、发多段消息都有明确结构。
- webhook 是标准传输方式（HTTP POST），不强制 WebSocket，反向代理和鉴权都好办。

## 传输

| 方向 | 地址 | 说明 |
| --- | --- | --- |
| 入站 | `POST http://{im_listen_addr}/im/event/{account}` | 插件推事件；成功回 204 |
| 入站探活 | `GET http://{im_listen_addr}/im/health` | 插件自检 |
| 出站 | `POST {im.api_base}` | 本进程调动作；按账号覆盖用 `im.accounts[].api_base` |

鉴权：

- 入站看 `Authorization`（`im.token`，可为空但只建议在回环地址上这么用）。
- 出站带同一个 token 作 Bearer。
- 入站还会看 `X-OneBot-Version`：写了非 `12` 的值直接回 400，不带则放行。

## 事件里我们实际用到的字段

Go 侧统一通过 `onebot.Event` 的访问方法读，不直接按 JSON 路径取值。

| 用途 | 来源 | 备注 |
| --- | --- | --- |
| 消息唯一 ID | `event.MessageID` | 去重与幂等键；同一条重推会被丢掉 |
| 平台 | `event.Platform()`（`self.platform`） | 工作空间 ID 第一段 |
| 机器人自身 ID | `event.Self.UserID` | 只用来学身份，**不参与归属** |
| 群 ID | `event.GroupID` | QQ 官方是按机器人分配的 `group_openid`，换个号看就是另一个值 |
| 发送者 | `event.UserID` | 群内 openid，换群会变 |
| 会话 ID | `event.ChatID()` | 群聊取群 ID，私聊取对端 ID |
| 账号类型 | 扩展字段 `account_kind` | 工作空间 ID 第二段：`official_bot` / `personal_account` / `app` |

`self.platform` 有格式约束：`[a-z][-a-z0-9]*`，**不允许下划线**。所以 QQ 官方机器人的平台是 `qq`，`official_bot` 是账号类型——两段拼起来才是 `qq_official_bot_1906072771`。

## 出站动作

| Go 方法 | 干什么 | 用在哪 |
| --- | --- | --- |
| `GetSelfInfo` / `GetSelfInfoFor` | 取机器人昵称与 ID | 启动、昵称核对（10 分钟一次） |
| `GetGroupInfo` | 群名、群人数 | 面板展示、群档案 |
| `GetUserInfo` | 用户资料 | 补名 |
| `GetGroupMemberInfo` | 群成员的名片与昵称 | 前置补名（`sync`） |
| `GetVersion` / `GetSupportedActions` | 实现端版本与支持的动作清单 | 排障、能力探测 |
| `SendGroupMessageE` | 发群消息（纯文本） | 普通回复 |
| `SendGroupReplyE` | 发群消息并带引用、带 @ | 回复被 @ 的那条 |
| `SendGroupMessageSegmentsE` | 发多段消息（图、语音、视频、文件、@、引用） | 富消息回复 |
| `For(workspaceID)` | 取某账号的客户端（可能换 base 地址） | 多账号 |

## 媒体与被动回复

- QQ 官方机器人发图片要先把本地文件上传换 `file_id`，再发消息段。图片在 AstrBot 侧是本地文件，所以 `llm.image_local_dirs` 要指到那个目录（默认 `~/.astrbot/data/temp`）。
- 同一条触发消息的被动回复最多 5 次。所以分句发送时最多 5 条，第 5 条要把剩下的句子合并进去，否则超出的部分发不出去。

## 和标准的差异

| 差异 | 原因 |
| --- | --- |
| 平台名用 `qq` 而不是 `qq_official` | OneBot 12 的平台名不允许下划线 |
| 多一个 `account_kind` 扩展字段 | 同一个平台下"官方机器人"和"个人号"是两种账号，路由必须分开 |
| `echo_self_messages` | 某些实现（NapCat）会把机器人自己发的消息回传成事件；AstrBot 桥不回传。为 true 时本地不再重复记录回复 |
| 白名单按账号写 | 同一个物理群在不同账号眼里是两个 `group_openid`，漏写一个就会出现"@ 了 A，B 接话" |

## 排错

| 现象 | 含义 |
| --- | --- |
| 插件报 404 | Go 不接管这个账号（未开通且不在名单、或重复消息）。**不是错误**，插件应让平台自己兜底 |
| 插件报 503 | 入站队列满，等 2 秒仍进不去。下游堵了，看 `pipeline_stage_backlog` |
| 插件报 400 | 事件 JSON 坏、超 1 MB，或 `X-OneBot-Version` 不是 12 |
| 事件收到了但没有回复 | 群不在白名单（`im_events_total{result=ignored}`），或没 @ 且主动插话没通过裁判 |
| 补名全是 openid | `get_group_member_info` 不支持或被限流；看日志里该动作的失败原因 |

## 代码位置

| 文件 | 看什么 |
| --- | --- |
| `onebot/types.go` | `Event` 与各动作的请求/响应结构、`Platform()` / `Kind()` / `ChatID()` |
| `onebot/client.go` | 动作方法、按账号取客户端、Bearer 与超时 |
| `onebot/message.go` | 消息段（文本、图片、@、引用、语音、视频、文件）的组装 |
| `im/server.go` | 入站路由、鉴权、体积上限、返回码 |
| `sidecar/astrbot_plugin_onebot_bridge/` | 插件侧实现（推事件、翻译动作） |
