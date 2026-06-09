import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  consultAdvisor,
  fetchAdvisorHealth,
  formatApiError,
  getAdvisorConsultation,
  listAdvisorHistory,
  listAdvisorPresets,
  type AdvisorConsultResponse,
  type AdvisorConsultationDetail,
  type AdvisorConsultationSummary,
  type AdvisorPreset,
  type AdvisorVerdict,
} from "../lib/api";
import { useAppPreferences } from "../lib/preferences";
import PersonaPresetPicker from "../components/PersonaPresetPicker";
import MasterVerdictCard from "../components/MasterVerdictCard";
import TacticVerdictCard from "../components/TacticVerdictCard";
import { ComplianceBanner } from "../components/ComplianceBanner";
import { ComplianceAckModal } from "../components/ComplianceAckModal";
import {
  formatModelVerdict,
  useCompliance,
  type ComplianceLocale,
} from "../lib/compliance";
import AdvisorTrackRecordPanel from "../components/AdvisorTrackRecordPanel";
import AdvisorBillingHeader from "../components/AdvisorBillingHeader";

// Advisor — the /advisor mode console.
//
// Flow:
//   1. user picks a preset (Conservative / GARP / Deep value / etc.)
//   2. user types a ticker (and optionally market / asset class)
//   3. user clicks "Consult"
//   4. backend fans out to N master agents → aggregate verdict + per-master cards
//
// The page is intentionally read-only: no trade buttons, no order
// surface. Footer carries a banner reiterating "advice only, not
// an order" so the legal compliance team can point a regulator at
// a single source of truth.

const VERDICT_ACCENT: Record<string, { ring: string; text: string; chip: string }> = {
  STRONG_BUY: { ring: "ring-emerald-300", text: "text-emerald-700", chip: "bg-emerald-100 text-emerald-800" },
  BUY: { ring: "ring-emerald-200", text: "text-emerald-700", chip: "bg-emerald-50 text-emerald-700" },
  HOLD: { ring: "ring-slate-200", text: "text-slate-700", chip: "bg-slate-100 text-slate-700" },
  MIXED: { ring: "ring-amber-200", text: "text-amber-800", chip: "bg-amber-100 text-amber-800" },
  AVOID: { ring: "ring-rose-200", text: "text-rose-700", chip: "bg-rose-50 text-rose-700" },
  SHORT: { ring: "ring-rose-300", text: "text-rose-800", chip: "bg-rose-100 text-rose-800" },
  SKIP: { ring: "ring-slate-200", text: "text-slate-500", chip: "bg-slate-50 text-slate-500" },
  PASS: { ring: "ring-slate-200", text: "text-slate-500", chip: "bg-slate-50 text-slate-500" },
};

function aggregateAccent(verdict: AdvisorVerdict | string) {
  const v = String(verdict || "HOLD").toUpperCase();
  return VERDICT_ACCENT[v] ?? VERDICT_ACCENT.HOLD;
}

// AdvisorVerdictLabel wraps the raw aggregate verdict in the
// Publisher-mode-safe phrasing ("Model rating: …" / "本模型评级：…").
// Encapsulated so the rendering of the verdict pill in the header
// stays in one place — if we add a third compliance mode later,
// we touch this one component and the rest of the page is
// unaware.
const AdvisorVerdictLabel: React.FC<{ verdict: string }> = ({ verdict }) => {
  const { mode } = useCompliance();
  const { language } = useAppPreferences();
  const locale: ComplianceLocale = language === "en-US" ? "en-US" : "zh-CN";
  return <>{formatModelVerdict(verdict, locale, mode)}</>;
};

const Advisor: React.FC = () => {
  const { language } = useAppPreferences();
  const copy = useStaticCopy(language);
  const [presets, setPresets] = useState<AdvisorPreset[] | null>(null);
  const [loadingPresets, setLoadingPresets] = useState(true);
  const [presetError, setPresetError] = useState<string | null>(null);
  const [selectedPreset, setSelectedPreset] = useState<string | null>(null);

  const [symbol, setSymbol] = useState("");
  const [market, setMarket] = useState("");
  const [notes, setNotes] = useState("");

  const [consulting, setConsulting] = useState(false);
  const [consultError, setConsultError] = useState<string | null>(null);
  const [response, setResponse] = useState<AdvisorConsultResponse | null>(null);
  // When the user clicks a history row, we fetch the persisted
  // consultation by id and project it into the same ConsultResponse
  // shape so the existing verdict / cards rendering can reuse it
  // verbatim. ``viewingHistoryId`` tracks "this response panel is
  // showing a historical row, not a fresh consult" so we can render
  // a clear banner + "back to latest" affordance.
  const [viewingHistoryId, setViewingHistoryId] = useState<string | null>(null);
  const [loadingHistoryDetail, setLoadingHistoryDetail] = useState<string | null>(null);
  const [historyDetailError, setHistoryDetailError] = useState<string | null>(null);
  // Snapshot of the live consult so users can flip back to it
  // after browsing the archive without re-issuing the LLM call.
  const [liveResponseSnapshot, setLiveResponseSnapshot] = useState<AdvisorConsultResponse | null>(null);

  const [history, setHistory] = useState<AdvisorConsultationSummary[]>([]);
  const [historyError, setHistoryError] = useState<string | null>(null);
  const [_health, setHealth] = useState<{ tactics_loaded: boolean; masters_loaded: boolean } | null>(null);

  // Load the preset list once on mount.
  useEffect(() => {
    let cancelled = false;
    setLoadingPresets(true);
    listAdvisorPresets()
      .then((r) => {
        if (cancelled) return;
        setPresets(r.presets);
        if (r.presets.length > 0 && !selectedPreset) {
          setSelectedPreset(r.presets[0].preset_key);
        }
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setPresetError(formatApiError(e, "无法加载预设"));
      })
      .finally(() => {
        if (!cancelled) setLoadingPresets(false);
      });
    return () => {
      cancelled = true;
    };
    // selectedPreset is read but not in deps — we only want to set
    // the default on first load.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Load the most recent 10 consultations + health on mount.
  useEffect(() => {
    let cancelled = false;
    listAdvisorHistory({ limit: 10 })
      .then((r) => {
        if (cancelled) return;
        setHistory(r.consultations || []);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setHistoryError(formatApiError(e, "无法加载历史咨询"));
      });
    fetchAdvisorHealth()
      .then((h) => {
        if (cancelled) return;
        setHealth({ tactics_loaded: h.tactics_loaded, masters_loaded: h.masters_loaded });
      })
      .catch(() => {
        // health is decorative — if it 5xxs we just don't render the
        // pill.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const selectedPresetDef = useMemo(
    () => presets?.find((p) => p.preset_key === selectedPreset) ?? null,
    [presets, selectedPreset],
  );

  const presetIsTacticOnly = selectedPresetDef?.kind === "tactics";

  const handleConsult = useCallback(async () => {
    setConsultError(null);
    setResponse(null);
    setViewingHistoryId(null);
    setLiveResponseSnapshot(null);
    setHistoryDetailError(null);
    if (!selectedPreset) {
      setConsultError(copy.errPresetRequired);
      return;
    }
    if (!symbol.trim()) {
      setConsultError(copy.errSymbolRequired);
      return;
    }
    setConsulting(true);
    try {
      const r = await consultAdvisor({
        symbol: symbol.trim().toUpperCase(),
        market: market.trim().toLowerCase() || undefined,
        preset_key: selectedPreset,
        notes: notes.trim() || undefined,
      });
      setResponse(r);
      // Optimistically prepend to history list.
      setHistory((h) => [
        {
          id: r.consultation_id,
          symbol: r.symbol,
          symbol_name: r.symbol_name,
          preset_key: r.preset_key,
          aggregate_verdict: r.aggregate_verdict,
          aggregate_confidence: r.aggregate_confidence,
          consensus_score: r.consensus_score,
          master_count: r.master_reports.length,
          tactic_count: r.tactic_reports.length,
          created_at: r.created_at,
        },
        ...h.slice(0, 9),
      ]);
    } catch (e: unknown) {
      setConsultError(formatApiError(e, "咨询大师团失败"));
    } finally {
      setConsulting(false);
    }
  }, [selectedPreset, symbol, market, notes, copy]);

  // detailToResponse adapts the GET /consultations/{id} shape onto the
  // ConsultResponse shape so the verdict panel + master / tactic cards
  // can render historical rows without a parallel rendering path.
  // Field names differ (id vs consultation_id) but the bodies are
  // identical — anything the detail endpoint doesn't carry (it's a
  // strict subset on purpose) falls through as undefined and the
  // cards already tolerate that.
  const detailToResponse = useCallback(
    (d: AdvisorConsultationDetail): AdvisorConsultResponse => ({
      consultation_id: d.id,
      symbol: d.symbol,
      symbol_name: d.symbol_name,
      preset_key: d.preset_key,
      aggregate_verdict: d.aggregate_verdict,
      aggregate_confidence: d.aggregate_confidence,
      consensus_score: d.consensus_score,
      master_reports: d.master_reports,
      tactic_reports: d.tactic_reports,
      created_at: d.created_at,
    }),
    [],
  );

  const handleSelectHistory = useCallback(
    async (id: string) => {
      if (!id || loadingHistoryDetail === id) return;
      // Avoid losing the in-flight live result when the user clicks
      // a history row while a consult panel is already showing.
      // Cached so dismiss-history can restore without an extra fetch.
      if (response && !viewingHistoryId) {
        setLiveResponseSnapshot(response);
      }
      setHistoryDetailError(null);
      setLoadingHistoryDetail(id);
      try {
        const d = await getAdvisorConsultation(id);
        setResponse(detailToResponse(d));
        setViewingHistoryId(id);
      } catch (e: unknown) {
        setHistoryDetailError(formatApiError(e, copy.historyDetailErr));
      } finally {
        setLoadingHistoryDetail(null);
      }
    },
    [response, viewingHistoryId, loadingHistoryDetail, copy, detailToResponse],
  );

  const handleDismissHistoryView = useCallback(() => {
    setViewingHistoryId(null);
    setHistoryDetailError(null);
    // Restore the live consult panel when we have one; otherwise
    // collapse the panel entirely so the user is back to a clean
    // "type a symbol" state.
    if (liveResponseSnapshot) {
      setResponse(liveResponseSnapshot);
      setLiveResponseSnapshot(null);
    } else {
      setResponse(null);
    }
  }, [liveResponseSnapshot]);

  const aggregate = response
    ? aggregateAccent(response.aggregate_verdict)
    : null;

  return (
    <div className="min-h-screen bg-slate-50 px-6 py-8">
      <ComplianceAckModal surface="advisor" />
      <div className="mx-auto max-w-6xl space-y-6">
        <header className="flex items-start justify-between gap-4">
          <div>
            <h1 className="text-2xl font-semibold tracking-tight text-slate-900">
              {copy.heading}
            </h1>
            <p className="mt-1 text-sm text-slate-500">{copy.sub}</p>
          </div>
          <div className="flex flex-col items-end gap-1">
            <Link
              to="/masters"
              className="rounded-full border border-slate-200 bg-white px-4 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-50"
            >
              {copy.backToFunds}
            </Link>
            <Link
              to="/daily-picks"
              className="text-[11px] text-indigo-600 hover:text-indigo-700 hover:underline"
            >
              {language === "zh-CN" ? "查看每日观察榜 →" : "Browse daily picks →"}
            </Link>
          </div>
        </header>
        <ComplianceBanner surface="advisor" />
        <AdvisorBillingHeader lang={language === "zh-CN" ? "zh" : "en"} />

        {/* Phase 5 track-record strip — keeps the user grounded on
            historical hit-rate before they request the next call. */}
        <AdvisorTrackRecordPanel compact />

        {/* Preset picker */}
        <section className="rounded-2xl bg-white p-5 shadow-sm">
          <h2 className="mb-3 text-sm font-semibold text-slate-800">{copy.presetTitle}</h2>
          {presetError ? (
            <div className="rounded-md bg-rose-50 px-3 py-2 text-xs text-rose-700">{presetError}</div>
          ) : loadingPresets ? (
            <div className="text-xs text-slate-500">{copy.loading}</div>
          ) : presets && presets.length > 0 ? (
            <PersonaPresetPicker
              presets={presets}
              selectedKey={selectedPreset}
              onSelect={setSelectedPreset}
              language={language}
              disabled={consulting}
            />
          ) : (
            <div className="rounded-md border border-dashed border-slate-200 p-4 text-center text-sm text-slate-500">
              {copy.emptyPresets}
            </div>
          )}
        </section>

        {/* Consult form */}
        <section className="rounded-2xl bg-white p-5 shadow-sm">
          <h2 className="mb-3 text-sm font-semibold text-slate-800">{copy.askTitle}</h2>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
            <label className="block">
              <span className="mb-1 block text-xs font-medium text-slate-600">{copy.symbolLabel}</span>
              <input
                type="text"
                value={symbol}
                onChange={(e) => setSymbol(e.target.value)}
                placeholder={presetIsTacticOnly ? "600519" : "AAPL"}
                className="w-full rounded-md border border-slate-300 px-3 py-2 text-sm uppercase focus:border-indigo-400 focus:outline-none focus:ring-1 focus:ring-indigo-400"
              />
            </label>
            <label className="block">
              <span className="mb-1 block text-xs font-medium text-slate-600">{copy.marketLabel}</span>
              <select
                value={market}
                onChange={(e) => setMarket(e.target.value)}
                className="w-full rounded-md border border-slate-300 px-3 py-2 text-sm focus:border-indigo-400 focus:outline-none focus:ring-1 focus:ring-indigo-400"
              >
                <option value="">{copy.marketAuto}</option>
                <option value="us_equity">US Equity</option>
                <option value="a_share">A-Share</option>
                <option value="hk_equity">HK Equity</option>
                <option value="crypto">Crypto</option>
              </select>
            </label>
            <label className="block sm:col-span-1">
              <span className="mb-1 block text-xs font-medium text-slate-600">{copy.notesLabel}</span>
              <input
                type="text"
                value={notes}
                onChange={(e) => setNotes(e.target.value)}
                placeholder={copy.notesPh}
                className="w-full rounded-md border border-slate-300 px-3 py-2 text-sm focus:border-indigo-400 focus:outline-none focus:ring-1 focus:ring-indigo-400"
              />
            </label>
          </div>
          {consultError ? (
            <div className="mt-3 rounded-md bg-rose-50 px-3 py-2 text-xs text-rose-700">
              {consultError}
            </div>
          ) : null}
          <div className="mt-4 flex items-center justify-between gap-3">
            <p className="text-[11px] text-slate-400">{copy.disclaimer}</p>
            <button
              type="button"
              disabled={consulting || !selectedPreset || !symbol.trim()}
              onClick={handleConsult}
              className="rounded-md bg-indigo-600 px-5 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700 disabled:cursor-not-allowed disabled:bg-slate-300"
            >
              {consulting ? copy.consulting : copy.consultCta}
            </button>
          </div>
        </section>

        {/* Verdict + cards */}
        {response ? (
          <section className={`rounded-2xl bg-white p-5 shadow-sm ring-2 ${aggregate?.ring}`}>
            {viewingHistoryId ? (
              <div className="mb-3 flex items-center justify-between gap-3 rounded-md border border-indigo-200 bg-indigo-50 px-3 py-2 text-xs text-indigo-800">
                <span>
                  {copy.viewingHistoryBanner(new Date(response.created_at).toLocaleString())}
                </span>
                <button
                  type="button"
                  onClick={handleDismissHistoryView}
                  className="rounded-full border border-indigo-200 bg-white px-3 py-1 text-[11px] font-medium text-indigo-700 hover:bg-indigo-100"
                >
                  {liveResponseSnapshot ? copy.backToLatest : copy.dismissHistory}
                </button>
              </div>
            ) : null}
            {historyDetailError ? (
              <div className="mb-3 rounded-md bg-rose-50 px-3 py-2 text-xs text-rose-700">
                {historyDetailError}
              </div>
            ) : null}
            <header className="mb-4 flex flex-col gap-1 sm:flex-row sm:items-end sm:justify-between">
              <div>
                <div className="text-xs uppercase tracking-wider text-slate-400">
                  {copy.aggregateLabel}
                </div>
                <div className="mt-1 flex items-center gap-3">
                  <h2 className="text-2xl font-semibold text-slate-900">
                    {response.symbol_name
                      ? `${response.symbol_name} (${response.symbol})`
                      : response.symbol}
                  </h2>
                  <span
                    title={response.aggregate_verdict}
                    className={`rounded-full px-3 py-1 text-sm font-semibold ${aggregate?.chip}`}
                  >
                    <AdvisorVerdictLabel verdict={response.aggregate_verdict} />
                  </span>
                  <span className="text-sm text-slate-500">
                    {copy.confidence}: {response.aggregate_confidence}%
                  </span>
                  <span className="text-sm text-slate-500">
                    {copy.consensus}: {response.consensus_score.toFixed(0)}%
                  </span>
                </div>
              </div>
              <div className="text-xs text-slate-400">
                {new Date(response.created_at).toLocaleString()}
              </div>
            </header>
            {response.consensus_score < 50 && response.master_reports.length > 1 ? (
              <div className="mb-4 rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-700">
                {copy.lowConsensusWarning}
              </div>
            ) : null}
            {response.master_reports.length > 0 ? (
              <div className="grid grid-cols-1 gap-4 lg:grid-cols-2 xl:grid-cols-3">
                {response.master_reports.map((r) => (
                  <MasterVerdictCard key={r.master_key} report={r} language={language} />
                ))}
              </div>
            ) : null}
            {response.tactic_reports.length > 0 ? (
              <div className="mt-4 grid grid-cols-1 gap-4 lg:grid-cols-2 xl:grid-cols-3">
                {response.tactic_reports.map((r) => (
                  <TacticVerdictCard key={r.tactic_key} report={r} language={language} />
                ))}
              </div>
            ) : null}
            {response.tactic_reports.length === 0 && presetIsTacticOnly ? (
              <div className="mt-6 rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-700">
                {copy.tacticNotReady}
              </div>
            ) : null}
          </section>
        ) : null}

        {/* History */}
        <section className="rounded-2xl bg-white p-5 shadow-sm">
          <h2 className="mb-3 text-sm font-semibold text-slate-800">{copy.historyTitle}</h2>
          {historyError ? (
            <div className="rounded-md bg-rose-50 px-3 py-2 text-xs text-rose-700">{historyError}</div>
          ) : null}
          {!response && historyDetailError ? (
            <div className="mb-3 rounded-md bg-rose-50 px-3 py-2 text-xs text-rose-700">
              {historyDetailError}
            </div>
          ) : null}
          {history.length === 0 && !historyError ? (
            <div className="rounded-md border border-dashed border-slate-200 p-4 text-center text-sm text-slate-500">
              {copy.emptyHistory}
            </div>
          ) : (
            <table className="w-full table-fixed text-sm">
              <thead className="text-left text-[11px] uppercase tracking-wider text-slate-400">
                <tr>
                  <th className="w-1/4 py-2">{copy.colTime}</th>
                  <th className="w-1/6 py-2">{copy.colSymbol}</th>
                  <th className="w-1/4 py-2">{copy.colPreset}</th>
                  <th className="w-1/6 py-2">{copy.colVerdict}</th>
                  <th className="w-1/6 py-2">{copy.colConfidence}</th>
                </tr>
              </thead>
              <tbody>
                {history.map((h) => {
                  const acc = aggregateAccent(h.aggregate_verdict);
                  const isActive = viewingHistoryId === h.id;
                  const isLoading = loadingHistoryDetail === h.id;
                  return (
                    <tr
                      key={h.id}
                      onClick={() => handleSelectHistory(h.id)}
                      className={`cursor-pointer border-t border-slate-100 text-slate-700 transition-colors hover:bg-slate-50 ${
                        isActive ? "bg-indigo-50/60" : ""
                      } ${isLoading ? "opacity-60" : ""}`}
                      aria-busy={isLoading}
                      aria-current={isActive ? "true" : undefined}
                      title={copy.historyRowHint}
                    >
                      <td className="py-2 text-xs text-slate-500">
                        {new Date(h.created_at).toLocaleString()}
                      </td>
                      <td className="py-2 font-medium">
                        {h.symbol_name ? (
                          <>
                            <span>{h.symbol_name}</span>{" "}
                            <span className="text-xs font-normal text-slate-500">
                              ({h.symbol})
                            </span>
                          </>
                        ) : (
                          h.symbol
                        )}
                      </td>
                      <td className="py-2 text-xs text-slate-500">{h.preset_key}</td>
                      <td className="py-2">
                        <span className={`rounded-full px-2 py-0.5 text-[11px] font-medium ${acc.chip}`}>
                          {h.aggregate_verdict}
                        </span>
                      </td>
                      <td className="py-2 text-xs tabular-nums text-slate-500">
                        {h.aggregate_confidence}%
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </section>

        {/* Footer disclaimer (also serves as the regulator-pointable
            source of truth that /advisor never places trades). */}
        <footer className="rounded-xl bg-slate-100 px-4 py-3 text-center text-[11px] text-slate-500">
          {copy.regulatoryDisclaimer}
        </footer>
      </div>
    </div>
  );
};

function useStaticCopy(language: string) {
  return useMemo(
    () =>
      language === "en-US"
        ? {
            heading: "Master team consultation",
            sub: "Pick a style, type a ticker, see what each investing master would say.",
            backToFunds: "Back to Master Team",
            presetTitle: "1) Pick a style",
            askTitle: "2) Ask about a ticker",
            symbolLabel: "Symbol",
            marketLabel: "Market",
            marketAuto: "Auto-detect",
            notesLabel: "Notes (optional)",
            notesPh: "e.g. earnings tomorrow, FOMC tomorrow",
            consultCta: "Consult the panel",
            consulting: "Consulting…",
            aggregateLabel: "Aggregate verdict",
            confidence: "Confidence",
            consensus: "Consensus",
            lowConsensusWarning:
              "Masters disagree significantly (low consensus). Treat the aggregate verdict with caution.",
            historyTitle: "Recent consultations",
            emptyHistory: "No consultations yet.",
            emptyPresets: "No presets available.",
            colTime: "Time",
            colSymbol: "Symbol",
            colPreset: "Preset",
            colVerdict: "Verdict",
            colConfidence: "Conf.",
            loading: "Loading…",
            errPresetRequired: "Please pick a style.",
            errSymbolRequired: "Please enter a ticker symbol.",
            historyRowHint: "Click to re-open this past consultation.",
            historyDetailErr: "Failed to load consultation details.",
            viewingHistoryBanner: (when: string) =>
              `Viewing a past consultation (${when}). The cards below are read from storage, not re-run.`,
            backToLatest: "Back to latest consult",
            dismissHistory: "Close historical view",
            disclaimer: "This service is read-only; no trades are placed.",
            tacticNotReady:
              "A-share short-term tactics are not yet wired in this build (Phase 4). The master panel verdict above still applies.",
            regulatoryDisclaimer:
              "/advisor is provided for informational purposes only. It is not investment advice and does not execute trades. To act on a thesis, go to the fund console.",
          }
        : {
            heading: "大师团队咨询",
            sub: "选风格、输入股票代码，看每位大师怎么说。",
            backToFunds: "返回大师团队",
            presetTitle: "1) 选择风格",
            askTitle: "2) 输入要咨询的股票",
            symbolLabel: "股票代码",
            marketLabel: "市场",
            marketAuto: "自动识别",
            notesLabel: "备注（可选）",
            notesPh: "如：明天发财报、明天 FOMC",
            consultCta: "请大师团把脉",
            consulting: "正在请教大师…",
            aggregateLabel: "综合结论",
            confidence: "置信度",
            consensus: "一致度",
            lowConsensusWarning: "大师之间分歧较大（一致度偏低），综合结论仅供参考。",
            historyTitle: "最近的咨询记录",
            emptyHistory: "暂无历史咨询。",
            emptyPresets: "暂无可用风格预设。",
            colTime: "时间",
            colSymbol: "代码",
            colPreset: "风格",
            colVerdict: "结论",
            colConfidence: "置信度",
            loading: "正在加载…",
            errPresetRequired: "请先选择一个风格。",
            errSymbolRequired: "请输入股票代码。",
            historyRowHint: "点击查看历史咨询的完整结论。",
            historyDetailErr: "加载历史咨询详情失败。",
            viewingHistoryBanner: (when: string) =>
              `正在查看历史咨询记录（${when}），以下结果来自存档，未重新调用模型。`,
            backToLatest: "返回最近一次咨询",
            dismissHistory: "关闭历史记录",
            disclaimer: "本服务仅供参考，不下任何单。",
            tacticNotReady:
              "A 股短线战法尚未在当前版本接入（Phase 4 上线）。上方大师组的结论仍然有效。",
            regulatoryDisclaimer:
              "/advisor 仅供信息参考，不构成投资建议，也不会执行任何交易。如需根据结论实操，请回到基金控制台。",
          },
    [language],
  );
}

export default Advisor;
