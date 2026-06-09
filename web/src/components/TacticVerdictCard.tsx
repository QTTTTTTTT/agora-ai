import React from "react";
import type { AdvisorTacticReport } from "../lib/api";
import {
  formatModelVerdict,
  formatPriceLabel,
  useCompliance,
  type ComplianceLocale,
} from "../lib/compliance";

// TacticVerdictCard — one card per A-share short-term tactic in the
// panel response. Distinct from MasterVerdictCard because tactic
// agents produce trading-plan data (entry / stop / target prices,
// expected holding window, market-regime gate result) instead of a
// long-horizon BUY/HOLD verdict.

const VERDICT_TONE: Record<string, { label: string; bg: string; fg: string }> = {
  BUY_TAIL: { label: "BUY TAIL", bg: "bg-emerald-100", fg: "text-emerald-800" },
  BUY_DIP: { label: "BUY DIP", bg: "bg-emerald-50", fg: "text-emerald-700" },
  BUY: { label: "BUY", bg: "bg-emerald-50", fg: "text-emerald-700" },
  CHASE_LIMIT_UP: { label: "CHASE LIMIT UP", bg: "bg-emerald-100", fg: "text-emerald-800" },
  WAIT_FOR_WINDOW: { label: "WAIT (窗口)", bg: "bg-amber-50", fg: "text-amber-700" },
  WAIT_FOR_CONFIRMATION: { label: "WAIT (确认)", bg: "bg-amber-50", fg: "text-amber-700" },
  SKIP: { label: "SKIP", bg: "bg-slate-100", fg: "text-slate-500" },
  MIXED: { label: "MIXED", bg: "bg-amber-50", fg: "text-amber-700" },
};

function verdictPill(verdict: string): { label: string; bg: string; fg: string } {
  const v = String(verdict || "SKIP").toUpperCase();
  return VERDICT_TONE[v] ?? VERDICT_TONE.SKIP;
}

function fmtPrice(p: number | null | undefined): string {
  if (p === null || p === undefined || Number.isNaN(p)) return "—";
  return Number(p).toFixed(2);
}

export interface TacticVerdictCardProps {
  report: AdvisorTacticReport;
  language: string;
}

const TacticVerdictCard: React.FC<TacticVerdictCardProps> = ({ report, language }) => {
  const pill = verdictPill(report.verdict);
  const name = language === "en-US" ? report.tactic_name_en : report.tactic_name_zh;
  const subName = language === "en-US" ? report.tactic_name_zh : report.tactic_name_en;
  const hasEntry = report.entry_price_low != null && report.entry_price_high != null;
  const { mode } = useCompliance();
  const complianceLocale: ComplianceLocale =
    language === "en-US" ? "en-US" : "zh-CN";
  const pillLabel = formatModelVerdict(report.verdict, complianceLocale, mode);

  return (
    <article className="flex flex-col gap-3 rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
      <header className="flex items-start justify-between gap-3">
        <div>
          <h3 className="text-base font-semibold text-slate-900">{name || report.tactic_key}</h3>
          {subName ? <p className="text-xs text-slate-500">{subName}</p> : null}
        </div>
        <span
          title={pill.label}
          className={`rounded-full px-3 py-1 text-xs font-semibold ${pill.bg} ${pill.fg}`}
        >
          {pillLabel}
        </span>
      </header>

      <ConfidenceBar value={report.confidence} />

      {!report.market_regime_pass && report.market_regime_reason ? (
        <div className="rounded-md bg-amber-50 p-3 text-xs text-amber-700">
          <div className="mb-1 font-semibold">
            {language === "en-US" ? "Market regime blocked" : "大盘环境否决"}
          </div>
          <div>{report.market_regime_reason}</div>
        </div>
      ) : null}

      {report.thesis ? (
        <p className="text-sm leading-relaxed text-slate-700">{report.thesis}</p>
      ) : null}

      {hasEntry ? (
        <div className="rounded-md border border-slate-100 bg-slate-50 p-3 text-xs">
          <div className="mb-2 grid grid-cols-2 gap-2">
            <Field
              label={formatPriceLabel("entryLow", complianceLocale, mode)}
              value={fmtPrice(report.entry_price_low)}
            />
            <Field
              label={formatPriceLabel("entryHigh", complianceLocale, mode)}
              value={fmtPrice(report.entry_price_high)}
            />
            <Field
              label={formatPriceLabel("stopLoss", complianceLocale, mode)}
              value={fmtPrice(report.stop_loss_price)}
              tone="negative"
            />
            <Field
              label={language === "en-US" ? "Holding (days)" : "预计持有(天)"}
              value={report.expected_holding_days != null ? String(report.expected_holding_days) : "—"}
            />
            <Field
              label={`${formatPriceLabel("target", complianceLocale, mode)} · T1`}
              value={fmtPrice(report.target_t1)}
              tone="positive"
            />
            <Field
              label={`${formatPriceLabel("target", complianceLocale, mode)} · T3`}
              value={fmtPrice(report.target_t3)}
              tone="positive"
            />
          </div>
        </div>
      ) : null}

      {report.red_lines_hit && report.red_lines_hit.length > 0 ? (
        <div className="rounded-md bg-rose-50 p-3 text-xs text-rose-700">
          <div className="mb-1 font-semibold">
            {language === "en-US" ? "Red lines hit" : "触发红线"}
          </div>
          <ul className="list-disc space-y-0.5 pl-4">
            {report.red_lines_hit.map((rl, i) => (
              <li key={i}>{rl}</li>
            ))}
          </ul>
        </div>
      ) : null}

      <BulletSection
        title={language === "en-US" ? "Key reasons" : "关键理由"}
        items={report.key_reasons}
        tone="positive"
      />
      <BulletSection
        title={language === "en-US" ? "Key risks" : "关键风险"}
        items={report.key_risks}
        tone="negative"
      />

      <footer className="flex items-center justify-between text-[10px] text-slate-400">
        <span>score={report.score.toFixed(2)}</span>
        <span>{report.generated_at ? new Date(report.generated_at).toLocaleString() : ""}</span>
      </footer>
    </article>
  );
};

const Field: React.FC<{ label: string; value: string; tone?: "positive" | "negative" }> = ({
  label,
  value,
  tone,
}) => {
  const toneCls =
    tone === "positive" ? "text-emerald-700" : tone === "negative" ? "text-rose-700" : "text-slate-800";
  return (
    <div className="flex flex-col">
      <span className="text-[10px] uppercase tracking-wide text-slate-500">{label}</span>
      <span className={`text-sm font-medium tabular-nums ${toneCls}`}>{value}</span>
    </div>
  );
};

const ConfidenceBar: React.FC<{ value: number }> = ({ value }) => {
  const clamped = Math.max(0, Math.min(100, value));
  return (
    <div className="flex items-center gap-2">
      <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-slate-100">
        <div className="h-full rounded-full bg-indigo-500" style={{ width: `${clamped}%` }} />
      </div>
      <span className="w-10 text-right text-xs font-medium tabular-nums text-slate-600">{clamped}%</span>
    </div>
  );
};

const BulletSection: React.FC<{
  title: string;
  items?: string[];
  tone: "positive" | "negative";
}> = ({ title, items, tone }) => {
  if (!items || items.length === 0) return null;
  const dotClass = tone === "positive" ? "bg-emerald-400" : "bg-rose-400";
  return (
    <section>
      <h4 className="mb-1 text-xs font-semibold uppercase tracking-wide text-slate-500">{title}</h4>
      <ul className="space-y-1">
        {items.map((it, i) => (
          <li key={i} className="flex gap-2 text-xs text-slate-700">
            <span className={`mt-1.5 h-1.5 w-1.5 flex-shrink-0 rounded-full ${dotClass}`} />
            <span className="leading-relaxed">{it}</span>
          </li>
        ))}
      </ul>
    </section>
  );
};

export default TacticVerdictCard;
