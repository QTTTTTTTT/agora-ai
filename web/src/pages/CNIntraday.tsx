import React, { useCallback, useMemo, useState } from "react";
import {
  dryRunCNIntradaySignal,
  formatApiError,
  type CNIntradayDryRunInput,
  type CNIntradayDryRunResult,
  type CNIntradayMarket,
} from "../lib/api";
import { useAppPreferences } from "../lib/preferences";

/**
 * Stage 5 — A-share intraday signal DRY-RUN page.
 *
 * This page does NOT run a live engine. It's a "signal preview"
 * tool: the operator pastes (or generates) a minute-bar series,
 * picks a market segment + rule profile, and gets back what the
 * production engine WOULD have emitted.
 *
 * Purpose:
 *   1. Sanity-check factor calculations against hand-computed data.
 *   2. Preview the Feishu card before deploying changes to the
 *      live engine.
 *   3. Show stakeholders / collaborators what the signal flow
 *      looks like without exposing them to the production
 *      home-network setup.
 */

const COPY = {
  zh: {
    title: "A 股日内信号 Dry-Run",
    subtitle:
      "粘贴或生成分钟数据，预览生产引擎在该数据下会推送的信号。不接实盘券商。",
    formSymbol: "代码",
    formName: "名称",
    formMarket: "市场",
    formPrevClose: "昨收价",
    formSectorRank: "板块内排名 (0-1)",
    formRuleSet: "规则",
    ruleConservative: "保守",
    ruleAggressive: "激进",
    formBars: "分钟数据 (CSV: time,open,high,low,close,volume[,bidAskRatio][,bigOrderNet])",
    generateSample: "生成 65 分钟示例（含突破）",
    submit: "运行 Dry-Run",
    submitting: "运行中...",
    resultSignal: "信号",
    resultFactors: "因子分解",
    resultFeishu: "飞书卡片预览",
    noSignal: "本数据不触发信号（factor 未满足或近涨停）",
    error: "Dry-Run 失败",
  },
  en: {
    title: "A-Share Intraday Signal Dry-Run",
    subtitle:
      "Paste or generate minute bars; preview what the live engine WOULD push for that data. No broker integration.",
    formSymbol: "Symbol",
    formName: "Name",
    formMarket: "Market",
    formPrevClose: "Prev close",
    formSectorRank: "Sector rank (0-1)",
    formRuleSet: "Rule set",
    ruleConservative: "Conservative",
    ruleAggressive: "Aggressive",
    formBars: "Minute bars (CSV: time,open,high,low,close,volume[,bidAskRatio][,bigOrderNet])",
    generateSample: "Generate 65-min sample (with breakout)",
    submit: "Run dry-run",
    submitting: "Running...",
    resultSignal: "Signal",
    resultFactors: "Factor breakdown",
    resultFeishu: "Feishu card preview",
    noSignal: "No signal triggered (factors unmet or too close to limit-up)",
    error: "Dry-run failed",
  },
} as const;

const SAMPLE_CSV = `09:30,10.00,10.02,9.98,10.00,1000000
09:31,10.00,10.02,9.99,10.01,1100000
09:32,10.01,10.03,10.00,10.02,1050000
09:33,10.02,10.04,10.01,10.03,1000000
09:34,10.03,10.04,10.02,10.03,1050000
09:35,10.03,10.05,10.02,10.04,1100000`;

const CNIntraday: React.FC = () => {
  const { language } = useAppPreferences();
  const lang: "zh" | "en" = language === "zh-CN" ? "zh" : "en";
  const copy = COPY[lang];

  const [symbol, setSymbol] = useState("002415");
  const [name, setName] = useState("海康威视");
  const [market, setMarket] = useState<CNIntradayMarket>("main_board");
  const [prevClose, setPrevClose] = useState("10.00");
  const [sectorRank, setSectorRank] = useState("0.75");
  const [ruleSet, setRuleSet] = useState<"conservative" | "aggressive">("conservative");
  const [barsCsv, setBarsCsv] = useState(SAMPLE_CSV);

  const [result, setResult] = useState<CNIntradayDryRunResult | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const generateSample = useCallback(() => {
    // Build a 65-bar series: flat at 10.0 for 60 bars, then a
    // clean breakout to 10.6 on bar 65 with 3× volume spike on
    // the last 5 bars (so VolumeSurge passes).
    const today = new Date();
    today.setHours(9, 30, 0, 0);
    const lines: string[] = [];
    for (let i = 0; i < 65; i++) {
      const ts = new Date(today.getTime() + i * 60_000);
      const hh = ts.getHours().toString().padStart(2, "0");
      const mm = ts.getMinutes().toString().padStart(2, "0");
      const base = 10.0 + (i % 3) * 0.005; // tiny noise → non-zero stdev
      const closeVal = i < 60 ? base : 10.0 + (i - 60) * 0.12; // breakout from bar 60
      const high = Math.max(base, closeVal) + 0.01;
      const low = Math.min(base, closeVal) - 0.01;
      const vol = i < 60 ? 1_000_000 : 3_000_000; // 3x surge on last 5
      const bigInflow = i >= 60 ? 250_000 : 0;
      lines.push(`${hh}:${mm},${base.toFixed(2)},${high.toFixed(2)},${low.toFixed(2)},${closeVal.toFixed(2)},${vol},1.5,${bigInflow}`);
    }
    setBarsCsv(lines.join("\n"));
  }, []);

  const parsedBars = useMemo(() => {
    return barsCsv
      .split("\n")
      .map((l) => l.trim())
      .filter(Boolean)
      .map((l) => {
        const parts = l.split(",").map((p) => p.trim());
        if (parts.length < 6) return null;
        const [t, o, h, lo, c, v, bar = "0", big = "0"] = parts;
        const today = new Date().toISOString().slice(0, 10);
        return {
          timestamp: `${today} ${t}`,
          open: Number(o),
          high: Number(h),
          low: Number(lo),
          close: Number(c),
          volume: Number(v),
          bidAskRatio: Number(bar),
          bigOrderNet: Number(big),
        };
      })
      .filter((b): b is NonNullable<typeof b> => b !== null);
  }, [barsCsv]);

  const submit = useCallback(async () => {
    setError(null);
    setSubmitting(true);
    try {
      const input: CNIntradayDryRunInput = {
        symbol,
        name,
        market,
        prevClose: Number(prevClose),
        sectorRank: sectorRank ? Number(sectorRank) : undefined,
        ruleSet,
        bars: parsedBars,
      };
      const out = await dryRunCNIntradaySignal(input);
      setResult(out);
    } catch (e) {
      setError(formatApiError(e, copy.error));
    } finally {
      setSubmitting(false);
    }
  }, [symbol, name, market, prevClose, sectorRank, ruleSet, parsedBars, copy.error]);

  return (
    <div className="mx-auto max-w-6xl space-y-5 p-6">
      <header>
        <h1 className="text-xl font-semibold text-slate-900">{copy.title}</h1>
        <p className="mt-1 text-sm text-slate-500">{copy.subtitle}</p>
      </header>

      {error ? (
        <div className="rounded-md border border-rose-200 bg-rose-50 px-3 py-2 text-sm text-rose-700">
          <span className="font-medium">{copy.error}: </span>
          {error}
        </div>
      ) : null}

      <div className="grid grid-cols-12 gap-5">
        {/* Form column */}
        <section className="col-span-12 space-y-3 rounded-xl border border-slate-200 bg-white p-5 shadow-sm md:col-span-5">
          <div className="grid grid-cols-2 gap-3 text-xs">
            <Field label={copy.formSymbol}>
              <input
                value={symbol}
                onChange={(e) => setSymbol(e.target.value)}
                className="w-full rounded-md border border-slate-200 px-2 py-1 font-mono"
              />
            </Field>
            <Field label={copy.formName}>
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                className="w-full rounded-md border border-slate-200 px-2 py-1"
              />
            </Field>
            <Field label={copy.formMarket}>
              <select
                value={market}
                onChange={(e) => setMarket(e.target.value as CNIntradayMarket)}
                className="w-full rounded-md border border-slate-200 px-2 py-1"
              >
                <option value="main_board">主板 (±10%)</option>
                <option value="chinext">创业板 (±20%)</option>
                <option value="star">科创板 (±20%)</option>
                <option value="st">ST (±5%)</option>
                <option value="bse">北交所 (±30%)</option>
              </select>
            </Field>
            <Field label={copy.formPrevClose}>
              <input
                value={prevClose}
                onChange={(e) => setPrevClose(e.target.value)}
                className="w-full rounded-md border border-slate-200 px-2 py-1 font-mono"
              />
            </Field>
            <Field label={copy.formSectorRank}>
              <input
                value={sectorRank}
                onChange={(e) => setSectorRank(e.target.value)}
                className="w-full rounded-md border border-slate-200 px-2 py-1 font-mono"
              />
            </Field>
            <Field label={copy.formRuleSet}>
              <select
                value={ruleSet}
                onChange={(e) => setRuleSet(e.target.value as "conservative" | "aggressive")}
                className="w-full rounded-md border border-slate-200 px-2 py-1"
              >
                <option value="conservative">{copy.ruleConservative}</option>
                <option value="aggressive">{copy.ruleAggressive}</option>
              </select>
            </Field>
          </div>

          <Field label={copy.formBars}>
            <textarea
              value={barsCsv}
              onChange={(e) => setBarsCsv(e.target.value)}
              rows={10}
              className="w-full rounded-md border border-slate-200 px-2 py-1 font-mono text-[11px]"
            />
          </Field>

          <div className="flex justify-between">
            <button
              type="button"
              onClick={generateSample}
              className="rounded-md border border-slate-200 px-3 py-1.5 text-xs text-slate-700 hover:border-slate-400"
            >
              {copy.generateSample}
            </button>
            <button
              type="button"
              onClick={() => void submit()}
              disabled={submitting || parsedBars.length === 0}
              className="rounded-md bg-slate-900 px-3 py-1.5 text-xs font-medium text-white hover:bg-slate-800 disabled:cursor-not-allowed disabled:bg-slate-400"
            >
              {submitting ? copy.submitting : copy.submit}
            </button>
          </div>
        </section>

        {/* Result column */}
        <section className="col-span-12 space-y-3 md:col-span-7">
          {result ? (
            <>
              <div className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
                <h3 className="text-sm font-semibold text-slate-900">{copy.resultSignal}</h3>
                {result.signal ? (
                  <div className="mt-3 space-y-2 text-sm">
                    <div className="flex items-center gap-2">
                      <SignalBadge type={result.signal.type} />
                      <span className="font-mono font-medium">{result.signal.symbol}</span>
                      <span className="text-slate-600">{result.signal.name}</span>
                      <span className="ml-auto font-mono">¥{result.signal.price.toFixed(2)}</span>
                    </div>
                    <div className="text-xs text-slate-500">
                      信心 {result.signal.confidence.toFixed(2)} · 建议仓位 {(result.signal.suggestedPosition * 100).toFixed(0)}%
                      {result.signal.targetPrice > 0
                        ? ` · 目标 ¥${result.signal.targetPrice.toFixed(2)} · 止损 ¥${result.signal.stopLoss.toFixed(2)}`
                        : null}
                    </div>
                    {result.signal.reasons.length > 0 ? (
                      <ul className="ml-4 list-disc text-xs text-slate-700">
                        {result.signal.reasons.map((r, i) => (
                          <li key={i}>{r}</li>
                        ))}
                      </ul>
                    ) : null}
                    {result.signal.riskWarnings.length > 0 ? (
                      <ul className="ml-4 list-disc text-xs text-amber-700">
                        {result.signal.riskWarnings.map((r, i) => (
                          <li key={i}>⚠ {r}</li>
                        ))}
                      </ul>
                    ) : null}
                  </div>
                ) : (
                  <p className="mt-2 text-xs text-slate-400">{copy.noSignal}</p>
                )}
              </div>

              <div className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
                <h3 className="text-sm font-semibold text-slate-900">{copy.resultFactors}</h3>
                <div className="mt-3 grid grid-cols-5 gap-2 text-[11px]">
                  <FactorPill label="Breakout (z)" value={result.factorScores.breakout.toFixed(2)} />
                  <FactorPill label="Vol Surge" value={`${result.factorScores.volumeSurge.toFixed(2)}x`} />
                  <FactorPill label="Big Inflow" value={`${(result.factorScores.bigInflow / 10_000).toFixed(0)}万`} />
                  <FactorPill label="Bid/Ask" value={result.factorScores.orderImbalance.toFixed(2)} />
                  <FactorPill label="Sector %ile" value={`${(result.factorScores.sectorRank * 100).toFixed(0)}%`} />
                </div>
              </div>

              {result.feishu ? (
                <div className="rounded-xl border border-slate-200 bg-slate-900 p-5 text-white shadow-sm">
                  <h3 className="text-xs uppercase tracking-wide text-slate-400">{copy.resultFeishu}</h3>
                  <div className="mt-2 rounded-lg bg-slate-800 p-3">
                    <div className="font-semibold">{result.feishu.title}</div>
                    <ul className="mt-1 space-y-0.5 text-xs text-slate-200">
                      {result.feishu.lines.map((l, i) => (
                        <li key={i}>{l}</li>
                      ))}
                    </ul>
                  </div>
                </div>
              ) : null}
            </>
          ) : (
            <div className="rounded-xl border border-dashed border-slate-200 bg-white p-8 text-center text-sm text-slate-500">
              {copy.subtitle}
            </div>
          )}
        </section>
      </div>
    </div>
  );
};

const Field: React.FC<{ label: string; children: React.ReactNode }> = ({ label, children }) => (
  <label className="flex flex-col gap-1">
    <span className="text-[10px] uppercase tracking-wide text-slate-500">{label}</span>
    {children}
  </label>
);

const SignalBadge: React.FC<{ type: string }> = ({ type }) => {
  const map: Record<string, string> = {
    BUY: "bg-emerald-100 text-emerald-700",
    ADD: "bg-emerald-100 text-emerald-700",
    SELL: "bg-rose-100 text-rose-700",
    WARNING: "bg-amber-100 text-amber-700",
  };
  return (
    <span className={`inline-flex rounded px-2 py-0.5 text-[10px] font-medium ${map[type] ?? "bg-slate-100 text-slate-600"}`}>
      {type}
    </span>
  );
};

const FactorPill: React.FC<{ label: string; value: string }> = ({ label, value }) => (
  <div className="rounded-md bg-slate-50 px-2 py-1.5">
    <div className="text-[9px] uppercase tracking-wide text-slate-500">{label}</div>
    <div className="mt-0.5 font-mono text-xs font-semibold text-slate-900">{value}</div>
  </div>
);

export default CNIntraday;
