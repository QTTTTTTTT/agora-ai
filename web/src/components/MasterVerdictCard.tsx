import React from "react";
import type { AdvisorMasterReport, AdvisorVerdict } from "../lib/api";
import {
  formatModelVerdict,
  useCompliance,
  type ComplianceLocale,
} from "../lib/compliance";

// MasterVerdictCard — one card per master in the panel response.
// Renders: master name (zh / en), verdict pill, confidence bar,
// thesis paragraph, key reasons, key risks, red-line warnings,
// and the master-specific JSON (intrinsic value / PEG / Graham
// number / CANSLIM score / …) as a key-value table.
//
// The visual language matches AnalystPanelSection.tsx's
// directionTone / confidence pill so a user moving between fund
// mode and advisor mode sees a consistent verdict UI.

const VERDICT_TONE: Record<string, { label: string; bg: string; fg: string }> = {
  STRONG_BUY: { label: "STRONG BUY", bg: "bg-emerald-100", fg: "text-emerald-800" },
  BUY: { label: "BUY", bg: "bg-emerald-50", fg: "text-emerald-700" },
  HOLD: { label: "HOLD", bg: "bg-slate-100", fg: "text-slate-700" },
  AVOID: { label: "AVOID", bg: "bg-rose-50", fg: "text-rose-700" },
  PASS: { label: "PASS", bg: "bg-slate-50", fg: "text-slate-500" },
  SKIP: { label: "SKIP", bg: "bg-slate-50", fg: "text-slate-500" },
  SHORT: { label: "SHORT", bg: "bg-rose-100", fg: "text-rose-800" },
  MIXED: { label: "MIXED", bg: "bg-amber-50", fg: "text-amber-700" },
};

function verdictPill(verdict: AdvisorVerdict | string): {
  label: string;
  bg: string;
  fg: string;
} {
  const v = String(verdict || "HOLD").toUpperCase();
  return VERDICT_TONE[v] ?? VERDICT_TONE.HOLD;
}

function formatMasterSpecific(value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "string") return value;
  if (typeof value === "number") return String(value);
  if (typeof value === "boolean") return value ? "✓" : "✗";
  return JSON.stringify(value);
}

export interface MasterVerdictCardProps {
  report: AdvisorMasterReport;
  language: string;
}

const MasterVerdictCard: React.FC<MasterVerdictCardProps> = ({ report, language }) => {
  const pill = verdictPill(report.verdict);
  const name = language === "en-US" ? report.master_name_en : report.master_name_zh;
  const subName = language === "en-US" ? report.master_name_zh : report.master_name_en;
  const specificEntries = report.master_specific
    ? Object.entries(report.master_specific).filter(([, v]) => v !== null && v !== undefined && v !== "")
    : [];
  const { mode } = useCompliance();
  const complianceLocale: ComplianceLocale =
    language === "en-US" ? "en-US" : "zh-CN";
  // We keep the colour-coded pill (so a quick visual scan still
  // works) but replace the bare verdict text with the
  // Publisher-mode-safe "Model rating: …" framing.
  const pillLabel = formatModelVerdict(report.verdict, complianceLocale, mode);

  return (
    <article className="flex flex-col gap-3 rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
      <header className="flex items-start justify-between gap-3">
        <div>
          <h3 className="text-base font-semibold text-slate-900">{name || report.master_key}</h3>
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

      {report.thesis ? (
        <p className="text-sm leading-relaxed text-slate-700">{report.thesis}</p>
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

      {specificEntries.length > 0 ? (
        <div className="rounded-md bg-slate-50 p-3 text-xs">
          <div className="mb-1 font-semibold text-slate-600">
            {language === "en-US" ? "Master-specific" : "大师专属指标"}
          </div>
          <table className="w-full table-fixed">
            <tbody>
              {specificEntries.map(([k, v]) => (
                <tr key={k} className="border-b border-slate-100 last:border-0">
                  <td className="w-2/5 py-1 pr-2 text-slate-500">{k}</td>
                  <td className="break-words py-1 text-slate-800">{formatMasterSpecific(v)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <footer className="flex items-center justify-between text-[10px] text-slate-400">
        <span>{report.llm_model || "fallback"}</span>
        <span>{report.generated_at ? new Date(report.generated_at).toLocaleString() : ""}</span>
      </footer>
    </article>
  );
};

const ConfidenceBar: React.FC<{ value: number }> = ({ value }) => {
  const clamped = Math.max(0, Math.min(100, value));
  return (
    <div className="flex items-center gap-2">
      <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-slate-100">
        <div
          className="h-full rounded-full bg-indigo-500"
          style={{ width: `${clamped}%` }}
        />
      </div>
      <span className="w-10 text-right text-xs font-medium tabular-nums text-slate-600">
        {clamped}%
      </span>
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

export default MasterVerdictCard;
