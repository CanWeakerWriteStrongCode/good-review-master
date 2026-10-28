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
import base64
import hashlib
import hmac
import json
import os
import re
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

# QQ 官方媒体类型（与 botpy / AstrBot 的上传接口一致）
QQOFFICIAL_IMAGE = 1
QQOFFICIAL_VIDEO = 2
QQOFFICIAL_VOICE = 3
QQOFFICIAL_FILE = 4

# 入站消息 ID → 引用 ID（REFIDX）。QQ 官方机器人可见引用需要 REFIDX，
# 而 Go 侧拿到的是哈希后的 int64，必须在这里留一层映射。
REF_CACHE_MAX = 512

# AstrBot 在"机器人没有被 @"时会把 self_id 填成占位值；这些值不能当成真实身份。
SELF_ID_PLACEHOLDERS = {"", "qq_official", "unknown_selfid"}

# 每个群最多记住多少 openid → 昵称；群成员再多也只影响展示名，不影响功能。
MAX_MEMBERS_PER_GROUP = 2000

# QQ 官方的三种 @ 写法：botpy 原始标记、通用 <@id>、以及本插件生成的 CQ 码。
QQBOT_AT_RE = re.compile(r'<qqbot-at-user\s+id="([^"]+)"\s*/>')
ANGLE_AT_RE = re.compile(r"<@!?([^<>\s]+)>")
CQ_AT_RE = re.compile(r"\[CQ:at,qq=([^,\]]+)[^\]]*\]")


@register(
    PLUGIN_NAME,
    "good-review",
    "把 AstrBot 事件转发给外部 Go 服务，并把 OneBot 动作用作平台无关的发送/查询协议",
    "1.5.2",
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
        # 每个群的 openid → 展示名，用于把 <@openid> 还原成「@昵称(openid)」。
        self._members: dict[str, dict[str, str]] = {}
        self._group_names: dict[str, str] = {}
        self._seen_groups: set[str] = set()
        # 最近一条入站事件来自哪个平台实例。出站优先回到同一个实例，
        # 避免同时配置了多个平台时把回复发到错的平台。
        self._last_platform_id = ""
        # 每个会话最近一条入站消息 ID：qq_official 的被动回复必须带它。
        self._last_msg_ids: dict[str, str] = {}
        # 每个会话的 msg_seq 计数器（官方 API 要求同一 msg_id 下唯一）。
        self._msg_seq: dict[str, int] = {}
        # 入站消息 ID / 哈希 ID → REFIDX，供出站引用使用。
        self._msg_ref_idx: dict[str, str] = {}

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
                await self._post_event(payload, self._platform_label(event))
        except Exception as exc:  # 插件异常不能拖垮 AstrBot 的事件循环
            logger.error(f"[onebot-bridge] 转发事件失败: {exc}")
        finally:
            event.stop_event()

    def _build_payload(self, event: AstrMessageEvent) -> dict | None:
        msg = event.message_obj
        raw = getattr(msg, "raw_message", None)

        if isinstance(raw, dict) and raw.get("post_type") in ("message", "message_sent"):
            # aiocqhttp：AstrBot 的 OneBot 原始事件已经是目标格式，直接透传并补齐字段。
            payload = dict(raw)
            payload["sender"] = dict(raw.get("sender") or {})
            payload["raw_message"] = raw.get("raw_message") or getattr(msg, "message_str", "") or ""
        else:
            # 其他平台（含 QQ 官方机器人）：用 AstrBot 的标准化字段拼一个 OneBot 11 事件。
            # raw_message 在这里是 botpy 对象，不能透传；CQ 串由消息链重新生成，
            # 覆盖 Go 侧需要的 @、引用、附件语义。
            chain = getattr(msg, "message", []) or []
            segments = self._to_segments(chain)
            raw_message = self._to_cq_string(chain)
            # 部分适配器只在 raw_data.message_reference 里给引用，消息链里没有 Reply 组件。
            # 补一条标准 reply 段，Go 才能从 OneBot 标准字段里拿到 quote_id。
            quote_id = self._quote_id(msg)
            if quote_id and not any(seg["type"] == "reply" for seg in segments):
                hashed_quote = str(_as_message_id(quote_id))
                segments.insert(0, {"type": "reply", "data": {"id": hashed_quote}})
                raw_message = f"[CQ:reply,id={hashed_quote}]" + raw_message
            payload = {
                "post_type": "message",
                "message_type": "group" if getattr(msg, "group_id", "") else "private",
                "group_id": str(getattr(msg, "group_id", "") or ""),
                "user_id": str(getattr(msg.sender, "user_id", "")),
                "sender": {
                    "nickname": getattr(msg.sender, "nickname", "") or "",
                    "card": getattr(msg.sender, "card", "") or "",
                },
                "message": segments,
                "raw_message": raw_message,
                "message_id": getattr(msg, "message_id", ""),
                "self_id": str(getattr(msg, "self_id", "")),
                "time": int(getattr(msg, "timestamp", 0) or time.time()),
            }

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
        payload["time"] = int(payload.get("time") or time.time())
        # Go 侧 MessageID 是 int64：QQ 官方机器人的消息 ID 是 ROBOT1.0_... 这种字符串，
        # 哈希成一个稳定 int64，保证去重与锚点在重启后仍然成立。
        original_message_id = payload.get("message_id")
        payload["message_id"] = _as_message_id(original_message_id)
        # REFIDX 只留在插件内部：Go 引用回复时传来哈希后的 message_id，
        # 出站再由这里映射回 QQ 官方的 REFIDX，不进入事件字段。
        self._remember_ref(
            original_message_id,
            payload["message_id"],
            self._extract_ref_idx(msg),
        )

        # 先学习身份，再翻译 @：本条的发送者与 mentions 可能就是待解析的 openid。
        self._remember_identity(msg, event)
        if payload["self_id"] in SELF_ID_PLACEHOLDERS and self._self_id:
            payload["self_id"] = self._self_id
        # raw_message 保持平台原样（排障用）；display_message 是给缓存和大模型看的
        # 统一形态：@昵称(openid)，机器人自己也是同样的格式。
        payload["display_message"] = self._replace_mentions(
            str(payload.get("raw_message") or ""), msg, payload["group_id"]
        )
        payload["mentioned"] = self._is_mentioned(event, msg)

        if payload["user_id"] == payload["self_id"] and payload["self_id"]:
            if not self._cfg_bool("forward_self_messages", False):
                return None
        return payload

    def _platform_label(self, event: AstrMessageEvent) -> str:
        """平台名只放在 HTTP 头里：非 ASCII 值会让 aiohttp 拒绝发送。"""
        label = str(self._cfg("platform_label", "") or event.get_platform_name() or "")
        return label if label.isascii() else ""

    @staticmethod
    def _extract_ref_idx(msg) -> str:
        """取本条消息的 REFIDX（message_scene.ext 里的 msg_idx），用于可见引用。"""
        raw = getattr(msg, "raw_message", None)
        data = getattr(raw, "raw_data", None)
        if not isinstance(data, dict):
            return ""
        scene = data.get("message_scene")
        if not isinstance(scene, dict):
            return ""
        for item in scene.get("ext") or []:
            text = str(item)
            if text.startswith("msg_idx="):
                return text[len("msg_idx="):].strip()
        return ""

    def _remember_ref(self, original_id, hashed_id, ref_idx: str) -> None:
        if not ref_idx:
            return
        # ref_idx 自身也映射一次：Go 可能把 REFIDX 原样带回来，格式可能是数字也可能是字符串。
        for key in (original_id, hashed_id, ref_idx):
            key = str(key or "")
            if not key:
                continue
            self._msg_ref_idx[key] = ref_idx
        while len(self._msg_ref_idx) > REF_CACHE_MAX:
            self._msg_ref_idx.pop(next(iter(self._msg_ref_idx)), None)

    def _remember_identity(self, msg, event: AstrMessageEvent | None = None) -> None:
        # self_id 在机器人没被 @ 时是 "qq_official"/"unknown_selfid" 这类占位值，
        # 不能覆盖之前从真实 @ 事件里学到的 openid。
        self_id = str(getattr(msg, "self_id", "") or "")
        if self_id not in SELF_ID_PLACEHOLDERS:
            self._self_id = self_id

        if event is not None:
            try:
                platform_id = event.get_platform_id()
            except Exception:
                platform_id = ""
            if platform_id:
                self._last_platform_id = str(platform_id)

        group_id = str(getattr(msg, "group_id", "") or "")
        sender = getattr(msg, "sender", None)
        sender_id = str(getattr(sender, "user_id", "") or "")
        sender_name = self._display_name(
            getattr(sender, "card", ""), getattr(sender, "nickname", "")
        )
        if sender_id and sender_name:
            self._remember_member(group_id, sender_id, sender_name)

        # QQ 官方群消息的 mentions 同时带普通成员和机器人（is_you）；
        # 昵称改过也没关系，openid 始终不变。
        for mention in self._mentions(msg):
            mention_id = str(getattr(mention, "id", "") or "")
            mention_name = self._display_name(getattr(mention, "username", ""))
            if mention_id and mention_name:
                self._remember_member(group_id, mention_id, mention_name)
            if mention_id and bool(getattr(mention, "is_you", False)):
                self._self_id = mention_id
                if mention_name:
                    self._self_name = mention_name

        message_id = str(getattr(msg, "message_id", "") or "")
        session_id = str(getattr(msg, "session_id", "") or group_id)
        if message_id:
            # AstrBot 的 session_id 可能带平台前缀，而 Go 侧只回传 group_openid；
            # 两个键都记一份，确保被动回复窗口能命中。
            for key in (session_id, group_id):
                if key:
                    self._last_msg_ids[key] = message_id
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
    def _display_name(*values) -> str:
        for value in values:
            text = str(value or "").strip()
            if text:
                return text
        return ""

    def _remember_member(self, group_id: str, openid: str, name: str) -> None:
        if not group_id or not openid or not name:
            return
        members = self._members.setdefault(group_id, {})
        if openid in members or len(members) < MAX_MEMBERS_PER_GROUP:
            members[openid] = name

    @staticmethod
    def _mentions(msg) -> list:
        raw = getattr(msg, "raw_message", None)
        if isinstance(raw, dict):
            return list(raw.get("mentions") or [])
        return list(getattr(raw, "mentions", None) or [])

    def _format_mention(self, group_id: str, mention_id: str, is_self: bool) -> str:
        mention_id = str(mention_id or "").strip()
        if not mention_id:
            return "@未知用户"
        if mention_id.lower() == "all":
            return "@所有人"
        if is_self:
            name = self._self_name or "你"
        else:
            name = self._members.get(group_id, {}).get(mention_id, "用户")
        return f"@{name}({mention_id})"

    def _replace_mentions(self, text: str, msg, group_id: str) -> str:
        """把平台的各种 @ 写法统一成「@昵称(openid)」。

        raw_message 仍保留原样给排障；display_message 用这个结果，缓存和大模型都只读它。
        未知成员也保留完整 openid：昵称以后可以补，但标识不会变。
        """
        if not text:
            return text

        is_self_by_id: dict[str, bool] = {}
        for mention in self._mentions(msg):
            mention_id = str(getattr(mention, "id", "") or "")
            if mention_id:
                is_self_by_id[mention_id] = bool(getattr(mention, "is_you", False))

        def replace(match: re.Match) -> str:
            mention_id = str(match.group(1) or "")
            is_self = (
                is_self_by_id.get(mention_id, False)
                or (bool(self._self_id) and mention_id == self._self_id)
                # group_at 事件在拿不到 mentions 元数据时会用 qq_official 占位
                or mention_id == "qq_official"
            )
            return self._format_mention(group_id, mention_id, is_self)

        text = QQBOT_AT_RE.sub(replace, text)
        text = ANGLE_AT_RE.sub(replace, text)
        text = CQ_AT_RE.sub(replace, text)
        return text

    @staticmethod
    def _quote_id(msg) -> str:
        """取当前消息引用的那条消息的平台原始 ID（没有引用时为空）。

        只用于补标准 reply 段：Go 侧只需要哈希后的 message_id，
        被引用消息不在 chat log 里时就对不上，不需要额外内容。
        """
        chain = getattr(msg, "message", []) or []
        reply = next(
            (comp for comp in chain if type(comp).__name__.lower() == "reply"), None
        )
        if reply is not None and getattr(reply, "id", ""):
            return str(reply.id)

        raw = getattr(msg, "raw_message", None)
        ref = getattr(raw, "message_reference", None)
        if ref is not None and getattr(ref, "message_id", ""):
            return str(ref.message_id)
        if isinstance(raw, dict):
            for segment in raw.get("message") or []:
                if isinstance(segment, dict) and segment.get("type") == "reply":
                    quote_id = (segment.get("data") or {}).get("id", "")
                    if quote_id:
                        return str(quote_id)
        # 部分 AstrBot/qbotpy 组合只把原始数据放在 raw_data 里
        raw_data = getattr(raw, "raw_data", None)
        if isinstance(raw_data, dict):
            ref = raw_data.get("message_reference") or {}
            if isinstance(ref, dict) and ref.get("message_id"):
                return str(ref["message_id"])
        return ""

    def _is_mentioned(self, event: AstrMessageEvent, msg) -> bool:
        # AstrBot 的唤醒阶段已经把 At/AtAll/Reply/wake_prefix 算过了；
        # 这里再用消息链和原始文本兜底，覆盖不同适配器的 self_id 形式。
        if bool(getattr(event, "is_at_or_wake_command", False)):
            return True
        self_id = str(getattr(msg, "self_id", "") or "")
        if self_id in SELF_ID_PLACEHOLDERS:
            self_id = self._self_id
        for comp in getattr(msg, "message", []) or []:
            if type(comp).__name__.lower() != "at":
                continue
            qq = str(getattr(comp, "qq", "") or "")
            if qq and (qq == self_id or qq == "qq_official"):
                return True
        if self_id:
            raw = getattr(msg, "raw_message", None)
            if isinstance(raw, dict):
                content = str(raw.get("raw_message") or "")
            else:
                content = str(getattr(raw, "content", "") or "")
            if content and (
                f"<@{self_id}>" in content
                or f"<@!{self_id}>" in content
                or f'<qqbot-at-user id="{self_id}"' in content
            ):
                return True
        return False

    async def _post_event(self, payload: dict[str, Any], platform: str = "") -> None:
        url = self._cfg("go_event_url", DEFAULT_EVENT_URL)
        headers = {"Content-Type": "application/json"}
        if platform:
            # 平台名只是排障信息，放头里，不进 OneBot 事件字段。
            headers["X-Platform"] = platform
        token = self._cfg("token", "")
        if token:
            headers["Authorization"] = f"Bearer {token}"

        # ensure_ascii=False：否则中文会被转成 \uXXXX，Go 侧日志和抓包都不可读。
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")

        retries = max(1, self._cfg_int("event_retry", 3))
        delay = 0.5
        last_error = ""
        for attempt in range(retries):
            try:
                async with self._session.post(
                    url,
                    data=body,
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

        segments = self._normalize_segments(params.get("message", ""))
        if not segments:
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

        # QQ 官方群消息走官方被动回复 API：AstrBot 的 send_by_session 在部分版本里
        # 不保证带 msg_id，这里自己控制 msg_id / msg_seq / message_reference。
        if self._is_qqofficial(adapter) and message_type == MessageType.GROUP_MESSAGE:
            await self._send_qqofficial(adapter, session_id, segments)
            return {"status": "ok", "retcode": 0}

        # aiocqhttp 直接透传给协议端，保留引用、@、图片、文件等全部 OneBot 语义。
        if self._is_aiocqhttp(adapter):
            try:
                result = await self._send_via_onebot(adapter, action, group_id, user_id, segments)
                if result is not None:
                    return result
            except Exception as exc:
                logger.warning(f"[onebot-bridge] OneBot 透传发送失败，回退到 AstrBot: {exc}")

        await self._send_by_session(adapter, session_id, message_type, segments)
        return {"status": "ok", "retcode": 0}

    @staticmethod
    def _normalize_segments(message: Any) -> list[dict]:
        """把入参归一成 OneBot 11 消息段数组；字符串按纯文本处理。"""
        if isinstance(message, str):
            return [{"type": "text", "data": {"text": message}}] if message else []
        if isinstance(message, dict):
            message = [message]
        if not isinstance(message, list):
            return []

        segments: list[dict] = []
        for raw in message:
            if isinstance(raw, str):
                if raw:
                    segments.append({"type": "text", "data": {"text": raw}})
                continue
            if not isinstance(raw, dict):
                continue
            seg_type = str(raw.get("type") or "").strip().lower()
            if not seg_type:
                continue
            data = raw.get("data") or {}
            data = dict(data) if isinstance(data, dict) else {}
            if seg_type == "text" and not str(data.get("text", "")):
                continue
            segments.append({"type": seg_type, "data": data})
        return segments

    async def _send_via_onebot(
        self,
        adapter,
        action: str,
        group_id: str,
        user_id: str,
        segments: list[dict],
    ) -> dict | None:
        call = self._onebot_call(adapter)
        if call is None:
            return None

        params: dict[str, Any] = {"message": segments}
        if action == "send_group_msg" or group_id:
            if not group_id.isdigit():
                return None
            params["group_id"] = int(group_id)
        else:
            if not user_id.isdigit():
                return None
            params["user_id"] = int(user_id)

        # OneBot 11 没有 markdown 段，透传前降级成文本。
        params["message"] = [
            {"type": "text", "data": {"text": str((seg.get("data") or {}).get("text", ""))}}
            if seg.get("type") == "markdown"
            else seg
            for seg in segments
        ]

        result = await call(action, **params)
        if isinstance(result, dict) and "retcode" in result:
            return result
        return {"status": "ok", "retcode": 0, "data": result}

    async def _send_by_session(self, adapter, session_id: str, message_type, segments: list[dict]) -> None:
        chain = self._segments_to_message_chain(segments)
        if chain is None:
            raise ValueError("消息段无法转换为该平台可发送的消息")
        session = SimpleNamespace(
            platform_id="",
            message_type=message_type,
            session_id=session_id,
        )
        await adapter.send_by_session(session, chain)

    @staticmethod
    def _segments_to_message_chain(segments: list[dict]):
        """OneBot 段 → AstrBot MessageChain（非 QQ 官方平台用）。"""
        try:
            from astrbot.api.message_components import At, Face, File, Image, Record, Reply, Video
        except Exception as exc:
            logger.warning(f"[onebot-bridge] 加载 AstrBot 消息组件失败: {exc}")
            return None

        chain = []
        for seg in segments:
            seg_type = seg["type"]
            data = seg.get("data") or {}
            source = str(data.get("url") or data.get("file") or "")
            try:
                if seg_type == "text":
                    chain.append(Plain(text=str(data.get("text", ""))))
                elif seg_type == "at":
                    chain.append(At(qq=str(data.get("qq") or "")))
                elif seg_type == "reply":
                    chain.append(Reply(id=str(data.get("id") or "")))
                elif seg_type == "face":
                    chain.append(Face(id=_as_int(data.get("id"))))
                elif seg_type == "image":
                    chain.append(Image.fromURL(source) if source.startswith(("http://", "https://")) else Image(file=source))
                elif seg_type == "record":
                    chain.append(Record.fromURL(source) if source.startswith(("http://", "https://")) else Record(file=source))
                elif seg_type == "video":
                    chain.append(Video.fromURL(source) if source.startswith(("http://", "https://")) else Video(file=source))
                elif seg_type == "file":
                    name = str(data.get("name") or "file")
                    if source.startswith(("http://", "https://")):
                        chain.append(File(name=name, url=source))
                    else:
                        chain.append(File(name=name, file=source))
                elif seg_type == "markdown":
                    # AstrBot 消息链没有 markdown 组件，非官方平台按纯文本发送。
                    chain.append(Plain(text=str(data.get("text", ""))))
                else:
                    logger.warning(f"[onebot-bridge] 忽略不支持的消息段: {seg_type}")
            except Exception as exc:
                logger.warning(f"[onebot-bridge] 转换消息段 {seg_type} 失败: {exc}")
        if not chain:
            return None
        return MessageChain(chain=chain)

    @staticmethod
    def _is_qqofficial(adapter) -> bool:
        try:
            return str(adapter.meta().name).startswith("qq_official")
        except Exception:
            return type(adapter).__name__ == "QQOfficialPlatformAdapter"

    async def _send_qqofficial(self, adapter, group_id: str, segments: list[dict]) -> None:
        client = adapter.get_client()
        api = getattr(client, "api", None)
        if api is None:
            raise RuntimeError("qq_official client 没有 api 对象")

        # plain_parts：@ 用人名的纯文本版本（媒体消息和 markdown 被拒时使用）；
        # rich_parts：@ 用 <qqbot-at-user/> 标签，只有 markdown 消息才会渲染成真 @。
        plain_parts: list[str] = []
        rich_parts: list[str] = []
        markdown_parts: list[str] = []
        media: list[tuple[str, dict]] = []
        reply_id = ""
        has_at = False
        for seg in segments:
            seg_type = seg["type"]
            data = seg.get("data") or {}
            if seg_type == "text":
                plain_parts.append(str(data.get("text", "")))
                rich_parts.append(str(data.get("text", "")))
            elif seg_type == "markdown":
                markdown_parts.append(str(data.get("text", "")))
            elif seg_type == "at":
                qq = str(data.get("qq") or "")
                if qq and qq != "all":
                    name = self._members.get(group_id, {}).get(qq) or "用户"
                    plain_parts.append(f"@{name} ")
                    rich_parts.append(f'<qqbot-at-user id="{qq}" /> ')
                    has_at = True
            elif seg_type == "reply":
                reply_id = str(data.get("id") or "")
            elif seg_type in ("image", "record", "video", "file"):
                media.append((seg_type, data))
            elif seg_type == "face":
                plain_parts.append("[表情]")
                rich_parts.append("[表情]")
            else:
                logger.warning(f"[onebot-bridge] QQ 官方不支持的消息段: {seg_type}")

        content = "".join(plain_parts).strip()
        markdown = "".join(markdown_parts).strip()
        # QQ 官方的 @ 只在 markdown 消息里生效，<qqbot-at-user/> 放进 content 会被原样显示。
        if has_at and not markdown:
            markdown = "".join(rich_parts).strip()
        if not content and not markdown and not media:
            return

        ref_idx = self._resolve_ref_idx(reply_id)
        msg_id = self._last_msg_ids.get(group_id, "")
        payload = self._qqofficial_payload(content, markdown, ref_idx, msg_id)
        payload["msg_seq"] = self._next_seq(group_id)

        if not media:
            await self._post_group_message(api, group_id, payload, content or markdown)
            return

        for index, (seg_type, data) in enumerate(media):
            current = dict(payload)
            current["msg_seq"] = self._next_seq(group_id)
            if index > 0:
                current.pop("message_reference", None)
                current.pop("markdown", None)
                current["content"] = None
            try:
                media_obj = await self._upload_media(adapter, group_id, seg_type, data)
            except Exception as exc:
                logger.warning(f"[onebot-bridge] 上传 {seg_type} 失败: {exc}")
                media_obj = None

            if not media_obj:
                label = {"image": "图片", "record": "语音", "video": "视频", "file": "文件"}.get(seg_type, seg_type)
                fallback_text = (content + f" [{label}发送失败]").strip()
                fallback = {"content": fallback_text, "msg_type": 0, "msg_seq": self._next_seq(group_id)}
                if msg_id:
                    fallback["msg_id"] = msg_id
                await self._post_group_message(api, group_id, fallback, fallback_text)
                continue

            current["media"] = media_obj
            current["msg_type"] = 7
            current.pop("markdown", None)
            current["content"] = content if index == 0 and content else None
            if seg_type in ("video", "file"):
                # 与 AstrBot 官方适配器保持一致：文件/视频的被动回复支持不稳定。
                current.pop("msg_id", None)
            elif msg_id:
                current["msg_id"] = msg_id
            await self._post_group_message(api, group_id, current, content if index == 0 else "")

    @staticmethod
    def _qqofficial_payload(
        content: str,
        markdown: str,
        ref_idx: str,
        msg_id: str,
    ) -> dict:
        if markdown:
            try:
                from botpy.types.message import MarkdownPayload

                markdown_value: Any = MarkdownPayload(content=markdown)
            except Exception:
                markdown_value = {"content": markdown}
            payload: dict[str, Any] = {"markdown": markdown_value, "msg_type": 2}
        else:
            payload = {"content": content, "msg_type": 0}
        payload["msg_seq"] = 1
        if ref_idx:
            payload["message_reference"] = {"message_id": ref_idx}
        if msg_id:
            payload["msg_id"] = msg_id
        return payload

    def _resolve_ref_idx(self, reply_id: str) -> str:
        if not reply_id:
            return ""
        mapped = self._msg_ref_idx.get(reply_id)
        if mapped:
            return mapped
        # Go 侧回传哈希后的 int64 时查不到映射，不能把数字当 REFIDX 用。
        return "" if reply_id.isdigit() else reply_id

    async def _upload_media(self, adapter, group_id: str, seg_type: str, data: dict):
        source = str(data.get("url") or data.get("file") or "")
        if not source:
            raise ValueError("媒体段缺少 url/file")

        from astrbot.core.platform.sources.qqofficial.qqofficial_message_event import (
            QQOfficialMessageEvent,
        )

        helper = SimpleNamespace(bot=adapter.get_client())
        if seg_type == "image":
            image_base64 = await self._read_media_base64(source)
            return await QQOfficialMessageEvent.upload_group_and_c2c_image(
                helper, image_base64, QQOFFICIAL_IMAGE, group_openid=group_id
            )
        if seg_type == "record":
            silk_path = await self._to_silk(source)
            return await QQOfficialMessageEvent.upload_group_and_c2c_media(
                helper, silk_path or source, QQOFFICIAL_VOICE, group_openid=group_id
            )
        if seg_type == "video":
            return await QQOfficialMessageEvent.upload_group_and_c2c_media(
                helper, source, QQOFFICIAL_VIDEO, group_openid=group_id
            )
        name = str(data.get("name") or os.path.basename(source.split("?", 1)[0]) or "file")
        return await QQOfficialMessageEvent.upload_group_and_c2c_media(
            helper, source, QQOFFICIAL_FILE, file_name=name, group_openid=group_id
        )

    async def _read_media_base64(self, source: str) -> str:
        if source.startswith("base64://"):
            return source[len("base64://"):]
        if source.startswith("data:"):
            return source.split(",", 1)[-1]
        if source.startswith(("http://", "https://")):
            await self._ensure_session()
            async with self._session.get(source, timeout=aiohttp.ClientTimeout(total=30)) as resp:
                resp.raise_for_status()
                return base64.b64encode(await resp.read()).decode("utf-8")
        path = source
        if source.startswith("file://"):
            from astrbot.core.utils.media_utils import file_uri_to_path

            path = file_uri_to_path(source)
        with open(path, "rb") as handle:
            return base64.b64encode(handle.read()).decode("utf-8")

    async def _to_silk(self, source: str) -> str:
        try:
            from astrbot.core.utils.media_utils import MediaResolver

            return await MediaResolver(
                source, media_type="audio", default_suffix=".wav"
            ).to_path(target_format="tencent_silk")
        except Exception as exc:
            logger.warning(f"[onebot-bridge] 语音转 silk 失败，尝试直接上传: {exc}")
            return ""

    async def _post_group_message(self, api, group_id: str, payload: dict, plain_text: str):
        try:
            return await api.post_group_message(group_openid=group_id, **payload)
        except Exception as exc:
            retry = dict(payload)
            changed = False
            if retry.get("msg_id"):
                retry.pop("msg_id", None)
                changed = True
            if retry.get("message_reference"):
                retry.pop("message_reference", None)
                changed = True
            if retry.get("markdown") and plain_text:
                retry.pop("markdown", None)
                retry["content"] = plain_text
                retry["msg_type"] = 0
                changed = True
            if not changed:
                raise
            logger.info(f"[onebot-bridge] QQ 官方发送失败，降级重试: {exc}")
            return await api.post_group_message(group_openid=group_id, **retry)

    def _onebot_call(self, adapter):
        client = adapter.get_client()
        api = getattr(client, "api", None)
        if api is not None and hasattr(api, "call_action"):
            return api.call_action
        if hasattr(client, "call_action"):
            return client.call_action
        return None

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
            name = str(name or "").strip()
            if name:
                self._self_name = name
            return {
                "status": "ok",
                "retcode": 0,
                "data": {
                    "user_id": _as_int(self._self_id),
                    "nickname": name,
                    "openid": self._self_id,
                },
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
        # 不同 AstrBot 版本把 call_action 挂在 client.api 或 client 上，两个都试。
        call = self._onebot_call(adapter)
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
    def _cq_value(value: Any) -> str:
        """转义 CQ 码属性值，避免 url/name 里的逗号、方括号破坏分段。"""
        text = str(value or "")
        return (
            text.replace("&", "&amp;")
            .replace("[", "&#91;")
            .replace("]", "&#93;")
            .replace(",", "&#44;")
        )

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
            elif name in ("at", "atall"):
                qq = "all" if name == "atall" else str(getattr(comp, "qq", ""))
                segments.append({"type": "at", "data": {"qq": qq}})
            elif name == "reply":
                # Reply 带的是平台原始 ID；与入站 message_id 用同一个哈希，
                # Go 才能把 reply 段里的 id 直接当成 chat log 的 msg_id。
                segments.append({
                    "type": "reply",
                    "data": {"id": str(_as_message_id(getattr(comp, "id", "")))},
                })
            elif name == "image":
                source = getattr(comp, "url", "") or getattr(comp, "file", "") or ""
                segments.append({"type": "image", "data": {"url": source, "file": source}})
            elif name == "record":
                source = getattr(comp, "url", "") or getattr(comp, "file", "") or ""
                segments.append({"type": "record", "data": {"url": source, "file": source}})
            elif name == "video":
                source = getattr(comp, "url", "") or getattr(comp, "file", "") or ""
                segments.append({"type": "video", "data": {"url": source, "file": source}})
            elif name == "file":
                segments.append({
                    "type": "file",
                    "data": {
                        "name": str(getattr(comp, "name", "") or ""),
                        "url": str(getattr(comp, "url", "") or ""),
                        "file": str(getattr(comp, "file_", "") or ""),
                    },
                })
            elif name == "face":
                segments.append({"type": "face", "data": {"id": str(getattr(comp, "id", ""))}})
        return segments

    @staticmethod
    def _to_cq_string(chain) -> str:
        """给非 aiocqhttp 平台生成 CQ 串，复用 Go 侧已有的 CQ 解析逻辑。"""
        parts = []
        for comp in chain:
            name = type(comp).__name__.lower()
            if name == "plain":
                parts.append(str(getattr(comp, "text", "")))
            elif name in ("at", "atall"):
                qq = "all" if name == "atall" else getattr(comp, "qq", "")
                parts.append(f"[CQ:at,qq={OneBotBridge._cq_value(qq)}]")
            elif name == "reply":
                reply_id = _as_message_id(getattr(comp, "id", ""))
                parts.append(f"[CQ:reply,id={OneBotBridge._cq_value(reply_id)}]")
            elif name == "image":
                url = getattr(comp, "url", "") or getattr(comp, "file", "") or ""
                parts.append(
                    f"[CQ:image,url={OneBotBridge._cq_value(url)}]" if url else "[CQ:image]"
                )
            elif name == "record":
                url = getattr(comp, "url", "") or getattr(comp, "file", "") or ""
                parts.append(
                    f"[CQ:record,url={OneBotBridge._cq_value(url)}]" if url else "[CQ:record]"
                )
            elif name == "video":
                url = getattr(comp, "url", "") or getattr(comp, "file", "") or ""
                parts.append(
                    f"[CQ:video,url={OneBotBridge._cq_value(url)}]" if url else "[CQ:video]"
                )
            elif name == "file":
                file_name = OneBotBridge._cq_value(getattr(comp, "name", "") or "")
                url = str(getattr(comp, "url", "") or "")
                if not url:
                    local_ref = str(getattr(comp, "file_", "") or "")
                    if local_ref.startswith(("http://", "https://", "file://")):
                        url = local_ref
                parts.append(f"[CQ:file,name={file_name},url={OneBotBridge._cq_value(url)}]")
            elif name == "face":
                parts.append(f"[CQ:face,id={OneBotBridge._cq_value(getattr(comp, 'id', ''))}]")
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
