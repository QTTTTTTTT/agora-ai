import React, { useCallback, useEffect, useState } from "react";

import {
  formatApiError,
  listAdvisorBillingCalls,
  type AdvisorBillingCall,
} from "../lib/api";

// ByokCallLogPanel — Phase D-3 (call log half).
//
// Shows the last N /consult calls with the model that actually
// served them. The whole point of the panel is to let a BYOK user
// audit "did my key really get used, or did the platform silently
// fall back?" — the answer is in the model column (an OpenAI key
// will show "gpt-4o@byok:openai" while a platform-pool call shows
// just "gpt-4o").

interface Props {
  lang: "zh" | "en";
}

const COPY = {
  zh: {
    title: "实时调用日志",
    sub: "最近 30 条 /advisor 调用，含使用的模型和扣费来源。",
    byokFilterOn: "只看 BYOK",
    byokFilterOff: "全部",
    cols: {
      time: "时间",
      symbol: "标的",
      preset: "预设",
      verdict: "判断",
      models: "模型",
      cost: "扣费",
    },
    sourcePlan: "套餐配额",
    sourceCredit: "credit",
    sourceUnmetered: "免费",
    sourceUnknown: "—",
    empty: "暂无调用记录 — 去 /advisor 做一次咨询，这里就会出现。",
    refresh: "刷新",
    error: "加载失败",
  },
  en: {
    title: "Live call log",
    sub: "Last 30 /advisor calls, with the model used and where the cost was deducted from.",
    byokFilterOn: "BYOK only",
    byokFilterOff: "All",
    cols: {
      time: "Time",
      symbol: "Symbol",
      preset: "Preset",
      verdict: "Verdict",
      models: "Model(s)",
      cost: "Cost",
    },
    sourcePlan: "plan quota",
    sourceCredit: "credit",
    sourceUnmetered: "unmetered",
    sourceUnknown: "—",
    empty: "No calls yet — make a consultation in /advisor and it will appear here.",
    refresh: "Refresh",
    error: "Load failed",
  },
};

const formatSource = (source: string, copy: typeof COPY["zh"]) => {
  switch (source) {
    case "plan":
      return copy.sourcePlan;
    case "credit":
      return copy.sourceCredit;
    case "unmetered":
      return copy.sourceUnmetered;
    default:
      return copy.sourceUnknown;
  }
};

const ByokCallLogPanel: React.FC<Props> = ({ lang }) => {
  const copy = COPY[lang];

  const [calls, setCalls] = useState<AdvisorBillingCall[]>([]);
  const [byokOnly, setByokOnly] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await listAdvisorBillingCalls({ byokOnly, limit: 30 });
      setCalls(res.calls);
    } catch (err) {
      setError(formatApiError(err, copy.error));
    } finally {
      setLoading(false);
    }
  }, [byokOnly, copy.error]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  return (
    <section className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-sm font-semibold text-slate-800">{copy.title}</h2>
          <p className="mt-0.5 text-xs text-slate-500">{copy.sub}</p>
        </div>
        <div className="flex items-center gap-2">
          <label className="flex items-center gap-1.5 text-xs text-slate-700">
            <input
              type="checkbox"
              checked={byokOnly}
              onChange={(e) => setByokOnly(e.target.checked)}
              className="h-3.5 w-3.5"
            />
            {byokOnly ? copy.byokFilterOn : copy.byokFilterOff}
          </label>
          <button
            type="button"
            onClick={() => void refresh()}
            className="rounded-md border border-slate-200 bg-white px-2 py-1 text-[11px] text-slate-600 hover:bg-slate-50"
          >
            {copy.refresh}
          </button>
        </div>
      </div>

      {error ? (
        <div className="mt-3 rounded-md border border-rose-200 bg-rose-50 p-2 text-xs text-rose-700">
          {error}
        </div>
      ) : null}

      {loading ? (
        <p className="mt-3 text-xs text-slate-500">{lang === "zh" ? "加载中…" : "Loading…"}</p>
      ) : calls.length === 0 ? (
        <p className="mt-3 text-sm text-slate-500">{copy.empty}</p>
      ) : (
        <div className="mt-3 overflow-x-auto">
          <table className="min-w-full divide-y divide-slate-200 text-xs">
            <thead>
              <tr className="text-left text-[10px] uppercase tracking-wider text-slate-500">
                <th className="py-2 pr-3">{copy.cols.time}</th>
                <th className="py-2 pr-3">{copy.cols.symbol}</th>
                <th className="py-2 pr-3">{copy.cols.preset}</th>
                <th className="py-2 pr-3">{copy.cols.verdict}</th>
                <th className="py-2 pr-3">{copy.cols.models}</th>
                <th className="py-2 pr-3">{copy.cols.cost}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-100">
              {calls.map((call) => (
                <tr key={call.id} className="hover:bg-slate-50/60">
                  <td className="py-2 pr-3 text-slate-600 whitespace-nowrap">
                    {new Date(call.created_at).toLocaleString(
                      lang === "zh" ? "zh-CN" : "en-US",
                      { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" },
                    )}
                  </td>
                  <td className="py-2 pr-3 font-mono font-medium text-slate-900">{call.symbol}</td>
                  <td className="py-2 pr-3 text-slate-600">{call.preset_key}</td>
                  <td className="py-2 pr-3">
                    <span
                      className={`rounded-full px-1.5 py-0.5 text-[10px] font-medium ${
                        call.aggregate_verdict === "STRONG_BUY" || call.aggregate_verdict === "BUY"
                          ? "bg-emerald-100 text-emerald-800"
                          : call.aggregate_verdict === "AVOID" || call.aggregate_verdict === "SHORT"
                          ? "bg-rose-100 text-rose-800"
                          : "bg-slate-100 text-slate-700"
                      }`}
                    >
                      {call.aggregate_verdict}
                    </span>
                  </td>
                  <td className="py-2 pr-3">
                    {call.models_used && call.models_used.length > 0 ? (
                      <div className="flex flex-wrap gap-1">
                        {call.models_used.map((m, i) => {
                          const isByok = m.includes("@byok:");
                          return (
                            <span
                              key={`${call.id}-m-${i}`}
                              className={`rounded px-1.5 py-0.5 font-mono text-[10px] ${
                                isByok
                                  ? "bg-emerald-50 text-emerald-700 border border-emerald-200"
                                  : "bg-slate-100 text-slate-600"
                              }`}
                              title={isByok ? "BYOK" : "platform"}
                            >
                              {m}
                            </span>
                          );
                        })}
                      </div>
                    ) : (
                      <span className="text-slate-400">—</span>
                    )}
                  </td>
                  <td className="py-2 pr-3 text-slate-600">
                    <div className="flex items-baseline gap-1">
                      <span className="font-medium text-slate-900">{call.service_unit_cost}</span>
                      <span className="text-[10px] text-slate-500">
                        ({formatSource(call.service_unit_source, copy)})
                      </span>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
};

export default ByokCallLogPanel;
