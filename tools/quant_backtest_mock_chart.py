#!/usr/bin/env python3
"""quant_backtest_mock_chart.py

Render a two-panel mockup that matches the style of the small-red-book
reference screenshot the user attached: top "metric strip" with 10 KPI
chips, big strategy-vs-benchmark-vs-excess line chart with a date
brush stub at the bottom.

Two panels generated as two separate PNGs (so we can pick a
horizontal-stack layout on the slide deck without overflow):

  - cn_a_share_backtest_mock.png — A-share intraday strategy vs.
    沪深 300 / 中证 1000, 2023-01 → 2025-12 (3y).
  - us_equity_backtest_mock.png — US monthly rebalance vs. IWM,
    2016-01 → 2025-12 (10y).

The numbers are stylized — the goal is to show what the
production report will look like AFTER we wire factorlab + backtest
output through Recharts. Synthetic returns are constructed so the
shape resembles the proposed system's expected behaviour:
    A股   ann ~38%, vol ~22%, MDD ~18%
    美股  ann ~22%, vol ~16%, MDD ~14%, excess vs IWM ~12%

Run:
    python3 tools/quant_backtest_mock_chart.py
"""

from __future__ import annotations

import os
import sys
from dataclasses import dataclass
from pathlib import Path

import matplotlib.pyplot as plt
import matplotlib.dates as mdates
import numpy as np
from matplotlib.gridspec import GridSpec

OUT_DIR = Path(__file__).resolve().parent.parent / "build" / "mockups"
OUT_DIR.mkdir(parents=True, exist_ok=True)

# Reference image palette
COLOR_STRATEGY = "#1890ff"  # blue
COLOR_BENCH = "#ff4d4f"     # red
COLOR_EXCESS = "#faad14"    # amber
COLOR_TEXT_RED = "#f5222d"
COLOR_TEXT_GREEN = "#52c41a"
COLOR_TEXT_DARK = "#262626"
COLOR_LABEL = "#8c8c8c"
COLOR_GRID = "#f0f0f0"


@dataclass
class Series:
    label: str
    color: str
    cum_return: np.ndarray  # cumulative return relative to 0 (e.g. 0.30 = +30%)


def _seeded_returns(seed: int, n_days: int, ann_drift: float, ann_vol: float) -> np.ndarray:
    """Generate cumulative returns from a deterministic geometric brownian motion."""
    rng = np.random.default_rng(seed)
    daily_drift = ann_drift / 252.0
    daily_vol = ann_vol / np.sqrt(252.0)
    daily_log = rng.normal(daily_drift - 0.5 * daily_vol**2, daily_vol, size=n_days)
    cum_log = np.cumsum(daily_log)
    # Translate to "started at 0" cumulative arithmetic return.
    return np.exp(cum_log) - 1.0


def _strategy_vs_benchmark(
    benchmark_seed: int,
    idio_seed: int,
    n_days: int,
    bench_ann_drift: float,
    bench_ann_vol: float,
    beta: float,
    alpha_ann: float,
    idio_vol_ann: float,
) -> tuple[np.ndarray, np.ndarray]:
    """Generate a benchmark + a strategy as `beta * benchmark + alpha + idiosyncratic`.

    This is how real strategies relate to indices: they share most of
    their variance with the benchmark, plus some skill (alpha) and
    some independent noise. Drawing strategy + benchmark as two
    independent random walks (the previous approach) creates an
    excess curve with sqrt(2) × vol of either leg, which looks like
    pure noise. With this decomposition the excess curve is
    dominated by the alpha trend + small idio noise.
    """
    rng_b = np.random.default_rng(benchmark_seed)
    rng_i = np.random.default_rng(idio_seed)
    # Daily log returns.
    bench_daily_drift = bench_ann_drift / 252.0
    bench_daily_vol = bench_ann_vol / np.sqrt(252.0)
    bench_log = rng_b.normal(bench_daily_drift - 0.5 * bench_daily_vol**2,
                             bench_daily_vol, size=n_days)
    alpha_daily = alpha_ann / 252.0
    idio_daily_vol = idio_vol_ann / np.sqrt(252.0)
    idio_log = rng_i.normal(alpha_daily - 0.5 * idio_daily_vol**2,
                            idio_daily_vol, size=n_days)
    strat_log = beta * bench_log + idio_log
    bench_cum = np.exp(np.cumsum(bench_log)) - 1.0
    strat_cum = np.exp(np.cumsum(strat_log)) - 1.0
    return strat_cum, bench_cum


def _compute_metrics(strategy: np.ndarray, benchmark: np.ndarray, years: float) -> dict:
    """Match the metric strip used in the screenshot."""
    total_strategy = strategy[-1]
    total_bench = benchmark[-1]
    excess = total_strategy - total_bench
    daily_strategy = np.diff(np.log1p(strategy + 1.0))
    daily_bench = np.diff(np.log1p(benchmark + 1.0))
    excess_daily = daily_strategy - daily_bench
    ann_return = (1.0 + total_strategy) ** (1.0 / years) - 1.0
    ann_vol = daily_strategy.std() * np.sqrt(252.0) if len(daily_strategy) > 1 else 0.0
    sharpe = (daily_strategy.mean() / daily_strategy.std() * np.sqrt(252.0)
              if daily_strategy.std() > 0 else 0.0)
    # Max drawdown on equity = (1 + cum_return).
    equity = 1.0 + strategy
    peak = np.maximum.accumulate(equity)
    drawdown = (equity / peak) - 1.0
    max_dd = drawdown.min()
    # Excess max drawdown.
    excess_eq = 1.0 + (strategy - benchmark)
    excess_peak = np.maximum.accumulate(excess_eq)
    excess_dd = ((excess_eq / excess_peak) - 1.0).min()
    # Beta + alpha via OLS.
    beta = np.cov(daily_strategy, daily_bench)[0, 1] / daily_bench.var() if daily_bench.var() > 0 else 0.0
    alpha = (daily_strategy.mean() - beta * daily_bench.mean()) * 252.0
    return {
        "today_return": daily_strategy[-1] if len(daily_strategy) > 0 else 0.0,
        "today_excess": excess_daily[-1] if len(excess_daily) > 0 else 0.0,
        "strategy_total": total_strategy,
        "excess_total": excess,
        "bench_total": total_bench,
        "alpha": alpha,
        "beta": beta,
        "sharpe": sharpe,
        "max_dd": max_dd,
        "excess_max_dd": excess_dd,
        "ann_return": ann_return,
        "ann_vol": ann_vol,
    }


def _draw_panel(
    title_cn: str,
    title_en: str,
    bench_label: str,
    dates: np.ndarray,
    series: list[Series],
    metrics: dict,
    out_path: Path,
):
    fig = plt.figure(figsize=(13.5, 7.5), dpi=160, facecolor="white")
    gs = GridSpec(
        nrows=2,
        ncols=1,
        height_ratios=[1.0, 2.4],
        hspace=0.42,
        left=0.06,
        right=0.97,
        top=0.93,
        bottom=0.10,
    )

    # ----- metric strip --------------------------------------------------
    ax_kpi = fig.add_subplot(gs[0, 0])
    ax_kpi.axis("off")
    ax_kpi.set_xlim(0, 100)
    ax_kpi.set_ylim(0, 100)
    ax_kpi.text(
        0, 92, "收益概述", fontsize=14, weight="bold", color=COLOR_TEXT_DARK,
    )

    # Two rows × five columns of KPIs, matching the reference image.
    kpi_cells = [
        # row 1
        ("当日收益", f"{metrics['today_return']*100:+.2f}%",
         COLOR_TEXT_GREEN if metrics['today_return'] >= 0 else COLOR_TEXT_RED),
        ("当日超额", f"{metrics['today_excess']*100:+.2f}%",
         COLOR_TEXT_GREEN if metrics['today_excess'] >= 0 else COLOR_TEXT_RED),
        ("策略收益", f"{metrics['strategy_total']*100:.2f}%", COLOR_TEXT_RED),
        ("超额收益", f"{metrics['excess_total']*100:.2f}%", COLOR_TEXT_RED),
        ("基准收益", f"{metrics['bench_total']*100:.2f}%", COLOR_TEXT_RED),
        # row 2
        ("Alpha", f"{metrics['alpha']:.3f}", COLOR_TEXT_DARK),
        ("Beta", f"{metrics['beta']:.3f}", COLOR_TEXT_DARK),
        ("Sharpe", f"{metrics['sharpe']:.3f}", COLOR_TEXT_DARK),
        ("最大回撤", f"{metrics['max_dd']*100:.2f}%", COLOR_TEXT_DARK),
        ("超额最大回撤", f"{metrics['excess_max_dd']*100:.2f}%", COLOR_TEXT_DARK),
    ]
    col_width = 100 / 5.0
    row_y = [60, 25]
    for idx, (label, value, value_color) in enumerate(kpi_cells):
        row = idx // 5
        col = idx % 5
        x = col * col_width
        ax_kpi.text(x, row_y[row] + 18, label, fontsize=10.5, color=COLOR_LABEL)
        ax_kpi.text(x, row_y[row], value, fontsize=16,
                    weight="bold", color=value_color)

    # ----- main chart ----------------------------------------------------
    ax = fig.add_subplot(gs[1, 0])

    # Title strip + tab pills (just decorative).
    ax.set_title(
        f"{title_cn}      ·    {title_en}",
        loc="left",
        fontsize=13,
        weight="bold",
        color=COLOR_TEXT_DARK,
        pad=14,
    )
    # Tab pills (近一月 / 近半年 / 近一年 / 近三年 / 全部) — drawn as decorative
    # annotations top-right of the axes.
    tabs = ["近一月", "近半年", "近一年", "近三年", "全部"]
    for i, t in enumerate(tabs):
        is_active = i in (2, 4)
        bbox_color = "#e6f4ff" if is_active else "white"
        edge_color = "#91caff" if is_active else "#d9d9d9"
        text_color = "#1677ff" if is_active else "#595959"
        ax.text(
            0.78 + i * 0.045,
            1.06,
            t,
            transform=ax.transAxes,
            fontsize=8.5,
            color=text_color,
            ha="center",
            va="center",
            bbox=dict(boxstyle="round,pad=0.25", facecolor=bbox_color,
                      edgecolor=edge_color, linewidth=0.6),
        )

    # Light grid only on Y axis.
    ax.yaxis.grid(True, color=COLOR_GRID, linewidth=0.8)
    ax.xaxis.grid(False)
    ax.set_axisbelow(True)

    for s in series:
        ax.plot(dates, s.cum_return * 100.0, color=s.color, linewidth=1.6,
                label=s.label)

    ax.legend(
        loc="upper center",
        bbox_to_anchor=(0.5, 1.06),
        ncol=3,
        frameon=False,
        fontsize=10,
        handlelength=2.0,
    )

    ax.set_ylabel("收益率", fontsize=10, color=COLOR_LABEL)
    ax.yaxis.set_major_formatter(plt.FuncFormatter(lambda y, _: f"{int(y)}%"))
    ax.xaxis.set_major_locator(mdates.AutoDateLocator(maxticks=10))
    ax.xaxis.set_major_formatter(mdates.DateFormatter("%Y-%m-%d"))
    for spine in ("top", "right"):
        ax.spines[spine].set_visible(False)
    for spine in ("left", "bottom"):
        ax.spines[spine].set_color("#d9d9d9")
    ax.tick_params(colors=COLOR_LABEL, labelsize=9)

    # Markers on excess curve peaks (matches the reference: small
    # circles at notable turning points on the gold line).
    excess_series = next((s for s in series if "超额" in s.label), None)
    if excess_series is not None and len(excess_series.cum_return) > 0:
        ec = excess_series.cum_return
        # Find ~3 local maxima evenly spaced as visual anchors.
        N = len(ec)
        for idx in (N // 4, N // 2, 3 * N // 4):
            ax.scatter([dates[idx]], [ec[idx] * 100.0],
                       color=excess_series.color, s=24, zorder=5,
                       edgecolor="white", linewidth=1.0)

    # Date brush stub below the axes.
    brush_ax = fig.add_axes([0.06, 0.045, 0.91, 0.025])
    brush_ax.axis("off")
    brush_ax.add_patch(plt.Rectangle((0.0, 0.0), 1.0, 1.0,
                                     facecolor="#e6f4ff", edgecolor="#bae0ff"))
    brush_ax.add_patch(plt.Rectangle((0.05, 0.0), 0.92, 1.0,
                                     facecolor="#1890ff", alpha=0.15,
                                     edgecolor="#1890ff", linewidth=1.5))

    fig.savefig(out_path, dpi=160, bbox_inches="tight", facecolor="white")
    plt.close(fig)
    print(f"wrote {out_path}", file=sys.stderr)


def render_cn_panel():
    # 2023-01-03 → 2025-12-31, ~730 trading days.
    days = np.busday_count(np.datetime64("2023-01-03"),
                           np.datetime64("2025-12-31"))
    dates = np.arange(np.datetime64("2023-01-03"),
                      np.datetime64("2023-01-03") + np.timedelta64(int(days * 1.4), "D"),
                      dtype="datetime64[D]")
    dates = dates[np.is_busday(dates)][:days]

    # Synthetic series — shaped to match the proposal's targets:
    #   A股 ann ~38%, MDD ~18%, Sharpe >1.5, 超额年化 ~30%
    # We draw the benchmark first (中证1000 ~7%/yr, ~19% vol) and
    # then build the strategy as 0.7×benchmark + 0.32 alpha + idio,
    # which matches the realistic beta of a stock-picking strategy
    # against a broad small-cap index.
    strategy, csi1000 = _strategy_vs_benchmark(
        benchmark_seed=11, idio_seed=42, n_days=days,
        bench_ann_drift=0.07, bench_ann_vol=0.19,
        beta=0.70, alpha_ann=0.32, idio_vol_ann=0.14,
    )
    excess = strategy - csi1000

    metrics = _compute_metrics(strategy, csi1000, years=days / 252.0)

    series = [
        Series("策略收益率",     COLOR_STRATEGY, strategy),
        Series("中证1000收益率", COLOR_BENCH,    csi1000),
        Series("超额收益",       COLOR_EXCESS,   excess),
    ]
    _draw_panel(
        title_cn="A 股日内策略回测",
        title_en="A-share intraday backtest · 2023-01 → 2025-12 · benchmark 中证1000",
        bench_label="中证1000",
        dates=dates,
        series=series,
        metrics=metrics,
        out_path=OUT_DIR / "cn_a_share_backtest_mock.png",
    )


def render_us_panel():
    # 2016-01-04 → 2025-12-31, ~2515 trading days.
    days = np.busday_count(np.datetime64("2016-01-04"),
                           np.datetime64("2025-12-31"))
    dates = np.arange(np.datetime64("2016-01-04"),
                      np.datetime64("2016-01-04") + np.timedelta64(int(days * 1.4), "D"),
                      dtype="datetime64[D]")
    dates = dates[np.is_busday(dates)][:days]

    # 美股 vs IWM (Russell 2000) ~7%/yr historical → 策略目标年化 ~18%,
    # 超额 ~11%. Seed 4 gives a benchmark that lands at +100% over
    # 10y (≈ 6.9% ann), matching IWM's historical 2016-2025 actual.
    strategy, iwm = _strategy_vs_benchmark(
        benchmark_seed=4, idio_seed=21, n_days=days,
        bench_ann_drift=0.09, bench_ann_vol=0.18,
        beta=0.80, alpha_ann=0.11, idio_vol_ann=0.10,
    )
    excess = strategy - iwm

    metrics = _compute_metrics(strategy, iwm, years=days / 252.0)

    series = [
        Series("Strategy",  COLOR_STRATEGY, strategy),
        Series("IWM (Russell 2000)", COLOR_BENCH, iwm),
        Series("Excess",    COLOR_EXCESS,   excess),
    ]
    _draw_panel(
        title_cn="美股月度调仓回测",
        title_en="US monthly rebalance · 2016-01 → 2025-12 · benchmark IWM",
        bench_label="IWM",
        dates=dates,
        series=series,
        metrics=metrics,
        out_path=OUT_DIR / "us_equity_backtest_mock.png",
    )


def _pick_cjk_font() -> str:
    """Pick the first CJK-capable font available on the host.

    Returns the *family name* matplotlib's font manager will resolve.
    Order is macOS-friendly first (PingFang HK is the system CJK font
    on every recent macOS), then Linux/CI fallbacks. We do not rely on
    PingFang SC — it's a family alias matplotlib's font_manager
    doesn't always resolve even when /System/Library/Fonts/PingFang.ttc
    is installed.
    """
    from matplotlib import font_manager
    candidates = [
        "PingFang HK", "PingFang TC", "PingFang SC",
        "Heiti TC", "Heiti SC", "STHeiti",
        "Arial Unicode MS", "Hiragino Sans",
        "Noto Sans CJK SC", "Source Han Sans SC",
        "WenQuanYi Zen Hei", "Microsoft YaHei",
    ]
    available = {f.name for f in font_manager.fontManager.ttflist}
    for c in candidates:
        if c in available:
            return c
    return "DejaVu Sans"


def main():
    font = _pick_cjk_font()
    plt.rcParams["font.family"] = font
    plt.rcParams["axes.unicode_minus"] = False
    print(f"using font: {font}", file=sys.stderr)

    render_cn_panel()
    render_us_panel()


if __name__ == "__main__":
    main()
