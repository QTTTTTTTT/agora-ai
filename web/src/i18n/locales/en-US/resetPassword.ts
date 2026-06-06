// Translations for ResetPassword.tsx — English (en-US).
// Migrated from the inline `copy` block as part of the W4-26 i18n
// rollout.
const resetPassword = {
  title: "Reset password",
  subtitle:
    "Paste the token from your email and choose a new password. Token expires 1 hour after issue.",
  labels: {
    token: "Reset token",
    password: "New password",
    confirm: "Confirm new password",
  },
  placeholders: {
    token: "Paste token here",
    password: "At least 8 characters",
    confirm: "Re-enter the password",
  },
  actions: {
    submitting: "Updating...",
    submit: "Update password",
    backLogin: "Back to sign in",
  },
  errors: {
    missingToken: "The reset link is missing the token parameter.",
    shortPassword: "Password must be at least 8 characters.",
    passwordMismatch: "Passwords do not match.",
    failed: "Could not reset password",
  },
  successDetail: "Password updated. Redirecting you to sign in...",
} as const;

export default resetPassword;
