/**
 * Shared i18n strings — web + Android 共享。
 *
 * Web 端用自己的 PreferencesProvider 路径，但所有"业务文案"（auth /
 * tabs / decisions / memory / team / more）都通过这里统一定义，避免
 * "web 改了文案 Android 没跟上"的产线漂移。
 *
 * 字典结构是嵌套 record，调用方按 dot path 解析 — 与 i18next 兼容。
 */

export type LocaleId = 'zh-CN' | 'en-US';

export interface Messages {
  auth: {
    loginTitle: string;
    email: string;
    password: string;
    submit: string;
    submitting: string;
    forgot: string;
    errorInvalid: string;
    errorGeneric: string;
    biometricsPrompt: string;
    biometricsRequired: string;
    biometricsBlockedHint: string;
    sessionErrorTitle: string;
    sessionErrorHint: string;
    sessionErrorRetry: string;
    forgotTitle: string;
    forgotHint: string;
    forgotSubmit: string;
    forgotSent: string;
    backToLogin: string;
    resetTitle: string;
    resetHint: string;
    resetNewPassword: string;
    resetConfirmPassword: string;
    resetSubmit: string;
    resetSubmitting: string;
    resetSuccess: string;
    resetTokenInvalid: string;
    resetPasswordMismatch: string;
  };
  tabs: {
    home: string;
    decisions: string;
    memory: string;
    team: string;
    more: string;
  };
  home: {
    title: string;
    empty: string;
    loading: string;
    error: string;
    retry: string;
    navLabel: string;
    assetsLabel: string;
  };
  decisions: {
    title: string;
    empty: string;
    loadFailed: string;
    retry: string;
    actionsLabel: string;
    approve: string;
    reject: string;
    refresh: string;
    rejecting: string;
    approving: string;
    refreshing: string;
    rejectReasonPrompt: string;
    rejectReasonRequired: string;
    confirm: string;
    cancel: string;
    successApproved: string;
    successRejected: string;
    successRefreshed: string;
    actionFailed: string;
    statusDraft: string;
    statusRiskReview: string;
    statusPendingUser: string;
    statusApproved: string;
    statusRejected: string;
    statusExecuting: string;
    statusCompleted: string;
    statusFailed: string;
    statusMixed: string;
    recentEvents: string;
  };
  memory: {
    title: string;
    tabs: { agent: string; reflection: string };
    empty: string;
    error: string;
    retry: string;
  };
  team: {
    title: string;
    empty: string;
    error: string;
    retry: string;
  };
  more: {
    title: string;
    language: string;
    logout: string;
    version: string;
    darkMode: string;
    appearanceSystem: string;
    appearanceLight: string;
    appearanceDark: string;
    accountSecurity: string;
    accountInfoLabel: string;
    accountInfoMissing: string;
    accountEmailVerifiedOn: string;
    accountEmailVerifiedOff: string;
    changePassword: string;
    biometric: string;
    biometricOn: string;
    biometricOff: string;
    biometricHint: string;
    biometricUnavailable: string;
    notifications: string;
    notificationsOn: string;
    notificationsOff: string;
    notificationsHint: string;
    notificationsRegistering: string;
    notificationsRegistrationFailed: string;
    sectionAccount: string;
    sectionAppearance: string;
    sectionLanguage: string;
    sectionDanger: string;
    recentEvents: string;
  };
  corpActions: {
    title: string;
    subtitle: string;
    expand: string;
    collapse: string;
    loading: string;
    error: string;
    retry: string;
    empty: string;
    typeSplit: string;
    typeCashDividend: string;
    typeStockDividend: string;
    typeCombined: string;
    sharesLabel: string;
    costLabel: string;
    cashLabel: string;
    exDateLabel: string;
  };
  benchmark: {
    title: string;
    subtitle: string;
    fund: string;
    days7: string;
    days30: string;
    days90: string;
    days180: string;
    days365: string;
    expand: string;
    collapse: string;
    loading: string;
    empty: string;
    error: string;
    retry: string;
    seriesPicker: string;
    addSeries: string;
    partialFailureToast: string;
    legendStart: string;
    holdingOverlapDominantTitle: string;
    holdingOverlapDominantBody: string;
    holdingOverlapPartialTitle: string;
    holdingOverlapPartialBody: string;
    holdingOverlapSwitchToAlpha: string;
  };
  holdingsSeries: {
    title: string;
    subtitle: string;
    expand: string;
    collapse: string;
    loading: string;
    error: string;
    retry: string;
    empty: string;
    vsEntry: string;
    vsStart: string;
    partialFailureToast: string;
    days30: string;
    days90: string;
    days180: string;
  };
  abShadow: {
    sectionTitle: string;
    sectionSubtitle: string;
    expand: string;
    collapse: string;
    loading: string;
    error: string;
    retry: string;
    empty: string;
    notAnalyzedYet: string;
    columnA: string;
    columnB: string;
    eventCount: string;
    latestDate: string;
    lessons: string;
    adjustments: string;
    summaries: string;
    timeline: string;
    memories: string;
    proposedDiff: string;
    diffAdded: string;
    diffChanged: string;
    diffRemoved: string;
    noDiff: string;
    deterministicShadowBanner: string;
  };
  abAttribution: {
    sectionTitle: string;
    sectionSubtitle: string;
    expand: string;
    collapse: string;
    loading: string;
    error: string;
    retry: string;
    empty: string;
    columnSymbol: string;
    columnTradesA: string;
    columnTradesB: string;
    columnPnLA: string;
    columnPnLB: string;
    columnTurnoverA: string;
    columnTurnoverB: string;
    columnGap: string;
    columnGapPct: string;
    columnWinner: string;
    winnerA: string;
    winnerB: string;
    winnerTie: string;
    totalsTitle: string;
    avgPnL: string;
    winRate: string;
  };
}

export const messages: Record<LocaleId, Messages> = {
  'zh-CN': {
    auth: {
      loginTitle: '登录',
      email: '邮箱',
      password: '密码',
      submit: '登录',
      submitting: '登录中…',
      forgot: '忘记密码？',
      errorInvalid: '邮箱或密码错误',
      errorGeneric: '登录失败，请稍后再试',
      biometricsPrompt: '使用生物识别解锁',
      biometricsRequired: '生物识别验证失败',
      biometricsBlockedHint: '生物识别失败或被取消，请改用密码登录。',
      sessionErrorTitle: '当前无法连接服务',
      sessionErrorHint: '网络异常或服务端暂时不可用，重试或重新登录均可。',
      sessionErrorRetry: '重试连接',
      forgotTitle: '重置密码',
      forgotHint: '我们会向该邮箱发送一封带链接的邮件。',
      forgotSubmit: '发送邮件',
      forgotSent: '已发送，请查收邮箱。',
      backToLogin: '返回登录',
      resetTitle: '设置新密码',
      resetHint: '请设置不少于 8 位的密码。重置完成后请重新登录。',
      resetNewPassword: '新密码',
      resetConfirmPassword: '确认新密码',
      resetSubmit: '更新密码',
      resetSubmitting: '更新中…',
      resetSuccess: '密码已更新，请重新登录。',
      resetTokenInvalid: '链接无效或已过期，请回到忘记密码页面重新发起。',
      resetPasswordMismatch: '两次输入的密码不一致。',
    },
    tabs: { home: '首页', decisions: '决策', memory: '记忆', team: '团队', more: '更多' },
    home: {
      title: '我的基金',
      empty: '暂无基金，先在 web 端创建。',
      loading: '加载中…',
      error: '加载失败',
      retry: '重试',
      navLabel: '净值',
      assetsLabel: '总资产',
    },
    decisions: {
      title: '最新决策',
      empty: '今天还没有计划生成。',
      loadFailed: '加载决策失败，请重试。',
      retry: '重试',
      actionsLabel: '动作',
      approve: '通过计划',
      reject: '驳回计划',
      refresh: '刷新报价',
      approving: '提交中…',
      rejecting: '驳回中…',
      refreshing: '刷新中…',
      rejectReasonPrompt: '请简述驳回原因（1-200 字）',
      rejectReasonRequired: '驳回需要填写原因',
      confirm: '确认',
      cancel: '取消',
      successApproved: '已通过，等待执行',
      successRejected: '已驳回',
      successRefreshed: '报价已刷新',
      actionFailed: '操作失败，请重试',
      statusDraft: '草稿',
      statusRiskReview: '风控审查中',
      statusPendingUser: '待审批',
      statusApproved: '已通过',
      statusRejected: '已驳回',
      statusExecuting: '执行中',
      statusCompleted: '已完成',
      statusFailed: '失败',
      statusMixed: '部分成交',
      recentEvents: '最近事件',
    },
    memory: {
      title: '记忆与反思',
      tabs: { agent: '每日学习', reflection: '长期反思' },
      empty: '尚未生成记忆。',
      error: '加载失败',
      retry: '重试',
    },
    team: { title: 'Agent 团队', empty: '当前基金未配置 agent。', error: '加载失败', retry: '重试' },
    more: {
      title: '更多',
      language: '语言',
      logout: '退出登录',
      version: '版本',
      darkMode: '暗色模式',
      appearanceSystem: '跟随系统',
      appearanceLight: '浅色',
      appearanceDark: '深色',
      accountSecurity: '账号与安全',
      accountInfoLabel: '当前账号',
      accountInfoMissing: '未获取到账号信息',
      accountEmailVerifiedOn: '邮箱已验证',
      accountEmailVerifiedOff: '邮箱待验证',
      changePassword: '修改密码',
      biometric: '生物识别',
      biometricOn: '已启用',
      biometricOff: '未启用',
      biometricHint: '关闭后下次启动直接进入主界面，不再要求生物识别。',
      biometricUnavailable: '设备未启用指纹/面容识别',
      notifications: '推送通知',
      notificationsOn: '已开启',
      notificationsOff: '已关闭',
      notificationsHint: '决策完成 / 风控异常 / 反思更新等关键事件会发送通知。',
      notificationsRegistering: '正在注册推送…',
      notificationsRegistrationFailed: '推送注册失败，请稍后再试。',
      sectionAccount: '账号',
      sectionAppearance: '界面',
      sectionLanguage: '语言与地区',
      sectionDanger: '会话',
      recentEvents: '最近事件',
    },
    corpActions: {
      title: '分红 · 拆股 · 配股记录',
      subtitle: '近期发生在持仓上的公司行动事件',
      expand: '展开',
      collapse: '收起',
      loading: '加载中…',
      error: '加载失败',
      retry: '重试',
      empty: '近期无公司行动事件',
      typeSplit: '拆股 / 送股',
      typeCashDividend: '现金分红',
      typeStockDividend: '送股转增',
      typeCombined: '派股 + 派现',
      sharesLabel: '份额',
      costLabel: '成本',
      cashLabel: '现金到账',
      exDateLabel: '除权日',
    },
    benchmark: {
      title: '基金 vs 大盘',
      subtitle: '净值与基准指数同起点归一化（起始 = 100）',
      fund: '本基金',
      days7: '7 天',
      days30: '30 天',
      days90: '90 天',
      days180: '180 天',
      days365: '1 年',
      expand: '展开',
      collapse: '收起',
      loading: '加载中…',
      empty: '暂无可对比的净值数据',
      error: '基准加载失败',
      retry: '重试',
      seriesPicker: '基准指数',
      addSeries: '添加基准',
      partialFailureToast: '部分基准未能加载，已跳过',
      legendStart: '起始 = 100',
      holdingOverlapDominantTitle: '本基金主仓 ≈ 大盘',
      holdingOverlapDominantBody: '基金主要持仓与所选基准为同一标的，"对比"模式下两条曲线会高度重合；建议切换到 Alpha 视图查看相对超额收益。',
      holdingOverlapPartialTitle: '部分持仓与基准重叠',
      holdingOverlapPartialBody: '基金部分持仓与所选基准为同一标的，"对比"视图可能不直观，可切到 Alpha 视图观察相对走势。',
      holdingOverlapSwitchToAlpha: '切换到 Alpha 视图',
    },
    holdingsSeries: {
      title: '持仓走势',
      subtitle: '每只持仓在该窗口内的归一化股价（起始 = 100）',
      expand: '展开',
      collapse: '收起',
      loading: '加载中…',
      error: '走势加载失败',
      retry: '重试',
      empty: '暂无可绘制的持仓',
      vsEntry: '相对成本',
      vsStart: '相对窗口起点',
      partialFailureToast: '以下持仓未能加载',
      days30: '30 天',
      days90: '90 天',
      days180: '180 天',
    },
    abShadow: {
      sectionTitle: '影子 Agent 对比',
      sectionSubtitle: '查看 A 组与 B 组每位 agent 在影子运行中学到的内容、调整建议与提议的演化配置差异',
      expand: '展开',
      collapse: '收起',
      loading: '加载影子 agent 数据…',
      error: '加载影子 agent 数据失败',
      retry: '重试',
      empty: '该测试暂无影子 agent 学习数据',
      notAnalyzedYet: '完成"生成分析"后即可查看 A vs B 影子 agent 学习对比',
      columnA: 'A 组',
      columnB: 'B 组',
      eventCount: '学习事件数',
      latestDate: '最新事件日期',
      lessons: '关键经验',
      adjustments: '建议调整',
      summaries: '近期总结',
      timeline: '逐日时间线',
      memories: '影子记忆',
      proposedDiff: '提议的 evolution_config 变更',
      diffAdded: '新增',
      diffChanged: '变更（旧 → 新）',
      diffRemoved: '移除',
      noDiff: '与当前 evolution_config 一致，无需变更',
      deterministicShadowBanner: '当前 B 组采用确定性影子执行策略，数据用于策略参数 sanity check；后续 Card K 将引入真实 LLM 影子运行。',
    },
    abAttribution: {
      sectionTitle: '按标的归因',
      sectionSubtitle: '比较 A vs B 在每只标的上的成交、成本与盈亏差异',
      expand: '展开',
      collapse: '收起',
      loading: '加载归因数据…',
      error: '加载归因数据失败',
      retry: '重试',
      empty: '该测试暂无影子交易归因数据',
      columnSymbol: '标的',
      columnTradesA: 'A 笔数',
      columnTradesB: 'B 笔数',
      columnPnLA: 'A 已实现盈亏',
      columnPnLB: 'B 已实现盈亏',
      columnTurnoverA: 'A 成交额',
      columnTurnoverB: 'B 成交额',
      columnGap: '差额（B − A）',
      columnGapPct: '差额占成交额',
      columnWinner: '胜出版本',
      winnerA: 'A',
      winnerB: 'B',
      winnerTie: '持平',
      totalsTitle: '总览',
      avgPnL: '平均盈亏',
      winRate: '盈利交易占比',
    },
  },
  'en-US': {
    auth: {
      loginTitle: 'Sign in',
      email: 'Email',
      password: 'Password',
      submit: 'Sign in',
      submitting: 'Signing in…',
      forgot: 'Forgot password?',
      errorInvalid: 'Invalid email or password',
      errorGeneric: 'Sign-in failed. Please try again later.',
      biometricsPrompt: 'Unlock with biometrics',
      biometricsRequired: 'Biometric authentication failed',
      biometricsBlockedHint: 'Biometrics failed or were cancelled. Please sign in again with your password.',
      sessionErrorTitle: 'Cannot reach the service',
      sessionErrorHint: 'A network or backend issue is blocking your session. Retry or sign in again.',
      sessionErrorRetry: 'Retry connection',
      forgotTitle: 'Reset password',
      forgotHint: 'We will send a link to that email.',
      forgotSubmit: 'Send email',
      forgotSent: 'Sent — check your inbox.',
      backToLogin: 'Back to sign-in',
      resetTitle: 'Set a new password',
      resetHint: 'Use at least 8 characters. You will need to sign in again afterwards.',
      resetNewPassword: 'New password',
      resetConfirmPassword: 'Confirm new password',
      resetSubmit: 'Update password',
      resetSubmitting: 'Updating…',
      resetSuccess: 'Password updated — please sign in again.',
      resetTokenInvalid: 'This link is invalid or expired. Please request a new one.',
      resetPasswordMismatch: 'The two passwords do not match.',
    },
    tabs: { home: 'Home', decisions: 'Decisions', memory: 'Memory', team: 'Team', more: 'More' },
    home: {
      title: 'My funds',
      empty: 'No funds yet; create one in the web app.',
      loading: 'Loading…',
      error: 'Failed to load',
      retry: 'Retry',
      navLabel: 'NAV',
      assetsLabel: 'Assets',
    },
    decisions: {
      title: 'Latest decisions',
      empty: 'No plans generated today.',
      loadFailed: 'Failed to load decisions. Please retry.',
      retry: 'Retry',
      actionsLabel: 'actions',
      approve: 'Approve plan',
      reject: 'Reject plan',
      refresh: 'Refresh quote',
      approving: 'Approving…',
      rejecting: 'Rejecting…',
      refreshing: 'Refreshing…',
      rejectReasonPrompt: 'Briefly describe the reason (1–200 chars)',
      rejectReasonRequired: 'A reason is required to reject',
      confirm: 'Confirm',
      cancel: 'Cancel',
      successApproved: 'Approved — queued for execution',
      successRejected: 'Plan rejected',
      successRefreshed: 'Quote refreshed',
      actionFailed: 'Action failed. Please retry.',
      statusDraft: 'Draft',
      statusRiskReview: 'Risk review',
      statusPendingUser: 'Awaiting approval',
      statusApproved: 'Approved',
      statusRejected: 'Rejected',
      statusExecuting: 'Executing',
      statusCompleted: 'Completed',
      statusFailed: 'Failed',
      statusMixed: 'Partially filled',
      recentEvents: 'Recent events',
    },
    memory: {
      title: 'Memory & reflections',
      tabs: { agent: 'Daily learning', reflection: 'Long-term reflections' },
      empty: 'No memories yet.',
      error: 'Failed to load',
      retry: 'Retry',
    },
    team: { title: 'Agent team', empty: 'No agents configured for this fund.', error: 'Failed to load', retry: 'Retry' },
    more: {
      title: 'More',
      language: 'Language',
      logout: 'Sign out',
      version: 'Version',
      darkMode: 'Appearance',
      appearanceSystem: 'System',
      appearanceLight: 'Light',
      appearanceDark: 'Dark',
      accountSecurity: 'Account & security',
      accountInfoLabel: 'Signed in as',
      accountInfoMissing: 'Account info unavailable',
      accountEmailVerifiedOn: 'Email verified',
      accountEmailVerifiedOff: 'Email pending verification',
      changePassword: 'Change password',
      biometric: 'Biometric unlock',
      biometricOn: 'Enabled',
      biometricOff: 'Disabled',
      biometricHint: 'When off, the app opens directly without a biometric prompt.',
      biometricUnavailable: 'No fingerprint / Face ID enrolled on this device',
      notifications: 'Push notifications',
      notificationsOn: 'On',
      notificationsOff: 'Off',
      notificationsHint: 'Notified on plan ready, risk anomalies and reflection updates.',
      notificationsRegistering: 'Registering for push…',
      notificationsRegistrationFailed: 'Push registration failed. Please retry.',
      sectionAccount: 'Account',
      sectionAppearance: 'Appearance',
      sectionLanguage: 'Language',
      sectionDanger: 'Session',
      recentEvents: 'Recent events',
    },
    corpActions: {
      title: 'Dividends · Splits · Rights Issues',
      subtitle: 'Recent corporate actions applied to this fund',
      expand: 'Show',
      collapse: 'Hide',
      loading: 'Loading…',
      error: 'Failed to load',
      retry: 'Retry',
      empty: 'No recent corporate actions',
      typeSplit: 'Split / Stock div.',
      typeCashDividend: 'Cash dividend',
      typeStockDividend: 'Stock dividend',
      typeCombined: 'Stock + cash',
      sharesLabel: 'Shares',
      costLabel: 'Cost',
      cashLabel: 'Cash credit',
      exDateLabel: 'Ex-date',
    },
    benchmark: {
      title: 'Fund vs Market',
      subtitle: 'Fund NAV and benchmarks rebased to 100 at start',
      fund: 'This fund',
      days7: '7d',
      days30: '30d',
      days90: '90d',
      days180: '180d',
      days365: '1y',
      expand: 'Show',
      collapse: 'Hide',
      loading: 'Loading…',
      empty: 'No NAV history yet',
      error: 'Failed to load benchmarks',
      retry: 'Retry',
      seriesPicker: 'Benchmarks',
      addSeries: 'Add benchmark',
      partialFailureToast: 'Some benchmarks could not be loaded',
      legendStart: 'start = 100',
      holdingOverlapDominantTitle: 'Your fund ≈ this benchmark',
      holdingOverlapDominantBody: 'The fund\'s largest position is the same instrument as the selected benchmark, so the two lines in Compare mode track each other closely. Switch to the Alpha view to see relative outperformance.',
      holdingOverlapPartialTitle: 'Holdings overlap the benchmark',
      holdingOverlapPartialBody: 'Some of the fund\'s holdings overlap the selected benchmark, which can make the Compare view harder to read. Switch to Alpha for relative performance.',
      holdingOverlapSwitchToAlpha: 'Switch to Alpha view',
    },
    holdingsSeries: {
      title: 'Holdings trends',
      subtitle: 'Per-holding normalized price (start = 100)',
      expand: 'Show',
      collapse: 'Hide',
      loading: 'Loading…',
      error: 'Failed to load trends',
      retry: 'Retry',
      empty: 'No holdings to plot',
      vsEntry: 'vs entry',
      vsStart: 'vs window start',
      partialFailureToast: 'Holdings that couldn\'t be loaded',
      days30: '30d',
      days90: '90d',
      days180: '180d',
    },
    abShadow: {
      sectionTitle: 'Shadow agent comparison',
      sectionSubtitle: 'See what each variant\u2019s agents learned during shadow execution \u2014 lessons, adjustments, and proposed evolution-config changes.',
      expand: 'Show',
      collapse: 'Hide',
      loading: 'Loading shadow agents\u2026',
      error: 'Failed to load shadow agents',
      retry: 'Retry',
      empty: 'No shadow learning data for this test yet',
      notAnalyzedYet: 'Run \u201cGenerate analysis\u201d first to compare A vs B shadow agent learning.',
      columnA: 'Variant A',
      columnB: 'Variant B',
      eventCount: 'Learning events',
      latestDate: 'Latest event',
      lessons: 'Lessons',
      adjustments: 'Adjustments',
      summaries: 'Recent summaries',
      timeline: 'Daily timeline',
      memories: 'Shadow memories',
      proposedDiff: 'Proposed evolution_config change',
      diffAdded: 'Added',
      diffChanged: 'Changed (prev \u2192 new)',
      diffRemoved: 'Removed',
      noDiff: 'No change vs current evolution_config',
      deterministicShadowBanner: 'Variant B currently uses deterministic shadow execution; numbers are sanity-check only. Card K will introduce real LLM shadow runs.',
    },
    abAttribution: {
      sectionTitle: 'Per-symbol attribution',
      sectionSubtitle: 'Compare A vs B trade count, turnover, and realized P&L per symbol.',
      expand: 'Show',
      collapse: 'Hide',
      loading: 'Loading attribution\u2026',
      error: 'Failed to load attribution',
      retry: 'Retry',
      empty: 'No shadow trade attribution for this test yet',
      columnSymbol: 'Symbol',
      columnTradesA: 'A trades',
      columnTradesB: 'B trades',
      columnPnLA: 'A realized P&L',
      columnPnLB: 'B realized P&L',
      columnTurnoverA: 'A turnover',
      columnTurnoverB: 'B turnover',
      columnGap: 'Gap (B \u2212 A)',
      columnGapPct: 'Gap % of turnover',
      columnWinner: 'Winner',
      winnerA: 'A',
      winnerB: 'B',
      winnerTie: 'Tie',
      totalsTitle: 'Totals',
      avgPnL: 'Avg P&L',
      winRate: 'Winning trade rate',
    },
  },
};

/**
 * resolveMessage — pure helper to look up a dot path like "auth.email".
 * Returns the fallback (== same path) when missing so the UI shows the
 * path rather than crashing — easier to debug.
 */
export function resolveMessage(locale: LocaleId, path: string): string {
  const bundle = messages[locale] ?? messages['zh-CN'];
  const segments = path.split('.');
  let cursor: unknown = bundle;
  for (const seg of segments) {
    if (cursor && typeof cursor === 'object' && seg in (cursor as Record<string, unknown>)) {
      cursor = (cursor as Record<string, unknown>)[seg];
    } else {
      return path;
    }
  }
  return typeof cursor === 'string' ? cursor : path;
}
