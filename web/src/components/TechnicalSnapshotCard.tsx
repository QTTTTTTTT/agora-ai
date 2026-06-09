import React from "react";
import type { MasterTechnicalBlock } from "../lib/api";

// TechnicalSnapshotCard renders the price-action / momentum /
// volatility snapshot that the master panel saw when it formed
// its verdict.
//
// Compliance rationale (mirrors master_agent.go rule 9):
//
//   * Every visible cell is RAW MARKET DATA — closing price,
//     moving-average values, RSI / MACD / KDJ readings, support
//     and resistance levels, breakout state.
//   * Zero projections. Zero buy/sell labels. The "trend regime"
//     tag is the algorithmic classification of the MA stack,
//     not an action recommendation.
//   * Every section header includes the word "snapshot" /
//     "indicator" / "level" to keep the framing observational.
//   * Bottom-of-card disclaimer reiterates the publisher contract:
//     factual market data, not personalised advice.
//
// The component is read-only — no interactivity beyond hover
// highlights. If a future iteration wants e.g. clicking a level
// to chart it, that's a separate component (e.g. a TradingView
// embed) and lives at the page level, not here.

interface Props {
  technical?: MasterTechnicalBlock;
  language?: string;
}

function fmt(n: number | undefined, decimals = 2): string {
  if (n === undefined || n === null || !isFinite(n)) return "—";
  return n.toLocaleString("en-US", {
    minimumFractionDigits: decimals,
    maximumFractionDigits: decimals,
  });
}

function fmtPct(n: number | undefined): string {
  if (n === undefined || n === null || !isFinite(n)) return "—";
  const v = n * 100;
  return (v >= 0 ? "+" : "") + v.toFixed(2) + "%";
}

function fmtBigInt(n: number | undefined): string {
  if (n === undefined || n === null || !isFinite(n)) return "—";
  if (Math.abs(n) >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (Math.abs(n) >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (Math.abs(n) >= 1e3) return (n / 1e3).toFixed(1) + "K";
  return n.toFixed(0);
}

// pctClass — colour the percentage red/green based on sign,
// neutral when zero. Keeps the eye drawn to the magnitude AND
// direction without needing to read the sign.
function pctClass(n: number | undefined): string {
  if (n === undefined || n === null || !isFinite(n) || n === 0) return "text-slate-700";
  return n > 0 ? "text-emerald-700" : "text-rose-700";
}

function alignmentLabel(a: string | undefined, zh: boolean): string {
  switch ((a || "").toLowerCase()) {
    case "bullish":
      return zh ? "多头排列（SMA20>SMA50>SMA200）" : "Bullish stack (SMA20>SMA50>SMA200)";
    case "bearish":
      return zh ? "空头排列（SMA20<SMA50<SMA200）" : "Bearish stack (SMA20<SMA50<SMA200)";
    case "mixed":
      return zh ? "均线交错" : "Mixed alignment";
    default:
      return zh ? "暂无足够样本" : "Insufficient bars";
  }
}

function rsiZoneLabel(z: string | undefined, zh: boolean): string {
  switch ((z || "").toLowerCase()) {
    case "overbought":
      return zh ? "超买区" : "Overbought zone";
    case "oversold":
      return zh ? "超卖区" : "Oversold zone";
    case "neutral":
      return zh ? "中性区" : "Neutral zone";
    default:
      return "";
  }
}

function macdCrossLabel(c: string | undefined, zh: boolean): string {
  switch ((c || "").toLowerCase()) {
    case "bullish":
      return zh
        ? "最新 bar 出现多头交叉（柱状由负转正）"
        : "Latest bar: bullish crossover (histogram flipped positive)";
    case "bearish":
      return zh
        ? "最新 bar 出现空头交叉（柱状由正转负）"
        : "Latest bar: bearish crossover (histogram flipped negative)";
    default:
      return "";
  }
}

function breakoutLabel(b: string | undefined, srWindow: number | undefined, zh: boolean): string {
  const w = srWindow || 20;
  switch ((b || "").toLowerCase()) {
    case "above_resistance":
      return zh ? `收盘上破前 ${w} 日高点` : `Close above prior ${w}-day high`;
    case "below_support":
      return zh ? `收盘下破前 ${w} 日低点` : `Close below prior ${w}-day low`;
    case "near_resistance":
      return zh ? `逼近前 ${w} 日阻力位（≤ 1×ATR）` : `Within 1×ATR of prior ${w}-day resistance`;
    case "near_support":
      return zh ? `逼近前 ${w} 日支撑位（≤ 1×ATR）` : `Within 1×ATR of prior ${w}-day support`;
    default:
      return zh ? "区间内运行" : "Inside prior range";
  }
}

export default function TechnicalSnapshotCard({ technical, language }: Props) {
  if (!technical) return null;
  const zh = language === "zh-CN";
  const t = technical;
  const heading = zh
    ? "技术面快照（客观市场数据，非投资建议）"
    : "Technical Snapshot (factual market data, not investment advice)";

  return (
    <section className="rounded-xl border border-slate-200 bg-white p-4 shadow-sm">
      <div className="mb-3 flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="text-sm font-semibold text-slate-800">{heading}</h3>
        {t.asof ? (
          <span className="text-[11px] text-slate-400">
            {zh ? "数据截止" : "As of"}: {new Date(t.asof).toUTCString()}
            {typeof t.bars_used === "number" ? ` · ${t.bars_used} bars` : null}
          </span>
        ) : null}
      </div>

      {/* Price action */}
      <div className="grid grid-cols-2 gap-y-1 gap-x-4 text-xs sm:grid-cols-4">
        <div>
          <div className="text-[10px] uppercase tracking-wider text-slate-400">
            {zh ? "收盘价" : "Close"}
          </div>
          <div className="font-medium text-slate-900">{fmt(t.last_close)}</div>
        </div>
        <div>
          <div className="text-[10px] uppercase tracking-wider text-slate-400">DoD</div>
          <div className={`font-medium ${pctClass(t.pct_change_1d)}`}>{fmtPct(t.pct_change_1d)}</div>
        </div>
        <div>
          <div className="text-[10px] uppercase tracking-wider text-slate-400">5D</div>
          <div className={`font-medium ${pctClass(t.pct_change_5d)}`}>{fmtPct(t.pct_change_5d)}</div>
        </div>
        <div>
          <div className="text-[10px] uppercase tracking-wider text-slate-400">20D</div>
          <div className={`font-medium ${pctClass(t.pct_change_20d)}`}>{fmtPct(t.pct_change_20d)}</div>
        </div>
      </div>

      {/* Moving averages */}
      <div className="mt-3 border-t border-slate-100 pt-3">
        <div className="mb-1 text-[10px] uppercase tracking-wider text-slate-400">
          {zh ? "均线指标（SMA）" : "Moving Averages (SMA)"}
        </div>
        <div className="grid grid-cols-3 gap-2 text-xs sm:grid-cols-4">
          <div>
            <span className="text-slate-500">SMA20</span>
            <span className="ml-1 font-medium text-slate-800">{fmt(t.sma20)}</span>
          </div>
          <div>
            <span className="text-slate-500">SMA50</span>
            <span className="ml-1 font-medium text-slate-800">{fmt(t.sma50)}</span>
          </div>
          <div>
            <span className="text-slate-500">SMA200</span>
            <span className="ml-1 font-medium text-slate-800">{fmt(t.sma200)}</span>
          </div>
          <div className="col-span-3 sm:col-span-1">
            <span className="text-slate-500">{zh ? "排列" : "Alignment"}</span>
            <span className="ml-1 font-medium text-slate-800">{alignmentLabel(t.ma_alignment, zh)}</span>
          </div>
        </div>
      </div>

      {/* Momentum / volatility */}
      <div className="mt-3 border-t border-slate-100 pt-3">
        <div className="mb-1 text-[10px] uppercase tracking-wider text-slate-400">
          {zh ? "动量与波动" : "Momentum & Volatility"}
        </div>
        <div className="grid grid-cols-2 gap-1 text-xs sm:grid-cols-4">
          <div>
            <span className="text-slate-500">RSI14</span>
            <span className="ml-1 font-medium text-slate-800">
              {fmt(t.rsi14, 1)}
              {t.rsi14_zone ? (
                <span className="ml-1 text-[10px] text-slate-500">({rsiZoneLabel(t.rsi14_zone, zh)})</span>
              ) : null}
            </span>
          </div>
          <div>
            <span className="text-slate-500">MACD</span>
            <span className="ml-1 font-medium text-slate-800">{fmt(t.macd_line, 3)}</span>
          </div>
          <div>
            <span className="text-slate-500">Signal</span>
            <span className="ml-1 font-medium text-slate-800">{fmt(t.macd_signal, 3)}</span>
          </div>
          <div>
            <span className="text-slate-500">Hist</span>
            <span className={`ml-1 font-medium ${pctClass(t.macd_hist)}`}>{fmt(t.macd_hist, 3)}</span>
          </div>
          {macdCrossLabel(t.macd_cross, zh) ? (
            <div className="col-span-2 sm:col-span-4 text-[11px] text-indigo-700">
              {macdCrossLabel(t.macd_cross, zh)}
            </div>
          ) : null}
          {typeof t.atr14_pct_of_price === "number" ? (
            <div>
              <span className="text-slate-500">ATR14</span>
              <span className="ml-1 font-medium text-slate-800">{fmtPct(t.atr14_pct_of_price)}</span>
              <span className="ml-1 text-[10px] text-slate-400">{zh ? "占价比" : "of price"}</span>
            </div>
          ) : null}
          {typeof t.kdj_j === "number" && t.kdj_j !== 0 ? (
            <div className="col-span-2">
              <span className="text-slate-500">KDJ</span>
              <span className="ml-1 font-medium text-slate-800">
                K={fmt(t.kdj_k, 1)} D={fmt(t.kdj_d, 1)} J={fmt(t.kdj_j, 1)}
              </span>
            </div>
          ) : null}
        </div>
      </div>

      {/* Volume */}
      {(t.volume || t.relative_volume) ? (
        <div className="mt-3 border-t border-slate-100 pt-3">
          <div className="mb-1 text-[10px] uppercase tracking-wider text-slate-400">
            {zh ? "成交量" : "Volume"}
          </div>
          <div className="grid grid-cols-2 gap-1 text-xs sm:grid-cols-4">
            <div>
              <span className="text-slate-500">{zh ? "成交量" : "Latest"}</span>
              <span className="ml-1 font-medium text-slate-800">{fmtBigInt(t.volume)}</span>
            </div>
            <div>
              <span className="text-slate-500">{zh ? "20 日均量比" : "vs 20D SMA"}</span>
              <span className="ml-1 font-medium text-slate-800">
                {t.relative_volume ? `${t.relative_volume.toFixed(2)}x` : "—"}
              </span>
            </div>
          </div>
        </div>
      ) : null}

      {/* Support / resistance / breakout state */}
      {(t.support || t.resistance || t.breakout_state) ? (
        <div className="mt-3 border-t border-slate-100 pt-3">
          <div className="mb-1 text-[10px] uppercase tracking-wider text-slate-400">
            {zh ? "支撑 / 阻力 / 区间状态（算法识别）" : "Support / Resistance / Range (algorithmic)"}
          </div>
          <div className="grid grid-cols-2 gap-1 text-xs sm:grid-cols-4">
            <div>
              <span className="text-slate-500">{zh ? "支撑" : "Support"}</span>
              <span className="ml-1 font-medium text-slate-800">{fmt(t.support)}</span>
            </div>
            <div>
              <span className="text-slate-500">{zh ? "阻力" : "Resistance"}</span>
              <span className="ml-1 font-medium text-slate-800">{fmt(t.resistance)}</span>
            </div>
            <div className="col-span-2">
              <span className="text-slate-500">{zh ? "状态" : "State"}</span>
              <span className="ml-1 font-medium text-slate-800">{breakoutLabel(t.breakout_state, t.sr_window, zh)}</span>
            </div>
          </div>
        </div>
      ) : null}

      {/* Tags */}
      {t.tags && t.tags.length > 0 ? (
        <div className="mt-3 border-t border-slate-100 pt-3">
          <div className="mb-1 text-[10px] uppercase tracking-wider text-slate-400">
            {zh ? "技术观察要点（事实陈述）" : "Technical Observations (factual)"}
          </div>
          <ul className="space-y-0.5 text-xs text-slate-700">
            {t.tags.map((tag, i) => (
              <li key={i} className="flex gap-1">
                <span className="text-slate-400">·</span>
                <span>{tag}</span>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      <div className="mt-3 border-t border-slate-100 pt-2 text-[10px] leading-relaxed text-slate-400">
        {zh
          ? "本页数值为算法计算的市场观察数据，不构成任何交易信号或投资建议。涨跌幅、均线、RSI、MACD、KDJ、支撑阻力等均基于公开行情数据按通用公式计算（Wilder RSI、12-26-9 MACD、9-3-3 KDJ）。过往表现不代表未来收益。"
          : "Values above are algorithmic market observations, not trade signals or personalised advice. RSI / MACD / KDJ / S・R levels are computed from public OHLC data via standard formulas (Wilder RSI, 12-26-9 MACD, 9-3-3 KDJ). Past performance is not indicative of future results."}
      </div>
    </section>
  );
}
