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
