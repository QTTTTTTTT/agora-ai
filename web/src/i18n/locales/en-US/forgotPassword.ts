// Translations for ForgotPassword.tsx — English (en-US).
const forgotPassword = {
  title: "Forgot password",
  subtitle:
    "Enter the email tied to your account. If it matches an active user we'll send a one-time reset link valid for 1 hour.",
  labels: {
    email: "Account email",
  },
  placeholders: {
    email: "you@example.com",
  },
  actions: {
    submitting: "Sending...",
    submit: "Send reset link",
    backLogin: "Back to sign in",
  },
  errors: {
    invalidEmail: "Enter a valid email address.",
    failed: "Could not request reset",
  },
  successTitle: "Check your inbox",
  successBody:
    "If the email matches an active account, you'll receive a reset link shortly. Don't see it? Check spam and wait 1–2 minutes.",
  devLinkTitle: "Dev mode reset link (SMTP unconfigured):",
} as const;

export default forgotPassword;
