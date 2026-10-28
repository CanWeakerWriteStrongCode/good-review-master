# AstrBot OneBot 12 桥接（sidecar）

AstrBot 只当 IM 网关：它负责连 QQ 官方机器人等平台，把消息转成 **OneBot 12** 事件推给
Go 核心；Go 仍然是业务大脑，回复通过 OneBot 12 动作服务交回 AstrBot 发送。

```
QQ 官方机器人 ──(WebSocket)──┐
其他 AstrBot 适配器 ─────────┼── AstrBot ── 本插件 ── POST /im/event/{account} ──> Go 核心
                             ▲                                                │
                             └──── OneBot 12 POST / (send_message) ◄──────────┘
```

- 入站：`type=message, detail_type=group`，`self={platform,user_id}`，消息体是
  `message` 段数组 + `alt_message`。
- 出站：插件在本地端口实现 OneBot 12 动作服务（`POST /`）：
  `send_message`、`get_self_info`、`get_group_info`、`get_group_member_info`、
  `get_user_info`、`get_version`、`get_supported_actions`。
- 事件转发后，只有 Go 返回 **2xx**（已接管）才 `stop_event()`；**4xx**（未授权 /
  账号不归 Go）交给 AstrBot 自己的 Agent 兜底。

部署要求：AstrBot 只当 IM 网关，它的 LLM Provider、人格、白名单等功能不要启用，
业务过滤（群白名单、@ 检测）全部在 Go 侧完成。

## 1. 启动顺序

1. AstrBot 启动，在「消息平台」添加 `QQ 官方机器人(Websocket, 推荐)`，填 AppID / AppSecret。
2. 安装本插件，配置共享 token；`platform` 与 `account` 通常都留空：
   平台名从平台实例自动识别（QQ/微信/飞书…），QQ 官方的账号自动取 AppID。
   一个 AstrBot 挂多个 QQ 官方号也不会混。
3. **Go 最后启动**。

## 2. 安装插件

把 `astrbot_plugin_onebot_bridge/` 整个目录放到 AstrBot 的 `data/plugins/` 下，重启 AstrBot。
插件依赖 `aiohttp`，首次加载会自动安装 `requirements.txt`。

## 3. AstrBot 平台配置

QQ 官方机器人的平台条目（WebUI 里填，或加到 `data/config/cmd_config.json` 的 `platform` 列表）：

```json
{
  "id": "qq_official",
  "type": "qq_official",
  "enable": true,
  "appid": "<AppID>",
  "secret": "<AppSecret>",
  "enable_group_c2c": true,
  "enable_guild_direct_message": false,
  "use_markdown": false
}
```

- `enable_group_c2c: true` 才会订阅群消息 intent。
- `use_markdown: false`：Go 默认发纯文本。
- AstrBot 自己的 LLM/人格不需要配置；Go 接管后插件会 `stop_event()`。
- AstrBot 的白名单、限流、内容安全、唤醒限制都不要开（或全放行）：这些过滤在 Go 侧做。

## 4. 插件配置

在 AstrBot WebUI 的插件配置页填写（示例见
[astrbot_plugin_onebot_bridge_config.example.json](astrbot_plugin_onebot_bridge_config.example.json)）：

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `account` | 空 | 接收账号兜底值；QQ 官方自动取平台实例的 AppID，通常留空。其它平台填了才会用 |
| `platform` | 空 | `self.platform` 兜底值；通常从平台实例自动识别（`qq_official`→`qq`、`wecom`、`lark`…），只有识别不到时才用它 |
| `platform_id` | 空 | 可选：AstrBot 平台实例 ID，动作请求没有群上下文时用它定位账号 |
| `go_event_url` | `http://127.0.0.1:18090/im/event` | Go 的事件入口前缀，实际路径追加 `/{account}` |
| `token` | 空 | 与 Go 的 `im_token`（`secret.yaml`）相同 |
| `action_listen_host` | `127.0.0.1` | OneBot 12 动作服务监听地址 |
| `action_listen_port` | `6199` | Go 的 `im.api_base` 指向 `http://host:port` |
| `forward_self_messages` | `false` | 是否转发机器人自己发的消息 |
| `action_timeout_sec` | `10` | 单个动作超时 |
| `event_retry` | `3` | 事件推送失败重试次数 |

> 插件用 `@filter.regex(".*", priority=100)` 让每条消息都通过 AstrBot 的唤醒检查。
> 这是功能必需：AstrBot 对未 @、无前缀的群消息会直接 `stop_event()`，插件收不到，
> Go 也就拿不到群聊上下文。删掉它会导致只有 @ 消息能进缓存。

## 5. Go 配置

```yaml
im:
  token: "换成一个随机长串"          # 也可以写进 secret.yaml 的 im_token
  # 插件动作服务地址；所有账号共用，账号 ID 由入站路径自动得到
  api_base: "http://127.0.0.1:6199"

bot:
  # QQ 官方机器人填 group_openid，不是 QQ 群号；填 "*" 表示所有群都响应
  allow_groups: "<group_openid>"
  # 允许自动开通的工作空间 ID；档案里已有的不受名单影响
  workspace_accounts: ["qq_<AppID>"]

runtime:
  im_listen_addr: "127.0.0.1:18090"
```

工作空间是收到第一条事件时自动开通的：提示词只有根目录一份，所有账号共享；
消息、锚点、人格等档案统一存在工作目录的 `state.db` 里，用 `workspace_id` 区分账号。
工作空间没有自己的目录或配置文件。名单只控制新开通，删掉名单不会让已有工作空间停止工作。

`group_openid` 从哪来：插件第一次收到某个群的消息时会打一条
`首次收到群消息：group_id=...` 的日志，复制到 `bot.allow_groups` 即可（多个用逗号分隔）。

## 6. 事件字段（OneBot 12）

```json
{
  "id": "ROBOT1.0_xxx",
  "time": 1700000000.5,
  "type": "message",
  "detail_type": "group",
  "sub_type": "",
  "self": {"platform": "qq", "user_id": "<openid 或 unknown_selfid>"},
  "message_id": "ROBOT1.0_xxx",
  "message": [
    {"type": "mention", "data": {"user_id": "<member_openid>"}},
    {"type": "text", "data": {"text": " 锐评下"}}
  ],
  "alt_message": "@小助理(<openid>) 锐评下",
  "group_id": "<group_openid>",
  "user_id": "<member_openid>"
}
```

- 事件体**没有** `platform_id` / `display_message` / `mentioned` 这类私有字段：
  接收账号由上报路径表达，平台由 `self.platform` 表达，是否 @ 由
  `mention.user_id == self.user_id` 判定。
- **`self.user_id` 按群学习**。QQ 官方的 openid 按群分配，插件按
  `(平台实例, 群)` 记住 `mentions[].is_you` 的那个 openid；没学到时发
  `unknown_selfid`。**拿不到通常是群内权限问题**：没给机器人「查看群所有信息」
  权限时，QQ 不下发 `mentions`，只剩占位 At；在 QQ 群设置里给机器人授予该权限后，@ 事件就会带
  `is_you` 和真实 openid（2026-10-28 实测）。不要拿 AppID / robot.id 去填，它们和
  mention 里的 openid 不是一个命名空间。权限仍缺失时，插件在事件顶层加
  `qq.at_me: true`；Go 只在哨兵时采信，普通消息不触发。
- **`self.user_id` 不参与工作空间归属**：归属永远是 `{platform}_{路径 account}`。
- `alt_message` 把 @ 渲染成 `@昵称(openid)`：昵称会变、也可能重名，括号里的 ID 才是
  模型判断「同一个人」的依据；成员名来自入站消息与 `mentions`，查不到时回落成 `@用户(ID)`。
- 引用是标准 `reply` 段，`data.message_id` 与入站 `message_id` 相同（字符串）。
  QQ 官方**群消息**引用时只给 REFIDX，所以 `reply.data.message_id` 可能为空，
  改由 `reply.data["qq.ref_msg_idx"]` 带被引消息的索引；每条事件顶层还有
  `qq.msg_idx`（这条消息自己的索引），Go 存 `ref_idx` 用于反查真正的 `reply_to`。
- 图片 / 语音 / 视频 / 文件：QQ 官方只给 URL，按扩展规则放在 `qq.url`（文件另带 `qq.name`）。
- 平台本身拿不到的：数字 QQ 号、数字群号、群名片、官方群成员昵称。

## 7. 出站动作

Go 调用的动作请求外壳（`POST /`）：

```json
{
  "action": "send_message",
  "params": {
    "detail_type": "group",
    "group_id": "<group_openid>",
    "message": [
      {"type": "reply", "data": {"message_id": "<入站 message_id>"}},
      {"type": "mention", "data": {"user_id": "<member_openid>"}},
      {"type": "text", "data": {"text": "锐评一下"}}
    ]
  },
  "echo": "send_message"
}
```

响应外壳：

```json
{"status": "ok", "retcode": 0, "data": {}, "message": "", "echo": "send_message"}
```

`status != ok` 或 `retcode != 0` 都算失败（Go 不只看 HTTP 200）。插件内部再把 OB12 段
翻译成平台能力：QQ 官方走官方被动回复 API（带 `msg_id` / `msg_seq` / REFIDX），
`aiocqhttp` 透传 OneBot 11 动作，其他适配器回退 `send_by_session`。

| 段类型 | QQ 官方机器人 | 其他 AstrBot 适配器 |
|---|---|---|
| `text` | ✅ | ✅ |
| `mention` | ✅ 走 markdown `<qqbot-at-user id>`；markdown 被拒时降级成纯文本 `@昵称` | ✅ |
| `reply` | ✅ `message_reference`（插件映射 REFIDX） | ✅ |
| `image` | ✅ 每条最多 1 张，自动上传 | ✅ |
| `voice` | ✅ 自动转 silk 上传 | ✅ |
| `video` | ✅ 上传；可能变成主动推送 | ✅ |
| `file` | ✅ 上传；可能变成主动推送 | ✅ |

多条富媒体会被拆成多条消息发送；`msg_id` 被动回复窗口对视频/文件支持不稳定，插件会按
AstrBot 的既有策略移除 `msg_id`，这类消息可能受主动推送额度限制。

## 8. QQ 官方机器人的硬限制

- **群消息默认只有 @ 机器人才会下发**。要让 Go 缓存普通群聊，需要在群里给机器人
  「接收全部消息」的权限；否则缓存里只有 @ 消息，锐评上下文会很稀薄。
- **没有群历史动作**：OneBot 12 没有标准历史接口，历史只来自共享 `state.db`
  （按 `workspace_id` 区分账号）；换机器或删库后从新消息开始攒。
- **被动回复有 `msg_id` 窗口**：超出窗口的主动推送受官方频率/额度限制，失败会返回 retcode。
- **消息 ID 是 `ROBOT1.0_...` 字符串**：OB12 原样透传，Go 侧从头到尾按字符串处理。
- **拿不到 `is_you` 的 @ 不会触发**：先查 QQ 群设置里是否给了机器人「查看群所有信息」
  权限——没有权限时 QQ 不下发 `mentions`，事件只能带占位 At / `unknown_selfid`；
  补上权限后 `is_you` 与真实 openid 恢复（2026-10-28 实测）。仍拿不到时不要用
  AppID 伪造匹配。

## 9. 验证

```bash
# 插件动作服务：get_version 应返回 onebot_version=12
curl -s http://127.0.0.1:6199/ \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"action":"get_version","params":{},"echo":"1"}'

# Go 事件入口
curl -i http://127.0.0.1:18090/im/health

# 模拟一条 @ 事件（token 为空时可省略 Authorization）
curl -X POST http://127.0.0.1:18090/im/event/<AppID> \
  -H "Authorization: Bearer <token>" \
  -H "X-OneBot-Version: 12" \
  -H "Content-Type: application/json" \
  -d '{"id":"e1","time":1700000000,"type":"message","detail_type":"group","sub_type":"","self":{"platform":"qq","user_id":"<openid>"},"message_id":"m1","message":[{"type":"mention","data":{"user_id":"<openid>"}},{"type":"text","data":{"text":" 锐评下"}}],"alt_message":"@小助理(<openid>) 锐评下","group_id":"<group_openid>","user_id":"<member_openid>"}'
```

Go 返回 204 表示已接管；日志里会出现 `已自动开通工作空间`、`收到 IM 事件`，
随后向配置的 `api_base` 发 `send_message`。

## 10. 故障排查

| 现象 | 原因 |
|---|---|
| Go 返回 404 | 账号没在 `bot.workspace_accounts` 里，且目录不存在（Go 不接管，交给 AstrBot） |
| Go 返回 401 | 两边 token 不一致（`im.token` / `im_token`） |
| Go 返回 503 | 事件队列满，检查是否卡在大模型调用 |
| Go 返回 400 | 路径 account 非法，或 `self.platform` 缺失 / 不是合法平台名 |
| 插件日志 `Go 未接管事件` | 按上一条确认；4xx 是预期的 AstrBot 兜底路径 |
| 机器人被 @ 但不回复 | 先查 QQ 群设置里是否给了机器人「查看群所有信息」权限；没权限时 `mentions=[]`、`self.user_id` 是 `unknown_selfid`，补上权限后重启 AstrBot 复测 |
| 回复里出现 `@用户(qq_official)` | 旧版插件把占位 At 当 mention 上报（v1.7.4 已丢弃占位 At）；升级插件并重启 Go 清掉内存里的旧聊天记录 |
| 回复发到另一个账号 | 两个账号用了不同的动作端口却只配了全局 `im.api_base`；给该账号加 `im.accounts[].api_base` 覆盖 |
| 面板工作空间为空 | 还没收到任何事件；账号开通发生在第一条事件到达时 |
| 官方机器人回复报主动消息额度/频率 | 超出被动回复窗口；减少主动推送，或在窗口内回复 |
