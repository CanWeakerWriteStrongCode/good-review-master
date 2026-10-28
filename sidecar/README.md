# AstrBot OneBot 11 桥接（sidecar）

AstrBot 只当 IM 网关：它负责连 QQ 官方机器人 / NapCat / 微信 / Telegram 等平台，把消息转成 OneBot 11 事件推给 Go 核心；Go 仍然是业务大脑，回复通过桥接插件交回 AstrBot 发送。

```
QQ 官方机器人 ──(WebSocket)──┐
NapCat ──(OneBot 11)─────────┼── AstrBot ── 本插件 ── POST /im/event ──> Go 核心
微信 / TG / ... ─────────────┘        ▲                                   │
                                      └──── send_group_msg / send_msg ◄───┘
```

出站：`aiocqhttp` 和其他平台交给 AstrBot 的 `Platform.send_by_session`；
`qq_official` 的群消息单独走官方 `post_group_message`（带最近一条入站消息的 `msg_id` 和
递增的 `msg_seq`），确保是**被动回复**而不是受额度限制的主动推送。换平台仍然不用改 Go。

## 1. 启动顺序

**QQ 官方机器人（qq_official）**

1. AstrBot 启动，在「消息平台」添加 `QQ 官方机器人(Websocket, 推荐)`，填 AppID / AppSecret。
2. 安装本插件，配置 Go 地址与共享 token。
3. **Go 最后启动**。

**NapCat（aiocqhttp）**

1. NapCat 登录 QQ，开启 OneBot 11 服务。
2. AstrBot 的 `aiocqhttp` 适配器连上 NapCat。
3. 安装本插件 → 启动 Go。

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
- `use_markdown: false`：Go 的回复是纯文本，避免 markdown 发送失败再回退。
- **不要再同时启用 `aiocqhttp`**（除非你确实有多平台）：插件在还没收到任何事件时，
  出站会退回「第一个找到的适配器」，同时配置两个平台容易把消息发到错的地方。
- AstrBot 自己的 LLM/人格不需要配置；插件转发后会 `stop_event()`，AstrBot 不会回复。
- AstrBot 的白名单、限流、内容安全、唤醒限制都不要开（或全放行）：这些过滤在 Go 侧做，
  AstrBot 拦掉的消息不会到达插件。

## 4. 插件配置

在 AstrBot WebUI 的插件配置页填写（示例见
[astrbot_plugin_onebot_bridge_config.example.json](astrbot_plugin_onebot_bridge_config.example.json)）：

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `go_event_url` | `http://127.0.0.1:18090/im/event` | 与 Go 的 `runtime.im_listen_addr` 对应 |
| `token` | 空 | 与 Go 的 `napcat.access_token` 填同一个值 |
| `platform_label` | 空 | 留空用 AstrBot 的平台名（`qq_official` / `aiocqhttp`） |
| `action_listen_host` | `127.0.0.1` | 动作代理监听地址 |
| `action_listen_port` | `6199` | Go 的 `napcat.http_api` 指向这里 |
| `forward_self_messages` | `false` | 是否转发机器人自己发的消息 |
| `action_timeout_sec` | `10` | 单个动作超时 |
| `event_retry` | `3` | 事件推送失败重试次数 |

> 插件用 `@filter.regex(".*", priority=100)` 让每条消息都通过 AstrBot 的唤醒检查。
> 这是功能必需：AstrBot 对未 @、无前缀的群消息会直接 `stop_event()`，插件收不到，
> Go 也就拿不到群聊上下文。删掉它会导致只有 @ 消息能进缓存。

## 5. Go 配置

```yaml
napcat:
  # 指向插件的动作代理，不再直连 NapCat
  http_api: "http://127.0.0.1:6199"
  # 与插件 token 相同
  access_token: "换成一个随机长串"

bot:
  # QQ 官方机器人填 group_openid，不是 QQ 群号
  allow_groups: "<group_openid>"

runtime:
  im_listen_addr: "127.0.0.1:18090"
  # QQ 官方机器人没有群历史接口，必须关
  backfill_on_start: false
```

`group_openid` 从哪来：插件第一次收到某个群的消息时会打一条
`首次收到群消息：group_id=...` 的日志，复制到 `bot.allow_groups` 即可（多个用逗号分隔）。
也可以从 AstrBot 的会话列表 / 日志里取 Unified Message Origin。

`bot.qq` 在官方机器人场景下只用于提示词展示和 CQ 兜底；真正的 @ 触发由插件写入事件的
`mentioned` 字段决定，不再依赖 `[CQ:at,qq=...]` 的号码匹配。

## 6. QQ 官方机器人的硬限制

- **群消息默认只有 @ 机器人才会下发**。要让 Go 缓存普通群聊，需要在群里给机器人
  「接收全部消息」的权限；否则缓存里只有 @ 消息，锐评上下文会很稀薄。
- **没有 `get_group_msg_history`**：启动回填必须关（`backfill_on_start: false`），
  重启后上下文缓存是空的，只能从新消息开始攒。
- **被动回复有 `msg_id` 窗口**：AstrBot 会把最近一条入站消息的 `msg_id` 记在会话上，
  Go 在窗口内回复走被动消息；超出窗口的主动推送受官方频率/额度限制，失败会返回 retcode。
- **消息 ID 是 `ROBOT1.0_...` 字符串**：桥接插件会做稳定哈希再交给 Go，保证去重和锚点可用。

## 7. 验证

```bash
# 插件动作代理（返回值里能看到当前适配器名）
curl http://127.0.0.1:6199/health

# Go 事件入口
curl -i http://127.0.0.1:18090/im/health

# 模拟一个事件（token 为空时可省略 Authorization）
curl -X POST http://127.0.0.1:18090/im/event \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"post_type":"message","message_type":"group","group_id":"<openid>","user_id":"1","nickname":"测试","raw_message":"[CQ:at,qq=qq_official] 锐评下","message_id":1,"time":1700000000,"platform":"qq_official","mentioned":true}'
```

Go 日志应出现 `收到 IM 事件`；如果群在白名单且 `mentioned=true`，会触发锐评。

## 8. 故障排查

| 现象 | 原因 |
|---|---|
| 插件日志 `首次收到群消息 group_id=...` 但 Go 不回复 | `bot.allow_groups` 里填的还是 QQ 群号，需要换成 `group_openid` |
| `mentioned` 一直是 false | 平台没有下发 @ 信息，或 AstrBot 适配器没识别到；先确认消息确实 @ 了机器人 |
| Go 启动日志 `获取机器人昵称失败` | AstrBot 还没连上平台，或 `napcat.http_api` 没指向 6199 |
| 插件日志 `Go 侧拒收事件 401` | 两边 token 不一致 |
| 插件日志 `Go 侧拒收事件 503` | Go 的事件队列满，检查是否卡在 LLM 调用 |
| AstrBot 和 Go 都回复 | 插件没加载成功，或其它插件抢在 `stop_event()` 之前回复 |
| 官方机器人回复报主动消息额度/频率 | 超出被动回复窗口；减少主动推送，或在窗口内回复 |
