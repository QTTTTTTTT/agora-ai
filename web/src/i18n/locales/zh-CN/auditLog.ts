// AuditLog.tsx 的中文翻译（W4-26 迁移）。
const auditLog = {
  title: "统一审计日志",
  subtitle: "在一条时间线中查看基金相关的数据访问、市场快照、记忆读取和其他可审计动作。",
  loading: "正在加载审计轨迹...",
  loadError: "加载审计日志失败",
  exportError: "导出审计日志失败",
  retry: "重试",
  exportCsv: "导出 CSV",
  exporting: "导出中...",
  emptyTitle: "暂无审计事件",
  emptyDescription: "当受保护数据被读取、导出、生成快照或共享后，相关事件会出现在这里。",
  columns: {
    time: "时间",
    action: "动作",
    resource: "资源",
    details: "详情",
  },
  searchPlaceholder: "搜索动作、资源或详情…",
  searchEmpty: "未找到匹配的审计事件。",
  matchSummary: "共 {{total}} 条，匹配 {{matched}} 条",
} as const;

export default auditLog;
