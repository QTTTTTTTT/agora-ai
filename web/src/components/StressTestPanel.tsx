// StressTestPanel — per-fund stress-test runner (S7 / P3-3).
//
// The PM picks a scenario from the admin-curated library, fires
// it against the current portfolio, and sees:
//   - Header card: NAV before / after, PnL, PnL %, holdings
//     count, shocked count.
//   - Per-holding drill-down table sorted by |PnL| descending so
//     the worst contributors are visually at the top.
//
// Read-only; the panel never mutates scenarios. Admin CRUD lives
// in AdminStressScenariosSection.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  formatApiError,
  listStressScenariosForFund,
  runFundStressScenario,
  type StressResult,
  type StressScenario,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  runButton: string;
  running: string;
  refresh: string;
  empty: string;
  error: string;
  scenarioLabel: string;
  scenarioPlaceholder: string;
  categoryLabel: string;
  descriptionLabel: string;
  shockCountLabel: string;
  navBeforeLabel: string;
  navAfterLabel: string;
  pnlTotalLabel: string;
  pnlPctLabel: string;
  holdingsLabel: string;
  shockedLabel: string;
  impactsTitle: string;
  impactsEmpty: string;
  impactSymbol: string;
  impactBefore: string;
  impactAfter: string;
  impactPnL: string;
  impactReturn: string;
  impactShock: string;
  categoryHistorical: string;
  categoryHypothetical: string;
  categoryRegulatory: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "压力测试",
    panelSubtitle: "选择一个压力情景，把对应冲击应用到当前持仓上，看 NAV 在该情景下的预计变动；展开下面的明细表能看到每个持仓在该情景下的损益贡献。",
    runButton: "运行场景",
    running: "运行中…",
    refresh: "刷新",
    empty: "请选择一个场景以查看影响",
    error: "运行失败",
    scenarioLabel: "场景",
    scenarioPlaceholder: "选择压力情景…",
    categoryLabel: "类别",
    descriptionLabel: "说明",
    shockCountLabel: "冲击数",
    navBeforeLabel: "冲击前 NAV",
    navAfterLabel: "冲击后 NAV",
    pnlTotalLabel: "损益合计",
    pnlPctLabel: "损益占比",
    holdingsLabel: "持仓数",
    shockedLabel: "受冲击持仓",
    impactsTitle: "持仓级影响",
    impactsEmpty: "暂无持仓影响",
    impactSymbol: "代码",
    impactBefore: "冲击前",
    impactAfter: "冲击后",
    impactPnL: "损益",
    impactReturn: "冲击收益率",
    impactShock: "匹配冲击",
    categoryHistorical: "历史复刻",
    categoryHypothetical: "假设情景",
    categoryRegulatory: "监管标准",
  },
  "en-US": {
    panelTitle: "Stress test",
    panelSubtitle: "Pick a scenario, project its shocks onto the current portfolio, and see the projected NAV impact. The per-holding table below sorts by |PnL| so the worst contributors are at the top.",
    runButton: "Run scenario",
    running: "Running…",
    refresh: "Refresh",
    empty: "Pick a scenario to see its impact",
    error: "Run failed",
    scenarioLabel: "Scenario",
    scenarioPlaceholder: "Choose a stress scenario…",
    categoryLabel: "Category",
    descriptionLabel: "Description",
    shockCountLabel: "Shocks",
    navBeforeLabel: "NAV before",
    navAfterLabel: "NAV after",
    pnlTotalLabel: "PnL",
    pnlPctLabel: "PnL %",
    holdingsLabel: "Holdings",
    shockedLabel: "Shocked holdings",
    impactsTitle: "Per-holding impact",
    impactsEmpty: "No holding-level impact yet",
    impactSymbol: "Symbol",
    impactBefore: "Before",
    impactAfter: "After",
    impactPnL: "PnL",
    impactReturn: "Applied return",
    impactShock: "Matched shock",
    categoryHistorical: "Historical",
    categoryHypothetical: "Hypothetical",
    categoryRegulatory: "Regulatory",
  },
};

interface StressTestPanelProps {
  fundId?: string;
  language?: Language;
}

function fmtPct(value: number): string {
  return `${(value * 100).toFixed(2)}%`;
}

function fmtMoney(value: number): string {
  return value.toLocaleString(undefined, { maximumFractionDigits: 0 });
}

function categoryLabel(m: Messages, c: string): string {
  if (c === "historical") return m.categoryHistorical;
  if (c === "hypothetical") return m.categoryHypothetical;
  if (c === "regulatory") return m.categoryRegulatory;
  return c;
}

export default function StressTestPanel({ fundId, language = "zh-CN" }: StressTestPanelProps) {
  const m = useMemo(() => messages[language], [language]);
  const [scenarios, setScenarios] = useState<StressScenario[]>([]);
  const [selected, setSelected] = useState<string>("");
  const [result, setResult] = useState<StressResult | null>(null);
  const [selectedScenario, setSelectedScenario] = useState<StressScenario | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const loadScenarios = useCallback(async () => {
    try {
      const resp = await listStressScenariosForFund();
      setScenarios(resp.scenarios ?? []);
    } catch (err) {
      setError(formatApiError(err, m.error));
    }
  }, [m.error]);

  useEffect(() => {
    loadScenarios().catch(() => {});
  }, [loadScenarios]);

  const runScenario = useCallback(async () => {
    if (!fundId || !selected) return;
    setLoading(true);
    setError(null);
    try {
      const resp = await runFundStressScenario(fundId, { scenarioId: selected });
      setResult(resp.result);
      setSelectedScenario(resp.scenario);
    } catch (err) {
      setError(formatApiError(err, m.error));
      setResult(null);
    } finally {
      setLoading(false);
    }
  }, [fundId, selected, m.error]);

  return (
    <section className="space-y-4 rounded-2xl border border-gray-100 bg-white p-6 shadow-sm">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <h2 className="text-base font-semibold text-gray-900">{m.panelTitle}</h2>
          <p className="mt-1 text-sm text-gray-500">{m.panelSubtitle}</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <label className="text-xs text-gray-500">{m.scenarioLabel}</label>
          <select
            value={selected}
            onChange={(e) => setSelected(e.target.value)}
            className="min-w-[16rem] rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
          >
            <option value="">{m.scenarioPlaceholder}</option>
            {scenarios.map((s) => (
              <option key={s.id} value={s.id}>
                [{categoryLabel(m, s.category)}] {s.name}
              </option>
            ))}
          </select>
          <button
            type="button"
            disabled={!fundId || !selected || loading}
            onClick={() => runScenario().catch(() => {})}
            className="rounded-md border border-blue-200 bg-blue-50 px-3 py-1.5 text-xs font-medium text-blue-700 hover:bg-blue-100 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {loading ? m.running : m.runButton}
          </button>
        </div>
      </header>

      {selectedScenario && (
        <div className="rounded-md border border-gray-100 bg-gray-50 p-3 text-xs text-gray-600">
          <div className="grid grid-cols-1 gap-2 md:grid-cols-3">
            <div>
              <span className="text-gray-400">{m.categoryLabel}: </span>
              <span className="font-medium text-gray-800">
                {categoryLabel(m, selectedScenario.category)}
              </span>
            </div>
            <div>
              <span className="text-gray-400">{m.shockCountLabel}: </span>
              <span className="font-mono text-gray-800">
                {selectedScenario.shocks.length}
              </span>
            </div>
          </div>
          {selectedScenario.description && (
            <div className="mt-2 whitespace-pre-wrap text-gray-700">
              {selectedScenario.description}
            </div>
          )}
        </div>
      )}

      {error && (
        <div className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          {error}
        </div>
      )}

      {!result && !loading && !error && (
        <p className="rounded-md border border-gray-100 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
          {m.empty}
        </p>
      )}

      {result && (
        <>
          <div className="grid grid-cols-2 gap-4 rounded-md border border-gray-100 bg-gray-50 p-4 text-xs text-gray-600 md:grid-cols-6">
            <div>
              <div className="text-gray-400">{m.navBeforeLabel}</div>
              <div className="mt-1 font-mono text-base text-gray-900">
                {fmtMoney(result.nav_before)}
              </div>
            </div>
            <div>
              <div className="text-gray-400">{m.navAfterLabel}</div>
              <div className="mt-1 font-mono text-base text-gray-900">
                {fmtMoney(result.nav_after)}
              </div>
            </div>
            <div>
              <div className="text-gray-400">{m.pnlTotalLabel}</div>
              <div className={`mt-1 font-mono text-base ${result.pnl_total < 0 ? "text-rose-600" : "text-emerald-600"}`}>
                {fmtMoney(result.pnl_total)}
              </div>
            </div>
            <div>
              <div className="text-gray-400">{m.pnlPctLabel}</div>
              <div className={`mt-1 font-mono text-base ${result.pnl_pct < 0 ? "text-rose-600" : "text-emerald-600"}`}>
                {fmtPct(result.pnl_pct)}
              </div>
            </div>
            <div>
              <div className="text-gray-400">{m.holdingsLabel}</div>
              <div className="mt-1 font-mono text-base text-gray-900">
                {result.holding_count}
              </div>
            </div>
            <div>
              <div className="text-gray-400">{m.shockedLabel}</div>
              <div className="mt-1 font-mono text-base text-gray-900">
                {result.shocked_count}/{result.holding_count}
              </div>
            </div>
          </div>

          <div>
            <h3 className="mb-2 text-sm font-semibold text-gray-800">
              {m.impactsTitle}
            </h3>
            {result.impacts.length === 0 ? (
              <p className="rounded-md border border-gray-100 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
                {m.impactsEmpty}
              </p>
            ) : (
              <div className="overflow-x-auto rounded-md border border-gray-100">
                <table className="min-w-full text-xs">
                  <thead className="bg-gray-50 text-gray-500">
                    <tr>
                      <th className="px-3 py-2 text-left">{m.impactSymbol}</th>
                      <th className="px-3 py-2 text-right">{m.impactBefore}</th>
                      <th className="px-3 py-2 text-right">{m.impactAfter}</th>
                      <th className="px-3 py-2 text-right">{m.impactPnL}</th>
                      <th className="px-3 py-2 text-right">{m.impactReturn}</th>
                      <th className="px-3 py-2 text-left">{m.impactShock}</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-gray-100">
                    {result.impacts.map((im) => (
                      <tr key={im.instrument_key}>
                        <td className="px-3 py-2 font-mono text-gray-800">{im.symbol || im.instrument_key}</td>
                        <td className="px-3 py-2 text-right font-mono text-gray-700">{fmtMoney(im.market_value_before)}</td>
                        <td className="px-3 py-2 text-right font-mono text-gray-700">{fmtMoney(im.market_value_after)}</td>
                        <td className={`px-3 py-2 text-right font-mono ${im.pnl < 0 ? "text-rose-600" : "text-emerald-600"}`}>
                          {fmtMoney(im.pnl)}
                        </td>
                        <td className={`px-3 py-2 text-right font-mono ${im.applied_return < 0 ? "text-rose-600" : "text-emerald-600"}`}>
                          {im.applied_shock_type ? fmtPct(im.applied_return) : "—"}
                        </td>
                        <td className="px-3 py-2 text-gray-600">
                          {im.applied_shock_type
                            ? `${im.applied_shock_type}/${im.applied_shock_key}`
                            : "—"}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </>
      )}
    </section>
  );
}
