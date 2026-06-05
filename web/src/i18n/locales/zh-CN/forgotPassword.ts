// Translations for ForgotPassword.tsx — Chinese (zh-CN).
//
// One namespace per logical page is the convention; if a future
// "auth-shared" need emerges (e.g. an "Email format invalid" error
// reused across login + register + forgot-password) we'll lift it
// into a `common` namespace and import via `useTranslation(['forgotPassword', 'common'])`.
const forgotPassword = {
  title: "忘记密码",
  subtitle: "填写注册邮箱，匹配到的活跃账号将收到 1 小时内有效的一次性重置链接。",
  labels: {
    email: "账号邮箱",
  },
  placeholders: {
    email: "you@example.com",
  },
  actions: {
    submitting: "发送中...",
    submit: "发送重置链接",
    backLogin: "返回登录",
  },
  errors: {
    invalidEmail: "请输入合法的邮箱地址。",
    failed: "重置请求失败",
  },
  successTitle: "请前往邮箱查收",
  successBody:
    "若该邮箱对应活跃账号，您将很快收到重置链接。若未收到，请检查垃圾邮件或稍候 1–2 分钟重试。",
  devLinkTitle: "开发模式直接重置链接（SMTP 未配置）：",
} as const;

export default forgotPassword;
