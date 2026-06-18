import React, { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import {
  createSubscriptionCheckout,
  formatApiError,
  listPlans,
  type PlanWire,
} from "../lib/api";
import { useAppPreferences } from "../lib/preferences";

/**
 * /pricing — 公开订阅引导页 (Master Team SaaS)。
 *
 * Pricing rev (2026-06-15)：
 *   - 5 张卡：Free / Pro / Premium / Team / Enterprise
 *   - 月付 / 年付 toggle，年付 = 月价 × 10（省 2 个月）
 *   - Team 档：seat-based, min 3，前端 stepper 选 seat 数
 *   - Enterprise 档：Contact Sales（mailto），不走 LS checkout
 *   - 卡片底部统一合规免责区
 */
type BillingPeriod = "monthly" | "yearly";

const Pricing: React.FC = () => {
  const { language } = useAppPreferences();
  const isEn = language === "en-US";
  const navigate = useNavigate();
  const [params] = useSearchParams();

  const copy = useMemo(
    () =>
      isEn
        ? {
            heading: "Master Team — AI-Powered Daily Stock Watch",
            sub: "10 master personas · 4 strategies · 50 US stocks · published every morning. Educational simulation only.",
            month: "/mo",
            yearShort: "/yr",
            perSeatMonth: "/seat/mo",
            saveSuffix: "Save",
            monthly: "Monthly",
            yearly: "Yearly · save 2 months",
            mostPopular: "★ Most Popular",
            forPower: "For power users",
            startFree: "Start free",
            currentPlan: "Current plan",
            cancelledHint:
              "Checkout cancelled. You can pick another plan or come back later.",
            error: "Failed to start checkout",
            featuresTitle: "What you get",
            picksTitle: "Daily Master Team picks",
            picksFree: "Top 5 (T+1 delayed)",
            picksPaid: "Top 20 real-time",
            stratTitle: "Strategies",
            stratFree: "Disruptive only (1 of 4)",
            stratPaid: "All 4 (Value / Growth / Disruptive / Macro)",
            askTitle: "Master deep consults",
            askUnlimited: "Unlimited (BYOK)",
            paperTitle: "Paper-trading simulation",
            paperFree: "Read-only",
            paperPaid: "Full access",
            backtestTitle: "Historical backtests + CSV/PDF export",
            alertsTitle: "Custom alerts (price / TA / news)",
            seatTitle: "Seats",
            byokTitle: "BYO LLM key (OpenAI / Anthropic / DeepSeek)",
            slaTitle: "SLA + compliance audits",
            seatLabel: "Seats",
            seatMinHint: "(min 3)",
            cta: {
              free: "Get Started Free",
              paid: "Subscribe",
              busy: "Redirecting…",
              contactSales: "Contact Sales →",
            },
            disclaimer:
              "Educational content only. Agora AI is not a registered investment advisor (RIA) and does not provide personalized financial advice. Past performance does not guarantee future results. Always do your own research and consult a licensed advisor before trading.",
            paymentNote:
              "💳 Visa · Mastercard · Amex · Apple Pay · Google Pay — secured by LemonSqueezy. 30-day money-back guarantee · cancel anytime.",
          }
        : {
            heading: "大师团队 — AI 每日股票观察",
            sub: "10 位大师 · 4 大策略 · 50 只美股 · 每日早盘前发布。仅供教育性模拟参考。",
            month: " /月",
            yearShort: " /年",
            perSeatMonth: " /席/月",
            saveSuffix: "省",
            monthly: "月付",
            yearly: "年付 · 省 2 个月",
            mostPopular: "★ 热门推荐",
            forPower: "进阶玩家",
            startFree: "免费开始",
            currentPlan: "当前方案",
            cancelledHint: "支付已取消，可重新选择方案或稍后再试。",
            error: "发起支付失败",
            featuresTitle: "包含功能",
            picksTitle: "每日大师团队榜单",
            picksFree: "Top 5（延迟 T+1）",
            picksPaid: "Top 20 实时",
            stratTitle: "策略",
            stratFree: "仅 Disruptive（1/4）",
            stratPaid: "全部 4 大（价值 / 成长 / 颠覆 / 宏观）",
            askTitle: "大师深度问答",
            askUnlimited: "无上限（BYOK 自付）",
            paperTitle: "模拟交易盘",
            paperFree: "仅查看",
            paperPaid: "完整访问",
            backtestTitle: "历史回测 + CSV/PDF 导出",
            alertsTitle: "自定义提醒（价格 / 技术 / 新闻）",
            seatTitle: "席位",
            byokTitle: "自带 LLM Key（OpenAI / Anthropic / DeepSeek）",
            slaTitle: "SLA + 合规审计",
            seatLabel: "席位数",
            seatMinHint: "（最少 3）",
            cta: {
              free: "免费注册",
              paid: "订阅",
              busy: "跳转中…",
              contactSales: "联系销售 →",
            },
            disclaimer:
              "本平台仅供教育性模拟使用。Agora AI 并非注册投资顾问（RIA），不提供个性化投资建议。过往表现不代表未来收益。投资决策前请自行研究并咨询持牌顾问。",
            paymentNote:
              "💳 Visa · Mastercard · Amex · Apple Pay · Google Pay — LemonSqueezy 担保结算。30 天无条件退款 · 随时可取消。",
          },
    [isEn],
  );

  const [plans, setPlans] = useState<PlanWire[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [period, setPeriod] = useState<BillingPeriod>("monthly");
  const [teamSeats, setTeamSeats] = useState<number>(3);

  const cancelled = params.get("status") === "cancelled";

  useEffect(() => {
    let cancel = false;
    listPlans()
      .then((res) => {
        if (!cancel) setPlans(res.plans ?? []);
      })
      .catch((e) => {
        if (!cancel) setErr(formatApiError(e, copy.error));
      })
      .finally(() => {
        if (!cancel) setLoading(false);
      });
    return () => {
      cancel = true;
    };
  }, [copy.error]);

  const handleSubscribe = async (tier: "pro" | "premium" | "team") => {
    setBusy(tier);
    setErr(null);
    try {
      const res = await createSubscriptionCheckout({
        tier,
        billing_period: period,
        seat_count: tier === "team" ? teamSeats : 1,
      });
      sessionStorage.setItem("pending_checkout_intent", res.intent_id);
      window.location.assign(res.checkout_url);
    } catch (e: any) {
      const code = (e && (e.code || e.error)) || "";
      if (code === "unauthorized") {
        navigate(`/login?return_to=${encodeURIComponent(`/pricing?tier=${tier}`)}`);
        return;
      }
      if (code === "already_subscribed") {
        navigate("/subscription");
        return;
      }
      if (code === "checkout_unavailable" || code === "variant_not_bound") {
        setErr(
          isEn
            ? "Online checkout is not configured yet — please contact support."
            : "线上支付暂未配置，请联系客服。",
        );
        return;
      }
      setErr(formatApiError(e, copy.error));
    } finally {
      setBusy(null);
    }
  };

  const fmtUSD = (cents: number) => {
    if (cents <= 0) return "$0";
    const dollars = cents / 100;
    if (dollars % 1 === 0) return `$${dollars.toFixed(0)}`;
    // 14.9 → "$14.9", 29 → "$29"
    return `$${dollars.toFixed(2).replace(/0$/, "").replace(/\.$/, "")}`;
  };

  const sortedPlans = useMemo(() => {
    const order: Record<string, number> = {
      free: 0,
      pro: 1,
      premium: 2,
      team: 3,
      enterprise: 4,
    };
    return [...plans].sort((a, b) => (order[a.tier] ?? 99) - (order[b.tier] ?? 99));
  }, [plans]);

  // 计算每张卡显示的当前价格（取决于 monthly/yearly toggle）
  const cardPrice = (p: PlanWire): { primary: string; secondary?: string } => {
    if (p.contact_sales) return { primary: isEn ? "Custom" : "定制" };
    if (p.tier === "free") return { primary: "$0", secondary: isEn ? "forever" : "永久免费" };
    if (p.tier === "team") {
      // Team always per-seat per-month
      return {
        primary: `${fmtUSD(p.price_cents_usd_month)}${copy.perSeatMonth}`,
        secondary: isEn ? `${copy.seatMinHint}` : copy.seatMinHint,
      };
    }
    if (period === "yearly" && p.price_cents_usd_year > 0) {
      const monthlyEq = p.price_cents_usd_year / 12;
      const yearlyTotal = fmtUSD(p.price_cents_usd_year);
      const fullYearly = p.price_cents_usd_month * 12;
      const save = fullYearly - p.price_cents_usd_year;
      return {
        primary: `${fmtUSD(Math.round(monthlyEq))}${copy.month}`,
        secondary: `${yearlyTotal}${copy.yearShort} · ${copy.saveSuffix} ${fmtUSD(save)}`,
      };
    }
    return { primary: `${fmtUSD(p.price_cents_usd_month)}${copy.month}` };
  };

  return (
    <div className="mx-auto max-w-7xl space-y-6 p-6">
      <header className="text-center">
        <h1 className="text-3xl font-bold text-slate-900">{copy.heading}</h1>
        <p className="mt-2 text-sm text-slate-500">{copy.sub}</p>
      </header>

      {/* Billing period toggle */}
      <div className="flex items-center justify-center gap-1 rounded-full border border-slate-200 bg-slate-50 p-1 text-xs font-medium w-fit mx-auto">
        <button
          type="button"
          onClick={() => setPeriod("monthly")}
          className={`rounded-full px-4 py-1.5 transition ${
            period === "monthly" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"
          }`}
        >
          {copy.monthly}
        </button>
        <button
          type="button"
          onClick={() => setPeriod("yearly")}
          className={`rounded-full px-4 py-1.5 transition ${
            period === "yearly" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"
          }`}
        >
          {copy.yearly}
        </button>
      </div>

      {cancelled ? (
        <div className="rounded-md border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-700">
          {copy.cancelledHint}
        </div>
      ) : null}
      {err ? (
        <div className="rounded-md border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700">
          {err}
        </div>
      ) : null}

      {loading ? (
        <div className="py-12 text-center text-sm text-slate-400">…</div>
      ) : (
        <div className="grid gap-5 sm:grid-cols-2 lg:grid-cols-5">
          {sortedPlans.map((p) => {
            const isFree = p.tier === "free";
            const isTeam = p.tier === "team";
            const isContact = p.contact_sales;
            const isPro = p.tier === "pro";
            const isPremium = p.tier === "premium";
            const tone = p.recommended
              ? "border-indigo-500 ring-2 ring-indigo-200"
              : isPremium
                ? "border-amber-300"
                : "border-slate-200";
            const price = cardPrice(p);
            return (
              <div
                key={p.tier}
                className={`relative flex flex-col rounded-2xl border bg-white p-5 shadow-sm ${tone}`}
              >
                {p.recommended ? (
                  <span className="absolute -top-3 left-1/2 -translate-x-1/2 whitespace-nowrap rounded-full bg-indigo-600 px-3 py-0.5 text-[11px] font-semibold text-white">
                    {copy.mostPopular}
                  </span>
                ) : null}
                {isPremium ? (
                  <span className="absolute -top-3 left-1/2 -translate-x-1/2 whitespace-nowrap rounded-full bg-amber-500 px-3 py-0.5 text-[11px] font-semibold text-white">
                    {copy.forPower}
                  </span>
                ) : null}
                <h3 className="mb-2 text-base font-semibold text-slate-900">{p.name}</h3>

                <div className="mb-3">
                  <div className="text-[26px] font-extrabold leading-none text-slate-900">
                    {price.primary}
                  </div>
                  {price.secondary ? (
                    <div className="mt-1 text-[11px] text-slate-500">{price.secondary}</div>
                  ) : null}
                </div>

                <p className="mb-4 text-xs leading-relaxed text-slate-500">{p.description}</p>

                <ul className="mb-5 flex-1 space-y-1.5 text-[12px] text-slate-700">
                  <li>
                    🎯 <strong>{copy.picksTitle}:</strong>{" "}
                    {isFree ? copy.picksFree : copy.picksPaid}
                  </li>
                  <li>
                    📊 <strong>{copy.stratTitle}:</strong>{" "}
                    {isFree ? copy.stratFree : copy.stratPaid}
                  </li>
                  <li>
                    💬 <strong>{copy.askTitle}:</strong>{" "}
                    {isFree
                      ? "10 / mo"
                      : isPro
                        ? "50 / mo"
                        : isPremium
                          ? "200 / mo"
                          : copy.askUnlimited}
                  </li>
                  <li>
                    🧪 <strong>{copy.paperTitle}:</strong>{" "}
                    {isFree ? copy.paperFree : copy.paperPaid}
                  </li>
                  {p.allow_export ? (
                    <li>
                      ✨ <strong>{copy.backtestTitle}</strong>
                    </li>
                  ) : null}
                  {(isPremium || isTeam) ? (
                    <li>
                      📧 <strong>{copy.alertsTitle}</strong>
                    </li>
                  ) : null}
                  {isTeam ? (
                    <>
                      <li>
                        👥 <strong>{copy.seatTitle}:</strong> min 3
                      </li>
                      <li>
                        🔑 <strong>{copy.byokTitle}</strong>
                      </li>
                    </>
                  ) : null}
                  {isContact ? (
                    <>
                      <li>
                        🏢 <strong>{copy.slaTitle}</strong>
                      </li>
                      <li>
                        🔑 <strong>{copy.byokTitle}</strong>
                      </li>
                    </>
                  ) : null}
                </ul>

                {/* Team seat stepper */}
                {isTeam ? (
                  <div className="mb-3 flex items-center justify-between rounded-md border border-slate-200 bg-slate-50 px-3 py-2 text-xs">
                    <span className="font-medium text-slate-700">{copy.seatLabel}</span>
                    <div className="flex items-center gap-2">
                      <button
                        type="button"
                        onClick={() => setTeamSeats((s) => Math.max(3, s - 1))}
                        className="h-6 w-6 rounded-full border border-slate-300 hover:border-slate-500"
                      >
                        −
                      </button>
                      <span className="min-w-[24px] text-center font-mono font-semibold">
                        {teamSeats}
                      </span>
                      <button
                        type="button"
                        onClick={() => setTeamSeats((s) => Math.min(100, s + 1))}
                        className="h-6 w-6 rounded-full border border-slate-300 hover:border-slate-500"
                      >
                        +
                      </button>
                    </div>
                  </div>
                ) : null}

                {/* CTA */}
                {isFree ? (
                  <Link
                    to="/login"
                    className="inline-flex w-full items-center justify-center rounded-md border border-slate-300 px-3 py-2 text-xs font-medium text-slate-700 hover:border-slate-500"
                  >
                    {copy.cta.free}
                  </Link>
                ) : isContact ? (
                  <a
                    href="mailto:sales@agora-ai.com?subject=Enterprise%20Inquiry"
                    className="inline-flex w-full items-center justify-center rounded-md border border-slate-900 bg-white px-3 py-2 text-xs font-medium text-slate-900 hover:bg-slate-50"
                  >
                    {copy.cta.contactSales}
                  </a>
                ) : (
                  <button
                    type="button"
                    disabled={busy !== null}
                    onClick={() => void handleSubscribe(p.tier as any)}
                    className={`inline-flex w-full items-center justify-center rounded-md px-3 py-2 text-xs font-medium text-white transition disabled:cursor-not-allowed disabled:opacity-60 ${
                      p.recommended
                        ? "bg-indigo-600 hover:bg-indigo-500"
                        : isPremium
                          ? "bg-amber-500 hover:bg-amber-400"
                          : isTeam
                            ? "bg-emerald-600 hover:bg-emerald-500"
                            : "bg-slate-900 hover:bg-slate-700"
                    }`}
                  >
                    {busy === p.tier ? copy.cta.busy : copy.cta.paid}
                  </button>
                )}
              </div>
            );
          })}
        </div>
      )}

      {/* 卡片下方统一区域：合规免责 + 支付方式 + 退款保证 */}
      <div className="mt-8 mx-auto max-w-3xl space-y-3 text-center text-[11px] leading-relaxed">
        <p className="rounded-md border border-amber-200 bg-amber-50 px-4 py-3 text-amber-800">
          ⚠️ {copy.disclaimer}
        </p>
        <p className="text-slate-500">{copy.paymentNote}</p>
      </div>
    </div>
  );
};

export default Pricing;
