// ResetPassword.tsx 的中文翻译（W4-26 迁移）。
const resetPassword = {
  title: "重置密码",
  subtitle: "粘贴邮件中的令牌并设置新密码。令牌发出后 1 小时内有效。",
  labels: {
    token: "重置令牌",
    password: "新密码",
    confirm: "确认新密码",
  },
  placeholders: {
    token: "请粘贴令牌",
    password: "至少 8 位",
    confirm: "请再次输入新密码",
  },
  actions: {
    submitting: "更新中...",
    submit: "更新密码",
    backLogin: "返回登录",
  },
  errors: {
    missingToken: "重置链接缺少令牌参数。",
    shortPassword: "密码长度至少为 8 位。",
    passwordMismatch: "两次输入的密码不一致。",
    failed: "密码重置失败",
  },
  successDetail: "密码已更新，正在跳转登录页...",
} as const;

export default resetPassword;
