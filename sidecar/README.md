# AstrBot OneBot 11 桥接（sidecar）

AstrBot 只当 IM 网关：它负责连 QQ 官方机器人等平台，把消息转成 OneBot 11 事件推给 Go 核心；Go 仍然是业务大脑，回复通过桥接插件交回 AstrBot 发送。

```
QQ 官方机器人 ──(WebSocket)──┐
其他 AstrBot 适配器 ─────────┼── AstrBot ── 本插件 ── POST /im/event ──> Go 核心
                             ▲                                   │
                             └──── send_group_msg / send_msg ◄───┘
```

出站统一走 OneBot 11 消息段数组（文本 / @ / 引用 / 图片 / 语音 / 视频 / 文件）：

- `qq_official` 群消息由插件直接调官方 `post_group_message`，带最近一条入站消息的
  `msg_id` 和递增的 `msg_seq`，确保是**被动回复**；可见引用用 `message_reference` + `REFIDX`。
- AstrBot 的其他适配器（包括 `aiocqhttp`）优先把 `send_group_msg` 透传给协议端，
  失败时回退到 `Platform.send_by_session`。换平台仍然不用改 Go。

## 1. 启动顺序

1. AstrBot 启动，在「消息平台」添加 `QQ 官方机器人(Websocket, 推荐)`，填 AppID / AppSecret。
2. 安装本插件，配置 Go 地址与共享 token。
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
- `use_markdown: false`：Go 默认发纯文本；需要 markdown 时在消息段里显式用 `markdown`，失败会自动回退。
- 没收到任何入站事件前，插件出站会退回「第一个找到的适配器」；多平台共存时建议只启用一个平台。
- AstrBot 自己的 LLM/人格不需要配置；插件转发后会 `stop_event()`，AstrBot 不会回复。
- AstrBot 的白名单、限流、内容安全、唤醒限制都不要开（或全放行）：这些过滤在 Go 侧做，
  AstrBot 拦掉的消息不会到达插件。

## 4. 插件配置

在 AstrBot WebUI 的插件配置页填写（示例见
[astrbot_plugin_onebot_bridge_config.example.json](astrbot_plugin_onebot_bridge_config.example.json)）：

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `go_event_url` | `http://127.0.0.1:18090/im/event` | 与 Go 的 `runtime.im_listen_addr` 对应 |
| `token` | 空 | 与 Go 的 `astrbot.access_token` 填同一个值 |
| `platform_label` | 空 | 作为 `X-Platform` 请求头的值；留空用 AstrBot 的平台名（`qq_official` / `aiocqhttp`），非 ASCII 值会被省略 |
| `action_listen_host` | `127.0.0.1` | 动作代理监听地址 |
| `action_listen_port` | `6199` | Go 的 `astrbot.http_api` 指向这里 |
| `forward_self_messages` | `false` | 是否转发机器人自己发的消息 |
| `action_timeout_sec` | `10` | 单个动作超时 |
| `event_retry` | `3` | 事件推送失败重试次数 |

> 插件用 `@filter.regex(".*", priority=100)` 让每条消息都通过 AstrBot 的唤醒检查。
> 这是功能必需：AstrBot 对未 @、无前缀的群消息会直接 `stop_event()`，插件收不到，
> Go 也就拿不到群聊上下文。删掉它会导致只有 @ 消息能进缓存。

## 5. Go 配置

```yaml
astrbot:
  # 插件的动作代理地址
  http_api: "http://127.0.0.1:6199"
  # 与插件 token 相同
  access_token: "换成一个随机长串"

bot:
  # QQ 官方机器人填 group_openid，不是 QQ 群号；填 "*" 表示所有群都响应
  allow_groups: "<group_openid>"
  # Go 接管的机器人账号（OneBot self_id）；填 "*" 表示全部。
  # 不在列表里的账号会收到 200 ignored，插件不 stop_event()，消息交给 AstrBot 自己的 Agent。
  allow_self_ids: "*"

runtime:
  im_listen_addr: "127.0.0.1:18090"
  # QQ 官方机器人没有群历史接口，保持关闭
  backfill_on_start: false
```

旧版配置里的 `napcat` 段和 `napcat_access_token` 仍会被读取并打迁移告警，建议尽快改成
`astrbot` / `astrbot_access_token`。

`group_openid` 从哪来：插件第一次收到某个群的消息时会打一条
`首次收到群消息：group_id=...` 的日志，复制到 `bot.allow_groups` 即可（多个用逗号分隔）。
也可以从 AstrBot 的会话列表 / 日志里取 Unified Message Origin。

多账号同时运行时，用 `bot.allow_self_ids` 指定哪些机器人账号归 Go 管。插件只在 Go 返回
`202 accepted` 时调用 `stop_event()`；返回 `200 ignored`（账号不在名单里、私聊、WebChat）
时消息会继续走到 AstrBot 自己的 Agent，因此两边不会互相抢消息。两个账号同群、同昵称时，
出站动作会带 `self_id`，插件按它选择账号，不会把回复从另一个账号发出去。

同一物理群里的两个账号会各自收到一条事件，所以 `mentioned` 必须按账号分别判定：只有
`mentions` 里标了 `is_you`，或消息里命中该账号已学到的真实 openid 才算被 @。
QQ 官方群消息有一个例外：`group_at_message_create`（真正被 @）在拿不到 openid 时，
AstrBot 会补 `At(qq=qq_official)`；普通 `group_message_create` 不会补。因此群消息里出现
`At(qq=占位值)` 可以判定为本账号被 @；频道消息的占位 At 是无条件补的，不能套用此规则。
机器人自己发出的 `message_sent` 消息默认不回灌到 Go 的上下文。

真正的 @ 触发由插件写入事件的 `mentioned` 字段决定，不依赖 `[CQ:at,qq=...]` 的号码匹配；
QQ 官方机器人只有 `openid`，没有数字 QQ 号，因此配置里也不再需要 `bot.qq`。

## 6. 事件字段

事件本身是 OneBot 11 群消息事件，只保留三个扩展字段：

| 字段 | 说明 |
|---|---|
| `raw_message` | 平台原样文本：文本、`[CQ:at]`、`[CQ:reply]`、`[CQ:image]`、`[CQ:record]`、`[CQ:video]`、`[CQ:file]`、`[CQ:face]`，保留给排障 |
| `message` | 同一份内容的消息段数组；引用是标准 `reply` 段 |
| `sender` | 标准发送者对象：`{"nickname": "...", "card": "..."}` |
| `message_id` | 各平台消息 ID 稳定哈希成 int64 |
| `self_id` | 机器人稳定 ID（QQ 官方是 openid）；适配器给占位值时由插件替换成学到的 openid |
| `platform_id` | **扩展**：AstrBot 平台实例 ID（配置里的 `id`），每个实例唯一且恒定；Go 用它做多账号工作空间归属的主键 |
| `display_message` | **扩展**：给缓存和大模型看的统一文本，所有 @ 已翻译成 `@昵称(openid)` |
| `mentioned` | **扩展**：是否真的 @ 了本账号（要求 `mentions.is_you` 或命中已学到的真实 openid；占位值 `qq_official` 不算） |

标准字段之外的信息不再塞进事件：

- **引用**：标准 `reply` 消息段，`data.id` 与入站 `message_id` 同一套哈希，
  Go 直接把它当作 chat log 里的 `msg_id`；被引用消息不在记录里时就不提供内容。
- **群名**：走 `get_group_info` 动作，不放进事件。
- **平台名**：放在 `X-Platform` 请求头里，只用于 Go 的排障日志。
- **REFIDX**：QQ 官方的引用 ID 只留在插件内部做映射，Go 出站引用时传 `message_id`，
  插件再换回 REFIDX。

`display_message` 里的昵称只是展示名，括号里的 ID 才是稳定身份：群友改昵称后 ID 不变，
大模型可以据此判断前后是不是同一个人。插件按群维护 `openid → 昵称` 目录，来源是每条消息的
发送者和 QQ 官方 `mentions`（含 `is_you`）；目录里没有的成员不会猜，直接显示 `@用户(openid)`。
`raw_message` 保持平台原样，因此 CQ at / `<@openid>` 仍可用于排障和旧版兼容。

平台本身拿不到的：数字 QQ 号、数字群号、群名片、官方群成员昵称。QQ 官方机器人只有
`member_openid` / `group_openid`；昵称取 AstrBot 的 `author.username`，接口不给时回落为 openid。

## 7. 出站消息段

Go 发给插件的 `send_group_msg` 支持字符串或 OneBot 11 段数组：

```json
{
  "group_id": "<group_openid>",
  "message": [
    {"type": "reply", "data": {"id": "3484799710571075834"}},
    {"type": "at", "data": {"qq": "<member_openid>"}},
    {"type": "text", "data": {"text": "锐评一下"}},
    {"type": "image", "data": {"file": "https://example.com/a.png"}}
  ]
}
```

`reply` 的 `id` 是 Go 侧的 `message_id`（入站事件的同一个值）。QQ 官方机器人需要
REFIDX 才能显示引用，插件内部维护 `message_id → REFIDX` 映射，Go 不需要知道。

| 段类型 | QQ 官方机器人 | 其他 AstrBot 适配器 |
|---|---|---|
| `text` | ✅ | ✅ |
| `markdown` | ✅（失败回退文本） | 按纯文本发送 |
| `at` | ✅ 走 markdown `<qqbot-at-user id>`；markdown 被拒时降级成纯文本 `@昵称` | ✅ |
| `reply` | ✅ `message_reference`（插件映射 REFIDX） | ✅ |
| `image` | ✅ 每条最多 1 张，自动上传 | ✅ |
| `record` | ✅ 自动转 silk 上传 | ✅ |
| `video` | ✅ 上传；可能变成主动推送 | ✅ |
| `file` | ✅ 上传；可能变成主动推送 | ✅ |
| `face` | ⚠️ 仅能作为文本占位 | ✅ |

多条富媒体会被拆成多条消息发送；`msg_id` 被动回复窗口对视频/文件支持不稳定，插件会按
AstrBot 的既有策略移除 `msg_id`，这类消息可能受主动推送额度限制。QQ 官方带媒体的消息不能
同时用 markdown，所以 `at` 会降级成纯文本 `@昵称`（不是真正的提及）。

## 8. QQ 官方机器人的硬限制

- **群消息默认只有 @ 机器人才会下发**。要让 Go 缓存普通群聊，需要在群里给机器人
  「接收全部消息」的权限；否则缓存里只有 @ 消息，锐评上下文会很稀薄。
- **没有 `get_group_msg_history`**：启动回填必须关（`backfill_on_start: false`），
  重启后上下文缓存是空的，只能从新消息开始攒。
- **被动回复有 `msg_id` 窗口**：超出窗口的主动推送受官方频率/额度限制，失败会返回 retcode。
- **消息 ID 是 `ROBOT1.0_...` 字符串**：插件会做稳定哈希再交给 Go，保证去重和锚点可用。

## 9. 验证

```bash
# 插件动作代理（返回值里能看到当前适配器名）
curl http://127.0.0.1:6199/health

# Go 事件入口
curl -i http://127.0.0.1:18090/im/health

# 模拟一个事件（token 为空时可省略 Authorization；X-Platform 只影响 Go 日志）
curl -X POST http://127.0.0.1:18090/im/event \
  -H "Authorization: Bearer <token>" \
  -H "X-Platform: qq_official" \
  -H "Content-Type: application/json" \
  -d '{"post_type":"message","message_type":"group","group_id":"<openid>","user_id":"1","sender":{"nickname":"测试","card":""},"raw_message":"[CQ:at,qq=qq_official] 锐评下","message_id":1,"time":1700000000,"self_id":"qq_official","display_message":"@机器人(qq_official) 锐评下","mentioned":true}'
```

Go 日志应出现 `收到 IM 事件`；如果群在白名单且 `mentioned=true`，会触发锐评。

## 10. 故障排查

| 现象 | 原因 |
|---|---|
| 插件日志 `首次收到群消息 group_id=...` 但 Go 不回复 | `bot.allow_groups` 里填的还是 QQ 群号，需要换成 `group_openid` |
| `mentioned` 一直是 false | 平台没有下发 @ 信息，或 AstrBot 适配器没识别到；先确认消息确实 @ 了机器人 |
| Go 启动日志 `获取机器人昵称失败` | AstrBot 还没连上平台，或 `astrbot.http_api` 没指向 6199 |
| 插件日志 `Go 侧拒收事件 401` | 两边 token 不一致 |
| 插件日志 `Go 侧拒收事件 503` | Go 的事件队列满，检查是否卡在 LLM 调用 |
| AstrBot 和 Go 都回复 | 插件没加载成功，或其它插件抢在 `stop_event()` 之前回复 |
| 官方机器人回复报主动消息额度/频率 | 超出被动回复窗口；减少主动推送，或在窗口内回复 |
