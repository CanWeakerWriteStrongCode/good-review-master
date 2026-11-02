/**
 * 中文语言包。
 *
 * key 是「命名空间.元素」两层，命名空间按页面切（monitor / groups / ...），
 * 通用文案统一放 common，避免每个页面各写一份「加载中...」。
 *
 * 两条规矩，改文案时别破：
 * 1. 带变量的整句放这里、用 {name} 占位。绝对不要在外面拼 `'消息 ' + n + ' 条'`，
 *    中英文语序不同，拼出来的英文一定是错的。
 * 2. 单位跟着数量变的（1 account / 2 accounts），用 `|` 分段：
 *    `'account | accounts'` 第一段给 1，第二段给其余。中文不区分，写一段就行。
 */
export default {
  common: {
    loading: '加载中...',
    refresh: '刷新',
    refreshing: '刷新中',
    collapse: '收起',
    hasRecords: '有记录',
    noData: '无数据',
    waiting: '等待接收消息...',
    unknown: '未知',
    // 会话卡前面的类型标记，只看 groups.type
    chatKind: {
      private: '私聊',
      group: '群聊',
    },
    unassignedAccount: '未归属账号',
    unknownAccount: '未知账号',
    // 群卡片和会话卡片共用的一组字段名
    stat: {
      messages: '消息数',
      members: '已收录成员',
      lastActive: '最近活动',
      monitorAccount: '监控账号',
    },
  },

  // 接口层兜底文案。正常情况下后端会带着翻好的 msg 过来，这里只在
  // 拿不到信封时用：断网、网关回了 HTML、或者后端漏带了 msg。
  api: {
    unauthorized: '未授权，请重新登录',
    httpError: '接口请求失败（HTTP {status}）',
    unknown: '未知错误，请稍后重试',
    network: '网络连接失败，请检查网络后重试',
    pprofHttp: 'pprof {name} 返回 HTTP {status}',
    pprofBinary: 'pprof {name} 未返回二进制数据',
  },

  // 顶栏和底部 tab
  nav: {
    monitor: '聊天记录',
    alerts: '报警',
    diagnostics: '诊断',
    groups: '群列表',
    messages: '消息详情',
    brandSub: '海豚智章 · 监控面板',
  },

  login: {
    title: '海豚智章',
    subtitle: 'Dolphin Mind · 监控面板',
    username: '账号',
    password: '密码',
    submit: '登录',
    submitting: '登录中...',
    usernameRequired: '请输入账号',
    passwordRequired: '请输入密码',
    failed: '登录失败',
  },

  monitor: {
    alerts: {
      none: '暂无报警',
      count: '报警 {count} 条',
      expandMore: '展开 {count} 条',
      showDetail: '看现场',
      hideDetail: '收起现场',
      global: '全局',
    },
    overview: {
      // 前面是大号数字，所以单位单独成段；带 count 是为了管英文单复数
      accountUnit: '个账号 | 个账号',
      convUnit: '个会话在监控 | 个会话在监控',
    },
    logout: '退出',
    mode: {
      byAccount: '按账号',
      byChat: '按群',
    },
    ws: {
      live: '有数据',
      idle: '等消息',
      statChats: '群 / 会话',
      statActive: '有消息',
      statMessages: '消息总数',
      chatList: '群列表',
      chatCount: '{count} 个',
    },
    group: {
      messages: '消息',
      members: '成员',
      emptyOwn: '该账号还没有群数据',
      reading: '读取中...',
      expandAll: '展开全部 {count} 个群',
    },
    foot: {
      lastActive: '最近活动 {time}',
      fullList: '完整列表 ›',
    },
    empty: {
      accounts: '还没有账号在监控',
      chats: '还没有任何会话记录',
    },
  },

  alerts: {
    summary: {
      unit: '条报警 | 条报警',
      showing: '显示最近 {count} 条',
    },
    empty: {
      title: '暂无报警',
      hint: '窗口太小、阶段重试超限这类异常会记在这里',
    },
    action: {
      detail: '看现场',
      hide: '收起现场',
    },
    ownerGlobal: '全局',
    // kind 是写库时定的分类码，值本身不翻译，只有展示这层翻
    kind: {
      windowTooSmall: '窗口太小，放弃回复',
      deadLetter: '{stage} 阶段重试超限，已隔离',
    },
  },

  groups: {
    mode: {
      byAccount: '按监控账号',
      all: '全部列表',
    },
    workspace: {
      meta: '{groups} 群 · {messages} 条',
    },
    account: {
      meta: '{groups} 群 · {messages} 条',
    },
    empty: {
      own: '该账号暂无群数据',
      config: '暂未配置群组，请在 config.yaml 中设置 allow_groups',
      all: '还没有任何会话记录',
    },
  },

  messages: {
    header: {
      workspace: '工作空间：{name}',
      account: '接收账号：{name}',
      total: '{count} 条消息',
      partial: '共 {total} 条，显示最近 {shown} 条',
      // 群名后面跟群号，全角括号中文里更协调
      labeled: '{name}（{id}）',
    },
    senderBot: '机器人',
    empty: '该群暂无消息记录',
  },

  diagnostics: {
    card: {
      goroutines: 'goroutine',
      heap: '堆占用',
      gc: 'GC 次数',
      uptime: '已运行',
    },
    toolbar: {
      auto: '自动 10s',
      stopAuto: '停止自动',
    },
    goroutines: {
      title: 'goroutine 按栈顶函数分组',
      count: '{total} 个 / {groups} 组',
    },
    dump: {
      expand: '展开完整栈',
      collapse: '收起完整栈',
      copy: '复制',
      copied: '已复制完整栈',
      truncated: '栈较长，仅显示前 {size}（完整内容可复制）',
    },
    pprofOff: {
      title: 'pprof 未开启',
      body: '上面的 goroutine 分组不依赖它。要看 heap / block / mutex 等 profile，或下载 profile 用 go tool pprof 出火焰图，需在 config.yaml 设置 runtime.enable_pprof: true 后重启。',
    },
    profile: {
      title: 'profile 文本视图',
    },
    download: {
      title: '下载',
      hint: '用 go tool pprof -http=:8081 文件路径 看火焰图',
      done: '已下载 {file}',
      h5Only: '下载仅 H5 端支持',
    },
  },
}
