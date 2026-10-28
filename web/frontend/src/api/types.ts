export interface GroupInfo {
  group_id: string
  group_name: string
  /** 所属工作空间（稳定 ID / 展示名） */
  workspace_id?: string
  workspace_name?: string
  /** 接收该群的机器人账号；空=无法归属 */
  self_id: string
  /** 账号昵称；解析不到时为空 */
  account_name: string
  message_count: number
  /** 已收录成员数（说过话或被 @ 过的人） */
  member_count: number
  last_activity: string
  cached: boolean
}

/** 一个机器人账号的工作空间汇总 */
export interface WorkspaceInfo {
  id: string
  name: string
  platform_id: string
  self_ids: string[]
  group_count: number
  cached_group_count: number
  message_count: number
  last_activity: string
}

/** 一个机器人账号的汇总（多账号面板用） */
export interface AccountInfo {
  self_id: string
  nickname: string
  group_count: number
  cached_group_count: number
  message_count: number
  last_activity: string
}

export interface BotStatus {
  bot_nickname: string
  api_key: string
  group_count: number
  account_count: number
  allow_all_groups: boolean
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
}

export interface APIResponse<T> {
  code: number
  data: T
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
