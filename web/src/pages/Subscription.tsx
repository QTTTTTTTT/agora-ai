import React, { useCallback, useEffect, useMemo, useState } from "react";
import { apiDelete, apiGet, apiPost, formatApiError } from "../lib/api";
import {
  formatDateForLanguage,
  formatMoneyMinorForDisplay,
  formatNumberForLanguage,
  useAppPreferences,
} from "../lib/preferences";

interface Plan {
  tier: string;
  name: string;
  price_cents_month: number;
  // USD cents — Phase 1 LS 接入后由后端回填，老部署兼容旧版仍可能为 0
  price_cents_usd_month?: number;
  max_funds: number;
  max_calls_per_day: number;
  model_tiers: string[];
  recommended: boolean;
  max_agents_per_fund: number;
  max_workflow_per_day: number;
  allow_custom_key: boolean;
  allow_ab_test: boolean;
  allow_export: boolean;
  simulation_capital: number;
  included_tokens: number;
  description: string;
}

interface SubscriptionRecord {
  id: string;
  user_id: string;
  plan_tier: string;
  status: string;
  start_date: string;
  end_date: string;
  auto_renew: boolean;
  payment_method: string;
}

interface PlansResponse {
  plans: Plan[];
}

interface SubscriptionPermissions {
  allow_custom_key: boolean;
  allow_ab_test: boolean;
  allow_export: boolean;
}

interface SubscriptionResponse {
  subscription: SubscriptionRecord | null;
  plan: Plan | null;
  permissions: SubscriptionPermissions;
}

type FeatureKey =
  | "funds"
  | "dailyCalls"
  | "workflow"
  | "agentsPerFund"
  | "modelTiers"
  | "customKey"
  | "abTest"
  | "export"
  | "autoRenew"
  | "status";

const FEATURE_KEYS: FeatureKey[] = [
  "funds",
  "dailyCalls",
  "workflow",
  "agentsPerFund",
  "modelTiers",
  "customKey",
  "abTest",
  "export",
  "autoRenew",
  "status",
];

const Subscription: React.FC = () => {
  const { language, displayCurrency } = useAppPreferences();
  const [plans, setPlans] = useState<Plan[]>([]);
  const [subscription, setSubscription] = useState<SubscriptionRecord | null>(null);
  const [effectivePlan, setEffectivePlan] = useState<Plan | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [successMessage, setSuccessMessage] = useState<string | null>(null);
  const [submittingTier, setSubmittingTier] = useState<string | null>(null);
  const [cancelling, setCancelling] = useState(false);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Subscription",
            subtitle:
              "Review your current plan, switch tiers, and compare limits for funds, workflow usage, and model access.",
            loading: "Loading subscription details...",
            loadFailed: "Failed to load subscription details",
            retry: "Retry",
            currentStatusTitle: "Current subscription",
            currentPlan: "Current plan",
            endDate: "End date",
            autoRenew: "Auto renew",
            notSubscribed: "Not subscribed",
            freePlan: "Free",
            enabled: "Enabled",
            disabled: "Disabled",
            cancelSubscription: "Cancel subscription",
            cancelling: "Cancelling...",
            choosePlan: "Choose a plan",
            noPlans: "No subscription plans are available yet. Refresh later or ask an admin to add plans.",
            recommended: "Recommended",
            current: "Current",
            currentPlanButton: "Current plan",
            submitting: "Submitting...",
            upgradeTo: "Upgrade to",
            switchTo: "Switch to",
            comparisonTitle: "Feature comparison",
            featureColumn: "Feature",
            availableToSwitch: "Available",
            unavailable: "Unavailable",
            unlimited: "Unlimited",
            supported: "Supported",
            unsupported: "Not supported",
            on: "On",
            off: "Off",
            monthSuffix: "/month",
            modelAccessSuffix: "model access",
            units: {
              funds: "funds",
              callsPerDay: "calls/day",
              workflowsPerDay: "runs/day",
              agents: "agents",
            },
            featureLabels: {
              funds: "Funds",
              dailyCalls: "Daily call limit",
              workflow: "Workflow limit",
              agentsPerFund: "Agents per fund",
              modelTiers: "Model access",
              customKey: "Custom key",
              abTest: "A/B test",
              export: "Export",
              autoRenew: "Auto renew",
              status: "Status",
            } as Record<FeatureKey, string>,
            statusLabels: {
              active: "Active",
              trialing: "Trialing",
              past_due: "Action needed",
              unpaid: "Unpaid",
              cancelled: "Cancelled",
              canceled: "Cancelled",
              expired: "Expired",
            } as Record<string, string>,
            modelTierLabels: {
              critical: "Critical decisions",
              standard: "Daily tasks",
              simple: "Simple tasks",
              tier1: "Critical decisions",
              tier2: "Daily tasks",
              tier3: "Simple tasks",
            } as Record<string, string>,
            successSwitched: "Switched to",
            successCancelled: "Subscription cancelled. The account has fallen back to the default plan.",
            subscribeFailed: "Failed to update subscription",
            cancelFailed: "Failed to cancel subscription",
          }
        : {
            title: "订阅管理",
            subtitle: "查看当前计划、切换版本与对比权益，快速确认基金数、调用额度和模型权限。",
            loading: "正在加载订阅信息...",
            loadFailed: "加载订阅信息失败",
            retry: "重试",
            currentStatusTitle: "当前订阅状态",
            currentPlan: "当前计划",
            endDate: "到期日",
            autoRenew: "自动续费",
            notSubscribed: "未订阅",
            freePlan: "免费版",
            enabled: "已开启",
            disabled: "已关闭",
            cancelSubscription: "取消订阅",
            cancelling: "取消中...",
            choosePlan: "选择计划",
            noPlans: "当前还没有可切换的订阅计划，请稍后刷新或联系管理员补充套餐。",
            recommended: "推荐",
            current: "当前",
            currentPlanButton: "当前计划",
            submitting: "提交中...",
            upgradeTo: "升级到",
            switchTo: "切换到",
            comparisonTitle: "功能对比表",
            featureColumn: "功能",
            availableToSwitch: "可切换",
            unavailable: "未配置",
            unlimited: "不限",
            supported: "支持",
            unsupported: "不支持",
            on: "已开启",
            off: "已关闭",
            monthSuffix: "/月",
            modelAccessSuffix: "模型权限",
            units: {
              funds: "个",
              callsPerDay: "次/日",
              workflowsPerDay: "次/日",
              agents: "个",
            },
            featureLabels: {
              funds: "基金数",
              dailyCalls: "每日调用上限",
              workflow: "工作流上限",
              agentsPerFund: "每基金 Agent 数",
              modelTiers: "模型权限",
              customKey: "自定义 Key",
              abTest: "A/B Test",
              export: "导出",
              autoRenew: "自动续费",
              status: "状态",
            } as Record<FeatureKey, string>,
            statusLabels: {
              active: "生效中",
              trialing: "试用中",
              past_due: "待处理",
              unpaid: "待支付",
              cancelled: "已取消",
              canceled: "已取消",
              expired: "已过期",
            } as Record<string, string>,
            modelTierLabels: {
              critical: "关键决策",
              standard: "日常任务",
              simple: "简单任务",
              tier1: "关键决策",
              tier2: "日常任务",
              tier3: "简单任务",
            } as Record<string, string>,
            successSwitched: "已切换到",
            successCancelled: "订阅已取消，当前已回退为默认计划。",
            subscribeFailed: "更新订阅失败",
            cancelFailed: "取消订阅失败",
          },
    [language],
  );

  const formatPrice = useCallback(
    (plan: Plan) => {
      // Phase 1 — LemonSqueezy 接入后所有付费档按 USD 计费。
      // PriceCentsUSDMonth > 0 时直接渲染美元；老部署 / 老 fund 仍
      // 走兼容路径用 CNY 价格，并按 displayCurrency 做 FX 换算。
      if (plan.price_cents_usd_month && plan.price_cents_usd_month > 0) {
        const dollars = plan.price_cents_usd_month / 100;
        const formatted = dollars % 1 === 0
          ? `$${dollars.toFixed(0)}`
          : `$${dollars.toFixed(2).replace(/0$/, "")}`;
        return `${formatted}${copy.monthSuffix}`;
      }
      if (!plan.price_cents_month || plan.price_cents_month <= 0) {
        return language === "en-US" ? "Free" : "免费";
      }
      return `${formatMoneyMinorForDisplay(plan.price_cents_month, "CNY", displayCurrency, language)}${copy.monthSuffix}`;
    },
    [copy.monthSuffix, displayCurrency, language],
  );

  const formatModelTiers = useCallback(
    (modelTiers: string[]) => {
      if (modelTiers.length === 0) {
        return copy.unavailable;
      }
      return modelTiers.map((tier) => copy.modelTierLabels[tier] ?? tier).join(" / ");
    },
    [copy.modelTierLabels, copy.unavailable],
  );

  const formatSubscriptionStatus = useCallback(
    (status?: string | null) => {
      if (!status) {
        return copy.notSubscribed;
      }
      return copy.statusLabels[status] ?? status;
    },
    [copy.notSubscribed, copy.statusLabels],
  );

  const yesNo = useCallback(
    (value: boolean) => (value ? copy.supported : copy.unsupported),
    [copy.supported, copy.unsupported],
  );

  const formatLimit = useCallback(
    (value: number, unit: string) => {
      if (value > 0) {
        const count = formatNumberForLanguage(value, language);
        return language === "en-US" ? `${count} ${unit}` : `${count}${unit}`;
      }
      return copy.unlimited;
    },
    [copy.unlimited, language],
  );

  const buildPlanFeatures = useCallback(
    (plan: Plan, record: SubscriptionRecord | null): Record<FeatureKey, string> => ({
      funds: formatLimit(plan.max_funds, copy.units.funds),
      dailyCalls: formatLimit(plan.max_calls_per_day, copy.units.callsPerDay),
      workflow: formatLimit(plan.max_workflow_per_day, copy.units.workflowsPerDay),
      agentsPerFund: formatLimit(plan.max_agents_per_fund, copy.units.agents),
      modelTiers: formatModelTiers(plan.model_tiers),
      customKey: yesNo(plan.allow_custom_key),
      abTest: yesNo(plan.allow_ab_test),
      export: yesNo(plan.allow_export),
      autoRenew: record?.plan_tier === plan.tier ? (record.auto_renew ? copy.on : copy.off) : "-",
      status: record?.plan_tier === plan.tier ? formatSubscriptionStatus(record.status) : copy.availableToSwitch,
    }),
    [copy.availableToSwitch, copy.on, copy.off, copy.units, formatLimit, formatModelTiers, formatSubscriptionStatus, yesNo],
  );

  const loadData = useCallback(async () => {
    setLoading(true);
    setError(null);

    try {
      const [plansRes, subscriptionRes] = await Promise.all([
        apiGet<PlansResponse>("/api/plans"),
        apiGet<SubscriptionResponse>("/api/subscription"),
      ]);
      setPlans(plansRes.plans ?? []);
      setSubscription(subscriptionRes.subscription ?? null);
      setEffectivePlan(subscriptionRes.plan ?? null);
    } catch (err) {
      setError(formatApiError(err, copy.loadFailed));
    } finally {
      setLoading(false);
    }
  }, [copy.loadFailed]);

  useEffect(() => {
    void loadData();
  }, [loadData]);

  const currentPlanTier = effectivePlan?.tier ?? subscription?.plan_tier ?? "free";
  const currentPlan = useMemo(
    () => plans.find((plan) => plan.tier === currentPlanTier) ?? effectivePlan,
    [plans, currentPlanTier, effectivePlan],
  );

  const currentPlanPrice = currentPlan?.price_cents_month ?? 0;

  const handleSubscribe = useCallback(
    async (tier: string) => {
      setSubmittingTier(tier);
      setActionError(null);
      setSuccessMessage(null);
      try {
        await apiPost("/api/subscription", { tier, payment_method: "manual" });
        await loadData();
        setSuccessMessage(`${copy.successSwitched} ${plans.find((plan) => plan.tier === tier)?.name ?? tier}.`);
      } catch (err) {
        setActionError(formatApiError(err, copy.subscribeFailed));
      } finally {
        setSubmittingTier(null);
      }
    },
    [copy.subscribeFailed, copy.successSwitched, loadData, plans],
  );

  const handleCancel = useCallback(async () => {
    setCancelling(true);
    setActionError(null);
    setSuccessMessage(null);
    try {
      await apiDelete("/api/subscription");
      await loadData();
      setSuccessMessage(copy.successCancelled);
    } catch (err) {
      setActionError(formatApiError(err, copy.cancelFailed));
    } finally {
      setCancelling(false);
    }
  }, [copy.cancelFailed, copy.successCancelled, loadData]);

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
          <button
            onClick={() => void loadData()}
            className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700"
          >
            {copy.retry}
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-8">
      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
      </div>

      <div className="rounded-lg border border-indigo-200 bg-indigo-50 p-6">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h2 className="mb-3 text-lg font-semibold text-indigo-900">{copy.currentStatusTitle}</h2>
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
              <div>
                <p className="text-sm text-indigo-600">{copy.currentPlan}</p>
                <p className="text-lg font-bold text-indigo-900">{currentPlan?.name ?? copy.freePlan}</p>
              </div>
              <div>
                <p className="text-sm text-indigo-600">{copy.endDate}</p>
                <p className="text-lg font-bold text-indigo-900">{formatDateForLanguage(subscription?.end_date, language)}</p>
              </div>
              <div>
                <p className="text-sm text-indigo-600">{copy.autoRenew}</p>
                <p className="text-lg font-bold text-green-700">
                  {subscription ? (subscription.auto_renew ? copy.enabled : copy.disabled) : copy.notSubscribed}
                </p>
              </div>
            </div>
          </div>
          {subscription && currentPlanTier !== "free" ? (
            <button
              onClick={() => void handleCancel()}
              disabled={cancelling}
              className="rounded-lg border border-red-300 bg-white px-4 py-2 text-sm font-medium text-red-600 hover:bg-red-50 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {cancelling ? copy.cancelling : copy.cancelSubscription}
            </button>
          ) : null}
        </div>
        {actionError ? <p className="mt-4 text-sm text-red-600">{actionError}</p> : null}
        {successMessage ? <p className="mt-4 text-sm text-emerald-700">{successMessage}</p> : null}
      </div>

      <div className="space-y-4">
        <h2 className="text-lg font-semibold text-gray-800">{copy.choosePlan}</h2>
        {plans.length === 0 ? (
          <div className="rounded-lg border border-dashed border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.noPlans}</div>
        ) : (
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-4">
            {plans.map((plan) => {
              const isCurrent = plan.tier === currentPlanTier;
              const isRecommended = plan.recommended;
              const planFeatures = buildPlanFeatures(plan, subscription);
              const isUpgrade = plan.price_cents_month > currentPlanPrice;
              const isBusy = submittingTier === plan.tier;

              return (
                <div
                  key={plan.tier}
                  className={`relative rounded-xl border-2 p-6 transition-shadow ${
                    isCurrent
                      ? "border-indigo-500 bg-indigo-50 shadow-lg"
                      : isRecommended
                        ? "border-amber-400 bg-amber-50 shadow-md"
                        : "border-gray-200 bg-white hover:shadow-md"
                  }`}
                >
                  {isRecommended ? (
                    <span className="absolute -top-3 left-4 rounded-full bg-amber-400 px-3 py-0.5 text-xs font-bold text-white">
                      {copy.recommended}
                    </span>
                  ) : null}
                  {isCurrent ? (
                    <span className="absolute -top-3 right-4 rounded-full bg-indigo-500 px-3 py-0.5 text-xs font-bold text-white">
                      {copy.current}
                    </span>
                  ) : null}
                  <h3 className="mb-1 text-xl font-bold text-gray-900">{plan.name}</h3>
                  <p className="mb-2 text-3xl font-extrabold text-gray-900">{formatPrice(plan)}</p>
                  <p className="mb-4 text-sm text-gray-500">
                    {formatModelTiers(plan.model_tiers)} {copy.modelAccessSuffix}
                  </p>
                  <ul className="mb-6 space-y-1 text-sm text-gray-700">
                    {FEATURE_KEYS.map((key) => (
                      <li key={key}>
                        <span className="mr-1 text-gray-400">•</span>
                        {copy.featureLabels[key]}: {planFeatures[key]}
                      </li>
                    ))}
                  </ul>
                  {isCurrent ? (
                    <button
                      disabled
                      className="w-full cursor-not-allowed rounded-lg bg-gray-300 py-2 text-sm font-medium text-gray-600"
                    >
                      {copy.currentPlanButton}
                    </button>
                  ) : (
                    <button
                      onClick={() => void handleSubscribe(plan.tier)}
                      disabled={Boolean(submittingTier)}
                      className={`w-full rounded-lg py-2 text-sm font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-60 ${
                        isUpgrade
                          ? "bg-indigo-600 text-white hover:bg-indigo-700"
                          : "border border-gray-300 text-gray-700 hover:bg-gray-50"
                      }`}
                    >
                      {isBusy ? copy.submitting : `${isUpgrade ? copy.upgradeTo : copy.switchTo}${language === "en-US" ? " " : ""}${plan.name}`}
                    </button>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </div>

      {plans.length > 0 ? (
        <div>
          <h2 className="mb-4 text-lg font-semibold text-gray-800">{copy.comparisonTitle}</h2>
          <div className="overflow-x-auto rounded-lg border border-gray-200">
            <table className="w-full text-sm">
              <thead className="bg-gray-50">
                <tr>
                  <th className="px-4 py-3 text-left font-medium text-gray-600">{copy.featureColumn}</th>
                  {plans.map((plan) => (
                    <th
                      key={plan.tier}
                      className={`px-4 py-3 text-center font-medium ${
                        plan.tier === currentPlanTier ? "bg-indigo-50 text-indigo-700" : "text-gray-600"
                      }`}
                    >
                      {plan.name}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-100">
                {FEATURE_KEYS.map((key) => (
                  <tr key={key} className="hover:bg-gray-50">
                    <td className="px-4 py-3 font-medium text-gray-700">{copy.featureLabels[key]}</td>
                    {plans.map((plan) => (
                      <td
                        key={plan.tier}
                        className={`px-4 py-3 text-center ${plan.tier === currentPlanTier ? "bg-indigo-50 font-medium" : ""}`}
                      >
                        {buildPlanFeatures(plan, subscription)[key]}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      ) : null}
    </div>
  );
};

export default Subscription;
