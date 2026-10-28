"""AstrBot ↔ OneBot 11 桥接插件。

拓扑（sidecar）：

    QQ 官方机器人 / OneBot / 微信 / TG ── AstrBot 适配器 ── 本插件 ──HTTP──> Go 核心
                                                    <──HTTP───  动作请求

- 入站：把 AstrBot 收到的事件转成 OneBot 11 事件，POST 给 Go 的 /im/event；
- 出站：监听一个本地 HTTP 端口，把 Go 发来的 send_group_msg / send_msg 交给平台适配器。
  aiocqhttp 走 AstrBot 的 send_by_session；qq_official 单独走官方被动回复 API
  （带 msg_id / msg_seq），否则会被当成主动推送受额度限制；
- 事件转发后调用 stop_event()，让 AstrBot 自己的 LLM/插件流水线不再回复。

部署要求：AstrBot 只当 IM 网关，它的 LLM Provider、人格、白名单等功能不要启用，
业务过滤（白名单、@ 检测）全部在 Go 侧完成。
"""

from __future__ import annotations

import asyncio
import hashlib
import hmac
import json
import time
from types import SimpleNamespace
from typing import Any

import aiohttp
from aiohttp import web

from astrbot.api import logger
from astrbot.api.event import AstrMessageEvent, MessageChain, filter
from astrbot.api.message_components import Plain
from astrbot.api.platform import MessageType
from astrbot.api.star import Context, Star, register

PLUGIN_NAME = "astrbot_plugin_onebot_bridge"

DEFAULT_EVENT_URL = "http://127.0.0.1:18090/im/event"
DEFAULT_LISTEN_HOST = "127.0.0.1"
DEFAULT_LISTEN_PORT = 6199

SEND_ACTIONS = ("send_group_msg", "send_msg")
QUERY_ACTIONS = ("get_login_info", "get_group_info", "get_group_msg_history")


@register(
    PLUGIN_NAME,
    "good-review",
    "把 AstrBot 事件转发给外部 Go 服务，并把 OneBot 动作用作平台无关的发送/查询协议",
    "1.1.0",
)
class OneBotBridge(Star):
    def __init__(self, context: Context, config: Any = None):
        super().__init__(context)
        self.config = config or {}
        self._session: aiohttp.ClientSession | None = None
        self._runner: web.AppRunner | None = None
        self._start_lock = asyncio.Lock()
        self._started = False

        # 从事件里顺手缓存的身份/群名，供 get_login_info / get_group_info 使用。
        # QQ 官方机器人没有与 OneBot 等价的查询接口，这些值只能这样攒。
        self._self_id = ""
        self._self_name = ""
        self._group_names: dict[str, str] = {}
        self._seen_groups: set[str] = set()
        # 最近一条入站事件来自哪个平台实例。出站优先回到同一个实例，
        # 避免同时配置了多个平台时把回复发到错的平台。
        self._last_platform_id = ""
        # 每个会话最近一条入站消息 ID：qq_official 的被动回复必须带它。
        self._last_msg_ids: dict[str, str] = {}
        # 每个会话的 msg_seq 计数器（官方 API 要求同一 msg_id 下唯一）。
        self._msg_seq: dict[str, int] = {}

    # ---------------- 生命周期 ----------------

    async def initialize(self):
        await self._ensure_session()
        await self._ensure_server()

    async def terminate(self):
        if self._session is not None:
            await self._session.close()
            self._session = None
        if self._runner is not None:
            await self._runner.cleanup()
            self._runner = None
        self._started = False

    # ---------------- 入站：AstrBot -> Go ----------------

    # regex 过滤器是必需的，不是为了匹配内容：
    # AstrBot 的 WakingCheckStage 对"未 @、未用唤醒前缀"的群消息会直接 stop_event()，
    # 普通群聊根本到不了这里，Go 侧也就拿不到上下文。任意 filter 通过都会把消息标记为
    # is_wake；用 `.*` 放行全部消息，再在下面 stop_event() 阻止 AstrBot 自己调 LLM。
    @filter.event_message_type(filter.EventMessageType.ALL)
    @filter.regex(r".*", priority=100)
    async def forward_event(self, event: AstrMessageEvent):
        # 无论转发成功与否都停掉 AstrBot 自己的流水线：
        # 这个部署里 AstrBot 只是网关，回复由 Go 侧生成。
        try:
            await self._ensure_session()
            try:
                await self._ensure_server()
            except Exception as exc:
                # 动作代理起不来只影响"Go 发消息"，不该影响入站转发。
                logger.error(f"[onebot-bridge] 动作代理启动失败: {exc}")
            payload = self._build_payload(event)
            if payload is not None:
                await self._post_event(payload)
        except Exception as exc:  # 插件异常不能拖垮 AstrBot 的事件循环
            logger.error(f"[onebot-bridge] 转发事件失败: {exc}")
        finally:
            event.stop_event()

    def _build_payload(self, event: AstrMessageEvent) -> dict | None:
        msg = event.message_obj
        raw = getattr(msg, "raw_message", None)

        if isinstance(raw, dict) and raw.get("post_type") in ("message", "message_sent"):
            # aiocqhttp：NapCat 的原始 OneBot 事件已经是目标格式，直接透传并补齐字段。
            payload = dict(raw)
            payload["sender"] = dict(raw.get("sender") or {})
            payload["raw_message"] = raw.get("raw_message") or getattr(msg, "message_str", "") or ""
        else:
            # 其他平台（含 QQ 官方机器人）：用 AstrBot 的标准化字段拼一个 OneBot 11 事件。
            # raw_message 在这里是 botpy 对象，不能透传；CQ 串由消息链重新生成，
            # 覆盖 Go 侧需要的 @ 和图片语义。
            chain = getattr(msg, "message", []) or []
            payload = {
                "post_type": "message",
                "message_type": "group" if getattr(msg, "group_id", "") else "private",
                "group_id": str(getattr(msg, "group_id", "") or ""),
                "user_id": str(getattr(msg.sender, "user_id", "")),
                "sender": {
                    "nickname": getattr(msg.sender, "nickname", "") or "",
                    "card": getattr(msg.sender, "card", "") or "",
                },
                "message": self._to_segments(chain),
                "raw_message": self._to_cq_string(chain),
                "message_id": getattr(msg, "message_id", ""),
                "self_id": str(getattr(msg, "self_id", "")),
                "time": int(getattr(msg, "timestamp", 0) or time.time()),
            }

        sender = payload.get("sender") or {}
        payload["nickname"] = sender.get("nickname", "") or ""
        payload["card"] = sender.get("card", "") or ""
        payload["group_id"] = str(payload.get("group_id") or "")
        payload["user_id"] = str(payload.get("user_id") or "")
        payload["self_id"] = str(payload.get("self_id") or getattr(msg, "self_id", ""))
        payload["post_type"] = payload.get("post_type") or "message"
        if payload["post_type"] == "message_sent":
            # OneBot 把机器人自己发出的消息标成 message_sent；归一成 message 后
            # Go 侧才能和普通消息走同一条缓存/去重路径。
            payload["post_type"] = "message"
        payload["message_type"] = payload.get("message_type") or (
            "group" if payload["group_id"] else "private"
        )
        payload["platform"] = self._cfg("platform_label", "") or event.get_platform_name()
        payload["time"] = int(payload.get("time") or time.time())
        # Go 侧 MessageID 是 int64：QQ 官方机器人的消息 ID 是 ROBOT1.0_... 这种字符串，
        # 哈希成一个稳定 int64，保证去重与锚点在重启后仍然成立。
        payload["message_id"] = _as_message_id(payload.get("message_id"))
        payload["mentioned"] = self._is_mentioned(event, msg)

        self._remember_identity(msg, event)
        if payload["user_id"] == payload["self_id"] and payload["self_id"]:
            if not self._cfg_bool("forward_self_messages", False):
                return None
        return payload

    def _remember_identity(self, msg, event: AstrMessageEvent | None = None) -> None:
        self_id = str(getattr(msg, "self_id", "") or "")
        if self_id:
            self._self_id = self_id

        if event is not None:
            try:
                platform_id = event.get_platform_id()
            except Exception:
                platform_id = ""
            if platform_id:
                self._last_platform_id = str(platform_id)

        group_id = str(getattr(msg, "group_id", "") or "")
        message_id = str(getattr(msg, "message_id", "") or "")
        session_id = str(getattr(msg, "session_id", "") or group_id)
        if session_id and message_id:
            self._last_msg_ids[session_id] = message_id
        group = getattr(msg, "group", None)
        group_name = str(getattr(group, "group_name", "") or "")
        if group_id and group_name:
            self._group_names[group_id] = group_name
        if group_id and group_id not in self._seen_groups:
            self._seen_groups.add(group_id)
            logger.info(
                f"[onebot-bridge] 首次收到群消息：group_id={group_id}。"
                f"把它加入 Go 的 bot.allow_groups（QQ 官方机器人这里是 group_openid）"
            )

    @staticmethod
    def _is_mentioned(event: AstrMessageEvent, msg) -> bool:
        # AstrBot 的唤醒阶段已经把 At/AtAll/Reply/wake_prefix 算过了；
        # 这里再用消息链兜底一次，覆盖不同适配器的 self_id 形式。
        if bool(getattr(event, "is_at_or_wake_command", False)):
            return True
        self_id = str(getattr(msg, "self_id", "") or "")
        for comp in getattr(msg, "message", []) or []:
            if type(comp).__name__.lower() != "at":
                continue
            qq = str(getattr(comp, "qq", "") or "")
            if qq and (qq == self_id or qq == "qq_official"):
                return True
        return False

    async def _post_event(self, payload: dict[str, Any]) -> None:
        url = self._cfg("go_event_url", DEFAULT_EVENT_URL)
        headers = {}
        token = self._cfg("token", "")
        if token:
            headers["Authorization"] = f"Bearer {token}"

        retries = max(1, self._cfg_int("event_retry", 3))
        delay = 0.5
        last_error = ""
        for attempt in range(retries):
            try:
                async with self._session.post(
                    url,
                    json=payload,
                    headers=headers,
                    timeout=aiohttp.ClientTimeout(total=5),
                ) as resp:
                    body = await resp.text()
                    if resp.status < 500:
                        if resp.status >= 300:
                            logger.warning(
                                f"[onebot-bridge] Go 侧拒收事件 {resp.status}: {body[:200]}"
                            )
                        return
                    last_error = f"HTTP {resp.status}: {body[:200]}"
            except (aiohttp.ClientError, asyncio.TimeoutError) as exc:
                last_error = str(exc)
            if attempt < retries - 1:
                await asyncio.sleep(delay)
                delay *= 2

        logger.error(f"[onebot-bridge] 事件推送失败（已重试 {retries} 次）: {last_error}")

    # ---------------- 出站：Go -> 平台 ----------------

    async def _ensure_session(self) -> None:
        if self._session is None or self._session.closed:
            self._session = aiohttp.ClientSession()

    async def _ensure_server(self) -> None:
        if self._started:
            return
        async with self._start_lock:
            if self._started:
                return
            host = self._cfg("action_listen_host", DEFAULT_LISTEN_HOST)
            port = self._cfg_int("action_listen_port", DEFAULT_LISTEN_PORT)
            if not self._cfg("token", "") and host not in ("127.0.0.1", "localhost", "::1"):
                logger.warning(
                    "[onebot-bridge] 动作代理监听在非回环地址但没有配置 token，存在被本机外调用的风险"
                )

            app = web.Application()
            app.router.add_get("/health", self._handle_health)
            app.router.add_post("/{action}", self._handle_action)

            runner = web.AppRunner(app, access_log=None)
            await runner.setup()
            try:
                await web.TCPSite(runner, host, port).start()
            except Exception:
                await runner.cleanup()
                raise
            self._runner = runner
            self._started = True
            logger.info(f"[onebot-bridge] 动作代理已监听 http://{host}:{port}")

    async def _handle_health(self, _request: web.Request):
        adapter = self._get_adapter()
        return web.json_response({
            "status": "ok",
            "platform": type(adapter).__name__ if adapter is not None else None,
        })

    async def _handle_action(self, request: web.Request):
        if not self._authorized(request):
            return web.json_response(
                {"status": "failed", "retcode": 401, "message": "unauthorized"}, status=401
            )

        action = request.match_info["action"]
        # get_login_info 这类动作没有参数，Go 侧发的是空 body；不能直接 request.json()。
        raw_body = await request.read()
        try:
            params = json.loads(raw_body) if raw_body.strip() else {}
            if not isinstance(params, dict):
                raise ValueError("body must be a JSON object")
        except Exception as exc:
            return web.json_response(
                {"status": "failed", "retcode": 400, "message": f"invalid params: {exc}"},
                status=400,
            )

        timeout = max(1, self._cfg_int("action_timeout_sec", 10))
        try:
            if action in SEND_ACTIONS:
                result = await asyncio.wait_for(self._send_message(action, params), timeout=timeout)
            elif action in QUERY_ACTIONS:
                result = await asyncio.wait_for(self._query_action(action, params), timeout=timeout)
            else:
                result = {
                    "status": "failed",
                    "retcode": 1404,
                    "message": f"不支持的动作: {action}",
                }
        except asyncio.TimeoutError:
            return web.json_response({
                "status": "failed",
                "retcode": 408,
                "message": f"action {action} timeout after {timeout}s",
            })
        except Exception as exc:
            logger.warning(f"[onebot-bridge] action {action} 失败: {exc}")
            return web.json_response({
                "status": "failed",
                "retcode": getattr(exc, "retcode", 1400),
                "message": str(exc),
            })
        return web.json_response(result)

    async def _send_message(self, action: str, params: dict) -> dict:
        adapter = self._get_adapter()
        if adapter is None:
            return {
                "status": "failed",
                "retcode": 1404,
                "message": "没有已连接的消息平台适配器（先让 AstrBot 连上平台）",
            }

        text = self._extract_text(params.get("message", ""))
        if not text:
            return {"status": "failed", "retcode": 400, "message": "message 为空"}

        group_id = str(params.get("group_id") or "")
        user_id = str(params.get("user_id") or "")
        if action == "send_group_msg" or group_id:
            if not group_id:
                return {"status": "failed", "retcode": 400, "message": "缺少 group_id"}
            session_id = group_id
            message_type = MessageType.GROUP_MESSAGE
        else:
            if not user_id:
                return {"status": "failed", "retcode": 400, "message": "缺少 user_id"}
            session_id = user_id
            message_type = MessageType.FRIEND_MESSAGE

        # send_by_session 是各平台适配器自己的实现；qq_official 的群消息例外：
        # AstrBot 当前版本固定走主动推送（不带 msg_id），会被官方额度限制，
        # 所以这里用官方被动回复 API 自己发。私聊仍交给 send_by_session。
        if self._is_qqofficial(adapter) and message_type == MessageType.GROUP_MESSAGE:
            await self._send_qqofficial(adapter, session_id, text)
        else:
            session = SimpleNamespace(
                platform_id="",
                message_type=message_type,
                session_id=session_id,
            )
            await adapter.send_by_session(session, MessageChain(chain=[Plain(text=text)]))
        return {"status": "ok", "retcode": 0}

    @staticmethod
    def _is_qqofficial(adapter) -> bool:
        try:
            return str(adapter.meta().name).startswith("qq_official")
        except Exception:
            return type(adapter).__name__ == "QQOfficialPlatformAdapter"

    async def _send_qqofficial(self, adapter, session_id: str, text: str) -> None:
        client = adapter.get_client()
        api = getattr(client, "api", None)
        if api is None:
            raise RuntimeError("qq_official client 没有 api 对象")

        payload = {
            "content": text,
            "msg_type": 0,  # 0 = 纯文本
            "msg_seq": self._next_seq(session_id),
        }
        msg_id = self._last_msg_ids.get(session_id, "")
        if msg_id:
            # 带上最近一条入站消息 ID 才算被动回复；没有它会被当成主动推送。
            payload["msg_id"] = msg_id

        await api.post_group_message(group_openid=session_id, **payload)

    def _next_seq(self, session_id: str) -> int:
        seq = self._msg_seq.get(session_id, 0) + 1
        if seq > 10000:
            seq = 1
        self._msg_seq[session_id] = seq
        return seq

    async def _query_action(self, action: str, params: dict) -> dict:
        adapter = self._get_adapter()
        if adapter is None:
            return {"status": "failed", "retcode": 1404, "message": "没有已连接的消息平台适配器"}
        if self._is_aiocqhttp(adapter):
            return await self._call_onebot_action(adapter, action, params)
        return self._local_query(adapter, action, params)

    @staticmethod
    def _is_aiocqhttp(adapter) -> bool:
        # 不直接 import AiocqhttpAdapter：它不在 astrbot.api.platform 的导出里，
        # 不同版本的位置不一样；按适配器名判断更稳。
        try:
            return str(adapter.meta().name) == "aiocqhttp"
        except Exception:
            return type(adapter).__name__ == "AiocqhttpAdapter"

    def _local_query(self, adapter, action: str, params: dict) -> dict:
        if action == "get_login_info":
            name = self._self_name
            if not name:
                try:
                    robot = getattr(adapter.get_client(), "robot", None)
                    name = str(getattr(robot, "name", "") or "")
                except Exception:
                    name = ""
            if name:
                self._self_name = name
            return {
                "status": "ok",
                "retcode": 0,
                "data": {"user_id": _as_int(self._self_id), "nickname": name},
            }
        if action == "get_group_info":
            group_id = str(params.get("group_id") or "")
            return {
                "status": "ok",
                "retcode": 0,
                "data": {
                    "group_id": group_id,
                    "group_name": self._group_names.get(group_id, group_id),
                },
            }
        if action == "get_group_msg_history":
            return {
                "status": "failed",
                "retcode": 1404,
                "message": "该平台不提供群历史消息接口（QQ 官方机器人常见）",
            }
        return {"status": "failed", "retcode": 1404, "message": f"不支持的动作: {action}"}

    async def _call_onebot_action(self, adapter, action: str, params: dict) -> dict:
        client = adapter.get_client()
        # 不同 AstrBot 版本把 call_action 挂在 client.api 或 client 上，两个都试。
        call = None
        api = getattr(client, "api", None)
        if api is not None and hasattr(api, "call_action"):
            call = api.call_action
        elif hasattr(client, "call_action"):
            call = client.call_action
        if call is None:
            return {
                "status": "failed",
                "retcode": 1404,
                "message": "aiocqhttp client 不支持 call_action",
            }

        result = await call(action, **self._coerce_params(params))
        if isinstance(result, dict) and "retcode" in result:
            return result
        return {"status": "ok", "retcode": 0, "data": result}

    def _get_adapter(self):
        # 优先回到最近一条入站事件的平台实例：多平台共存时，
        # 回复必须发回消息来源，而不是"第一个匹配到的适配器"。
        if self._last_platform_id:
            try:
                adapter = self.context.get_platform_inst(self._last_platform_id)
            except Exception:
                adapter = None
            if adapter is not None:
                return adapter

        for platform_type in (
            getattr(filter.PlatformAdapterType, "AIOCQHTTP", None),
            getattr(filter.PlatformAdapterType, "QQOFFICIAL", None),
        ):
            if platform_type is None:
                continue
            try:
                adapter = self.context.get_platform(platform_type)
            except Exception:
                continue
            if adapter is not None:
                return adapter
        return None

    def _authorized(self, request: web.Request) -> bool:
        token = self._cfg("token", "")
        if not token:
            return True
        header = request.headers.get("Authorization", "")
        if not header.startswith("Bearer "):
            return False
        return hmac.compare_digest(header[len("Bearer "):], token)

    # ---------------- 工具 ----------------

    def _cfg(self, key: str, default):
        try:
            value = self.config.get(key, default)
        except Exception:
            return default
        return default if value is None else value

    def _cfg_int(self, key: str, default: int) -> int:
        try:
            return int(self._cfg(key, default))
        except (TypeError, ValueError):
            return default

    def _cfg_bool(self, key: str, default: bool) -> bool:
        value = self._cfg(key, default)
        if isinstance(value, bool):
            return value
        return str(value).strip().lower() in ("1", "true", "yes", "on")

    @staticmethod
    def _extract_text(message: Any) -> str:
        if isinstance(message, str):
            return message
        if isinstance(message, list):
            parts = []
            for seg in message:
                if isinstance(seg, dict) and seg.get("type") == "text":
                    parts.append(str((seg.get("data") or {}).get("text", "")))
            return "".join(parts)
        return str(message or "")

    @staticmethod
    def _coerce_params(params: dict) -> dict:
        out = dict(params)
        for key in ("group_id", "user_id", "message_id", "count"):
            value = out.get(key)
            if isinstance(value, str) and value.isdigit():
                out[key] = int(value)
        return out

    @staticmethod
    def _to_segments(chain) -> list[dict]:
        segments = []
        for comp in chain:
            name = type(comp).__name__.lower()
            if name == "plain":
                segments.append({"type": "text", "data": {"text": getattr(comp, "text", "")}})
            elif name == "at":
                segments.append({"type": "at", "data": {"qq": str(getattr(comp, "qq", ""))}})
            elif name == "image":
                segments.append({
                    "type": "image",
                    "data": {"url": getattr(comp, "url", "") or ""},
                })
        return segments

    @staticmethod
    def _to_cq_string(chain) -> str:
        """给非 aiocqhttp 平台生成 CQ 串，复用 Go 侧已有的 CQ 解析逻辑。"""
        parts = []
        for comp in chain:
            name = type(comp).__name__.lower()
            if name == "plain":
                parts.append(str(getattr(comp, "text", "")))
            elif name == "at":
                parts.append(f"[CQ:at,qq={getattr(comp, 'qq', '')}]")
            elif name == "image":
                url = getattr(comp, "url", "") or ""
                parts.append(f"[CQ:image,url={url}]" if url else "[CQ:image]")
        return "".join(parts)


def _as_int(value) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return 0


def _as_message_id(value) -> int:
    """把各平台消息 ID 归一成 Go 侧使用的 int64。

    QQ 官方机器人的消息 ID 是 ROBOT1.0_... 字符串，直接 int() 会得到 0，
    导致所有消息在 Go 的去重 map 里都变成同一条。这里对非数字 ID 做稳定哈希。
    """
    if isinstance(value, bool):
        return 0
    if isinstance(value, int):
        return value
    text = str(value or "").strip()
    if not text:
        return 0
    if text.isdigit():
        return int(text)
    digest = hashlib.sha256(text.encode("utf-8")).digest()
    return int.from_bytes(digest[:8], "big") & 0x7FFFFFFFFFFFFFFF
