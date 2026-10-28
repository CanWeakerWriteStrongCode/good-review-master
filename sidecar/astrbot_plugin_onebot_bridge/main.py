"""AstrBot ↔ OneBot 12 桥接插件。

拓扑（sidecar）：

    QQ 官方机器人 / OneBot / 微信 / TG ── AstrBot 适配器 ── 本插件 ──HTTP──> Go 核心
                                                    <──HTTP───  动作请求

- 入站：把 AstrBot 收到的事件转成 OneBot 12 事件，POST 到 Go 的 /im/event/{account}；
  account 优先取事件所属平台实例的 AppID（一个进程挂多个 QQ 官方账号时各走各的）；
- 出站：监听一个本地 HTTP 端口，实现 OneBot 12 的 POST / 动作服务
  （send_message / get_self_info / get_group_info / get_group_member_info / ...）。
  aiocqhttp 走 AstrBot 的 send_by_session；qq_official 单独走官方被动回复 API
  （带 msg_id / msg_seq），否则会被当成主动推送受额度限制；
- 事件转发后，仅在 Go 返回 2xx（已接管）时调用 stop_event()，
  4xx（未接管）交给 AstrBot 自己的 Agent 兜底。

部署要求：AstrBot 只当 IM 网关，它的 LLM Provider、人格、白名单等功能不要启用，
业务过滤（白名单、@ 检测）全部在 Go 侧完成。
"""

from __future__ import annotations

import asyncio
import base64
import hmac
import json
import os
import re
import time
import uuid
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

ONEBOT_VERSION = "12"
IMPL_NAME = "astrbot-plugin-onebot-bridge"
PLUGIN_VERSION = "1.7.4"

# AstrBot 适配器类型 → OB12 平台名。OB12 平台名不允许下划线，
# 所以 qq_official 这类适配器名必须显式映射；其它（wecom/lark/telegram…）
# 本身就是合法平台名，原样使用。
PLATFORM_ALIASES = {
    "qq_official": "qq",
}

QUERY_ACTIONS = (
    "get_self_info",
    "get_group_info",
    "get_group_member_info",
    "get_user_info",
    "get_version",
    "get_supported_actions",
)
SUPPORTED_ACTIONS = ("send_message",) + QUERY_ACTIONS

# QQ 官方媒体类型（与 botpy / AstrBot 的上传接口一致）
QQOFFICIAL_IMAGE = 1
QQOFFICIAL_VIDEO = 2
QQOFFICIAL_VOICE = 3
QQOFFICIAL_FILE = 4

# 入站消息 ID → 引用 ID（REFIDX）。QQ 官方机器人可见引用需要 REFIDX，
# 而 Go 侧拿到的是哈希后的 int64，必须在这里留一层映射。
REF_CACHE_MAX = 512

# AstrBot 在拿不到本账号 openid 时会把 self_id 填成占位值；这些值不能当成真实身份。
# 常见根因是QQ 群设置里没给机器人「查看群所有信息」权限：QQ 不下发 mentions，
# 适配器只能补占位 At。补上权限后 mentions[].is_you 会带真实 openid（本机实测）。
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
    "把 AstrBot 事件以 OneBot 12 转发给外部 Go 服务，并实现 OneBot 12 动作服务",
    "1.7.4",
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
        # 多账号部署时身份必须按平台实例分开：两个账号可能同昵称、同群同时收消息。
        self._self_ids: dict[str, str] = {}     # platform_id -> 机器人稳定 ID
        self._self_names: dict[str, str] = {}   # platform_id -> 机器人昵称
        # (platform_id, group_id) -> 本群可比较的机器人 user_id。
        # QQ 官方的 openid 按群分配，不能跨群复用；没学到时发哨兵，Go 不触发回复。
        self._self_by_group: dict[str, str] = {}
        # 每个群的 openid → 展示名，用于把 <@openid> 还原成「@昵称(openid)」。
        self._members: dict[str, dict[str, str]] = {}
        self._group_names: dict[str, str] = {}
        self._seen_groups: set[str] = set()
        # 最近一条入站事件来自哪个平台实例；没有更精确信息时用它兜底。
        self._last_platform_id = ""
        # 出站路由：self_id → 平台实例是首选（同群多账号也能选对），
        # group_id → 平台实例只在拿不到 self_id 时兜底。
        self._self_platform: dict[str, str] = {}
        self._group_platform: dict[str, str] = {}
        # 每个会话最近一条入站消息 ID：qq_official 的被动回复必须带它。
        self._last_msg_ids: dict[str, str] = {}
        # 每个会话的 msg_seq 计数器（官方 API 要求同一 msg_id 下唯一）。
        self._msg_seq: dict[str, int] = {}
        # 入站消息 ID / 哈希 ID → REFIDX，供出站引用使用。
        self._msg_ref_idx: dict[str, str] = {}
        # 每个平台实例只记一次 mention 元数据样本，用于排查 QQ 群消息是否带 is_you。
        self._mention_diag_logged: set[str] = set()

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
    # is_wake；用 `.*` 放行全部消息，转发成功后再 stop_event() 阻止 AstrBot 自己调 LLM。
    @filter.event_message_type(filter.EventMessageType.ALL)
    @filter.regex(r".*", priority=100)
    async def forward_event(self, event: AstrMessageEvent):
        # 只有 Go 真正接管了这条消息才 stop_event()。
        # OB12 约定：2xx = 已接管；4xx（未授权/账号不归 Go）交给 AstrBot 自己的 Agent。
        stop = False
        try:
            msg = event.message_obj
            group_id = str(getattr(msg, "group_id", "") or "")
            if not group_id:
                # 私聊 / WebChat 不归 Go 管：不转发也不拦，交给 AstrBot 自己的 Agent。
                return
            await self._ensure_session()
            try:
                await self._ensure_server()
            except Exception as exc:
                # 动作服务起不来只影响"Go 发消息"，不该影响入站转发。
                logger.error(f"[onebot-bridge] 动作服务启动失败: {exc}")
            self._log_mention_diag(event, msg)
            payload = self._build_payload(event)
            if payload is None:
                # 机器人自己发的消息：不转发，也不让 AstrBot 再回复一遍。
                stop = True
            else:
                account = self._account_for(self._platform_id(event))
                if not account:
                    logger.error(
                        "[onebot-bridge] 无法确定接收账号：QQ 官方应从平台实例读到 AppID；"
                        "其它平台请在插件配置里填 account"
                    )
                    return
                base = str(self._cfg("go_event_url", DEFAULT_EVENT_URL) or DEFAULT_EVENT_URL)
                stop = await self._post_event(f"{base.rstrip('/')}/{account}", payload)
        except Exception as exc:  # 插件异常不能拖垮 AstrBot 的事件循环
            logger.error(f"[onebot-bridge] 转发事件失败: {exc}")
        finally:
            if stop:
                event.stop_event()

    def _build_payload(self, event: AstrMessageEvent) -> dict | None:
        """把一条 AstrBot 群消息转成 OneBot 12 事件。

        事件体只用标准字段（id/time/type/detail_type/sub_type/self + 群消息字段），
        没有任何私有路由字段：接收账号由上报路径表达，平台由 self.platform 表达。
        """
        msg = event.message_obj
        platform_id = self._platform_id(event)
        group_id = str(getattr(msg, "group_id", "") or "")
        chain = getattr(msg, "message", []) or []

        # 先学习身份与成员名，再决定 self.user_id 和渲染 @昵称(ID)。
        self._remember_identity(msg, event)
        self_id = self._ob12_self_user_id(platform_id, group_id, msg)

        segments = self._to_ob12_segments(chain)
        raw_message_id = str(getattr(msg, "message_id", "") or "")
        message_id = raw_message_id or f"local-{uuid.uuid4().hex}"
        timestamp = float(getattr(msg, "timestamp", 0) or time.time())

        payload = {
            "id": message_id,
            "time": timestamp,
            "type": "message",
            "detail_type": "group",
            "sub_type": "",
            "self": {"platform": self._ob12_platform(platform_id), "user_id": self_id},
            "message_id": message_id,
            "message": segments,
            "alt_message": self._render_ob12(segments, group_id),
            "group_id": group_id,
            "user_id": str(getattr(getattr(msg, "sender", None), "user_id", "") or ""),
        }
        # 群内没给机器人「查看群所有信息」权限时，QQ 的群 @ 事件只给占位 At、
        # mentions 为空（本机 A 账号实测）；补上权限后 mentions[].is_you 会带真实
        # openid，这里自然就不生效。兜底用带前缀的扩展字段显式报告“这是 @ 本账号”，
        # 不伪造 self.user_id。
        if self._placeholder_at_me(msg):
            payload["qq.at_me"] = True

        # Go 回复时会把 message_id 原样带回，这里顺手记下 REFIDX 供可见引用使用。
        self._remember_ref(raw_message_id, message_id, self._extract_ref_idx(msg))

        if not self._cfg_bool("forward_self_messages", False):
            raw = getattr(msg, "raw_message", None)
            if isinstance(raw, dict) and raw.get("post_type") == "message_sent":
                return None
            if self_id and payload["user_id"] == self_id:
                return None
        return payload

    def _ob12_platform(self, platform_id: str = "") -> str:
        """事件 self.platform：优先按平台实例自动识别，配置值只作兜底。

        平台名必须匹配 [a-z][-a-z0-9]*(\.[-a-z0-9]+)*，下划线不合法；
        AstrBot 的 qq_official 通过 PLATFORM_ALIASES 映射成 qq。
        """
        candidates = (self._platform_type(platform_id), str(self._cfg("platform", "") or ""))
        for candidate in candidates:
            value = PLATFORM_ALIASES.get(candidate.strip().lower(), candidate.strip().lower())
            value = re.sub(r"[^a-z0-9.-]", "-", value)
            if re.match(r"^[a-z][-a-z0-9]*(\.[-a-z0-9]+)*$", value):
                return value
        logger.warning(
            "[onebot-bridge] 无法识别平台实例 %s 的平台类型，临时使用 unknown",
            platform_id or "<empty>",
        )
        return "unknown"

    def _platform_type(self, platform_id: str) -> str:
        """平台适配器类型，如 qq_official / wecom / lark；拿不到返回空串。"""
        if not platform_id:
            return ""
        try:
            inst = self.context.get_platform_inst(platform_id)
        except Exception:
            return ""
        if inst is None:
            return ""
        meta = getattr(inst, "meta", None)
        if callable(meta):
            try:
                name = str(getattr(meta(), "name", "") or "")
                if name:
                    return name
            except Exception:
                pass
        return str(getattr(inst, "name", "") or "")

    def _ob12_self_user_id(self, platform_id: str, group_id: str, msg) -> str:
        """本群可用于和 mention.user_id 比较的机器人 ID；学不到时发哨兵。

        QQ 官方的 openid 按群分配，绝不能拿 AppID / robot.id 去填：
        它们和 mention 里的 openid 不是同一个命名空间，填进去只会伪造匹配。
        """
        key = self._account_key(platform_id, group_id)
        learned = self._self_by_group.get(key, "")
        if learned:
            return learned
        self_id = str(getattr(msg, "self_id", "") or "")
        if self_id not in SELF_ID_PLACEHOLDERS:
            return self_id
        return "unknown_selfid"

    @staticmethod
    def _to_ob12_segments(chain) -> list[dict]:
        segments: list[dict] = []
        for comp in chain:
            name = type(comp).__name__.lower()
            if name == "plain":
                text = str(getattr(comp, "text", "") or "")
                if text:
                    segments.append({"type": "text", "data": {"text": text}})
            elif name == "atall":
                segments.append({"type": "mention_all", "data": {}})
            elif name == "at":
                qq = str(getattr(comp, "qq", "") or "")
                # 占位 At 不是任何人：它是权限缺失时“@ 了本账号”的替身，由 qq.at_me 表达。
                # 当成 mention 上报，Go 会渲染出「@用户(qq_official)」写进聊天记录，
                # 模型再照抄进回复。这里直接丢弃。
                if qq and qq not in SELF_ID_PLACEHOLDERS:
                    segments.append({"type": "mention", "data": {"user_id": qq}})
            elif name == "reply":
                # Go 侧缓存/锚点用同一条 message_id，引用关系因此能直接落地。
                segments.append({
                    "type": "reply",
                    "data": {"message_id": str(getattr(comp, "id", "") or "")},
                })
            elif name == "image":
                url = str(getattr(comp, "url", "") or getattr(comp, "file", "") or "")
                if url:
                    segments.append({"type": "image", "data": {"qq.url": url}})
            elif name == "record":
                url = str(getattr(comp, "url", "") or getattr(comp, "file", "") or "")
                if url:
                    segments.append({"type": "voice", "data": {"qq.url": url}})
            elif name == "video":
                url = str(getattr(comp, "url", "") or getattr(comp, "file", "") or "")
                if url:
                    segments.append({"type": "video", "data": {"qq.url": url}})
            elif name == "file":
                url = str(getattr(comp, "url", "") or getattr(comp, "file_", "") or "")
                segments.append({
                    "type": "file",
                    "data": {
                        "qq.name": str(getattr(comp, "name", "") or ""),
                        "qq.url": url,
                    },
                })
            elif name == "face":
                segments.append({"type": "face", "data": {}})
        return segments

    def _render_ob12(self, segments: list[dict], group_id: str) -> str:
        """alt_message：@ 统一成「@昵称(ID)」，媒体用占位符。

        保留括号里的 openid：昵称会变、也可能重名，模型要靠它判断是不是同一个人。
        """
        parts: list[str] = []
        for seg in segments:
            seg_type = seg.get("type")
            data = seg.get("data") or {}
            if seg_type == "text":
                parts.append(str(data.get("text", "")))
            elif seg_type == "mention":
                user_id = str(data.get("user_id", "") or "")
                name = self._members.get(group_id, {}).get(user_id, "") or "用户"
                parts.append(f"@{name}({user_id})")
            elif seg_type == "mention_all":
                parts.append("@所有人")
            elif seg_type == "image":
                parts.append("[图片]")
            elif seg_type == "voice":
                parts.append("[语音]")
            elif seg_type == "video":
                parts.append("[视频]")
            elif seg_type == "file":
                parts.append("[文件]")
            elif seg_type == "face":
                parts.append("[表情]")
        return "".join(parts).strip()

    @staticmethod
    def _platform_id(event) -> str:
        try:
            return str(event.get_platform_id() or "")
        except Exception:
            return ""

    @staticmethod
    def _placeholder_at_me(msg) -> bool:
        """AstrBot 用占位 At 表示群 @，但没给可比较的 openid。

        权限缺失（QQ 群设置里没给机器人「查看群所有信息」）时 QQ 不下发 mentions，
        适配器只能补占位 At；补上权限后 mentions[].is_you 带真实 openid，不再走这里。
        只在 self_id 也是占位值时成立；有真实 self_id 时走标准 mention 匹配。
        """
        self_id = str(getattr(msg, "self_id", "") or "")
        if self_id not in ("qq_official", "unknown_selfid"):
            return False
        for comp in getattr(msg, "message", []) or []:
            if type(comp).__name__.lower() != "at":
                continue
            if str(getattr(comp, "qq", "") or "") in ("qq_official", "unknown_selfid"):
                return True
        return False

    def _log_mention_diag(self, event, msg) -> None:
        """临时诊断：占位 @ 事件到底有没有 mentions / 真实 openid。

        只在“占位 self + 占位 At”时打印，避免普通群消息和唤醒前缀刷屏。
        at_wake 是 AstrBot 的唤醒判断（含 @ 与唤醒前缀），不作为 @ 证据、也不触发。
        确认 QQ 官方的 mentions 行为后可删除。
        """
        if not self._placeholder_at_me(msg):
            return
        chain = getattr(msg, "message", []) or []
        self_id = str(getattr(msg, "self_id", "") or "")

        raw = getattr(msg, "raw_message", None)
        mentions = []
        for mention in (getattr(raw, "mentions", None) or []):
            mentions.append({
                "id": str(getattr(mention, "id", "") or ""),
                "is_you": getattr(mention, "is_you", None),
                "username": str(getattr(mention, "username", "") or ""),
            })
        chain_dump = []
        for comp in chain:
            item = {"type": type(comp).__name__}
            if item["type"].lower() == "at":
                item["qq"] = str(getattr(comp, "qq", "") or "")
            text = getattr(comp, "text", None)
            if text is not None:
                item["text"] = str(text)[:200]
            chain_dump.append(item)
        raw_data = getattr(raw, "raw_data", None)
        raw_summary = {}
        if isinstance(raw_data, dict):
            for key in ("id", "content", "mentions", "message_scene", "group_openid"):
                if key in raw_data:
                    raw_summary[key] = raw_data[key]
        logger.info(
            "[onebot-bridge][diag] platform_id=%s self_id=%r "
            "mentions=%s chain=%s raw=%s",
            self._platform_id(event),
            self_id,
            json.dumps(mentions, ensure_ascii=False, default=str),
            json.dumps(chain_dump, ensure_ascii=False, default=str),
            json.dumps(raw_summary, ensure_ascii=False, default=str),
        )

    def _account_for(self, platform_id: str) -> str:
        """入站上报路径里的账号：优先平台实例的 AppID，取不到才用配置兜底。

        一个 AstrBot 进程可能挂多个 QQ 官方实例，静态 account 只能填一个值；
        QQ 官方适配器保存了 appid，按事件所属实例取才不会把两个账号混成一个。
        """
        appid = self._platform_appid(platform_id)
        if appid:
            return appid
        return str(self._cfg("account", "") or "").strip()

    def _platform_appid(self, platform_id: str) -> str:
        if not platform_id:
            return ""
        try:
            inst = self.context.get_platform_inst(platform_id)
        except Exception:
            return ""
        if inst is None:
            return ""
        appid = getattr(inst, "appid", "")
        if appid:
            return str(appid)
        # 非 QQ 官方适配器没有 appid 时，退到 botpy 的 robot.id（若已 READY）
        robot = getattr(getattr(inst, "client", None), "robot", None)
        return str(getattr(robot, "id", "") or "")

    def _self_id_for(self, platform_id: str) -> str:
        return self._self_ids.get(platform_id, "")

    def _self_name_for(self, platform_id: str) -> str:
        return self._self_names.get(platform_id, "")

    def _platform_for_action(self, self_id: str, group_id: str) -> str:
        """出站选账号：self_id 最精确，其次按群，最后用最近一次入站事件的平台。"""
        if self_id:
            mapped = self._self_platform.get(self_id, "")
            if mapped:
                return mapped
        if group_id:
            mapped = self._group_platform.get(group_id, "")
            if mapped:
                return mapped
        configured = str(self._cfg("platform_id", "") or "")
        if configured:
            return configured
        return self._last_platform_id

    @staticmethod
    def _account_key(platform_id: str, session_id: str) -> str:
        """被动回复窗口/msg_seq 按 (账号, 会话) 分桶，避免同群多账号互相覆盖。"""
        return f"{platform_id}\x00{session_id}"

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
        platform_id = self._platform_id(event) if event is not None else ""
        if platform_id:
            self._last_platform_id = platform_id

        group_id = str(getattr(msg, "group_id", "") or "")
        if group_id and platform_id:
            # 出站动作只带 group_id；两个账号同群时 self_id 更可靠，这里只做兜底。
            self._group_platform[group_id] = platform_id

        # self_id 在机器人没被 @ 时是 "qq_official"/"unknown_selfid" 这类占位值，
        # 不能覆盖之前从真实 @ 事件里学到的 openid。
        self_id = str(getattr(msg, "self_id", "") or "")
        if self_id not in SELF_ID_PLACEHOLDERS:
            self._self_ids[platform_id] = self_id
            if platform_id:
                self._self_platform[self_id] = platform_id
                if group_id:
                    self._self_by_group[self._account_key(platform_id, group_id)] = self_id

        sender = getattr(msg, "sender", None)
        sender_id = str(getattr(sender, "user_id", "") or "")
        sender_name = self._display_name(
            getattr(sender, "card", ""), getattr(sender, "nickname", "")
        )
        if sender_id and sender_name:
            self._remember_member(group_id, sender_id, sender_name)

        # QQ 官方群消息的 mentions 同时带普通成员和机器人（is_you）；
        # 昵称改过也没关系，openid 始终不变。身份按平台实例分别记，避免两个账号互相覆盖。
        for mention in self._mentions(msg):
            mention_id = str(getattr(mention, "id", "") or "")
            mention_name = self._display_name(getattr(mention, "username", ""))
            if mention_id and mention_name:
                self._remember_member(group_id, mention_id, mention_name)
            if mention_id and bool(getattr(mention, "is_you", False)):
                self._self_ids[platform_id] = mention_id
                if platform_id:
                    self._self_platform[mention_id] = platform_id
                    if group_id:
                        self._self_by_group[self._account_key(platform_id, group_id)] = mention_id
                if mention_name:
                    self._self_names[platform_id] = mention_name

        # 原始 mentions 兜底：群消息里 id/is_you 有时只存在于 raw_data，
        # botpy 对象上没有；两个来源都学一遍，多账号下才不会漏掉自己的身份。
        for item in self._raw_mention_items(msg):
            mention_id = str(item.get("id") or item.get("member_openid") or "")
            mention_name = self._display_name(item.get("username"))
            if mention_id and mention_name:
                self._remember_member(group_id, mention_id, mention_name)
            if mention_id and item.get("is_you") is True:
                self._self_ids[platform_id] = mention_id
                if platform_id:
                    self._self_platform[mention_id] = platform_id
                    if group_id:
                        self._self_by_group[self._account_key(platform_id, group_id)] = mention_id
                if mention_name:
                    self._self_names[platform_id] = mention_name

        message_id = str(getattr(msg, "message_id", "") or "")
        session_id = str(getattr(msg, "session_id", "") or group_id)
        if message_id:
            # AstrBot 的 session_id 可能带平台前缀，而 Go 侧只回传 group_openid；
            # 多个键都记一份，确保被动回复窗口能命中。key 里带平台实例，
            # 两个账号同群时各自的 msg_id 不会互相覆盖。
            for key in (
                self._account_key(platform_id, session_id),
                self._account_key(platform_id, group_id),
            ):
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

    @staticmethod
    def _raw_mention_items(msg) -> list[dict]:
        """从 botpy 原始数据里取 mentions。

        部分 AstrBot/botpy 组合会把 mentions 里的 is_you / member_openid 丢掉，
        只保留 id/username；这里直接读 raw_data，补回判断「是不是在 @ 本账号」的依据。
        """
        raw = getattr(msg, "raw_message", None)
        data = getattr(raw, "raw_data", None)
        if not isinstance(data, dict):
            return []
        items = data.get("mentions")
        if not isinstance(items, list):
            return []
        return [item for item in items if isinstance(item, dict)]

    async def _post_event(self, url: str, payload: dict[str, Any]) -> bool:
        """推送一条 OB12 事件；返回 True 表示 Go 已接管（调用方应 stop_event）。"""
        headers = {
            "Content-Type": "application/json",
            "X-OneBot-Version": ONEBOT_VERSION,
            "X-Impl": IMPL_NAME,
        }
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
                    text = await resp.text()
                    if resp.status < 300:
                        # OB12：204/200 = 推送成功（本项目的部署约定里 = Go 已接管）。
                        return True
                    if resp.status < 500:
                        # 400/401/404 = 未被接管（账号非法、鉴权失败、名单拒绝），
                        # 不能拦，AstrBot 自己的 Agent 要能兜底。
                        logger.debug(
                            f"[onebot-bridge] Go 未接管事件（{resp.status}），交给 AstrBot: {text[:200]}"
                        )
                        return False
                    last_error = f"HTTP {resp.status}: {text[:200]}"
            except (aiohttp.ClientError, asyncio.TimeoutError) as exc:
                last_error = str(exc)
            if attempt < retries - 1:
                await asyncio.sleep(delay)
                delay *= 2

        logger.error(f"[onebot-bridge] 事件推送失败（已重试 {retries} 次）: {last_error}")
        return False

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
            # OB12 的 HTTP API 统一入口是 POST /（action 在 body 里）。
            # 同时保留 /{action} 便于人工 curl 调试。
            app.router.add_post("/", self._handle_action)
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

        raw_body = await request.read()
        try:
            body = json.loads(raw_body) if raw_body.strip() else {}
            if not isinstance(body, dict):
                raise ValueError("body must be a JSON object")
        except Exception as exc:
            return web.json_response(
                {"status": "failed", "retcode": 400, "message": f"invalid request: {exc}"},
                status=400,
            )

        if "action" in body:
            action = str(body.get("action") or "")
            params = body.get("params") or {}
            echo = body.get("echo")
        else:
            # 兼容 /{action} 的调试写法：path 决定动作，body 直接是 params。
            action = str(request.match_info.get("action") or "")
            params = body
            echo = None
        if not isinstance(params, dict):
            return web.json_response({
                "status": "failed", "retcode": 400, "message": "params must be an object", "echo": echo,
            })

        timeout = max(1, self._cfg_int("action_timeout_sec", 10))
        try:
            if action == "send_message":
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
            result = {
                "status": "failed",
                "retcode": 408,
                "message": f"action {action} timeout after {timeout}s",
            }
        except Exception as exc:
            logger.warning(f"[onebot-bridge] action {action} 失败: {exc}")
            result = {
                "status": "failed",
                "retcode": getattr(exc, "retcode", 1400),
                "message": str(exc),
            }

        result.setdefault("status", "ok")
        result.setdefault("retcode", 0)
        result["echo"] = echo
        return web.json_response(result)

    async def _send_message(self, action: str, params: dict) -> dict:
        segments = self._normalize_segments(params.get("message", ""))
        if not segments:
            return {"status": "failed", "retcode": 400, "message": "message 为空"}
        # 平台发送链路（官方被动回复 / send_by_session）仍是 OneBot 11 语义，
        # 在这里统一翻译一次，后面的分支都拿到同一份段。
        segments = [self._ob12_segment_to_ob11(seg) for seg in segments]
        if action == "send_message":
            detail_type = str(params.get("detail_type") or "")
            if detail_type and detail_type != "group":
                return {
                    "status": "failed",
                    "retcode": 1404,
                    "message": f"暂不支持 detail_type={detail_type}（当前只有群消息）",
                }
            action = "send_group_msg"

        group_id = str(params.get("group_id") or "")
        user_id = str(params.get("user_id") or "")
        self_id = str(params.get("self_id") or "")
        adapter = self._get_adapter(group_id, self_id)
        if adapter is None:
            return {
                "status": "failed",
                "retcode": 1404,
                "message": "没有已连接的消息平台适配器（先让 AstrBot 连上平台）",
            }

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
            await self._send_qqofficial(adapter, session_id, segments, self_id)
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

    @staticmethod
    def _ob12_segment_to_ob11(seg: dict) -> dict:
        """OB12 段 → OneBot 11 段（aiocqhttp 透传用）。不认识的段降级为文本。"""
        seg_type = str(seg.get("type") or "")
        data = dict(seg.get("data") or {})
        if seg_type == "mention":
            return {"type": "at", "data": {"qq": str(data.get("user_id", ""))}}
        if seg_type == "mention_all":
            return {"type": "at", "data": {"qq": "all"}}
        if seg_type == "reply":
            return {"type": "reply", "data": {"id": str(data.get("message_id", ""))}}
        if seg_type == "voice":
            return {"type": "record", "data": {"url": str(data.get("qq.url", ""))}}
        if seg_type == "image":
            url = str(data.get("qq.url", "") or data.get("url", ""))
            return {"type": "image", "data": {"url": url, "file": url}}
        if seg_type == "video":
            url = str(data.get("qq.url", "") or data.get("url", ""))
            return {"type": "video", "data": {"url": url, "file": url}}
        if seg_type == "file":
            url = str(data.get("qq.url", "") or data.get("url", ""))
            return {"type": "file", "data": {"name": str(data.get("qq.name", "")), "url": url, "file": url}}
        if seg_type == "text":
            return seg
        return {"type": "text", "data": {"text": str(data.get("text", ""))}}

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

        params["message"] = segments

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

    async def _send_qqofficial(self, adapter, group_id: str, segments: list[dict], self_id: str = "") -> None:
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
        platform_id = self._platform_for_action(self_id, group_id)
        account_key = self._account_key(platform_id, group_id)
        msg_id = self._last_msg_ids.get(account_key) or self._last_msg_ids.get(
            self._account_key("", group_id), ""
        )
        payload = self._qqofficial_payload(content, markdown, ref_idx, msg_id)
        payload["msg_seq"] = self._next_seq(account_key)

        if not media:
            await self._post_group_message(api, group_id, payload, content or markdown)
            return

        for index, (seg_type, data) in enumerate(media):
            current = dict(payload)
            current["msg_seq"] = self._next_seq(account_key)
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
                fallback = {"content": fallback_text, "msg_type": 0, "msg_seq": self._next_seq(account_key)}
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
        group_id = str(params.get("group_id") or "")
        self_id = str(params.get("self_id") or "")
        platform_id = self._platform_for_action(self_id, group_id)
        adapter = self._get_adapter(group_id, self_id)

        if action in ("get_group_member_info", "get_user_info") and adapter is not None:
            if self._is_aiocqhttp(adapter):
                try:
                    return await self._call_onebot_action(adapter, action, params)
                except Exception:
                    pass

        if action not in ("get_version", "get_supported_actions") and adapter is None:
            return {"status": "failed", "retcode": 1404, "message": "没有已连接的消息平台适配器"}
        return self._local_query(adapter, action, params, platform_id)

    @staticmethod
    def _is_aiocqhttp(adapter) -> bool:
        # 不直接 import AiocqhttpAdapter：它不在 astrbot.api.platform 的导出里，
        # 不同版本的位置不一样；按适配器名判断更稳。
        try:
            return str(adapter.meta().name) == "aiocqhttp"
        except Exception:
            return type(adapter).__name__ == "AiocqhttpAdapter"

    def _local_query(self, adapter, action: str, params: dict, platform_id: str = "") -> dict:
        if action == "get_self_info":
            wanted_self = str(params.get("self_id") or "")
            target_platform = str(params.get("platform_id") or "") or platform_id
            if wanted_self:
                target_platform = self._self_platform.get(wanted_self, target_platform)
            if not target_platform:
                target_platform = self._last_platform_id
            self_id = self._self_id_for(target_platform) or wanted_self
            name = self._self_name_for(target_platform)
            if not name:
                try:
                    robot = getattr(adapter.get_client(), "robot", None)
                    name = str(getattr(robot, "name", "") or "")
                except Exception:
                    name = ""
            name = str(name or "").strip()
            if name:
                self._self_names[target_platform] = name
            return {
                "status": "ok",
                "retcode": 0,
                "data": {
                    "user_id": self_id,
                    "user_name": name,
                    "user_displayname": name,
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
        if action == "get_group_member_info":
            group_id = str(params.get("group_id") or "")
            user_id = str(params.get("user_id") or "")
            name = self._members.get(group_id, {}).get(user_id, "")
            return {
                "status": "ok",
                "retcode": 0,
                "data": {"user_id": user_id, "user_name": name, "user_displayname": name},
            }
        if action == "get_user_info":
            user_id = str(params.get("user_id") or "")
            name = ""
            for members in self._members.values():
                if members.get(user_id):
                    name = members[user_id]
                    break
            return {
                "status": "ok",
                "retcode": 0,
                "data": {"user_id": user_id, "user_name": name, "user_displayname": name},
            }
        if action == "get_version":
            return {
                "status": "ok",
                "retcode": 0,
                "data": {
                    "impl": IMPL_NAME,
                    "version": PLUGIN_VERSION,
                    "onebot_version": ONEBOT_VERSION,
                    "platform": self._ob12_platform(platform_id),
                },
            }
        if action == "get_supported_actions":
            return {"status": "ok", "retcode": 0, "data": list(SUPPORTED_ACTIONS)}
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

    def _get_adapter(self, group_id: str = "", self_id: str = ""):
        # 优先用 self_id 定位账号：两个账号在同一条物理群里收消息时，
        # 单靠 group_id 无法区分，会把回复发到另一个账号。
        platform_id = self._platform_for_action(self_id, group_id)
        if platform_id:
            try:
                adapter = self.context.get_platform_inst(platform_id)
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
    def _coerce_params(params: dict) -> dict:
        out = dict(params)
        for key in ("group_id", "user_id", "message_id", "count"):
            value = out.get(key)
            if isinstance(value, str) and value.isdigit():
                out[key] = int(value)
        return out

def _as_int(value) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return 0
