export interface GroupInfo {
  group_id: string
  group_name: string
  message_count: number
  last_activity: string
  cached: boolean
}

export interface BotStatus {
  bot_qq: string
  bot_nickname: string
  api_key: string
  group_count: number
}

export interface Message {
  msg_id: number
  group_id: string
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
