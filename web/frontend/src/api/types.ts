export interface GroupInfo {
  group_id: string
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
  content: string
  time: number
}

export interface APIResponse<T> {
  code: number
  data: T
}
