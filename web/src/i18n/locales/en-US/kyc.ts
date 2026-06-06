// Translations for KYC.tsx — English (en-US).
const kyc = {
  title: "Identity verification (KYC)",
  subtitle:
    "Submit or review your verification status. KYC is required before live trading, marketplace publishing, and wallet recharge.",
  back: "Back to companies",
  loading: "Loading KYC status...",
  loadError: "Failed to load KYC status",
  currentStatus: "Current status",
  currentLevel: "Current level",
  submitTitle: "Submit application",
  fullName: "Full legal name",
  level: "Requested level",
  documentType: "Document type",
  documentNumber: "Document number",
  documentUrls: "Document image URLs (optional, one per line)",
  submit: "Submit for review",
  submitting: "Submitting...",
  submitError: "Failed to submit KYC application",
  submitSuccess: "KYC application submitted for admin review.",
  pendingBlock:
    "You already have a pending KYC application. Please wait for admin review before submitting another one.",
  verifiedBlock:
    "Your account already has this KYC level or higher. Choose a higher level only if you need an upgrade.",
  fullNameRequired: "Please enter your full legal name.",
  documentRequired: "Please enter your document number.",
  history: "Application history",
  noHistory: "No KYC applications yet.",
  attachments: "Attachments",
  rejectionReason: "Rejection reason",
  submittedAt: "Submitted",
} as const;

export default kyc;
