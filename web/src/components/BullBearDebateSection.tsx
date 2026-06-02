// BullBearDebateSection — S8.2 per-fund Bull / Bear debate UI.
//
// Operator picks a symbol + rounds count, hits Run. The server
// runs the analyst panel and immediately drives Bull vs Bear
// debate over it. The component then renders:
//   - the panel-level aggregate verdict (so the operator sees
//     what the advocates were arguing over),
//   - the debate verdict (winner, contested flag, bull vs bear
//     confidence),
//   - per-round Bull / Bear cards laid out side-by-side so the
//     adversarial structure is visually obvious.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  formatApiError,
  getFundDebate,
  listFundDebates,
  runFundDebate,
  type AdvocateStance,
  type AnalystDirection,
  type AnalystPanelReport,
  type DebateArgument,
  type DebateTranscript,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface BullBearDebateMessages {
  title: string;
  subtitle: string;
  symbolLabel: string;
  symbolPlaceholder: string;
  roundsLabel: string;
  runButton: string;
  running: string;
  notesLabel: string;
  verdictTitle: string;
  verdictDirection: string;
  verdictConfidence: string;
  verdictContested: string;
  verdictNotContested: string;
  bullConfidence: string;
  bearConfidence: string;
  winnerBull: string;
  winnerBear: string;
  winnerTie: string;
  argumentsTitle: string;
  roundLabel: string;
  stanceBull: string;
  stanceBear: string;
  thesisLabel: string;
  supportPointsLabel: string;
  rebuttalsLabel: string;
  citedReportsLabel: string;
  noDebateYet: string;
  error: string;
  historyTitle: string;
  historyEmpty: string;
  historyLoading: string;
  confidenceLabel: string;
  llmModelFallback: string;
  llmModelLLM: string;
}

const messages: Record<Language, BullBearDebateMessages> = {
  "zh-CN": {
    title: "多空对辩",
    subtitle:
      "基于分析师面板的结论，强制 Bull / Bear 两位研究员各执一词、交替反驳。Bull 必须找出最强买入理由，Bear 必须找出最强卖出 / 回避理由，谁也不能中立。",
    symbolLabel: "标的代码",
    symbolPlaceholder: "如 AAPL / 600519",
    roundsLabel: "辩论轮数",
    runButton: "运行多空对辩",
    running: "多空研究员对辩中…",
    notesLabel: "备注",
    verdictTitle: "对辩裁决",
    verdictDirection: "方向",
    verdictConfidence: "置信度",
    verdictContested: "势均力敌（多空差距 < 20%）",
    verdictNotContested: "分差明显",
    bullConfidence: "多头平均置信度",
    bearConfidence: "空头平均置信度",
    winnerBull: "多头胜出",
    winnerBear: "空头胜出",
    winnerTie: "平局",
    argumentsTitle: "逐轮发言",
    roundLabel: "第 {round} 轮",
    stanceBull: "多头",
    stanceBear: "空头",
    thesisLabel: "论点",
    supportPointsLabel: "支撑证据",
    rebuttalsLabel: "反驳对手",
    citedReportsLabel: "引用分析师",
    noDebateYet: "尚未运行对辩",
    error: "对辩运行失败",
    historyTitle: "历史对辩",
    historyEmpty: "暂无历史对辩",
    historyLoading: "加载中…",
    confidenceLabel: "置信度 {value}%",
    llmModelFallback: "规则回退",
    llmModelLLM: "LLM",
  },
  "en-US": {
    title: "Bull / Bear Debate",
    subtitle:
      "Two forced personas — Bull and Bear — argue against each other over the analyst panel's conclusions. Bull must find the strongest reason to buy; Bear must find the strongest reason to sell or avoid. Neither is allowed to settle on neutral.",
    symbolLabel: "Symbol",
    symbolPlaceholder: "e.g. AAPL / 600519",
    roundsLabel: "Debate rounds",
    runButton: "Run debate",
    running: "Researchers debating…",
    notesLabel: "Notes",
    verdictTitle: "Verdict",
    verdictDirection: "Direction",
    verdictConfidence: "Confidence",
    verdictContested: "Contested (margin < 20%)",
    verdictNotContested: "Decisive margin",
    bullConfidence: "Bull avg confidence",
    bearConfidence: "Bear avg confidence",
    winnerBull: "Bull wins",
    winnerBear: "Bear wins",
    winnerTie: "Tie",
    argumentsTitle: "Per-round arguments",
    roundLabel: "Round {round}",
    stanceBull: "Bull",
    stanceBear: "Bear",
    thesisLabel: "Thesis",
    supportPointsLabel: "Support points",
    rebuttalsLabel: "Rebuttals",
    citedReportsLabel: "Cited analysts",
    noDebateYet: "No debate run yet",
    error: "Debate run failed",
    historyTitle: "Historical debates",
    historyEmpty: "No historical debates yet",
    historyLoading: "Loading…",
    confidenceLabel: "confidence {value}%",
    llmModelFallback: "rule fallback",
    llmModelLLM: "LLM",
  },
};

export interface BullBearDebateSectionProps {
  fundId?: string;
  language?: Language;
}

function directionTone(d: AnalystDirection | string): string {
  if (d === "bullish") return "text-emerald-700 bg-emerald-50 border-emerald-200";
  if (d === "bearish") return "text-rose-700 bg-rose-50 border-rose-200";
  return "text-slate-700 bg-slate-50 border-slate-200";
}

function stanceTone(s: AdvocateStance | string): string {
  if (s === "bull") return "text-emerald-700 bg-emerald-50 border-emerald-200";
  return "text-rose-700 bg-rose-50 border-rose-200";
}

function directionLabel(m: BullBearDebateMessages, d: AnalystDirection | string): string {
  if (d === "bullish") return m.winnerBull;
  if (d === "bearish") return m.winnerBear;
  return m.winnerTie;
}

function stanceLabel(m: BullBearDebateMessages, s: AdvocateStance | string): string {
  if (s === "bull") return m.stanceBull;
  return m.stanceBear;
}

function formatTime(value?: string): string {
  if (!value) return "";
  try {
    const d = new Date(value);
    if (Number.isNaN(d.getTime())) return value;
    return d.toLocaleString();
  } catch {
    return value;
  }
}

function interpolate(template: string, vars: Record<string, string | number>): string {
  return template.replace(/\{(\w+)\}/g, (_, k) => String(vars[k] ?? ""));
}

// groupByRound returns a map round → [bull?, bear?]. Stable
// order: bull first, bear second.
function groupByRound(args: DebateArgument[]): Map<number, { bull?: DebateArgument; bear?: DebateArgument }> {
  const out = new Map<number, { bull?: DebateArgument; bear?: DebateArgument }>();
  for (const a of args) {
    const slot = out.get(a.round) ?? {};
    if (a.stance === "bull") slot.bull = a;
    else slot.bear = a;
    out.set(a.round, slot);
  }
  return out;
}

export default function BullBearDebateSection({
  fundId,
  language = "zh-CN",
}: BullBearDebateSectionProps) {
  const m = useMemo(() => messages[language], [language]);
  const [symbol, setSymbol] = useState("");
  const [rounds, setRounds] = useState<number>(2);
  const [notes, setNotes] = useState("");
  const [debate, setDebate] = useState<DebateTranscript | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [history, setHistory] = useState<DebateTranscript[]>([]);
  const [historyLoading, setHistoryLoading] = useState(false);

  const loadHistory = useCallback(async () => {
    if (!fundId) return;
    setHistoryLoading(true);
    try {
      const resp = await listFundDebates(fundId, { limit: 20 });
      setHistory(resp.debates ?? []);
    } catch {
      setHistory([]);
    } finally {
      setHistoryLoading(false);
    }
  }, [fundId]);

  useEffect(() => {
    loadHistory().catch(() => {});
  }, [loadHistory]);

  const runDebate = useCallback(async () => {
    const sym = symbol.trim().toUpperCase();
    if (!fundId || !sym) return;
    setLoading(true);
    setError(null);
    try {
      const resp = await runFundDebate(fundId, {
        symbol: sym,
        rounds: rounds,
        notes: notes.trim() || undefined,
        persist: true,
      });
      setDebate(resp.debate);
      loadHistory().catch(() => {});
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoading(false);
    }
  }, [fundId, symbol, rounds, notes, m.error, loadHistory]);

  const loadHistorical = useCallback(
    async (debateId: string) => {
      if (!fundId || !debateId) return;
      try {
        const resp = await getFundDebate(fundId, debateId);
        setDebate(resp.debate);
        setSymbol(resp.debate.symbol);
      } catch (err) {
        setError(formatApiError(err, m.error));
      }
    },
    [fundId, m.error],
  );

  const grouped = useMemo(() => (debate ? groupByRound(debate.arguments) : new Map()), [debate]);
  const sortedRounds = useMemo(
    () => (debate ? Array.from(new Set(debate.arguments.map((a) => a.round))).sort((a, b) => a - b) : []),
    [debate],
  );

  return (
    <section className="space-y-4 rounded-2xl border border-gray-100 bg-white p-6 shadow-sm">
      <header className="space-y-1">
        <h2 className="text-lg font-semibold text-gray-900">{m.title}</h2>
        <p className="text-sm text-gray-500">{m.subtitle}</p>
      </header>

      <div className="grid grid-cols-1 gap-3 md:grid-cols-4">
        <label className="flex flex-col gap-1 text-sm">
          <span className="text-gray-700">{m.symbolLabel}</span>
          <input
            value={symbol}
            onChange={(e) => setSymbol(e.target.value)}
            placeholder={m.symbolPlaceholder}
            className="rounded-lg border border-gray-200 px-3 py-2"
          />
        </label>
        <label className="flex flex-col gap-1 text-sm">
          <span className="text-gray-700">{m.roundsLabel}</span>
          <input
            type="number"
            min={1}
            max={5}
            value={rounds}
            onChange={(e) => setRounds(Math.max(1, Math.min(5, Number(e.target.value) || 1)))}
            className="rounded-lg border border-gray-200 px-3 py-2"
          />
        </label>
        <label className="flex flex-col gap-1 text-sm md:col-span-2">
          <span className="text-gray-700">{m.notesLabel}</span>
          <input
            value={notes}
            onChange={(e) => setNotes(e.target.value)}
            placeholder=""
            className="rounded-lg border border-gray-200 px-3 py-2"
          />
        </label>
      </div>

      <div>
        <button
          type="button"
          onClick={runDebate}
          disabled={loading || !symbol.trim() || !fundId}
          className="rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700 disabled:cursor-not-allowed disabled:bg-indigo-300"
        >
          {loading ? m.running : m.runButton}
        </button>
      </div>

      {error ? (
        <div className="rounded-lg border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700">
          {error}
        </div>
      ) : null}

      {debate ? (
        <>
          <DebateVerdictBanner m={m} debate={debate} />
          {debate.panel ? <PanelMiniBadge m={m} panel={debate.panel} /> : null}
          <div>
            <h3 className="mb-2 text-sm font-semibold text-gray-900">{m.argumentsTitle}</h3>
            <div className="space-y-3">
              {sortedRounds.map((r) => (
                <RoundRow key={r} m={m} round={r} bull={grouped.get(r)?.bull} bear={grouped.get(r)?.bear} />
              ))}
            </div>
          </div>
        </>
      ) : (
        <p className="text-sm text-gray-500">{m.noDebateYet}</p>
      )}

      <div>
        <h3 className="mb-2 text-sm font-semibold text-gray-900">{m.historyTitle}</h3>
        {historyLoading ? (
          <p className="text-sm text-gray-500">{m.historyLoading}</p>
        ) : history.length === 0 ? (
          <p className="text-sm text-gray-500">{m.historyEmpty}</p>
        ) : (
          <ul className="divide-y divide-gray-100 rounded-lg border border-gray-100">
            {history.map((h) => (
              <li key={h.id ?? `${h.symbol}-${h.generated_at}`} className="flex flex-wrap items-center justify-between gap-2 px-3 py-2 text-sm">
                <span className="font-medium text-gray-900">{h.symbol}</span>
                <span className={`rounded-full border px-2 py-0.5 text-xs ${directionTone(h.verdict.direction)}`}>
                  {directionLabel(m, h.verdict.direction)} · {h.verdict.confidence}%
                </span>
                <span className="text-xs text-gray-500">{formatTime(h.generated_at)}</span>
                <button
                  type="button"
                  onClick={() => h.id && loadHistorical(h.id)}
                  className="rounded border border-gray-200 px-2 py-1 text-xs text-gray-700 hover:bg-gray-50"
                >
                  Open
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

interface DebateVerdictBannerProps {
  m: BullBearDebateMessages;
  debate: DebateTranscript;
}

function DebateVerdictBanner({ m, debate }: DebateVerdictBannerProps) {
  const tone = directionTone(debate.verdict.direction);
  return (
    <div className={`rounded-xl border px-4 py-3 ${tone}`}>
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div className="text-sm font-medium uppercase tracking-wide opacity-70">
          {m.verdictTitle} · {debate.symbol}
        </div>
        <div className="text-xs opacity-70">{formatTime(debate.generated_at)}</div>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-x-6 gap-y-1 text-sm">
        <span>
          <span className="font-medium">{m.verdictDirection}：</span>
          {directionLabel(m, debate.verdict.direction)}
        </span>
        <span>
          <span className="font-medium">{m.verdictConfidence}：</span>
          {debate.verdict.confidence}%
        </span>
        <span>
          <span className="font-medium">{m.bullConfidence}：</span>
          {debate.verdict.bull_confidence}%
        </span>
        <span>
          <span className="font-medium">{m.bearConfidence}：</span>
          {debate.verdict.bear_confidence}%
        </span>
        <span className="text-xs opacity-80">
          {debate.verdict.contested ? m.verdictContested : m.verdictNotContested}
        </span>
      </div>
    </div>
  );
}

interface PanelMiniBadgeProps {
  m: BullBearDebateMessages;
  panel: AnalystPanelReport;
}

function PanelMiniBadge({ panel }: PanelMiniBadgeProps) {
  return (
    <div className="rounded-lg border border-gray-200 bg-gray-50 px-3 py-2 text-xs text-gray-700">
      <span className="font-medium">Panel feed:</span> {panel.symbol} —{" "}
      <span className={`inline-block rounded-full border px-2 py-0.5 ${directionTone(panel.aggregate_direction)}`}>
        {panel.aggregate_direction} · {panel.aggregate_confidence}%
      </span>{" "}
      ({panel.categories_voted}/4 voted)
    </div>
  );
}

interface RoundRowProps {
  m: BullBearDebateMessages;
  round: number;
  bull?: DebateArgument;
  bear?: DebateArgument;
}

function RoundRow({ m, round, bull, bear }: RoundRowProps) {
  return (
    <div>
      <div className="mb-1 text-xs font-semibold uppercase tracking-wide text-gray-500">
        {interpolate(m.roundLabel, { round })}
      </div>
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        {bull ? <ArgumentCard m={m} a={bull} /> : <PlaceholderCard m={m} stance="bull" />}
        {bear ? <ArgumentCard m={m} a={bear} /> : <PlaceholderCard m={m} stance="bear" />}
      </div>
    </div>
  );
}

interface ArgumentCardProps {
  m: BullBearDebateMessages;
  a: DebateArgument;
}

function ArgumentCard({ m, a }: ArgumentCardProps) {
  return (
    <div className={`flex h-full flex-col gap-2 rounded-xl border px-4 py-3 ${stanceTone(a.stance)}`}>
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div className="text-sm font-semibold text-gray-900">{stanceLabel(m, a.stance)}</div>
        <div className="text-xs opacity-70">
          {a.llm_model === "llm" ? m.llmModelLLM : m.llmModelFallback} ·{" "}
          {interpolate(m.confidenceLabel, { value: a.confidence })}
        </div>
      </div>
      <div className="text-xs text-gray-500">{a.agent_name}</div>
      <p className="text-sm text-gray-800">{a.thesis}</p>
      {a.support_points && a.support_points.length > 0 ? (
        <div>
          <div className="text-xs font-medium text-gray-700">{m.supportPointsLabel}</div>
          <ul className="ml-4 list-disc text-xs text-gray-700">
            {a.support_points.map((s, i) => (
              <li key={`${a.agent_id}-sp-${i}`}>{s}</li>
            ))}
          </ul>
        </div>
      ) : null}
      {a.rebuttals && a.rebuttals.length > 0 ? (
        <div>
          <div className="text-xs font-medium text-gray-700">{m.rebuttalsLabel}</div>
          <ul className="ml-4 list-disc text-xs text-rose-700">
            {a.rebuttals.map((s, i) => (
              <li key={`${a.agent_id}-rb-${i}`}>{s}</li>
            ))}
          </ul>
        </div>
      ) : null}
      {a.cited_reports && a.cited_reports.length > 0 ? (
        <div className="text-xs text-gray-600">
          <span className="font-medium">{m.citedReportsLabel}:</span> {a.cited_reports.join(", ")}
        </div>
      ) : null}
    </div>
  );
}

interface PlaceholderCardProps {
  m: BullBearDebateMessages;
  stance: AdvocateStance;
}

function PlaceholderCard({ m, stance }: PlaceholderCardProps) {
  return (
    <div className={`flex h-full flex-col gap-2 rounded-xl border border-dashed px-4 py-3 ${stanceTone(stance)} opacity-60`}>
      <div className="text-sm font-semibold text-gray-900">{stanceLabel(m, stance)}</div>
      <p className="text-xs text-gray-500">—</p>
    </div>
  );
}
