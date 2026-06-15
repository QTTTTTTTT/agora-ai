// Translations for web/src/lib/api.ts error fallbacks — Simplified Chinese (zh-CN).
//
// See ../en-US/apiErrors.ts for the rationale. The two files MUST stay
// key-compatible; the namespace parity test (Step 12) enforces this.
const apiErrors = {
  missingToken: "当前会话缺少访问凭证，请先登录。",
  timeout: "请求超时，请稍后重试。",
  sessionExpired: "登录状态已失效，请重新登录后再试。",
  loginBadResponse: "登录失败，响应体异常",
  loginFailedStatus: "登录失败，状态码 {{status}}",
  requestFailedStatus: "请求失败，状态码 {{status}}",
  sessionFailedStatus: "会话请求失败，状态码 {{status}}",
  planValidationFailed: "AI 输出的方案未通过校验",
} as const;

export default apiErrors;
