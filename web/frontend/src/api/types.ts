export interface GroupInfo {
  group_id: string
  group_name: string
  /** 会话类型：group = 群聊，private = 私聊 */
  type?: string
  /** 所属工作空间（稳定 ID / 展示名） */
  workspace_id?: string
  workspace_name?: string
  /** 本账号的机器人 openid（openid 由账号决定，所有会话同一个值）；空=还没学到 */
  self_id: string
  /** 账号昵称；解析不到时为空 */
  account_name: string
  /** 该群在库里的消息总条数 */
  message_count: number
  /** 已收录成员数（说过话或被 @ 过的人） */
  member_count: number
  /** 最近一条消息的时间（Unix 秒），0 = 没有记录。显示用的日期由前端按语言拼 */
  last_ts: number
  /** 库里有这个群的消息记录；false 时整块统计不显示 */
  has_messages: boolean
}

/** 一条报警（state.db 的 alerts 表）：放弃回复、节点重试超限这类异常现场 */
export interface AlertItem {
  id: number
  workspace_id: string
  /** 账号展示名；账号已不在运行时为空 */
  workspace_name: string
  /** 分类：window_too_small、dead_letter_<node> */
  kind: string
  /** 发生时的完整现场 JSON */
  detail: string
  /** Unix 秒，显示格式由前端按当前语言拼 */
  created_at: number
}

/** 一个机器人账号的工作空间汇总 */
export interface WorkspaceInfo {
  id: string
  name: string
  platform_id: string
  self_ids: string[]
  group_count: number
  /** 库里有消息记录的群数 */
  active_group_count: number
  message_count: number
  /** 最近一条消息时间（Unix 秒），0 = 没有记录 */
  last_ts: number
}

/** 一个机器人账号的汇总（多账号面板用） */
export interface AccountInfo {
  self_id: string
  nickname: string
  group_count: number
  active_group_count: number
  message_count: number
  last_ts: number
}

export interface BotStatus {
  bot_nickname: string
  group_count: number
  account_count: number
  allow_all_groups: boolean
}

/** 全局图库（image_notes 表）里一张图的描述，与 Message.images 一一对齐 */
export interface ImageNote {
  sha?: string
  size?: number
  /** 图库里写的描述；空串表示这张图还没描述或描述失败 */
  note?: string
  status?: string
}

export interface Message {
  msg_id: number
  group_id: string
  workspace_id?: string
  self_id?: string
  user_id: string
  nick: string
  card: string
  content: string
  time: number
  /** 与 images 一一对齐的图片描述，正文里的 [图片] 占位靠它补出说明 */
  image_notes?: ImageNote[]
}

/**
 * 每个接口的统一信封。
 *
 * 成功时 code=200 且 data 有值；失败时 code 是六位业务码、msg 是后端按 X-Lang
 * 翻好的句子。**前端只判断 code != 200，然后弹 msg**，不认任何具体码——
 * 所以后端加错误码不用前端跟着发版。
 */
export interface APIResponse<T> {
  /** 200 = 成功；其余是六位业务码（4xxxx 请求/身份、5xxxx 服务器、6xxxx 业务拒绝） */
  code: number
  data: T
  /** 错误码的名字（如 workspace.not_found），排查用，前端不做判断 */
  err?: string
  /** 后端翻好的错误文案，直接弹给用户 */
  msg?: string
  /** 只有兜底的未知错误才带，是给用户报障用的单号 */
  req_id?: string
}

/** 一类 goroutine 的聚合，按栈里第一个"非运行时"帧归类（服务端算好） */
export interface GoroutineGroup {
  frame: string
  state: string
  count: number
}

export interface GoroutinesInfo {
  total: number
  group_count: number
  groups: GoroutineGroup[]
  /** 仅 ?full=1 时返回：栈全文。自动刷新不请求它，避免每次传几 MB */
  stack_dump?: string
  truncated?: boolean
}

export interface RuntimeInfo {
  goroutines: number
  heap_alloc_bytes: number
  heap_inuse_bytes: number
  heap_sys_bytes: number
  stack_inuse_bytes: number
  num_gc: number
  gc_pause_total_ms: number
  gomaxprocs: number
  num_cpu: number
  go_version: string
  uptime_seconds: number
  version: string
  /** 后端开关状态，前端不自己维护一份 */
  pprof_enabled: boolean
}

export interface DiagnosticsData {
  runtime: RuntimeInfo
  goroutines: GoroutinesInfo
}
