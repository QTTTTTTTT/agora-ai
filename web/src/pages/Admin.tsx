import React, { useCallback, useEffect, useMemo, useState } from "react";
import {
  apiGet,
  apiPost,
  apiPut,
  decideAdminKYCApplication,
  fetchAdminKYCApplications,
  fetchWorkflowSchedulerSnapshot,
  formatApiError,
  triggerFundWorkflow,
  type AdminKYCApplication,
  type AdminMarketDataHealthResponse,
  type AdminMarketDataProviderHealth,
  type FundSchedulerSnapshot,
  type FundSchedulerStatus,
} from "../lib/api";
import { formatDateTimeForLanguage, formatMoneyForLanguage, useAppPreferences } from "../lib/preferences";
import AdminBrokerLinksSection from "../components/AdminBrokerLinksSection";
import AdminFundingSection from "../components/AdminFundingSection";
import AdminFXSection from "../components/AdminFXSection";
import AdminReconSection from "../components/AdminReconSection";
import AdminSurveillanceSection from "../components/AdminSurveillanceSection";
import AdminBorrowSection from "../components/AdminBorrowSection";
import AdminDrawdownSection from "../components/AdminDrawdownSection";
import AdminFactorExposureSection from "../components/AdminFactorExposureSection";
import AdminStressScenariosSection from "../components/AdminStressScenariosSection";
import AdminBrinsonCompositionsSection from "../components/AdminBrinsonCompositionsSection";
import AdminAgentReputationSection from "../components/AdminAgentReputationSection";
import { AdminWorkflowCheckpointsSection } from "../components/AdminWorkflowCheckpointsSection";
import { AdminModelABSection } from "../components/AdminModelABSection";
import { AdminModelABPromotionSection } from "../components/AdminModelABPromotionSection";
import { AdminLLMHealthSection } from "../components/AdminLLMHealthSection";
import { AdminMemReembedSection } from "../components/AdminMemReembedSection";
import { AdminEmbedQuotaSection } from "../components/AdminEmbedQuotaSection";
import { AdminEmbedQuotaPerFundSection } from "../components/AdminEmbedQuotaPerFundSection";
import { AdminDBPoolSection } from "../components/AdminDBPoolSection";
import { AdminAlertsSection } from "../components/AdminAlertsSection";
import { AdminLLMProvidersSection } from "../components/AdminLLMProvidersSection";
import AdminLLMObservabilitySection from "../components/AdminLLMObservabilitySection";
import AdminLockupSection from "../components/AdminLockupSection";
import AdminMarketImpactSection from "../components/AdminMarketImpactSection";
import AdminMarketStatusSection from "../components/AdminMarketStatusSection";
import AdminWSFeedSection from "../components/AdminWSFeedSection";
import AdminStopTriggerSection from "../components/AdminStopTriggerSection";

interface PlatformSettings {
  access_mode: "paid_open" | "free_open";
  default_team_interval_minutes: number;
  updated_at?: string;
}

interface AdminTeamMemberInfo {
  agent_id: string;
  member_id: string;
  name?: string;
  role: string;
  focus?: string;
  status: string;
  joined_at: string;
  model_provider?: string;
  model_name?: string;
}

interface AdminFundSummary {
  id: string;
  company_id: string;
  name: string;
  description?: string;
  trading_mode: string;
  status: string;
  total_assets: number;
  nav: number;
  market?: string;
  exchange?: string;
  asset_class?: string;
  base_currency?: string;
  benchmark_symbol?: string;
  primary_direction?: string;
  created_at: string;
  updated_at: string;
  team: AdminTeamMemberInfo[];
}

interface AdminCompanySummary {
  id: string;
  owner_user_id: string;
  name: string;
  description?: string;
  created_at: string;
  updated_at: string;
  funds: AdminFundSummary[];
}

interface AdminUserSummary {
  id: string;
  email: string;
  display_name: string;
  role: string;
  created_at: string;
  companies: AdminCompanySummary[];
}

interface AdminOverviewResponse {
  users: AdminUserSummary[];
}

interface AdminRechargeFormState {
  user_id: string;
  amount_minor: string;
  reference_id: string;
  note: string;
}

type AdminKYCStatusFilter = "pending" | "approved" | "rejected";

function statusLabel(status: string | undefined, language: "zh-CN" | "en-US"): string {
  const labels: Record<string, { zh: string; en: string }> = {
    active: { zh: "运行中", en: "Active" },
    paused: { zh: "已暂停", en: "Paused" },
    closed: { zh: "已关闭", en: "Closed" },
  };
  const value = (status ?? "").trim();
  if (!value) {
    return language === "en-US" ? "Unknown" : "未知";
  }
  const matched = labels[value];
  if (matched) {
    return language === "en-US" ? matched.en : matched.zh;
  }
  return value.replace(/[_-]+/g, " ");
}

function statusTone(status: string | undefined): string {
  switch ((status ?? "").trim()) {
    case "active":
      return "bg-emerald-50 text-emerald-700 ring-1 ring-emerald-200";
    case "paused":
      return "bg-amber-50 text-amber-700 ring-1 ring-amber-200";
    case "closed":
      return "bg-gray-100 text-gray-600 ring-1 ring-gray-200";
    default:
      return "bg-gray-100 text-gray-600 ring-1 ring-gray-200";
  }
}

function roleLabel(role: string, language: "zh-CN" | "en-US"): string {
  const labels: Record<string, { zh: string; en: string }> = {
    super_admin: { zh: "超级管理员", en: "Super admin" },
    user: { zh: "普通用户", en: "User" },
    pm: { zh: "组合经理", en: "PM" },
    trader: { zh: "交易员", en: "Trader" },
    researcher: { zh: "研究员", en: "Researcher" },
    risk: { zh: "风控", en: "Risk" },
  };
  const matched = labels[role];
  if (matched) {
    return language === "en-US" ? matched.en : matched.zh;
  }
  return role;
}

function roleTone(role: string): string {
  switch (role) {
    case "super_admin":
      return "bg-indigo-50 text-indigo-700 ring-1 ring-indigo-200";
    case "pm":
      return "bg-sky-50 text-sky-700 ring-1 ring-sky-200";
    case "trader":
      return "bg-violet-50 text-violet-700 ring-1 ring-violet-200";
    case "researcher":
      return "bg-cyan-50 text-cyan-700 ring-1 ring-cyan-200";
    case "risk":
      return "bg-rose-50 text-rose-700 ring-1 ring-rose-200";
    default:
      return "bg-gray-100 text-gray-700 ring-1 ring-gray-200";
  }
}

function kycLevelLabel(level: string | undefined, language: "zh-CN" | "en-US"): string {
  const labels: Record<string, { zh: string; en: string }> = {
    tier1_basic: { zh: "Tier 1 基础认证", en: "Tier 1 basic" },
    tier2_advanced: { zh: "Tier 2 高级认证", en: "Tier 2 advanced" },
  };
  const value = (level ?? "").trim();
  if (!value) {
    return language === "en-US" ? "Unknown level" : "未知等级";
  }
  const matched = labels[value];
  return matched ? (language === "en-US" ? matched.en : matched.zh) : value.replace(/[_-]+/g, " ");
}

function kycStatusTone(status: string | undefined): string {
  switch ((status ?? "").trim()) {
    case "approved":
      return "bg-emerald-50 text-emerald-700 ring-1 ring-emerald-200";
    case "rejected":
      return "bg-rose-50 text-rose-700 ring-1 ring-rose-200";
    case "pending":
      return "bg-amber-50 text-amber-700 ring-1 ring-amber-200";
    default:
      return "bg-gray-100 text-gray-600 ring-1 ring-gray-200";
  }
}

function countFunds(user: AdminUserSummary): number {
  return user.companies.reduce((sum, company) => sum + company.funds.length, 0);
}

function countTeamMembers(user: AdminUserSummary): number {
  return user.companies.reduce(
    (sum, company) => sum + company.funds.reduce((fundSum, fund) => fundSum + fund.team.length, 0),
    0,
  );
}

const Admin: React.FC = () => {
  const { language } = useAppPreferences();
  const [overview, setOverview] = useState<AdminOverviewResponse>({ users: [] });
  const [settings, setSettings] = useState<PlatformSettings | null>(null);
  const [settingsDraft, setSettingsDraft] = useState<PlatformSettings>({
    access_mode: "paid_open",
    default_team_interval_minutes: 15,
  });
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saveSuccess, setSaveSuccess] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [selectedUserId, setSelectedUserId] = useState("");
  const [rechargeForm, setRechargeForm] = useState<AdminRechargeFormState>({
    user_id: "",
    amount_minor: "",
    reference_id: "",
    note: "",
  });
  const [rechargeSaving, setRechargeSaving] = useState(false);
  const [rechargeError, setRechargeError] = useState<string | null>(null);
  const [rechargeSuccess, setRechargeSuccess] = useState<string | null>(null);
  const [kycApplications, setKYCApplications] = useState<AdminKYCApplication[]>([]);
  const [kycStatusFilter, setKYCStatusFilter] = useState<AdminKYCStatusFilter>("pending");
  const [kycDecisionId, setKYCDecisionId] = useState<string | null>(null);
  const [kycRejectReasons, setKYCRejectReasons] = useState<Record<string, string>>({});
  const [kycError, setKYCError] = useState<string | null>(null);
  const [kycSuccess, setKYCSuccess] = useState<string | null>(null);
  const [marketHealth, setMarketHealth] = useState<Record<string, AdminMarketDataProviderHealth>>({});
  const [marketHealthLoading, setMarketHealthLoading] = useState(false);
  const [marketHealthRefreshedAt, setMarketHealthRefreshedAt] = useState<string | null>(null);
  const [marketHealthError, setMarketHealthError] = useState<string | null>(null);

  // F7 — workflow scheduler dashboard state.
  const [schedulerSnapshot, setSchedulerSnapshot] = useState<FundSchedulerSnapshot | null>(null);
  const [schedulerLoading, setSchedulerLoading] = useState(false);
  const [schedulerError, setSchedulerError] = useState<string | null>(null);
  const [schedulerRefreshedAt, setSchedulerRefreshedAt] = useState<string | null>(null);
  const [triggeringFundId, setTriggeringFundId] = useState<string | null>(null);
  const [triggerNotice, setTriggerNotice] = useState<string | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Admin console",
            loading: "Loading platform and tenant overview...",
            loadError: "Failed to load admin data",
            retry: "Retry",
            subtitle: "Inspect all users, companies, funds, and team members, and switch the platform between paid and free-open modes.",
            users: "Users",
            companies: "Companies",
            funds: "Funds",
            teamMembers: "Team members",
            platformSettings: "Platform settings",
            platformHint: "When free-open is enabled, the effective plan is elevated to unlimited enterprise capabilities immediately.",
            updatedAt: "Last updated",
            accessMode: "Access mode",
            paidOpen: "Paid open",
            freeOpen: "Free open",
            interval: "Default team interval (minutes)",
            save: "Save platform settings",
            saving: "Saving...",
            saveSuccess: "Platform settings saved. Free-open mode now applies unlimited capabilities immediately.",
            saveError: "Failed to save platform settings",
            rechargeTitle: "Admin recharge",
            rechargeHint: "Pick a user from the overview and credit the wallet in minor units. The underlying ledger currency currently remains USD.",
            rechargeUser: "Target user",
            rechargeAmount: "Amount (minor units)",
            rechargeReference: "Reference ID",
            rechargeNote: "Note",
            recharge: "Recharge wallet",
            recharging: "Recharging...",
            rechargeSuccess: "Wallet recharge applied. The user will see the new balance after refreshing the wallet page.",
            rechargeUserRequired: "Please select the target user.",
            rechargeAmountInvalid: "Please enter a valid recharge amount greater than 0.",
            rechargeError: "Wallet recharge failed",
            kycTitle: "KYC review queue",
            kycHint: "Review pending identity applications before enabling live trading, marketplace publishing, or wallet recharge.",
            kycEmpty: "No KYC applications in this status.",
            kycStatusPending: "Pending",
            kycStatusApproved: "Approved",
            kycStatusRejected: "Rejected",
            kycApprove: "Approve",
            kycReject: "Reject",
            kycReviewing: "Reviewing...",
            kycReason: "Rejection reason",
            kycReasonRequired: "Please enter a rejection reason before rejecting this application.",
            kycDecisionError: "Failed to update KYC application",
            kycDecisionSuccess: "KYC application updated.",
            kycSubmittedAt: "Submitted",
            kycDocument: "Document",
            kycApplicant: "Applicant",
            kycAttachments: "Attachments",
            overviewTitle: "Users and tenant overview",
            overviewHint: "Use the left rail to switch users. The detail panel focuses on one tenant tree at a time.",
            emptyUsers: "There is no user data yet.",
            registeredAt: "Registered",
            owner: "Owner",
            noCompany: "This user does not have any companies yet.",
            noFund: "This company does not have any funds yet.",
            noTeam: "There are no team members in this fund yet.",
            noDescription: "No description",
            mode: "Mode",
            nav: "NAV",
            totalAssets: "Total assets",
            market: "Market",
            exchange: "Exchange",
            direction: "Direction",
            teamTitle: "Team members",
            unknown: "Unknown",
            userListTitle: "User list",
            userListHint: "Select a user to inspect the owned companies, funds, and trading team.",
            userId: "User ID",
            companyUpdatedAt: "Updated",
            benchmark: "Benchmark",
            assetClass: "Asset class",
            baseCurrency: "Base currency",
            joinedAt: "Joined",
            selectedUserTitle: "Selected user",
            selectUser: "Select a user from the left to inspect details.",
            marketHealthTitle: "Market data provider health",
            marketHealthHint:
              "Per-provider circuit-breaker and call counters. Use this to spot upstream outages before they reach traders.",
            marketHealthEmpty: "No provider activity recorded yet. Trigger a quote or news fetch and refresh.",
            marketHealthLoadError: "Failed to load market data provider health",
            marketHealthRefresh: "Refresh",
            marketHealthRefreshing: "Refreshing...",
            marketHealthAuto: "Auto-refreshes every 15s.",
            marketHealthLastFetched: "Last fetched",
            marketHealthColumnProvider: "Provider",
            marketHealthColumnCalls: "Calls",
            marketHealthColumnSuccess: "Success",
            marketHealthColumnFailures: "Failures",
            marketHealthColumnConsecutive: "Consec. fails",
            marketHealthColumnLatency: "Latency (ms)",
            marketHealthColumnCircuit: "Circuit",
            marketHealthColumnLastError: "Last error",
            marketHealthCircuitClosed: "Closed",
            marketHealthCircuitOpenUntil: "Open until",
          }
        : {
            title: "管理员后台",
            loading: "正在加载平台与租户概览...",
            loadError: "加载管理员数据失败",
            retry: "重试",
            subtitle: "集中查看当前所有用户、公司、基金和团队成员摘要，并控制平台免费开放或收费开放状态。",
            users: "用户数",
            companies: "公司数",
            funds: "基金数",
            teamMembers: "团队成员数",
            platformSettings: "平台设置",
            platformHint: "免费开放时，订阅有效套餐会直接按企业版无限制能力生效。",
            updatedAt: "最近更新时间",
            accessMode: "开放模式",
            paidOpen: "收费开放",
            freeOpen: "免费开放",
            interval: "默认团队间隔（分钟）",
            save: "保存平台设置",
            saving: "保存中...",
            saveSuccess: "平台设置已保存。免费开放模式会立即把有效套餐切到无限制能力。",
            saveError: "保存平台设置失败",
            rechargeTitle: "管理员充值",
            rechargeHint: "直接从总览里选择目标用户，为对应钱包入账，金额单位为分，当前仅支持 USD 账本。",
            rechargeUser: "目标用户",
            rechargeAmount: "充值金额（分）",
            rechargeReference: "引用编号",
            rechargeNote: "备注",
            recharge: "执行充值",
            recharging: "充值中...",
            rechargeSuccess: "钱包充值已入账。用户刷新钱包页后即可看到最新余额和流水。",
            rechargeUserRequired: "请选择目标用户。",
            rechargeAmountInvalid: "请输入合法的充值金额（分），且必须大于 0。",
            rechargeError: "钱包充值失败",
            kycTitle: "KYC 审核队列",
            kycHint: "审核待处理的实名申请，通过后用户才可进行实盘交易、市场发布或钱包充值。",
            kycEmpty: "当前状态下暂无 KYC 申请。",
            kycStatusPending: "待审核",
            kycStatusApproved: "已通过",
            kycStatusRejected: "已拒绝",
            kycApprove: "通过",
            kycReject: "拒绝",
            kycReviewing: "处理中...",
            kycReason: "拒绝原因",
            kycReasonRequired: "拒绝前请先填写拒绝原因。",
            kycDecisionError: "更新 KYC 申请失败",
            kycDecisionSuccess: "KYC 申请已更新。",
            kycSubmittedAt: "提交于",
            kycDocument: "证件",
            kycApplicant: "申请账号",
            kycAttachments: "附件",
            overviewTitle: "用户与租户概览",
            overviewHint: "左侧切换用户，右侧只聚焦当前选中的租户树，避免整页无限拉长。",
            emptyUsers: "当前还没有用户数据。",
            registeredAt: "注册于",
            owner: "owner",
            noCompany: "该用户当前还没有公司。",
            noFund: "该公司还没有基金。",
            noTeam: "当前没有团队成员。",
            noDescription: "暂无描述",
            mode: "模式",
            nav: "净值",
            totalAssets: "总资产",
            market: "市场",
            exchange: "交易所",
            direction: "主方向",
            teamTitle: "团队成员",
            unknown: "未知",
            userListTitle: "用户列表",
            userListHint: "点击左侧用户，查看其名下公司、基金和团队成员。",
            userId: "用户 ID",
            companyUpdatedAt: "更新时间",
            benchmark: "基准标的",
            assetClass: "资产类别",
            baseCurrency: "基础货币",
            joinedAt: "加入时间",
            selectedUserTitle: "当前选中用户",
            selectUser: "请先从左侧选择一个用户查看详情。",
            marketHealthTitle: "行情数据源健康",
            marketHealthHint:
              "每个上游 provider 的调用计数与熔断状态。线上交易团队出现报价异常前，先在这里就能看到苗头。",
            marketHealthEmpty: "尚无 provider 调用记录。触发一次报价或新闻拉取再刷新即可。",
            marketHealthLoadError: "加载行情数据源健康信息失败",
            marketHealthRefresh: "刷新",
            marketHealthRefreshing: "刷新中...",
            marketHealthAuto: "每 15 秒自动刷新一次。",
            marketHealthLastFetched: "最近拉取时间",
            marketHealthColumnProvider: "数据源",
            marketHealthColumnCalls: "调用",
            marketHealthColumnSuccess: "成功",
            marketHealthColumnFailures: "失败",
            marketHealthColumnConsecutive: "连续失败",
            marketHealthColumnLatency: "延迟 (ms)",
            marketHealthColumnCircuit: "熔断",
            marketHealthColumnLastError: "上次错误",
            marketHealthCircuitClosed: "已闭合",
            marketHealthCircuitOpenUntil: "断路至",
          },
    [language],
  );

  const loadData = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [overviewRes, settingsRes, kycRes] = await Promise.all([
        apiGet<AdminOverviewResponse>("/api/admin/overview"),
        apiGet<PlatformSettings>("/api/admin/platform-settings"),
        fetchAdminKYCApplications(kycStatusFilter, 50),
      ]);
      setOverview(overviewRes);
      setSettings(settingsRes);
      setSettingsDraft(settingsRes);
      setKYCApplications(kycRes);
      setKYCError(null);
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError, kycStatusFilter]);

  const loadMarketHealth = useCallback(async () => {
    setMarketHealthLoading(true);
    try {
      const res = await apiGet<AdminMarketDataHealthResponse>("/api/admin/marketdata/health");
      setMarketHealth(res.providers ?? {});
      setMarketHealthError(null);
      setMarketHealthRefreshedAt(new Date().toISOString());
    } catch (err) {
      setMarketHealthError(formatApiError(err, copy.marketHealthLoadError));
    } finally {
      setMarketHealthLoading(false);
    }
  }, [copy.marketHealthLoadError]);

  useEffect(() => {
    void loadMarketHealth();
    const interval = window.setInterval(() => {
      void loadMarketHealth();
    }, 15000);
    return () => window.clearInterval(interval);
  }, [loadMarketHealth]);

  const loadSchedulerSnapshot = useCallback(async () => {
    setSchedulerLoading(true);
    try {
      const snap = await fetchWorkflowSchedulerSnapshot();
      setSchedulerSnapshot(snap);
      setSchedulerError(null);
      setSchedulerRefreshedAt(new Date().toISOString());
    } catch (err) {
      setSchedulerError(formatApiError(err, language === "zh-CN" ? "调度器信息加载失败" : "Failed to load scheduler snapshot"));
    } finally {
      setSchedulerLoading(false);
    }
  }, [language]);

  useEffect(() => {
    void loadSchedulerSnapshot();
    const interval = window.setInterval(() => {
      void loadSchedulerSnapshot();
    }, 20000);
    return () => window.clearInterval(interval);
  }, [loadSchedulerSnapshot]);

  const handleTriggerFund = useCallback(
    async (fundId: string) => {
      setTriggeringFundId(fundId);
      setTriggerNotice(null);
      try {
        const result = await triggerFundWorkflow(fundId);
        setTriggerNotice(
          language === "zh-CN"
            ? `已触发 ${fundId} · 状态=${result.state}${result.step ? ` · 步骤=${result.step}` : ""}`
            : `Triggered ${fundId} · state=${result.state}${result.step ? ` · step=${result.step}` : ""}`,
        );
        await loadSchedulerSnapshot();
      } catch (err) {
        setTriggerNotice(formatApiError(err, language === "zh-CN" ? "触发失败" : "Trigger failed"));
      } finally {
        setTriggeringFundId(null);
      }
    },
    [language, loadSchedulerSnapshot],
  );

  useEffect(() => {
    void loadData();
  }, [loadData]);

  useEffect(() => {
    if (overview.users.length === 0) {
      setSelectedUserId("");
      setRechargeForm((current) => ({ ...current, user_id: "" }));
      return;
    }

    setSelectedUserId((current) => (overview.users.some((user) => user.id === current) ? current : overview.users[0].id));
    setRechargeForm((current) => {
      if (current.user_id && overview.users.some((user) => user.id === current.user_id)) {
        return current;
      }
      return { ...current, user_id: overview.users[0].id };
    });
  }, [overview.users]);

  const totals = useMemo(() => {
    let companyCount = 0;
    let fundCount = 0;
    let teamCount = 0;
    for (const user of overview.users) {
      companyCount += user.companies.length;
      for (const company of user.companies) {
        fundCount += company.funds.length;
        for (const fund of company.funds) {
          teamCount += fund.team.length;
        }
      }
    }
    return { userCount: overview.users.length, companyCount, fundCount, teamCount };
  }, [overview.users]);

  const selectedUser = useMemo(
    () => overview.users.find((user) => user.id === selectedUserId) ?? overview.users[0] ?? null,
    [overview.users, selectedUserId],
  );

  const selectedUserMetrics = useMemo(
    () =>
      selectedUser
        ? {
            companies: selectedUser.companies.length,
            funds: countFunds(selectedUser),
            teamMembers: countTeamMembers(selectedUser),
          }
        : { companies: 0, funds: 0, teamMembers: 0 },
    [selectedUser],
  );

  const handleSave = useCallback(async () => {
    setSaving(true);
    setSaveError(null);
    setSaveSuccess(null);
    try {
      const next = await apiPut<PlatformSettings>("/api/admin/platform-settings", settingsDraft);
      setSettings(next);
      setSettingsDraft(next);
      setSaveSuccess(copy.saveSuccess);
    } catch (err) {
      setSaveError(formatApiError(err, copy.saveError));
    } finally {
      setSaving(false);
    }
  }, [copy.saveError, copy.saveSuccess, settingsDraft]);

  const handleRecharge = useCallback(async () => {
    const userId = rechargeForm.user_id.trim();
    const amountMinor = Number(rechargeForm.amount_minor);
    if (!userId) {
      setRechargeError(copy.rechargeUserRequired);
      setRechargeSuccess(null);
      return;
    }
    if (!Number.isFinite(amountMinor) || amountMinor <= 0) {
      setRechargeError(copy.rechargeAmountInvalid);
      setRechargeSuccess(null);
      return;
    }

    setRechargeSaving(true);
    setRechargeError(null);
    setRechargeSuccess(null);
    try {
      await apiPost("/api/admin/wallets/recharge", {
        user_id: userId,
        amount_minor: Math.round(amountMinor),
        currency: "USD",
        reference_id: rechargeForm.reference_id.trim() || undefined,
        note: rechargeForm.note.trim() || undefined,
      });
      setRechargeSuccess(copy.rechargeSuccess);
      setRechargeForm((current) => ({
        ...current,
        amount_minor: "",
        reference_id: "",
        note: "",
      }));
    } catch (err) {
      setRechargeError(formatApiError(err, copy.rechargeError));
    } finally {
      setRechargeSaving(false);
    }
  }, [copy.rechargeAmountInvalid, copy.rechargeError, copy.rechargeSuccess, copy.rechargeUserRequired, rechargeForm]);

  const handleKYCDecision = useCallback(
    async (application: AdminKYCApplication, action: "approve" | "reject") => {
      const reason = (kycRejectReasons[application.id] ?? "").trim();
      if (action === "reject" && !reason) {
        setKYCError(copy.kycReasonRequired);
        setKYCSuccess(null);
        return;
      }
      setKYCDecisionId(application.id);
      setKYCError(null);
      setKYCSuccess(null);
      try {
        await decideAdminKYCApplication(application.id, action, action === "reject" ? reason : undefined);
        setKYCSuccess(copy.kycDecisionSuccess);
        setKYCRejectReasons((current) => {
          const next = { ...current };
          delete next[application.id];
          return next;
        });
        setKYCApplications(await fetchAdminKYCApplications(kycStatusFilter, 50));
      } catch (err) {
        setKYCError(formatApiError(err, copy.kycDecisionError));
      } finally {
        setKYCDecisionId(null);
      }
    },
    [copy.kycDecisionError, copy.kycDecisionSuccess, copy.kycReasonRequired, kycRejectReasons, kycStatusFilter],
  );

  if (loading) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <div className="rounded-lg border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <div className="rounded-lg border border-red-200 bg-red-50 p-6 text-sm text-red-700">
          <p>{error}</p>
          <button onClick={() => void loadData()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
            {copy.retry}
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-5">
      <section className="rounded-2xl border border-gray-200 bg-white px-5 py-5 shadow-sm">
        <div className="flex flex-col gap-3 xl:flex-row xl:items-end xl:justify-between">
          <div>
            <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
            <p className="mt-2 max-w-3xl text-sm text-gray-500">{copy.subtitle}</p>
          </div>
          {settings?.updated_at ? (
            <p className="text-xs text-gray-500">
              {copy.updatedAt}：{formatDateTimeForLanguage(settings.updated_at, language)}
            </p>
          ) : null}
        </div>
      </section>

      <section className="grid grid-cols-2 gap-4 xl:grid-cols-4">
        <div className="rounded-2xl border border-gray-200 bg-white px-4 py-4 shadow-sm">
          <p className="text-xs font-medium uppercase tracking-wide text-gray-500">{copy.users}</p>
          <p className="mt-2 text-2xl font-bold text-gray-900">{totals.userCount}</p>
        </div>
        <div className="rounded-2xl border border-gray-200 bg-white px-4 py-4 shadow-sm">
          <p className="text-xs font-medium uppercase tracking-wide text-gray-500">{copy.companies}</p>
          <p className="mt-2 text-2xl font-bold text-gray-900">{totals.companyCount}</p>
        </div>
        <div className="rounded-2xl border border-gray-200 bg-white px-4 py-4 shadow-sm">
          <p className="text-xs font-medium uppercase tracking-wide text-gray-500">{copy.funds}</p>
          <p className="mt-2 text-2xl font-bold text-gray-900">{totals.fundCount}</p>
        </div>
        <div className="rounded-2xl border border-gray-200 bg-white px-4 py-4 shadow-sm">
          <p className="text-xs font-medium uppercase tracking-wide text-gray-500">{copy.teamMembers}</p>
          <p className="mt-2 text-2xl font-bold text-gray-900">{totals.teamCount}</p>
        </div>
      </section>

      <section className="grid grid-cols-1 gap-5 2xl:grid-cols-[minmax(0,1.6fr)_380px]">
        <div className="space-y-4">
          <div className="rounded-2xl border border-gray-200 bg-white px-5 py-4 shadow-sm">
            <h2 className="text-lg font-semibold text-gray-900">{copy.overviewTitle}</h2>
            <p className="mt-1 text-sm text-gray-500">{copy.overviewHint}</p>
          </div>

          <div className="grid grid-cols-1 gap-5 xl:grid-cols-[320px_minmax(0,1fr)]">
            <aside className="space-y-3">
              <div className="rounded-2xl border border-gray-200 bg-white px-4 py-4 shadow-sm">
                <p className="text-xs font-semibold uppercase tracking-wider text-gray-500">{copy.userListTitle}</p>
                <p className="mt-1 text-sm text-gray-500">{copy.userListHint}</p>
              </div>
              <div className="max-h-[72vh] overflow-y-auto rounded-2xl border border-gray-200 bg-white shadow-sm">
                {overview.users.length === 0 ? (
                  <div className="px-4 py-8 text-center text-sm text-gray-500">{copy.emptyUsers}</div>
                ) : (
                  <div className="divide-y divide-gray-100">
                    {overview.users.map((user) => {
                      const isSelected = selectedUser?.id === user.id;
                      const fundCount = countFunds(user);
                      const teamCount = countTeamMembers(user);
                      return (
                        <button
                          key={user.id}
                          type="button"
                          onClick={() => {
                            setSelectedUserId(user.id);
                            setRechargeForm((current) => ({ ...current, user_id: user.id }));
                          }}
                          className={`w-full border-l-4 px-4 py-3 text-left transition hover:bg-gray-50 ${
                            isSelected ? "border-indigo-500 bg-indigo-50/70" : "border-transparent"
                          }`}
                        >
                          <div className="flex items-start justify-between gap-3">
                            <div className="min-w-0">
                              <p className="truncate text-sm font-semibold text-gray-900">{user.display_name || user.email || user.id}</p>
                              <p className="mt-1 truncate text-xs text-gray-500">{user.email || user.id}</p>
                            </div>
                            <span className={`rounded-full px-2.5 py-1 text-[11px] font-medium ${roleTone(user.role)}`}>
                              {roleLabel(user.role, language)}
                            </span>
                          </div>
                          <div className="mt-2 flex flex-wrap gap-2 text-[11px] text-gray-500">
                            <span className="rounded-full bg-gray-100 px-2.5 py-1">{user.companies.length} {copy.companies}</span>
                            <span className="rounded-full bg-gray-100 px-2.5 py-1">{fundCount} {copy.funds}</span>
                            <span className="rounded-full bg-gray-100 px-2.5 py-1">{teamCount} {copy.teamMembers}</span>
                          </div>
                          <p className="mt-2 text-[11px] text-gray-500">
                            {copy.registeredAt} · {formatDateTimeForLanguage(user.created_at, language)}
                          </p>
                        </button>
                      );
                    })}
                  </div>
                )}
              </div>
            </aside>

            <div className="space-y-4">
              {selectedUser ? (
                <>
                  <section className="rounded-2xl border border-gray-200 bg-white px-5 py-5 shadow-sm">
                    <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
                      <div>
                        <p className="text-xs font-semibold uppercase tracking-wider text-gray-500">{copy.selectedUserTitle}</p>
                        <h3 className="mt-2 text-xl font-semibold text-gray-900">{selectedUser.display_name || selectedUser.email || selectedUser.id}</h3>
                        <p className="mt-1 text-sm text-gray-500">{selectedUser.email || copy.unknown}</p>
                        <p className="mt-3 font-mono text-xs text-gray-400">{copy.userId}: {selectedUser.id}</p>
                      </div>
                      <div className="flex flex-wrap gap-2">
                        <span className={`rounded-full px-3 py-1 text-xs font-medium ${roleTone(selectedUser.role)}`}>
                          {roleLabel(selectedUser.role, language)}
                        </span>
                        <span className="rounded-full bg-gray-100 px-3 py-1 text-xs font-medium text-gray-600">
                          {copy.registeredAt} {formatDateTimeForLanguage(selectedUser.created_at, language)}
                        </span>
                      </div>
                    </div>
                    <div className="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-3">
                      <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3">
                        <p className="text-xs text-gray-500">{copy.companies}</p>
                        <p className="mt-1 text-lg font-semibold text-gray-900">{selectedUserMetrics.companies}</p>
                      </div>
                      <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3">
                        <p className="text-xs text-gray-500">{copy.funds}</p>
                        <p className="mt-1 text-lg font-semibold text-gray-900">{selectedUserMetrics.funds}</p>
                      </div>
                      <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3">
                        <p className="text-xs text-gray-500">{copy.teamMembers}</p>
                        <p className="mt-1 text-lg font-semibold text-gray-900">{selectedUserMetrics.teamMembers}</p>
                      </div>
                    </div>
                  </section>

                  {selectedUser.companies.length === 0 ? (
                    <div className="rounded-2xl border border-dashed border-gray-200 bg-white px-5 py-8 text-sm text-gray-500 shadow-sm">
                      {copy.noCompany}
                    </div>
                  ) : (
                    selectedUser.companies.map((company) => (
                      <details key={company.id} className="rounded-2xl border border-gray-200 bg-white shadow-sm" open>
                        <summary className="flex cursor-pointer list-none flex-col gap-3 px-5 py-4 lg:flex-row lg:items-start lg:justify-between">
                          <div>
                            <h4 className="text-base font-semibold text-gray-900">{company.name}</h4>
                            <p className="mt-1 text-sm text-gray-500">{company.description || copy.noDescription}</p>
                          </div>
                          <div className="flex flex-wrap gap-2 text-xs text-gray-500 lg:justify-end">
                            <span className="rounded-full bg-gray-100 px-3 py-1">{company.funds.length} {copy.funds}</span>
                            <span className="rounded-full bg-gray-100 px-3 py-1">{copy.companyUpdatedAt} {formatDateTimeForLanguage(company.updated_at, language)}</span>
                          </div>
                        </summary>
                        <div className="border-t border-gray-100 px-5 py-5">
                          {company.funds.length === 0 ? (
                            <div className="rounded-xl border border-dashed border-gray-200 bg-gray-50 p-4 text-sm text-gray-500">{copy.noFund}</div>
                          ) : (
                            <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
                              {company.funds.map((fund) => (
                                <section key={fund.id} className="rounded-2xl border border-gray-200 bg-gray-50 px-4 py-4">
                                  <div className="flex flex-col gap-3 lg:flex-row lg:items-start lg:justify-between">
                                    <div>
                                      <div className="flex flex-wrap items-center gap-2">
                                        <h5 className="text-base font-semibold text-gray-900">{fund.name}</h5>
                                        <span className={`rounded-full px-2.5 py-1 text-[11px] font-medium ${statusTone(fund.status)}`}>
                                          {statusLabel(fund.status, language)}
                                        </span>
                                      </div>
                                      <p className="mt-1 text-sm text-gray-500">{fund.description || copy.noDescription}</p>
                                    </div>
                                    <div className="flex flex-wrap gap-2 text-[11px] text-gray-500 lg:justify-end">
                                      <span className="rounded-full bg-white px-2.5 py-1 ring-1 ring-gray-200">{copy.mode} {fund.trading_mode}</span>
                                      <span className="rounded-full bg-white px-2.5 py-1 ring-1 ring-gray-200">{copy.nav} {fund.nav.toFixed(4)}</span>
                                    </div>
                                  </div>

                                  <div className="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-2">
                                    <div className="rounded-xl border border-gray-200 bg-white px-3 py-3 text-sm">
                                      <p className="text-xs text-gray-500">{copy.totalAssets}</p>
                                      <p className="mt-1 font-medium text-gray-900">{formatMoneyForLanguage(fund.total_assets, fund.base_currency || "USD", language)}</p>
                                    </div>
                                    <div className="rounded-xl border border-gray-200 bg-white px-3 py-3 text-sm">
                                      <p className="text-xs text-gray-500">{copy.market}</p>
                                      <p className="mt-1 font-medium text-gray-900">{fund.market || "-"}</p>
                                    </div>
                                    <div className="rounded-xl border border-gray-200 bg-white px-3 py-3 text-sm">
                                      <p className="text-xs text-gray-500">{copy.exchange}</p>
                                      <p className="mt-1 font-medium text-gray-900">{fund.exchange || "-"}</p>
                                    </div>
                                    <div className="rounded-xl border border-gray-200 bg-white px-3 py-3 text-sm">
                                      <p className="text-xs text-gray-500">{copy.direction}</p>
                                      <p className="mt-1 font-medium text-gray-900">{fund.primary_direction || "-"}</p>
                                    </div>
                                    <div className="rounded-xl border border-gray-200 bg-white px-3 py-3 text-sm">
                                      <p className="text-xs text-gray-500">{copy.assetClass}</p>
                                      <p className="mt-1 font-medium text-gray-900">{fund.asset_class || "-"}</p>
                                    </div>
                                    <div className="rounded-xl border border-gray-200 bg-white px-3 py-3 text-sm">
                                      <p className="text-xs text-gray-500">{copy.baseCurrency}</p>
                                      <p className="mt-1 font-medium text-gray-900">{fund.base_currency || "-"}</p>
                                    </div>
                                  </div>

                                  <div className="mt-3 rounded-xl border border-gray-200 bg-white px-3 py-3 text-sm">
                                    <p className="text-xs text-gray-500">{copy.benchmark}</p>
                                    <p className="mt-1 font-medium text-gray-900">{fund.benchmark_symbol || "-"}</p>
                                  </div>

                                  <div className="mt-4">
                                    <div className="flex items-center justify-between gap-3">
                                      <p className="text-sm font-medium text-gray-800">{copy.teamTitle}</p>
                                      <span className="rounded-full bg-white px-2.5 py-1 text-[11px] text-gray-500 ring-1 ring-gray-200">
                                        {fund.team.length} {copy.teamMembers}
                                      </span>
                                    </div>
                                    {fund.team.length === 0 ? (
                                      <div className="mt-3 rounded-xl border border-dashed border-gray-200 bg-white p-4 text-sm text-gray-500">{copy.noTeam}</div>
                                    ) : (
                                      <div className="mt-3 divide-y divide-gray-100 overflow-hidden rounded-xl border border-gray-200 bg-white">
                                        {fund.team.map((member) => (
                                          <div key={member.member_id} className="flex flex-col gap-2 px-3 py-3 md:flex-row md:items-center md:justify-between">
                                            <div className="min-w-0">
                                              <div className="flex flex-wrap items-center gap-2">
                                                <p className="truncate text-sm font-medium text-gray-900">{member.name || member.agent_id}</p>
                                                <span className={`rounded-full px-2.5 py-1 text-[11px] font-medium ${roleTone(member.role)}`}>
                                                  {roleLabel(member.role, language)}
                                                </span>
                                                <span className={`rounded-full px-2.5 py-1 text-[11px] font-medium ${statusTone(member.status)}`}>
                                                  {statusLabel(member.status, language)}
                                                </span>
                                                {member.focus ? (
                                                  <span className="rounded-full bg-gray-100 px-2.5 py-1 text-[11px] text-gray-600">{member.focus}</span>
                                                ) : null}
                                              </div>
                                              <p className="mt-1 text-xs text-gray-500">
                                                {copy.joinedAt} · {formatDateTimeForLanguage(member.joined_at, language)}
                                              </p>
                                            </div>
                                            <p className="text-xs text-gray-500 md:text-right">
                                              {member.model_provider || "-"} / {member.model_name || "-"}
                                            </p>
                                          </div>
                                        ))}
                                      </div>
                                    )}
                                  </div>
                                </section>
                              ))}
                            </div>
                          )}
                        </div>
                      </details>
                    ))
                  )}
                </>
              ) : (
                <div className="rounded-2xl border border-dashed border-gray-200 bg-white px-5 py-8 text-sm text-gray-500 shadow-sm">
                  {copy.selectUser}
                </div>
              )}
            </div>
          </div>
        </div>

        <aside className="space-y-4 2xl:sticky 2xl:top-4 2xl:self-start">
          <section className="rounded-2xl border border-indigo-200 bg-indigo-50 px-5 py-5 shadow-sm">
            <div>
              <h2 className="text-lg font-semibold text-indigo-900">{copy.platformSettings}</h2>
              <p className="mt-2 text-sm text-indigo-700">{copy.platformHint}</p>
            </div>
            <div className="mt-4 space-y-4">
              <label className="block text-sm font-medium text-indigo-900">
                {copy.accessMode}
                <select
                  value={settingsDraft.access_mode}
                  onChange={(event) => setSettingsDraft((current) => ({ ...current, access_mode: event.target.value as PlatformSettings["access_mode"] }))}
                  className="mt-2 w-full rounded-xl border border-indigo-200 bg-white px-4 py-2.5 text-sm text-gray-900 outline-none focus:border-indigo-500"
                >
                  <option value="paid_open">{copy.paidOpen}</option>
                  <option value="free_open">{copy.freeOpen}</option>
                </select>
              </label>
              <label className="block text-sm font-medium text-indigo-900">
                {copy.interval}
                <input
                  type="number"
                  min={5}
                  max={1440}
                  step={5}
                  value={settingsDraft.default_team_interval_minutes}
                  onChange={(event) =>
                    setSettingsDraft((current) => ({
                      ...current,
                      default_team_interval_minutes: Number(event.target.value) || 15,
                    }))
                  }
                  className="mt-2 w-full rounded-xl border border-indigo-200 bg-white px-4 py-2.5 text-sm text-gray-900 outline-none focus:border-indigo-500"
                />
              </label>
            </div>
            <div className="mt-4 flex flex-wrap items-center gap-3">
              <button
                onClick={() => void handleSave()}
                disabled={saving}
                className="rounded-xl bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {saving ? copy.saving : copy.save}
              </button>
              {saveError ? <p className="text-sm text-red-600">{saveError}</p> : null}
              {saveSuccess ? <p className="text-sm text-emerald-700">{saveSuccess}</p> : null}
            </div>
          </section>

          <section className="rounded-2xl border border-emerald-200 bg-emerald-50 px-5 py-5 shadow-sm">
            <div>
              <h2 className="text-lg font-semibold text-emerald-900">{copy.rechargeTitle}</h2>
              <p className="mt-2 text-sm text-emerald-700">{copy.rechargeHint}</p>
            </div>
            <div className="mt-4 space-y-4">
              <label className="block text-sm font-medium text-emerald-900">
                {copy.rechargeUser}
                <select
                  value={rechargeForm.user_id}
                  onChange={(event) => setRechargeForm((current) => ({ ...current, user_id: event.target.value }))}
                  className="mt-2 w-full rounded-xl border border-emerald-200 bg-white px-4 py-2.5 text-sm text-gray-900 outline-none focus:border-emerald-500"
                >
                  <option value="">--</option>
                  {overview.users.map((user) => (
                    <option key={user.id} value={user.id}>
                      {(user.display_name || user.email || user.id).slice(0, 60)}
                    </option>
                  ))}
                </select>
              </label>
              <label className="block text-sm font-medium text-emerald-900">
                {copy.rechargeAmount}
                <input
                  type="number"
                  min={1}
                  step={1}
                  value={rechargeForm.amount_minor}
                  onChange={(event) => setRechargeForm((current) => ({ ...current, amount_minor: event.target.value }))}
                  className="mt-2 w-full rounded-xl border border-emerald-200 bg-white px-4 py-2.5 text-sm text-gray-900 outline-none focus:border-emerald-500"
                  placeholder="10000"
                />
              </label>
              <label className="block text-sm font-medium text-emerald-900">
                {copy.rechargeReference}
                <input
                  value={rechargeForm.reference_id}
                  onChange={(event) => setRechargeForm((current) => ({ ...current, reference_id: event.target.value }))}
                  className="mt-2 w-full rounded-xl border border-emerald-200 bg-white px-4 py-2.5 text-sm text-gray-900 outline-none focus:border-emerald-500"
                  placeholder="manual-topup-001"
                />
              </label>
              <label className="block text-sm font-medium text-emerald-900">
                {copy.rechargeNote}
                <input
                  value={rechargeForm.note}
                  onChange={(event) => setRechargeForm((current) => ({ ...current, note: event.target.value }))}
                  className="mt-2 w-full rounded-xl border border-emerald-200 bg-white px-4 py-2.5 text-sm text-gray-900 outline-none focus:border-emerald-500"
                  placeholder={language === "en-US" ? "Test recharge" : "测试充值"}
                />
              </label>
            </div>
            <div className="mt-4 flex flex-wrap items-center gap-3">
              <button
                onClick={() => void handleRecharge()}
                disabled={rechargeSaving}
                className="rounded-xl bg-emerald-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-emerald-700 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {rechargeSaving ? copy.recharging : copy.recharge}
              </button>
              {rechargeError ? <p className="text-sm text-red-600">{rechargeError}</p> : null}
              {rechargeSuccess ? <p className="text-sm text-emerald-700">{rechargeSuccess}</p> : null}
            </div>
          </section>

          <section className="rounded-2xl border border-amber-200 bg-amber-50 px-5 py-5 shadow-sm">
            <div>
              <h2 className="text-lg font-semibold text-amber-900">{copy.kycTitle}</h2>
              <p className="mt-2 text-sm text-amber-700">{copy.kycHint}</p>
            </div>
            <div className="mt-4 flex flex-wrap gap-2">
              {([
                ["pending", copy.kycStatusPending],
                ["approved", copy.kycStatusApproved],
                ["rejected", copy.kycStatusRejected],
              ] as Array<[AdminKYCStatusFilter, string]>).map(([status, label]) => (
                <button
                  key={status}
                  type="button"
                  onClick={() => {
                    setKYCStatusFilter(status);
                    setKYCError(null);
                    setKYCSuccess(null);
                  }}
                  className={`rounded-full px-3 py-1.5 text-xs font-semibold ring-1 transition ${
                    kycStatusFilter === status
                      ? "bg-amber-600 text-white ring-amber-600"
                      : "bg-white text-amber-700 ring-amber-200 hover:bg-amber-100"
                  }`}
                >
                  {label}
                </button>
              ))}
            </div>
            <div className="mt-4 space-y-3">
              {kycApplications.length === 0 ? (
                <div className="rounded-xl border border-dashed border-amber-200 bg-white/70 p-4 text-sm text-amber-700">{copy.kycEmpty}</div>
              ) : (
                kycApplications.map((application) => {
                  const isReviewing = kycDecisionId === application.id;
                  return (
                    <article key={application.id} className="rounded-2xl border border-amber-200 bg-white px-4 py-4 shadow-sm">
                      <div className="flex items-start justify-between gap-3">
                        <div className="min-w-0">
                          <p className="truncate text-sm font-semibold text-gray-900">{application.full_name || application.user_display_name || application.user_email || application.user_id}</p>
                          <p className="mt-1 truncate text-xs text-gray-500">
                            {copy.kycApplicant}: {application.user_display_name || application.user_email || application.user_id}
                          </p>
                          <p className="mt-1 truncate font-mono text-[11px] text-gray-400">{application.user_id}</p>
                        </div>
                        <span className={`rounded-full px-2.5 py-1 text-[11px] font-medium ${kycStatusTone(application.status)}`}>
                          {application.status}
                        </span>
                      </div>
                      <div className="mt-3 space-y-2 text-xs text-gray-600">
                        <p>
                          <span className="font-medium text-gray-800">{kycLevelLabel(application.kyc_level, language)}</span>
                        </p>
                        <p>
                          {copy.kycDocument}: {application.id_document_type || "-"} · {application.id_document_number || "-"}
                        </p>
                        {(application.document_image_urls ?? []).length > 0 ? (
                          <div>
                            <span>{copy.kycAttachments}: </span>
                            <div className="mt-1 flex flex-wrap gap-1.5">
                              {(application.document_image_urls ?? []).slice(0, 3).map((url, index) => (
                                <a
                                  key={`${application.id}-${url}-${index}`}
                                  href={url}
                                  target="_blank"
                                  rel="noreferrer"
                                  className="rounded-full bg-amber-50 px-2 py-1 text-[11px] font-medium text-amber-700 ring-1 ring-amber-200 hover:bg-amber-100"
                                >
                                  #{index + 1}
                                </a>
                              ))}
                              {(application.document_image_urls ?? []).length > 3 ? (
                                <span className="rounded-full bg-gray-100 px-2 py-1 text-[11px] text-gray-500">+{(application.document_image_urls ?? []).length - 3}</span>
                              ) : null}
                            </div>
                          </div>
                        ) : null}
                        <p>
                          {copy.kycSubmittedAt} · {formatDateTimeForLanguage(application.created_at, language)}
                        </p>
                      </div>
                      {application.status === "pending" ? (
                        <>
                          <label className="mt-3 block text-xs font-medium text-amber-900">
                            {copy.kycReason}
                            <input
                              value={kycRejectReasons[application.id] ?? ""}
                              onChange={(event) =>
                                setKYCRejectReasons((current) => ({
                                  ...current,
                                  [application.id]: event.target.value,
                                }))
                              }
                              className="mt-1 w-full rounded-xl border border-amber-200 bg-white px-3 py-2 text-sm text-gray-900 outline-none focus:border-amber-500"
                              placeholder={language === "en-US" ? "Missing document / mismatch" : "资料缺失 / 信息不一致"}
                            />
                          </label>
                          <div className="mt-3 flex flex-wrap gap-2">
                            <button
                              onClick={() => void handleKYCDecision(application, "approve")}
                              disabled={isReviewing}
                              className="rounded-xl bg-emerald-600 px-3 py-2 text-xs font-medium text-white hover:bg-emerald-700 disabled:cursor-not-allowed disabled:opacity-60"
                            >
                              {isReviewing ? copy.kycReviewing : copy.kycApprove}
                            </button>
                            <button
                              onClick={() => void handleKYCDecision(application, "reject")}
                              disabled={isReviewing}
                              className="rounded-xl bg-rose-600 px-3 py-2 text-xs font-medium text-white hover:bg-rose-700 disabled:cursor-not-allowed disabled:opacity-60"
                            >
                              {isReviewing ? copy.kycReviewing : copy.kycReject}
                            </button>
                          </div>
                        </>
                      ) : application.rejection_reason ? (
                        <div className="mt-3 rounded-xl border border-rose-100 bg-rose-50 px-3 py-2 text-xs text-rose-700">
                          {copy.kycReason}: {application.rejection_reason}
                        </div>
                      ) : null}
                    </article>
                  );
                })
              )}
            </div>
            {kycError ? <p className="mt-3 text-sm text-red-600">{kycError}</p> : null}
            {kycSuccess ? <p className="mt-3 text-sm text-emerald-700">{kycSuccess}</p> : null}
          </section>

          <AdminBrokerLinksSection language={language} />

          <AdminFundingSection language={language} />

          <AdminFXSection language={language} />

          <AdminReconSection language={language} />

          <AdminSurveillanceSection language={language} />

          <AdminDrawdownSection language={language} />

          <AdminMarketStatusSection language={language} />

          <AdminMarketImpactSection language={language} />

          <AdminLockupSection language={language} />

          <AdminBorrowSection language={language} />

          <AdminFactorExposureSection language={language} />

          <AdminStressScenariosSection language={language} />

          <AdminBrinsonCompositionsSection language={language} />

          {/* S8.4 — per-agent reputation ledger (cross-fund view +
              rebuild trigger). */}
          <AdminAgentReputationSection language={language} />

          {/* S9.2 — per-step workflow checkpoint timeline + resume. */}
          <AdminWorkflowCheckpointsSection language={language} />

          {/* S13 — platform LLM provider admin (CRUD + hot reload). */}
          {/* Renders ABOVE A/B so operators configure providers first; */}
          {/* the A/B form's provider dropdown pulls from this table. */}
          <AdminLLMProvidersSection language={language} />

          {/* S14.A — provider observability (health & cost dashboard). */}
          {/* Reads from the 5-min probe loop + hourly rollup loop. */}
          <AdminLLMObservabilitySection language={language} />

          {/* S10.3 / S10.4 — model A/B experiments (list, report, CRUD). */}
          <AdminModelABSection language={language} />

          {/* S13.3 — model A/B auto-promotion drafts (one-click apply / reject). */}
          <AdminModelABPromotionSection language={language} />

          {/* S11.4 — LLM health dashboard: decision_source / fallback */}
          {/* aggregates + recent fallback rows with raw provider summaries. */}
          <AdminLLMHealthSection language={language} />

          {/* W9-2 — memory re-embed queue depth + lifetime counters. */}
          {/* Pairs with the Prometheus exporter from W7-1 so an SRE */}
          {/* with only a browser handy can see backlog growth at 2 AM. */}
          <AdminMemReembedSection language={language} />

          {/* W11-2 — embedquota.Limiter live state + histogram tails. */}
          {/* Sibling of the memreembed panel; together they cover the */}
          {/* whole embed pipeline ("are we throttling?" + "is the */}
          {/* re-embed worker keeping up with the throttle?"). */}
          <AdminEmbedQuotaSection language={language} />

          {/* W14-3 — per-fund drill-down for the embed quota */}
          {/* limiter. Renders the AdminEmbedQuotaSection's */}
          {/* "throttle is firing" → "which fund is firing it" */}
          {/* follow-up. Recorder is opt-in; the panel renders */}
          {/* a clean disabled state until the EMBED_QUOTA_OBS_ENABLED */}
          {/* flag is flipped on the server. */}
          <AdminEmbedQuotaPerFundSection language={language} />

          {/* W13-1 — DB connection pool live state. Completes the */}
          {/* infra triad (memreembed + embedquota + db-pool) the */}
          {/* runbook §8 enumerates as the "Grafana down? curl this" */}
          {/* fallback. Three panels together answer "is the */}
          {/* pipeline healthy from disk to LLM provider?". */}
          <AdminDBPoolSection language={language} />

          {/* S12.3 — alertmanager-ingested events + ack flow. */}
          <AdminAlertsSection language={language} />

          {/* Stop-trigger watch — pending stops + poller status. */}
          <AdminStopTriggerSection language={language} />

          <AdminWSFeedSection language={language} />

          <section className="rounded-2xl border border-sky-200 bg-sky-50 px-5 py-5 shadow-sm">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div>
                <h2 className="text-lg font-semibold text-sky-900">{copy.marketHealthTitle}</h2>
                <p className="mt-2 text-sm text-sky-700">{copy.marketHealthHint}</p>
                <p className="mt-1 text-xs text-sky-600">
                  {copy.marketHealthAuto}
                  {marketHealthRefreshedAt ? ` · ${copy.marketHealthLastFetched}: ${formatDateTimeForLanguage(marketHealthRefreshedAt, language)}` : null}
                </p>
              </div>
              <button
                type="button"
                onClick={() => void loadMarketHealth()}
                disabled={marketHealthLoading}
                className="rounded-xl bg-sky-600 px-3 py-2 text-xs font-medium text-white hover:bg-sky-700 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {marketHealthLoading ? copy.marketHealthRefreshing : copy.marketHealthRefresh}
              </button>
            </div>
            <div className="mt-4 overflow-x-auto rounded-2xl border border-sky-200 bg-white">
              {Object.keys(marketHealth).length === 0 ? (
                <div className="p-4 text-sm text-sky-700">{copy.marketHealthEmpty}</div>
              ) : (
                <table className="w-full min-w-[680px] text-left text-xs">
                  <thead className="border-b border-sky-100 bg-sky-50/60 text-[11px] uppercase tracking-wider text-sky-800">
                    <tr>
                      <th className="px-3 py-2">{copy.marketHealthColumnProvider}</th>
                      <th className="px-3 py-2 text-right">{copy.marketHealthColumnCalls}</th>
                      <th className="px-3 py-2 text-right">{copy.marketHealthColumnSuccess}</th>
                      <th className="px-3 py-2 text-right">{copy.marketHealthColumnFailures}</th>
                      <th className="px-3 py-2 text-right">{copy.marketHealthColumnConsecutive}</th>
                      <th className="px-3 py-2 text-right">{copy.marketHealthColumnLatency}</th>
                      <th className="px-3 py-2">{copy.marketHealthColumnCircuit}</th>
                      <th className="px-3 py-2">{copy.marketHealthColumnLastError}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(() => {
                      // Capture "now" once per render so the circuit-open
                      // check below stays a pure function of props/state
                      // (lint rule react-hooks/purity).
                      const renderedAtMs = marketHealthRefreshedAt ? new Date(marketHealthRefreshedAt).getTime() : 0;
                      return Object.entries(marketHealth)
                        .sort(([a], [b]) => a.localeCompare(b))
                        .map(([name, stats]) => {
                          const circuitOpen =
                            !!stats.circuitOpenUntil && new Date(stats.circuitOpenUntil).getTime() > renderedAtMs;
                          return (
                          <tr key={name} className="border-b border-sky-50 last:border-b-0 align-top">
                            <td className="px-3 py-2 font-mono font-medium text-gray-900">{name}</td>
                            <td className="px-3 py-2 text-right tabular-nums">{stats.totalCalls}</td>
                            <td className="px-3 py-2 text-right tabular-nums text-emerald-700">{stats.totalSuccesses}</td>
                            <td className={`px-3 py-2 text-right tabular-nums ${stats.totalFailures > 0 ? "text-rose-700" : "text-gray-600"}`}>{stats.totalFailures}</td>
                            <td className={`px-3 py-2 text-right tabular-nums ${stats.consecutiveFailures > 0 ? "text-rose-700" : "text-gray-600"}`}>{stats.consecutiveFailures}</td>
                            <td className="px-3 py-2 text-right tabular-nums text-gray-700">
                              {stats.emaLatencyMs && stats.emaLatencyMs > 0
                                ? `${stats.emaLatencyMs}${stats.lastLatencyMs ? ` / ${stats.lastLatencyMs}` : ""}`
                                : "—"}
                            </td>
                            <td className="px-3 py-2">
                              {circuitOpen ? (
                                <span className="inline-flex items-center rounded-full bg-rose-100 px-2 py-0.5 font-medium text-rose-700">
                                  {copy.marketHealthCircuitOpenUntil} {formatDateTimeForLanguage(stats.circuitOpenUntil ?? "", language)}
                                </span>
                              ) : (
                                <span className="inline-flex items-center rounded-full bg-emerald-100 px-2 py-0.5 font-medium text-emerald-700">
                                  {copy.marketHealthCircuitClosed}
                                </span>
                              )}
                            </td>
                            <td className="px-3 py-2 text-rose-700">
                              {stats.lastError ? <span className="line-clamp-2 break-all">{stats.lastError}</span> : <span className="text-gray-400">—</span>}
                            </td>
                          </tr>
                        );
                      });
                    })()}
                  </tbody>
                </table>
              )}
            </div>
            {marketHealthError ? <p className="mt-3 text-sm text-red-600">{marketHealthError}</p> : null}
          </section>

          <section className="rounded-2xl border border-indigo-200 bg-indigo-50 px-5 py-5 shadow-sm">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div>
                <h2 className="text-lg font-semibold text-indigo-900">
                  {language === "zh-CN" ? "每日工作流调度器" : "Daily Workflow Scheduler"}
                </h2>
                <p className="mt-2 text-sm text-indigo-700">
                  {language === "zh-CN"
                    ? "每只活跃基金的下一次自动触发时间、leader 状态以及上一次轮询结果。可在此处强制开跑某只基金（例如修复配置后立即应用）。"
                    : "Next trigger time for each active fund, the leader replica, and the last poll outcome. Force-trigger a fund here after config fixes."}
                </p>
                {schedulerRefreshedAt ? (
                  <p className="mt-1 text-xs text-indigo-600">
                    {language === "zh-CN" ? "上次刷新" : "Last fetched"}: {formatDateTimeForLanguage(schedulerRefreshedAt, language)}
                  </p>
                ) : null}
              </div>
              <button
                type="button"
                onClick={() => void loadSchedulerSnapshot()}
                disabled={schedulerLoading}
                className="rounded-xl bg-indigo-600 px-3 py-2 text-xs font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {schedulerLoading
                  ? language === "zh-CN" ? "刷新中…" : "Refreshing…"
                  : language === "zh-CN" ? "刷新" : "Refresh"}
              </button>
            </div>

            {schedulerSnapshot ? (
              <div className="mt-3 grid grid-cols-2 gap-2 text-xs text-indigo-800 sm:grid-cols-4">
                <div>
                  <span className="block uppercase tracking-wide text-indigo-600">
                    {language === "zh-CN" ? "Leader" : "Leader"}
                  </span>
                  <span className={`mt-0.5 inline-flex rounded-full px-2 py-0.5 font-semibold ${schedulerSnapshot.isLeader ? "bg-emerald-100 text-emerald-700" : "bg-amber-100 text-amber-700"}`}>
                    {schedulerSnapshot.isLeader ? (language === "zh-CN" ? "是" : "yes") : (language === "zh-CN" ? "否" : "no")}
                  </span>
                </div>
                <div>
                  <span className="block uppercase tracking-wide text-indigo-600">
                    {language === "zh-CN" ? "上次轮询" : "Last poll"}
                  </span>
                  <span className="mt-0.5 block font-mono text-indigo-900">
                    {schedulerSnapshot.lastPollAt ? formatDateTimeForLanguage(schedulerSnapshot.lastPollAt, language) : "—"}
                  </span>
                </div>
                <div>
                  <span className="block uppercase tracking-wide text-indigo-600">
                    {language === "zh-CN" ? "下次轮询" : "Next poll"}
                  </span>
                  <span className="mt-0.5 block font-mono text-indigo-900">
                    {schedulerSnapshot.nextPollAt ? formatDateTimeForLanguage(schedulerSnapshot.nextPollAt, language) : "—"}
                  </span>
                </div>
                <div>
                  <span className="block uppercase tracking-wide text-indigo-600">
                    {language === "zh-CN" ? "本轮触发" : "Triggered now"}
                  </span>
                  <span className="mt-0.5 block font-semibold text-indigo-900">
                    {schedulerSnapshot.triggeredCount} / {schedulerSnapshot.totalActive}
                  </span>
                </div>
              </div>
            ) : null}

            {schedulerSnapshot?.warnings?.length ? (
              <div className="mt-3 rounded-xl border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
                {schedulerSnapshot.warnings.join(" · ")}
              </div>
            ) : null}

            <div className="mt-4 overflow-x-auto rounded-2xl border border-indigo-200 bg-white">
              {!schedulerSnapshot || schedulerSnapshot.funds.length === 0 ? (
                <div className="p-4 text-sm text-indigo-700">
                  {language === "zh-CN" ? "暂无活跃基金或调度器尚未轮询。" : "No active funds or scheduler has not polled yet."}
                </div>
              ) : (
                <table className="w-full min-w-[680px] text-left text-xs">
                  <thead className="border-b border-indigo-100 bg-indigo-50/60 text-[11px] uppercase tracking-wider text-indigo-800">
                    <tr>
                      <th className="px-3 py-2">{language === "zh-CN" ? "基金" : "Fund"}</th>
                      <th className="px-3 py-2">{language === "zh-CN" ? "日历" : "Calendar"}</th>
                      <th className="px-3 py-2">{language === "zh-CN" ? "下一交易日" : "Trading day"}</th>
                      <th className="px-3 py-2">{language === "zh-CN" ? "下一触发时刻" : "Trigger at"}</th>
                      <th className="px-3 py-2">{language === "zh-CN" ? "本轮状态" : "Outcome"}</th>
                      <th className="px-3 py-2 text-right">{language === "zh-CN" ? "操作" : "Action"}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {schedulerSnapshot.funds
                      .slice()
                      .sort((a: FundSchedulerStatus, b: FundSchedulerStatus) => (a.nextTriggerAt ?? "").localeCompare(b.nextTriggerAt ?? ""))
                      .map((fund: FundSchedulerStatus) => {
                        const outcome = fund.started
                          ? (language === "zh-CN" ? "已触发" : "triggered")
                          : fund.error
                          ? (language === "zh-CN" ? "失败" : "error")
                          : fund.skipReason
                          ? fund.skipReason
                          : (language === "zh-CN" ? "等待" : "waiting");
                        const outcomeClass = fund.started
                          ? "bg-emerald-100 text-emerald-700"
                          : fund.error
                          ? "bg-rose-100 text-rose-700"
                          : "bg-gray-100 text-gray-700";
                        return (
                          <tr key={fund.fundId} className="border-b border-indigo-50 last:border-b-0 align-top">
                            <td className="px-3 py-2">
                              <div className="font-semibold text-gray-900">{fund.fundName || fund.fundId}</div>
                              <div className="font-mono text-[10px] text-gray-500">{fund.fundId}</div>
                            </td>
                            <td className="px-3 py-2 text-gray-700">
                              {fund.calendarCode || "—"}
                              {fund.timeZone ? <span className="ml-1 text-gray-500">({fund.timeZone})</span> : null}
                            </td>
                            <td className="px-3 py-2 font-mono text-gray-800">{fund.nextTradingDay || "—"}</td>
                            <td className="px-3 py-2 font-mono text-gray-800">
                              {fund.nextTriggerAt ? formatDateTimeForLanguage(fund.nextTriggerAt, language) : "—"}
                            </td>
                            <td className="px-3 py-2">
                              <span className={`inline-flex rounded-full px-2 py-0.5 text-[10px] font-semibold ${outcomeClass}`}>{outcome}</span>
                              {fund.lastStatus ? (
                                <span className="ml-1 text-[10px] text-gray-500">{language === "zh-CN" ? "上次=" : "last="}{fund.lastStatus}</span>
                              ) : null}
                              {fund.error ? (
                                <div className="mt-1 text-[10px] text-rose-700">{fund.error}</div>
                              ) : null}
                            </td>
                            <td className="px-3 py-2 text-right">
                              <button
                                type="button"
                                onClick={() => void handleTriggerFund(fund.fundId)}
                                disabled={triggeringFundId === fund.fundId}
                                className="rounded-xl bg-indigo-600 px-2 py-1 text-[10px] font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
                              >
                                {triggeringFundId === fund.fundId
                                  ? language === "zh-CN" ? "触发中…" : "Triggering…"
                                  : language === "zh-CN" ? "立即开跑" : "Trigger now"}
                              </button>
                            </td>
                          </tr>
                        );
                      })}
                  </tbody>
                </table>
              )}
            </div>
            {schedulerError ? <p className="mt-3 text-sm text-red-600">{schedulerError}</p> : null}
            {triggerNotice ? <p className="mt-3 text-sm text-indigo-700">{triggerNotice}</p> : null}
          </section>
        </aside>
      </section>
    </div>
  );
};

export default Admin;
