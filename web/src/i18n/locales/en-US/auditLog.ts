// Translations for AuditLog.tsx — English (en-US).
const auditLog = {
  title: "Unified audit log",
  subtitle:
    "Review fund-scoped data access, marketplace snapshots, memory reads, and other auditable actions in one timeline.",
  loading: "Loading audit trail...",
  loadError: "Failed to load audit logs",
  exportError: "Failed to export audit logs",
  retry: "Retry",
  exportCsv: "Export CSV",
  exporting: "Exporting...",
  emptyTitle: "No audit events yet",
  emptyDescription:
    "Auditable events will appear here after protected data is read, exported, snapshotted, or shared.",
  columns: {
    time: "Time",
    action: "Action",
    resource: "Resource",
    details: "Details",
  },
  searchPlaceholder: "Search by action, resource, or detail…",
  searchEmpty: "No entries match your search.",
  matchSummary: "Showing {{matched}} of {{total}} entries",
} as const;

export default auditLog;
