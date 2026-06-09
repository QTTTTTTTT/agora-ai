import React, { useEffect, useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { fetchAdvisorHealth, type AdvisorHealthResponse } from "../lib/api";
import { useAppPreferences } from "../lib/preferences";
import { useFeatureFlag } from "../lib/featureFlags";
import AdvisorTrackRecordPanel from "../components/AdvisorTrackRecordPanel";

// Welcome — landing page that lets the user pick between the two
// product modes:
//
//   1. /companies         — the existing "build your own AI team"
//                           flow (Company → Fund → Team).
//   2. /advisor           — the new "consult an AI master panel"
//                           flow (just type a ticker, get N pros'
//                           verdicts).
//
// Loaded for first-time users (no company yet) and reachable from
// the global nav for everyone else. We deliberately avoid auto-
// redirecting either way — the user always picks. The advisor card
// is hidden when the `advisor_mode` feature flag is OFF.
//
// Visual: two large cards on a light background, mirrors the
// "pillar" layout that Companies / MultiFundOverview use.

const Welcome: React.FC = () => {
  const navigate = useNavigate();
  const { language } = useAppPreferences();
  const advisorFlagOn = useFeatureFlag("advisor_mode", true);
  const [health, setHealth] = useState<AdvisorHealthResponse | null>(null);
  const [healthError, setHealthError] = useState<string | null>(null);

  useEffect(() => {
    if (!advisorFlagOn) return;
    let cancelled = false;
    fetchAdvisorHealth()
      .then((h) => {
        if (!cancelled) setHealth(h);
      })
      .catch((e: unknown) => {
        if (!cancelled) setHealthError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [advisorFlagOn]);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            heading: "How would you like to invest today?",
            sub: "Pick a workflow. You can switch any time from the top nav.",
            companyTitle: "Build your own AI team",
            companyDesc:
              "Run a fund powered by your hand-tuned PM, researcher, trader, and risk agents. Full workflow: research, debate, plan, trade, review.",
            companyCta: "Open the fund console",
            advisorTitle: "Consult a master team",
            advisorDesc:
              "Type a ticker, pick a style (Buffett, Lynch, Marks…) — get the panel's verdict in seconds. Read-only: this surface never places trades.",
            advisorCta: "Ask the master panel",
            advisorMastersLoaded: (n: boolean) =>
              n ? "Master personas loaded" : "Master personas not loaded",
            advisorTacticsLoaded: (n: boolean) =>
              n ? "A-share tactics loaded" : "A-share tactics not loaded yet",
            advisorOff: "Advisor mode has been paused by your platform admin.",
            healthError: "Advisor service unreachable: ",
          }
        : {
            heading: "今天打算怎么投？",
            sub: "选一个工作流，随时可在顶部导航切换。",
            companyTitle: "自己培养 AI 团队",
            companyDesc:
              "由你手调的 PM / 研究员 / 交易员 / 风控组成的基金团队，完整的研究 → 辩论 → 计划 → 执行 → 复盘工作流。",
            companyCta: "进入基金控制台",
            advisorTitle: "向大师团队咨询",
            advisorDesc:
              "输入股票代码，选风格（巴菲特 / 林奇 / 马克斯…），数秒拿到大师团的投票结论。本模式只读，不会下任何单。",
            advisorCta: "请大师团把脉",
            advisorMastersLoaded: (n: boolean) => (n ? "国际大师 persona 已加载" : "大师 persona 未加载"),
            advisorTacticsLoaded: (n: boolean) =>
              n ? "A 股短线战法已加载" : "A 股短线战法尚未上线（Phase 4 后开放）",
            advisorOff: "管理员已暂停大师团队咨询模式。",
            healthError: "无法连接大师团服务：",
          },
    [language],
  );

  const advisorVisible = advisorFlagOn;

  return (
    <div className="min-h-screen bg-gradient-to-br from-slate-50 via-white to-emerald-50/40 py-16 px-6">
      <div className="mx-auto max-w-5xl">
        <header className="mb-10 text-center">
          <h1 className="text-3xl font-semibold tracking-tight text-slate-900 sm:text-4xl">
            {copy.heading}
          </h1>
          <p className="mt-3 text-sm text-slate-500">{copy.sub}</p>
        </header>

        <div className="grid grid-cols-1 gap-6 md:grid-cols-2">
          <ModeCard
            title={copy.companyTitle}
            description={copy.companyDesc}
            accent="emerald"
            ctaLabel={copy.companyCta}
            onActivate={() => navigate("/companies")}
            badge={language === "en-US" ? "Existing" : "已有功能"}
          />
          {advisorVisible ? (
            <ModeCard
              title={copy.advisorTitle}
              description={copy.advisorDesc}
              accent="indigo"
              ctaLabel={copy.advisorCta}
              onActivate={() => navigate("/advisor")}
              badge={language === "en-US" ? "New" : "新功能"}
              footer={
                <div className="mt-4 space-y-1 text-xs text-slate-500">
                  {health ? (
                    <>
                      <div className={health.masters_loaded ? "text-emerald-600" : "text-amber-600"}>
                        • {copy.advisorMastersLoaded(health.masters_loaded)}
                      </div>
                      <div className={health.tactics_loaded ? "text-emerald-600" : "text-slate-400"}>
                        • {copy.advisorTacticsLoaded(health.tactics_loaded)}
                      </div>
                    </>
                  ) : healthError ? (
                    <div className="text-rose-600">• {copy.healthError}{healthError}</div>
                  ) : (
                    <div className="text-slate-400">• …</div>
                  )}
                </div>
              }
            />
          ) : (
            <div className="rounded-2xl border border-dashed border-slate-200 bg-white/70 p-8 text-center text-sm text-slate-400">
              {copy.advisorOff}
            </div>
          )}
        </div>

        {advisorVisible ? (
          <div className="mt-10">
            <AdvisorTrackRecordPanel />
          </div>
        ) : null}

        <footer className="mt-10 text-center text-xs text-slate-400">
          <Link to="/masters" className="hover:text-slate-600">
            {language === "en-US" ? "Skip — go to Master Team Hub" : "跳过，进入大师团队 Hub"}
          </Link>
        </footer>
      </div>
    </div>
  );
};

interface ModeCardProps {
  title: string;
  description: string;
  accent: "emerald" | "indigo";
  ctaLabel: string;
  onActivate: () => void;
  badge?: string;
  footer?: React.ReactNode;
}

const ModeCard: React.FC<ModeCardProps> = ({
  title,
  description,
  accent,
  ctaLabel,
  onActivate,
  badge,
  footer,
}) => {
  const accentBg = accent === "indigo" ? "bg-indigo-600 hover:bg-indigo-700" : "bg-emerald-600 hover:bg-emerald-700";
  const accentRing = accent === "indigo" ? "ring-indigo-100" : "ring-emerald-100";
  const accentBadge =
    accent === "indigo" ? "bg-indigo-50 text-indigo-700" : "bg-emerald-50 text-emerald-700";
  return (
    <button
      type="button"
      onClick={onActivate}
      className={`group flex h-full flex-col rounded-2xl border border-slate-200 bg-white p-8 text-left shadow-sm transition hover:shadow-md hover:ring-4 ${accentRing}`}
    >
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold text-slate-900">{title}</h2>
        {badge ? (
          <span className={`rounded-full px-2.5 py-0.5 text-[10px] font-medium ${accentBadge}`}>
            {badge}
          </span>
        ) : null}
      </div>
      <p className="mt-3 flex-1 text-sm leading-relaxed text-slate-600">{description}</p>
      {footer ? <div>{footer}</div> : null}
      <span
        className={`mt-6 inline-flex w-fit items-center gap-2 rounded-full px-4 py-2 text-sm font-medium text-white ${accentBg}`}
      >
        {ctaLabel}
        <svg viewBox="0 0 20 20" className="h-4 w-4 fill-current" aria-hidden>
          <path d="M7.05 4.05a1 1 0 0 1 1.4 0l5.3 5.3a1 1 0 0 1 0 1.4l-5.3 5.3a1 1 0 1 1-1.4-1.4L11.6 11H4a1 1 0 1 1 0-2h7.6L7.05 5.45a1 1 0 0 1 0-1.4z" />
        </svg>
      </span>
    </button>
  );
};

export default Welcome;
