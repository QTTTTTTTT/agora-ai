// VerifyEmail.tsx 的中文翻译（W4-26 迁移）。
const verifyEmail = {
  title: "邮箱验证",
  subtitle: "点击下方按钮，向当前账号邮箱发送 6 位验证码，然后在此处输入完成验证。",
  labels: { code: "验证码" },
  placeholders: { code: "6 位验证码" },
  actions: {
    sending: "发送中...",
    send: "发送验证码",
    verifying: "验证中...",
    verify: "验证邮箱",
    backHome: "返回控制台",
  },
  errors: {
    shortCode: "请输入邮件中的 6 位验证码。",
    sendFailed: "验证码发送失败",
    verifyFailed: "验证失败",
  },
  verified: "邮箱已成功验证！",
  sentInfo: "验证码已发送，请前往邮箱（含垃圾邮件）查收。",
  devCodeTitle: "开发模式验证码（SMTP 未配置）：",
} as const;

export default verifyEmail;
