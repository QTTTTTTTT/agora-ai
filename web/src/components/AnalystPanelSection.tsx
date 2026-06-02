// AnalystPanelSection — S8.1 fund-scoped analyst panel UI.
//
// Operator types a symbol (and optionally injects a snapshot of
// price / fundamentals / sentiment / news / technical data), then
// hits Run. The 4 analysts (fundamentals / sentiment / news /
// technical) each return a structured AnalystReport; the panel
// aggregates them into one bullish / bearish / neutral verdict.
//
// For S8.1 the data inputs are minimal — price + change are
// always populated from the form; the per-category blocks are
// only populated when the operator wants to override or test.
// S8.3+ wires in real-time data sources behind a single
// "snapshot" toggle.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  ALL_ANALYST_CATEGORIES,
  formatApiError,
  getFundAnalystPanel,
  listFundAnalystPanels,
  runFundAnalystPanel,
  type AnalystCategory,
  type AnalystDirection,
  type AnalystPanelReport,
  type AnalystReport,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface AnalystPanelMessages {
  title: string;
  subtitle: string;
  symbolLabel: string;
  symbolPlaceholder: string;
  runButton: string;
  running: string;
  persistLabel: string;
  aggregateTitle: string;
  aggregateDirection: string;
  aggregateConfidence: string;
  categoriesVoted: string;
  voteSummary: string;
  perCategoryTitle: string;
  asof: string;
  generatedAt: string;
  directionBullish: string;
  directionBearish: string;
  directionNeutral: string;
  categoryFundamentals: string;
  categorySentiment: string;
  categoryNews: string;
  categoryTechnical: string;
  thesisLabel: string;
  keyFindingsLabel: string;
  risksLabel: string;
  dataPointsLabel: string;
  sourcesLabel: string;
  noPanelYet: string;
  error: string;
  historyTitle: string;
  historyEmpty: string;
  historyLoading: string;
  confidenceLabel: string;
  llmModelFallback: string;
  llmModelLLM: string;
}

const messages: Record<Language, AnalystPanelMessages> = {
  "zh-CN": {
    title: "分析师面板",
    subtitle:
      "基本面 / 情绪 / 新闻 / 技术 四位专业化分析师独立给出结论，面板按各自置信度加权得出整体判断。每位分析师都基于规则给出确定性的方向锚点，LLM 仅在叙述层加成。",
    symbolLabel: "标的代码",
    symbolPlaceholder: "如 AAPL / 600519",
    runButton: "运行分析师面板",
    running: "4 位分析师并行打分中…",
    persistLabel: "存档此次面板",
    aggregateTitle: "综合判断",
    aggregateDirection: "方向",
    aggregateConfidence: "置信度",
    categoriesVoted: "参与表态分析师数",
    voteSummary: "{voted}/{total} 位分析师明确表态",
    perCategoryTitle: "各分析师报告",
    asof: "截至",
    generatedAt: "生成于",
    directionBullish: "看多",
    directionBearish: "看空",
    directionNeutral: "中性",
    categoryFundamentals: "基本面",
    categorySentiment: "情绪",
    categoryNews: "新闻 / 催化",
    categoryTechnical: "技术面",
    thesisLabel: "论点",
    keyFindingsLabel: "关键发现",
    risksLabel: "风险点",
    dataPointsLabel: "数据指标",
    sourcesLabel: "信息源",
    noPanelYet: "尚未运行分析师面板",
    error: "面板运行失败",
    historyTitle: "历史面板",
    historyEmpty: "暂无历史面板",
    historyLoading: "加载中…",
    confidenceLabel: "置信度 {value}%",
    llmModelFallback: "规则回退",
    llmModelLLM: "LLM",
  },
  "en-US": {
    title: "Analyst Panel",
    subtitle:
      "Four specialised analysts (fundamentals / sentiment / news / technical) each produce an independent verdict; the panel blends them by confidence weight. Every analyst is anchored to a deterministic rule; the LLM only fills in the narrative on top.",
    symbolLabel: "Symbol",
    symbolPlaceholder: "e.g. AAPL / 600519",
    runButton: "Run analyst panel",
    running: "Polling 4 analysts in parallel…",
    persistLabel: "Archive this panel",
    aggregateTitle: "Aggregate verdict",
    aggregateDirection: "Direction",
    aggregateConfidence: "Confidence",
    categoriesVoted: "Analysts that voted",
    voteSummary: "{voted} of {total} analysts took a side",
    perCategoryTitle: "Per-analyst reports",
    asof: "As of",
    generatedAt: "Generated at",
    directionBullish: "Bullish",
    directionBearish: "Bearish",
    directionNeutral: "Neutral",
    categoryFundamentals: "Fundamentals",
    categorySentiment: "Sentiment",
    categoryNews: "News / catalysts",
    categoryTechnical: "Technical",
    thesisLabel: "Thesis",
    keyFindingsLabel: "Key findings",
    risksLabel: "Risks",
    dataPointsLabel: "Data points",
    sourcesLabel: "Sources",
    noPanelYet: "No panel run yet",
    error: "Panel run failed",
    historyTitle: "Historical panels",
    historyEmpty: "No historical panels yet",
    historyLoading: "Loading…",
    confidenceLabel: "confidence {value}%",
    llmModelFallback: "rule fallback",
    llmModelLLM: "LLM",
  },
};

export interface AnalystPanelSectionProps {
  fundId?: string;
  language?: Language;
}

function directionTone(d: AnalystDirection): string {
  if (d === "bullish") return "text-emerald-700 bg-emerald-50 border-emerald-200";
  if (d === "bearish") return "text-rose-700 bg-rose-50 border-rose-200";
  return "text-slate-700 bg-slate-50 border-slate-200";
}

function directionLabel(m: AnalystPanelMessages, d: AnalystDirection): string {
  if (d === "bullish") return m.directionBullish;
  if (d === "bearish") return m.directionBearish;
  return m.directionNeutral;
}

function categoryLabel(m: AnalystPanelMessages, c: AnalystCategory): string {
  if (c === "fundamentals") return m.categoryFundamentals;
  if (c === "sentiment") return m.categorySentiment;
  if (c === "news") return m.categoryNews;
  if (c === "technical") return m.categoryTechnical;
  return c;
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

export default function AnalystPanelSection({
  fundId,
  language = "zh-CN",
}: AnalystPanelSectionProps) {
  const m = useMemo(() => messages[language], [language]);
  const [symbol, setSymbol] = useState("");
  const [priceLast, setPriceLast] = useState<string>("");
  const [priceChange, setPriceChange] = useState<string>("");
  const [notes, setNotes] = useState<string>("");
  const [persist, setPersist] = useState(true);
  const [panel, setPanel] = useState<AnalystPanelReport | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [history, setHistory] = useState<AnalystPanelReport[]>([]);
  const [historyLoading, setHistoryLoading] = useState(false);

  const loadHistory = useCallback(async () => {
    if (!fundId) return;
    setHistoryLoading(true);
    try {
      const resp = await listFundAnalystPanels(fundId, { limit: 20 });
      setHistory(resp.panels ?? []);
    } catch {
      setHistory([]);
    } finally {
      setHistoryLoading(false);
    }
  }, [fundId]);

  useEffect(() => {
    loadHistory().catch(() => {});
  }, [loadHistory]);

  const runPanel = useCallback(async () => {
    const sym = symbol.trim().toUpperCase();
    if (!fundId || !sym) return;
    setLoading(true);
    setError(null);
    try {
      const body: import("@fundai/api-client").AnalystRunRequest = {
        symbol: sym,
        notes: notes.trim() || undefined,
        persist,
      };
      const last = Number(priceLast);
      if (!Number.isNaN(last) && priceLast !== "") body.price_last = last;
      const chg = Number(priceChange);
      if (!Number.isNaN(chg) && priceChange !== "") body.price_change = chg;
      const resp = await runFundAnalystPanel(fundId, body);
      setPanel(resp.panel);
      if (persist) {
        loadHistory().catch(() => {});
      }
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoading(false);
    }
  }, [fundId, symbol, priceLast, priceChange, notes, persist, m.error, loadHistory]);

  const loadHistorical = useCallback(
    async (panelId: string) => {
      if (!fundId || !panelId) return;
      try {
        const resp = await getFundAnalystPanel(fundId, panelId);
        setPanel(resp.panel);
        setSymbol(resp.panel.symbol);
      } catch (err) {
        setError(formatApiError(err, m.error));
      }
    },
    [fundId, m.error],
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
          <span className="text-gray-700">Price last</span>
          <input
            value={priceLast}
            onChange={(e) => setPriceLast(e.target.value)}
            inputMode="decimal"
            placeholder="100.0"
            className="rounded-lg border border-gray-200 px-3 py-2"
          />
        </label>
        <label className="flex flex-col gap-1 text-sm">
          <span className="text-gray-700">1d return (frac)</span>
          <input
            value={priceChange}
            onChange={(e) => setPriceChange(e.target.value)}
            inputMode="decimal"
            placeholder="0.012"
            className="rounded-lg border border-gray-200 px-3 py-2"
          />
        </label>
        <label className="flex flex-col gap-1 text-sm">
          <span className="text-gray-700">Notes</span>
          <input
            value={notes}
            onChange={(e) => setNotes(e.target.value)}
            placeholder=""
            className="rounded-lg border border-gray-200 px-3 py-2"
          />
        </label>
      </div>

      <div className="flex flex-wrap items-center gap-4">
        <label className="flex items-center gap-2 text-sm text-gray-700">
          <input
            type="checkbox"
            checked={persist}
            onChange={(e) => setPersist(e.target.checked)}
          />
          {m.persistLabel}
        </label>
        <button
          type="button"
          onClick={runPanel}
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

      {panel ? (
        <>
          <div className={`rounded-xl border px-4 py-3 ${directionTone(panel.aggregate_direction)}`}>
            <div className="flex flex-wrap items-baseline justify-between gap-2">
              <div className="text-sm font-medium uppercase tracking-wide opacity-70">
                {m.aggregateTitle} · {panel.symbol}
              </div>
              <div className="text-xs opacity-70">
                {m.generatedAt} {formatTime(panel.generated_at)}
              </div>
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-x-6 gap-y-1 text-sm">
              <span>
                <span className="font-medium">{m.aggregateDirection}：</span>
                {directionLabel(m, panel.aggregate_direction)}
              </span>
              <span>
                <span className="font-medium">{m.aggregateConfidence}：</span>
                {panel.aggregate_confidence}%
              </span>
              <span>
                <span className="font-medium">{m.categoriesVoted}：</span>
                {interpolate(m.voteSummary, {
                  voted: panel.categories_voted,
                  total: ALL_ANALYST_CATEGORIES.length,
                })}
              </span>
            </div>
          </div>

          <div>
            <h3 className="mb-2 text-sm font-semibold text-gray-900">{m.perCategoryTitle}</h3>
            <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
              {panel.reports.map((r) => (
                <AnalystReportCard key={`${r.agent_id}-${r.category}`} m={m} r={r} />
              ))}
            </div>
          </div>
        </>
      ) : (
        <p className="text-sm text-gray-500">{m.noPanelYet}</p>
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
                <span className={`rounded-full border px-2 py-0.5 text-xs ${directionTone(h.aggregate_direction)}`}>
                  {directionLabel(m, h.aggregate_direction)} · {h.aggregate_confidence}%
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

interface AnalystReportCardProps {
  m: AnalystPanelMessages;
  r: AnalystReport;
}

function AnalystReportCard({ m, r }: AnalystReportCardProps) {
  return (
    <div className={`flex h-full flex-col gap-2 rounded-xl border px-4 py-3 ${directionTone(r.direction)}`}>
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div className="text-sm font-semibold text-gray-900">
          {categoryLabel(m, r.category)}
        </div>
        <div className="text-xs opacity-70">
          {r.llm_model === "llm" ? m.llmModelLLM : m.llmModelFallback} ·{" "}
          {interpolate(m.confidenceLabel, { value: r.confidence })} ·{" "}
          {directionLabel(m, r.direction)}
        </div>
      </div>
      <div className="text-xs text-gray-500">
        {r.agent_name} · {m.asof} {formatTime(r.asof)}
      </div>
      <p className="text-sm text-gray-800">{r.thesis}</p>
      {r.key_findings.length > 0 ? (
        <div>
          <div className="text-xs font-medium text-gray-700">{m.keyFindingsLabel}</div>
          <ul className="ml-4 list-disc text-xs text-gray-700">
            {r.key_findings.map((k, i) => (
              <li key={`${r.agent_id}-finding-${i}`}>{k}</li>
            ))}
          </ul>
        </div>
      ) : null}
      {r.risks.length > 0 ? (
        <div>
          <div className="text-xs font-medium text-gray-700">{m.risksLabel}</div>
          <ul className="ml-4 list-disc text-xs text-rose-700">
            {r.risks.map((k, i) => (
              <li key={`${r.agent_id}-risk-${i}`}>{k}</li>
            ))}
          </ul>
        </div>
      ) : null}
      {r.data_points && r.data_points.length > 0 ? (
        <details className="text-xs">
          <summary className="cursor-pointer text-gray-600">
            {m.dataPointsLabel} ({r.data_points.length})
          </summary>
          <ul className="mt-1 ml-4 list-disc text-gray-600">
            {r.data_points.map((dp, i) => (
              <li key={`${r.agent_id}-dp-${i}`}>
                <span className="font-mono">{dp.name}</span> = {dp.value}
              </li>
            ))}
          </ul>
        </details>
      ) : null}
      {r.sources && r.sources.length > 0 ? (
        <details className="text-xs">
          <summary className="cursor-pointer text-gray-600">
            {m.sourcesLabel} ({r.sources.length})
          </summary>
          <ul className="mt-1 ml-4 list-disc text-gray-600">
            {r.sources.map((s, i) => (
              <li key={`${r.agent_id}-src-${i}`}>
                <a
                  href={s}
                  className="text-indigo-600 hover:underline"
                  target="_blank"
                  rel="noreferrer"
                >
                  {s}
                </a>
              </li>
            ))}
          </ul>
        </details>
      ) : null}
    </div>
  );
}
