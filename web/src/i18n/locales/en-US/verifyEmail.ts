// Translations for VerifyEmail.tsx — English (en-US).
const verifyEmail = {
  title: "Verify email",
  subtitle:
    "Click below to send a 6-digit code to your account email, then enter it here.",
  labels: { code: "Verification code" },
  placeholders: { code: "6-digit code" },
  actions: {
    sending: "Sending...",
    send: "Send verification code",
    verifying: "Verifying...",
    verify: "Verify email",
    backHome: "Back to console",
  },
  errors: {
    shortCode: "Enter the 6-digit code from your email.",
    sendFailed: "Could not send code",
    verifyFailed: "Verification failed",
  },
  verified: "Email verified — thanks!",
  sentInfo: "Code sent. Check your inbox (and spam).",
  devCodeTitle: "Dev mode code (SMTP unconfigured):",
} as const;

export default verifyEmail;
