// validation.ts (zh-CN) — shared field-level validation messages.
// Mirrors the en-US namespace; see ../en-US/validation.ts for the
// full doc comment.

const validation = {
  required: "必填",
  invalidEmail: "请输入有效的邮箱地址",
  minLength: "至少 {{n}} 个字符",
  mustMatch: "两次输入不一致",
  passwordLetter: "需至少包含一个字母",
  passwordDigit: "需至少包含一个数字",
} as const;

export default validation;
