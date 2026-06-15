// MastersHub.tsx — the new authenticated landing page for the
// "Master Team" product (个股诊断 / 每日榜单 / 模拟盘).
//
// Why a dedicated hub?
//   The previous landing was /companies, which conflates two very
//   different products: (a) the famous-investor PERSONA team that
//   publishes daily picks + answers per-stock consults, and (b)
//   the AI agent team that manages a fund. The persona product is
//   the everyday surface most users want; the fund-team product
//   is a heavier, opt-in workflow. Splitting them lets us:
//     1. Send fresh sessions straight to the surface they came for.
//     2. Hide the fund-team product behind an admin-controlled
//        flag (`agent_team_mode`, seeded OFF) without losing the
//        ability to demo it manually as super_admin.
//
// Page anatomy
//   - Header / utility pills (wallet, KYC, marketplace, admin,
//     account, logout) reuse the same visual vocabulary as
//     Companies.tsx so the brand stays consistent.
//   - "Active strategy" pill row mirrors the four daily-picks
//     presets (disruptive / conservative / garp / macro). Each
//     pill deep-links to /daily-picks?preset=KEY — the page reads
//     that param on mount.
//   - Hero card grid: four (or five, with agent_team_mode ON)
//     large entry tiles, one per master-team feature plus the
//     optional fund-team surface.
//   - "This Week's Insights" placeholder — intentionally a stub
//     card with Coming Soon copy; the user explicitly asked to
//     leave content-stream work for a follow-up.
//
// Accessibility & i18n
//   Copy is split zh-CN / en-US and selected from useAppPreferences
//   like the rest of the SPA. All cards are real <Link>s (not
//   role="button" divs) so keyboard nav and right-click "open in
//   new tab" Just Work.

import React, { useCallback, useMemo } from "react";
import { Link, useNavigate } from "react-router-dom";
import { getStoredSession, logoutSession } from "../lib/api";
import { useAppPreferences } from "../lib/preferences";
import { useFeatureFlag } from "../lib/featureFlags";

type CardAccent = "coral" | "sage" | "mint" | "info" | "amber" | "violet";

interface HeroCard {
  id: string;
  to: string;
  titleZh: string;
  titleEn: string;
  descZh: string;
  descEn: string;
  ctaZh: string;
  ctaEn: string;
  accent: CardAccent;
}

// PRESET_PILLS mirrors the four lenses configured in DailyPicks.tsx
// (and seeded in migration 108). Keep the keys in sync — clicking
// a pill deep-links to /daily-picks?preset=KEY and the page reads
// the query param on mount to seed its filter dropdown.
const PRESET_PILLS = [
  { key: "disruptive",   labelZh: "颠覆创新",   labelEn: "Disruptive",   subZh: "Wood 视角",                                subEn: "Wood lens" },
  { key: "conservative", labelZh: "价值稳健",   labelEn: "Conservative", subZh: "Buffett · Munger · Graham",                subEn: "Buffett · Munger · Graham" },
  { key: "garp",         labelZh: "成长合理价", labelEn: "GARP",         subZh: "Lynch · O'Neil",                           subEn: "Lynch · O'Neil" },
  { key: "macro",        labelZh: "宏观择时",   labelEn: "Macro",        subZh: "Marks · Dalio · Druckenmiller",            subEn: "Marks · Dalio · Druckenmiller" },
];

function copyForLang(language: string) {
  const zh = language === "zh-CN";
  return {
    brand: "FundAI",
    heroEyebrow: zh ? "大师团队" : "Master Team",
    heroTitle: zh ? "今日大师视角与策略入口" : "Today's master-team view & strategy entry",
    heroSub: zh
      ? "由 11 位经典价值 / 成长 / 宏观大师 LLM 集体产出的合规视角。一切结果均为市场观察，非投资建议。"
      : "Outputs from 11 classic value / growth / macro investor LLM personas, framed as compliant market observation — not investment advice.",
    presetTitle: zh ? "选一种策略视角" : "Pick a strategy lens",
    presetSub: zh ? "点击进入对应预设的当日榜单" : "Open today's picks under that lens",
    cardsTitle: zh ? "功能入口" : "Surfaces",
    weeklyTitle: zh ? "本周观察精选" : "This Week's Insights",
    weeklyComingSoon: zh
      ? "近 7 天高置信度命中、被反复点名的标的，以及大师之间的分歧将在此聚合。开发中。"
      : "Coming soon: a roll-up of the past week's high-conviction calls, repeat mentions, and master-team disagreements.",
    wallet: zh ? "钱包" : "Wallet",
    kyc: zh ? "KYC" : "KYC",
    marketplace: zh ? "Agent 市场" : "Agent marketplace",
    admin: zh ? "管理员" : "Admin",
    userConsole: zh ? "用户管理" : "User console",
    accountSecurity: zh ? "账户安全" : "Account",
    logout: zh ? "退出" : "Logout",
    currentUser: zh ? "当前用户" : "Signed in as",
    footerCompliance: zh
      ? "页面所有内容仅供研究参考，不构成证券、保险、税务或法律建议。模型产出可能存在错误，请结合自身判断。"
      : "All content on this page is for research only and does not constitute securities, insurance, tax or legal advice. Model output may contain errors; use your own judgement.",
    cards: {
      advisorTitleZh: "大师团队 · 个股诊断",
      advisorTitleEn: "Master Team · Single-stock consult",
      advisorDescZh: "输入一只股票，11 位大师人格 + 战术分析师并行出具多视角观察，附最新技术面快照。",
      advisorDescEn: "Enter one ticker; 11 master personas + tactical analysts return parallel multi-lens observations with the latest technical snapshot.",
      advisorCtaZh: "去诊断 →",
      advisorCtaEn: "Open consult →",

      dailyTitleZh: "大师团队 · 每日榜单",
      dailyTitleEn: "Master Team · Daily picks",
      dailyDescZh: "每日预生成、四套预设各 50 只大盘股的并行视角榜单，按日期归档可回看。",
      dailyDescEn: "Daily pre-computed lens grid: four presets × 50 large caps, archived by date and reviewable in-place.",
      dailyCtaZh: "看今日榜单 →",
      dailyCtaEn: "View today's grid →",

      paperTitleZh: "模拟交易盘",
      paperTitleEn: "Paper trading desk",
      paperDescZh: "把大师视角落到模拟账户：下单 / 复盘 / 盈亏追踪，全部纸面、零真实风险。",
      paperDescEn: "Translate the master-team view into a sandbox account: orders, replays, P&L tracking — fully paper, zero real risk.",
      paperCtaZh: "进入模拟盘 →",
      paperCtaEn: "Enter desk →",

      trendingTitleZh: "市场热门榜",
      trendingTitleEn: "Trending — Most Active",
      trendingDescZh: "按成交量异动客观排序的市场观察榜，配披露与方法学说明，不含选股推荐。",
      trendingDescEn: "Observation-only ranking by volume anomaly, with disclosure + methodology — no stock-picking recommendation.",
      trendingCtaZh: "查看热门榜 →",
      trendingCtaEn: "View trending →",

      teamTitleZh: "AI 团队炒股（基金管理）",
      teamTitleEn: "AI team trading (fund management)",
      teamDescZh: "进阶模式：让 PM / 研究员 / 交易员 / 风控 AI 团队协作管理基金组合，含决策中心与回测。",
      teamDescEn: "Advanced mode: PM / Researcher / Trader / Risk AI agents collaborate on a fund portfolio, with decision center & backtests.",
      teamCtaZh: "管理基金 →",
      teamCtaEn: "Manage funds →",
    },
  };
}

const PILL_BASE =
  "inline-flex h-11 items-center rounded-full px-4 text-sm font-semibold ring-1 shadow-pill transition";
const PILL_SAGE   = `${PILL_BASE} bg-sage-50 text-sage-700 ring-sage-200/70 hover:bg-sage-100`;
const PILL_CORAL  = `${PILL_BASE} bg-coral-50 text-coral-500 ring-coral-200/70 hover:bg-coral-100`;
const PILL_INFO   = `${PILL_BASE} bg-cream-0 text-ink-700 ring-ink-100/80 hover:bg-cream-50`;
const PILL_MINT   = `${PILL_BASE} bg-cream-0 text-sage-700 ring-sage-200/70 hover:bg-sage-50`;

// Per-accent card chrome. Centralised so the four (or five) hero
// tiles read at a glance instead of repeating ~20 utility classes
// inline. accent colour ≈ feature family:
//   coral  → personalised / per-stock
//   sage   → published / shared
//   mint   → opt-in / advanced (fund-team)
//   info   → utility (paper / trending)
//   amber  → warning / placeholder
//   violet → reserved for future surfaces
const CARD_ACCENTS: Record<CardAccent, { ring: string; eyebrow: string; cta: string }> = {
  coral:  { ring: "ring-coral-200/80",  eyebrow: "text-coral-500",  cta: "text-coral-500" },
  sage:   { ring: "ring-sage-200/80",   eyebrow: "text-sage-700",   cta: "text-sage-700" },
  mint:   { ring: "ring-emerald-200/80", eyebrow: "text-emerald-700", cta: "text-emerald-700" },
  info:   { ring: "ring-ink-100/80",    eyebrow: "text-ink-700",    cta: "text-ink-700" },
  amber:  { ring: "ring-amber-200/80",  eyebrow: "text-amber-700",  cta: "text-amber-700" },
  violet: { ring: "ring-violet-200/80", eyebrow: "text-violet-700", cta: "text-violet-700" },
};

const MastersHub: React.FC = () => {
  const { language } = useAppPreferences();
  const navigate = useNavigate();
  const copy = useMemo(() => copyForLang(language), [language]);
  const session = getStoredSession();
  const isAdmin = session?.role === "super_admin";
  // The /admin/users console accepts both admin and super_admin
  // server-side (admin_users_handler.requireAdmin), so the nav pill
  // mirrors that. Kept separate from `isAdmin` because the legacy
  // /admin entry remains super-admin-only.
  const canSeeUserConsole = session?.role === "super_admin" || session?.role === "admin";

  // agent_team_mode gates the optional 5th card. We *show* the card
  // when (a) the flag is on for everybody, or (b) the current user
  // is super_admin (who can always reach /companies anyway via the
  // route gate's escape hatch). Hiding it for non-admins when the
  // flag is OFF avoids the dead-link footgun of a clickable tile
  // that just redirects right back here.
  const agentTeamModeEnabled = useFeatureFlag("agent_team_mode", false);
  const showAgentTeamCard = agentTeamModeEnabled || isAdmin;

  const handleLogout = useCallback(async () => {
    try {
      await logoutSession();
    } catch {
      // logoutSession already clears local storage on its own
      // error path; we just want to land on /login regardless.
    }
    navigate("/login", { replace: true });
  }, [navigate]);

  // Card list is computed once per render — small, immutable, and
  // keeps the JSX below uncluttered. The optional team card is
  // appended conditionally.
  const cards: HeroCard[] = useMemo(() => {
    const c = copy.cards;
    const base: HeroCard[] = [
      {
        id: "advisor",
        to: "/advisor",
        titleZh: c.advisorTitleZh, titleEn: c.advisorTitleEn,
        descZh: c.advisorDescZh,   descEn: c.advisorDescEn,
        ctaZh: c.advisorCtaZh,     ctaEn: c.advisorCtaEn,
        accent: "coral",
      },
      {
        id: "daily",
        to: "/daily-picks",
        titleZh: c.dailyTitleZh, titleEn: c.dailyTitleEn,
        descZh: c.dailyDescZh,   descEn: c.dailyDescEn,
        ctaZh: c.dailyCtaZh,     ctaEn: c.dailyCtaEn,
        accent: "sage",
      },
      {
        id: "paper",
        to: "/papertrading",
        titleZh: c.paperTitleZh, titleEn: c.paperTitleEn,
        descZh: c.paperDescZh,   descEn: c.paperDescEn,
        ctaZh: c.paperCtaZh,     ctaEn: c.paperCtaEn,
        accent: "info",
      },
      {
        id: "trending",
        to: "/trending/most-active",
        titleZh: c.trendingTitleZh, titleEn: c.trendingTitleEn,
        descZh: c.trendingDescZh,   descEn: c.trendingDescEn,
        ctaZh: c.trendingCtaZh,     ctaEn: c.trendingCtaEn,
        accent: "info",
      },
    ];
    if (showAgentTeamCard) {
      base.push({
        id: "team",
        to: "/companies",
        titleZh: c.teamTitleZh, titleEn: c.teamTitleEn,
        descZh: c.teamDescZh,   descEn: c.teamDescEn,
        ctaZh: c.teamCtaZh,     ctaEn: c.teamCtaEn,
        accent: "mint",
      });
    }
    return base;
  }, [copy, showAgentTeamCard]);

  const userLabel = session?.displayName || session?.email || copy.brand;

  return (
    <div className="min-h-screen bg-cream-50/40 p-6 sm:p-8 dark:bg-slate-950">
      <div className="mx-auto max-w-6xl">
        {/* Top utility bar — pills mirror Companies.tsx so the
            brand stays consistent if you bounce between surfaces. */}
        <div className="mb-8 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
          <div>
            <p className="text-[11px] font-semibold uppercase tracking-[0.16em] text-sage-700">
              {copy.brand} · {copy.heroEyebrow}
            </p>
            <h1 className="mt-1 text-3xl font-extrabold text-ink-900 dark:text-slate-100">
              {copy.heroTitle}
            </h1>
            <p className="mt-2 max-w-2xl text-sm text-ink-300 dark:text-slate-400">
              {copy.heroSub}
            </p>
          </div>
          <div className="flex flex-wrap items-center justify-end gap-3">
            <div className="rounded-envelope bg-cream-0 px-5 py-4 text-sm text-ink-300 shadow-envelope ring-1 ring-ink-100/60 dark:bg-slate-900 dark:text-slate-400 dark:ring-slate-700">
              <p className="truncate text-xs text-ink-300/80">
                {copy.currentUser}：{userLabel}
              </p>
            </div>
            <Link to="/wallet" className={PILL_SAGE}>{copy.wallet}</Link>
            <Link to="/kyc" className={PILL_CORAL}>{copy.kyc}</Link>
            <Link to="/marketplace" className={PILL_MINT}>{copy.marketplace}</Link>
            {isAdmin ? <Link to="/admin" className={PILL_INFO}>{copy.admin}</Link> : null}
            {canSeeUserConsole ? <Link to="/admin/users" className={PILL_INFO}>{copy.userConsole}</Link> : null}
            <Link to="/account/security" className={PILL_INFO}>{copy.accountSecurity}</Link>
            <button onClick={() => void handleLogout()} className={PILL_INFO}>
              {copy.logout}
            </button>
          </div>
        </div>

        {/* Strategy preset switcher — deep links into DailyPicks
            with ?preset=KEY. We keep this above the hero grid so
            users who just want "today's value picks" can one-click
            past the rest of the navigation. */}
        <section className="mb-8 rounded-envelope bg-cream-0 p-6 shadow-envelope ring-1 ring-ink-100/60 dark:bg-slate-900 dark:ring-slate-700">
          <div className="mb-4 flex flex-col gap-1 sm:flex-row sm:items-end sm:justify-between">
            <div>
              <p className="text-[11px] font-semibold uppercase tracking-[0.16em] text-coral-500">
                {copy.presetTitle}
              </p>
              <p className="text-xs text-ink-300 dark:text-slate-400">{copy.presetSub}</p>
            </div>
          </div>
          <div className="flex flex-wrap gap-2">
            {PRESET_PILLS.map((p) => (
              <Link
                key={p.key}
                to={`/daily-picks?preset=${encodeURIComponent(p.key)}`}
                className="group inline-flex flex-col rounded-2xl bg-cream-50 px-4 py-3 ring-1 ring-ink-100/60 transition hover:bg-sage-50 hover:ring-sage-200 dark:bg-slate-800 dark:ring-slate-700 dark:hover:bg-slate-700"
              >
                <span className="text-sm font-semibold text-ink-900 group-hover:text-sage-700 dark:text-slate-100">
                  {language === "zh-CN" ? p.labelZh : p.labelEn}
                </span>
                <span className="mt-0.5 text-[11px] text-ink-300 dark:text-slate-400">
                  {language === "zh-CN" ? p.subZh : p.subEn}
                </span>
              </Link>
            ))}
          </div>
        </section>

        {/* Hero card grid — 1 col on mobile, 2 cols ≥ md.
            Each card is a real <Link> for keyboard / new-tab nav. */}
        <section>
          <p className="mb-3 text-[11px] font-semibold uppercase tracking-[0.16em] text-sage-700">
            {copy.cardsTitle}
          </p>
          <div className="grid grid-cols-1 gap-5 md:grid-cols-2">
            {cards.map((card) => {
              const a = CARD_ACCENTS[card.accent];
              return (
                <Link
                  key={card.id}
                  to={card.to}
                  className={`group flex h-full flex-col justify-between rounded-envelope bg-cream-0 p-6 shadow-envelope ring-1 ${a.ring} transition hover:-translate-y-0.5 hover:shadow-lg dark:bg-slate-900 dark:ring-slate-700`}
                >
                  <div>
                    <p className={`text-[11px] font-semibold uppercase tracking-[0.16em] ${a.eyebrow}`}>
                      {copy.heroEyebrow}
                    </p>
                    <h2 className="mt-2 text-xl font-bold text-ink-900 dark:text-slate-100">
                      {language === "zh-CN" ? card.titleZh : card.titleEn}
                    </h2>
                    <p className="mt-2 text-sm text-ink-500 dark:text-slate-400">
                      {language === "zh-CN" ? card.descZh : card.descEn}
                    </p>
                  </div>
                  <p
                    className={`mt-6 text-sm font-semibold ${a.cta} transition group-hover:translate-x-1`}
                  >
                    {language === "zh-CN" ? card.ctaZh : card.ctaEn}
                  </p>
                </Link>
              );
            })}
          </div>
        </section>

        {/* Insights placeholder. Kept intentionally lightweight per
            spec — content stream is a follow-up task once we wire
            an aggregator endpoint. */}
        <section className="mt-10">
          <p className="mb-3 text-[11px] font-semibold uppercase tracking-[0.16em] text-amber-700">
            {copy.weeklyTitle}
          </p>
          <div className="rounded-envelope bg-cream-0 p-6 text-sm text-ink-300 shadow-envelope ring-1 ring-amber-200/60 dark:bg-slate-900 dark:text-slate-400 dark:ring-amber-500/30">
            {copy.weeklyComingSoon}
          </div>
        </section>

        <footer className="mt-10 text-[11px] leading-relaxed text-ink-300/80 dark:text-slate-500">
          {copy.footerCompliance}
        </footer>
      </div>
    </div>
  );
};

export default MastersHub;
