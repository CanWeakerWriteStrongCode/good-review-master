/**
 * English language pack.
 *
 * Same key structure as zh-CN.ts — the two files are compared key by key in
 * src/locale/__tests__/parity.test.ts, so adding a key here means adding it
 * there too.
 *
 * Note on plurals: Chinese writes one form and vue-i18n uses it for every
 * count; English needs `singular | plural`, with the count passed to t().
 */
export default {
  common: {
    loading: 'Loading...',
    refresh: 'Refresh',
    refreshing: 'Refreshing',
    collapse: 'Show less',
    hasRecords: 'Has data',
    noData: 'No data',
    waiting: 'Waiting for messages...',
    unknown: 'Unknown',
    privateLabel: 'Private {id}',
    unassignedAccount: 'Unassigned account',
    unknownAccount: 'Unknown account',
    // field labels shared by chat cards and group cards
    stat: {
      messages: 'Messages',
      members: 'Members',
      lastActive: 'Last active',
      monitorAccount: 'Account',
    },
  },

  // Fallbacks for the API layer. Normally the backend sends a translated
  // msg; these are only used when no envelope arrives (offline, an HTML error
  // page from a proxy, or a missing msg).
  api: {
    unauthorized: 'Session expired, please sign in again',
    httpError: 'Request failed (HTTP {status})',
    unknown: 'Something went wrong, please try again later',
    network: 'Network unavailable, please check your connection',
    pprofHttp: 'pprof {name} returned HTTP {status}',
    pprofBinary: 'pprof {name} did not return binary data',
  },

  // Top bar and bottom tab bar
  nav: {
    monitor: 'Monitor',
    alerts: 'Alerts',
    diagnostics: 'Diagnostics',
    groups: 'Chats',
    messages: 'Messages',
    brandSub: 'Dolphin Mind · Dashboard',
  },

  login: {
    title: 'Dolphin Mind',
    subtitle: 'Operations dashboard',
    username: 'Username',
    password: 'Password',
    submit: 'Sign in',
    submitting: 'Signing in...',
    usernameRequired: 'Enter your username',
    passwordRequired: 'Enter your password',
    failed: 'Sign-in failed',
  },

  monitor: {
    alerts: {
      none: 'No alerts',
      count: '{count} alerts',
      expandMore: 'Show {count}',
      showDetail: 'Details',
      hideDetail: 'Hide',
      global: 'Global',
    },
    overview: {
      accountUnit: 'account | accounts',
      convUnit: 'conversation monitored | conversations monitored',
    },
    logout: 'Sign out',
    mode: {
      byAccount: 'By account',
      byChat: 'By chat',
    },
    ws: {
      live: 'Live',
      idle: 'Idle',
      statChats: 'Chats',
      statActive: 'With messages',
      statMessages: 'All messages',
      chatList: 'Chats',
      chatCount: '{count}',
    },
    group: {
      messages: 'Msgs',
      members: 'Members',
      emptyOwn: 'No chats for this account yet',
      reading: 'Loading...',
      expandAll: 'Show all {count}',
    },
    foot: {
      lastActive: 'Last active {time}',
      fullList: 'Full list ›',
    },
    empty: {
      accounts: 'No accounts are being monitored yet',
      chats: 'No conversations recorded yet',
    },
  },

  alerts: {
    summary: {
      unit: 'alert | alerts',
      showing: 'Showing latest {count}',
    },
    empty: {
      title: 'No alerts',
      hint: 'Dropped replies and stage retries land here',
    },
    action: {
      detail: 'Details',
      hide: 'Hide',
    },
    ownerGlobal: 'Global',
    // kind is a stored classification code; only the display layer is translated
    kind: {
      windowTooSmall: 'Context window too small, reply dropped',
      deadLetter: '{stage} stage retries exhausted, quarantined',
    },
  },

  groups: {
    mode: {
      byAccount: 'By account',
      all: 'All chats',
    },
    workspace: {
      meta: '{groups} chats · {messages} msgs',
    },
    account: {
      meta: '{groups} chats · {messages} msgs',
    },
    empty: {
      own: 'No chats for this account',
      config: 'No chats configured. Set allow_groups in config.yaml',
      all: 'No conversations recorded yet',
    },
  },

  messages: {
    header: {
      workspace: 'Workspace: {name}',
      account: 'Receiving account: {name}',
      total: '{count} message | {count} messages',
      partial: '{total} total, showing latest {shown}',
      labeled: '{name} ({id})',
    },
    senderBot: 'Bot',
    empty: 'No messages in this chat',
  },

  diagnostics: {
    card: {
      goroutines: 'goroutines',
      heap: 'Heap in use',
      gc: 'GC runs',
      uptime: 'Uptime',
    },
    toolbar: {
      auto: 'Auto 10s',
      stopAuto: 'Stop auto',
    },
    goroutines: {
      title: 'Goroutines grouped by top frame',
      count: '{total} total / {groups} groups',
    },
    dump: {
      expand: 'Show full stacks',
      collapse: 'Hide full stacks',
      copy: 'Copy',
      copied: 'Full stacks copied',
      truncated: 'Stacks are long; showing the first {size} (copy for the full text)',
    },
    pprofOff: {
      title: 'pprof is off',
      body: 'The goroutine grouping above does not need it. To read heap / block / mutex profiles, or to download one and build a flame graph with go tool pprof, set runtime.enable_pprof: true in config.yaml and restart.',
    },
    profile: {
      title: 'Profile text view',
    },
    download: {
      title: 'Download',
      hint: 'Flame graph: go tool pprof -http=:8081 profile-file',
      done: 'Downloaded {file}',
      h5Only: 'Downloading works on H5 only',
    },
  },
}
