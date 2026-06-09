"""Fundamentals sidecar for the AI Fund Platform.

The Go-side ``fundamental.AkshareProvider`` (server/internal/fundamental/
provider_akshare.go) tries four endpoints in order until one returns a
JSON body it can parse:

    GET /api/fundamental?symbol=688205&market=a_share
    GET /fundamental?symbol=688205&market=a_share
    GET /api/key_metrics?symbol=688205&market=a_share
    GET /key_metrics?symbol=688205&market=a_share

The parser accepts either a raw object or ``{"data": {...}}`` and
tolerates both English and Chinese field aliases (``pe`` / ``pe_ratio``
/ ``市盈率`` etc.). To keep this sidecar small and reproducible we always
emit the English aliases.

Numeric conventions follow the consuming Go formatter
(``fundamental.FormatForPrompt``):

* ROE / margins / growth / dividend yield are **decimal fractions** —
  ROE of 18.5% goes on the wire as 0.185.
* MarketCap is in raw currency units (CNY for A-shares).

Why this set of fields and not PE/PB/market-cap?
================================================

Per-stock live valuation metrics (PE, PB, total market cap) in akshare
1.18.x are sourced from eastmoney's ``push2.eastmoney.com`` quote API,
which is not consistently reachable from outside mainland China and
often gets TCP-RST from this container. The functions that DO work
reliably are the statement-derived ones served from 新浪 / 同花顺 /
datacenter.eastmoney — namely ``stock_financial_analysis_indicator``
and ``stock_financial_abstract_ths``. Those expose ROE, profit margin,
gross margin, debt-to-asset, revenue growth, earnings growth, and
operating cash flow per share, which is exactly the cluster the
master agents — especially the value/disruptive-innovation personas
(Buffett, Munger, Lynch, Wood, Marks) — care about most.

If a future deployment puts the sidecar behind a CN-routed egress and
``push2.eastmoney.com`` becomes reachable, ``_latest_quote_em`` can be
re-enabled to populate ``pe`` / ``pb`` / ``market_cap`` from
``stock_individual_info_em`` and ``stock_bid_ask_em``.
"""

import logging
import math
import os
import re
import threading
import time
import urllib.request
import urllib.error
from datetime import datetime, timezone
from typing import Any

import akshare as ak
from flask import Flask, jsonify, request

logging.basicConfig(
    level=os.environ.get("LOG_LEVEL", "INFO").upper(),
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)
log = logging.getLogger("akshare-fundamental")

app = Flask(__name__)


# ---------------------------------------------------------------------------
# Company name resolution
#
# Most akshare name sources require the eastmoney / xueqiu / szse hosts,
# which are not reliably reachable from this container. Sina's
# ``hq.sinajs.cn`` IS reachable and returns a tiny CSV line per symbol
# starting with the Chinese short name. We parse that.
#
# Results are cached in-process for a week — listed-company short names
# rarely change, and the upstream call is ~200 ms so caching is more
# about reducing dependency on a third-party API than about latency.
# ---------------------------------------------------------------------------

_NAME_CACHE_TTL_SECONDS = 7 * 24 * 3600
_name_cache: dict[str, tuple[str, float]] = {}
_name_cache_lock = threading.Lock()
_SINA_QUOTE_RE = re.compile(r'var\s+hq_str_[^=]+="([^,"]+)')


def _exchange_prefix(symbol: str) -> str | None:
    """Map a bare 6-digit A-share code to the sina exchange prefix.

    * ``6xxxxx`` — Shanghai main board + STAR market → ``sh``
    * ``0xxxxx`` / ``3xxxxx`` — Shenzhen main + ChiNext → ``sz``
    * ``8xxxxx`` / ``4xxxxx`` / ``9xxxxx`` — Beijing Stock Exchange → ``bj``

    Returns ``None`` for anything that doesn't look like a CN equity
    code so we don't hammer sina with garbage requests.
    """
    if len(symbol) != 6 or not symbol.isdigit():
        return None
    head = symbol[0]
    if head == "6":
        return "sh"
    if head in ("0", "3"):
        return "sz"
    if head in ("4", "8", "9"):
        return "bj"
    return None


def lookup_name(symbol: str) -> str | None:
    """Resolve a 6-digit A-share code to its Chinese short name via
    sina's hqjs endpoint, with a 7-day in-process cache.

    Returns ``None`` for unknown symbols, network failures, or any
    response that doesn't match the expected ``var hq_str_..="NAME,..."``
    shape — callers should treat ``None`` as "we don't know" rather
    than synthesizing a placeholder.
    """
    symbol = (symbol or "").strip()
    if not symbol:
        return None
    now = time.time()
    with _name_cache_lock:
        cached = _name_cache.get(symbol)
        if cached and (now - cached[1]) < _NAME_CACHE_TTL_SECONDS:
            return cached[0] or None

    prefix = _exchange_prefix(symbol)
    if prefix is None:
        return None
    url = f"https://hq.sinajs.cn/list={prefix}{symbol}"
    try:
        req = urllib.request.Request(
            url,
            headers={
                # Sina's hqjs rejects requests without a Referer header
                # claiming origin from finance.sina.com.cn — this is
                # the same workaround akshare uses internally.
                "Referer": "https://finance.sina.com.cn",
                "User-Agent": "Mozilla/5.0 (akshare-fundamental sidecar)",
            },
        )
        with urllib.request.urlopen(req, timeout=5) as resp:
            raw = resp.read()
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        log.warning("name lookup failed for %s: %s", symbol, exc)
        return None
    text = raw.decode("gb18030", errors="replace")
    match = _SINA_QUOTE_RE.search(text)
    # Cache the resolved name OR an empty string when sina answered
    # but the response didn't contain a name (delisted / suspended
    # / unallocated code). Network-level failures above this point
    # are deliberately NOT cached so a transient outage doesn't
    # poison the cache for the full TTL.
    name = match.group(1).strip() if match else ""
    with _name_cache_lock:
        _name_cache[symbol] = (name, now)
    return name or None


# ---------------------------------------------------------------------------
# Listing date / company tenure
#
# Eastmoney's ``stock_individual_info_em`` would be the obvious source for
# 上市日期 but is not reachable from this container (same blockade as the
# live-quote API documented above). ``stock_profile_cninfo`` is served from
# 巨潮资讯 (cninfo.com.cn — the China Securities Regulatory Commission's
# official disclosure portal) and IS reachable, returns the canonical
# listing date plus a few other rarely-changing company-profile fields.
#
# Cached aggressively (30 days) because the listing date is, by definition,
# a one-time fact that never changes once a company is on the exchange.
# ---------------------------------------------------------------------------

_PROFILE_CACHE_TTL_SECONDS = 30 * 24 * 3600
# value: (listing_date_iso_or_empty_string, cached_at_epoch)
_profile_cache: dict[str, tuple[str, float]] = {}
_profile_cache_lock = threading.Lock()


def lookup_listing_date(symbol: str) -> str | None:
    """Resolve a 6-digit A-share code to its listing date (``YYYY-MM-DD``)
    via cninfo's company profile, with a 30-day in-process cache.

    Returns ``None`` for unknown symbols, network failures, or any
    upstream payload that doesn't carry a parseable 上市日期 — callers
    should treat ``None`` as "we don't know" rather than synthesizing a
    placeholder year.

    Negative results (sina/cninfo answered but had no listing date) are
    cached as the empty string so a transient gap doesn't trigger
    repeated upstream lookups; network failures are NOT cached so a
    blip can self-heal on the next request.
    """
    symbol = (symbol or "").strip()
    if not symbol:
        return None
    now = time.time()
    with _profile_cache_lock:
        cached = _profile_cache.get(symbol)
        if cached and (now - cached[1]) < _PROFILE_CACHE_TTL_SECONDS:
            return cached[0] or None

    listing_date = ""
    try:
        df = ak.stock_profile_cninfo(symbol=symbol)
    except Exception as exc:  # noqa: BLE001 — graceful degradation
        log.warning("stock_profile_cninfo failed for %s: %s", symbol, exc)
        return None  # don't cache transient failures
    if df is not None and not df.empty:
        row = _row_to_dict(df.iloc[0])
        raw = row.get("上市日期") or row.get("listing_date") or ""
        listing_date = _normalize_iso_date(raw)

    with _profile_cache_lock:
        _profile_cache[symbol] = (listing_date, now)
    return listing_date or None


def _normalize_iso_date(value: Any) -> str:
    """Coerce a date-like value into a ``YYYY-MM-DD`` string.

    Accepts ``datetime.date``, ``datetime.datetime``, pandas ``Timestamp``,
    or strings like ``"2022-08-09"`` / ``"2022/08/09"`` / ``"20220809"``.
    Returns ``""`` for anything else so the caller can treat missing data
    uniformly.
    """
    if value is None or value is False:
        return ""
    # datetime / pandas Timestamp / numpy datetime all expose strftime
    if hasattr(value, "strftime"):
        try:
            return value.strftime("%Y-%m-%d")
        except Exception:  # noqa: BLE001
            return ""
    s = str(value).strip()
    if not s or s.lower() in ("nan", "nat", "none", "--"):
        return ""
    # Match common Chinese-source variants: "YYYY-MM-DD", "YYYY/MM/DD",
    # "YYYYMMDD". Anything else we hand back as-is — the Go side parses
    # ISO strings with a tolerant time.Parse anyway.
    s = s.replace("/", "-")
    m = re.match(r"^(\d{4})-?(\d{2})-?(\d{2})", s)
    if m:
        return f"{m.group(1)}-{m.group(2)}-{m.group(3)}"
    return s[:10] if len(s) >= 10 else ""


def _years_since(date_iso: str, asof: datetime | None = None) -> float | None:
    """Return the number of decimal years between ``date_iso`` and
    ``asof`` (defaults to now-UTC), or ``None`` if the input is empty
    or unparseable.

    Used to render ``listing_years`` for the advisor prompt — a
    next-best stand-in for the 10-year-history check that personas
    like Buffett/Graham/Wood apply. Decimal precision (rather than
    int-truncating to whole years) lets the LLM tell a fresh IPO
    (0.3y) apart from a 2-year-old listing (2.0y) without coarse
    bucketing.
    """
    if not date_iso:
        return None
    try:
        listed = datetime.strptime(date_iso[:10], "%Y-%m-%d")
    except ValueError:
        return None
    if asof is None:
        asof = datetime.now(timezone.utc).replace(tzinfo=None)
    elif asof.tzinfo is not None:
        asof = asof.astimezone(timezone.utc).replace(tzinfo=None)
    delta = asof - listed
    if delta.days < 0:
        return None
    return round(delta.days / 365.25, 2)


def _to_float(value: Any) -> float | None:
    """Coerce a pandas / numpy / native value to a JSON-safe float.

    Returns ``None`` for NaN, infinity, the literal ``False``
    (akshare uses ``False`` as a missing-data placeholder in some
    DataFrames), or anything that won't parse as a number.
    """
    if value is None or value is False:
        return None
    if isinstance(value, str):
        s = value.strip().rstrip("%")
        if not s or s in ("--", "False", "None", "nan"):
            return None
        try:
            f = float(s)
        except ValueError:
            return None
    else:
        try:
            f = float(value)
        except (TypeError, ValueError):
            return None
    if math.isnan(f) or math.isinf(f):
        return None
    return f


def _pct_to_frac(value: Any) -> float | None:
    """akshare returns financial-statement percentages as bare numbers
    or as strings with a trailing ``%`` (``18.5`` or ``"18.5%"`` both
    mean 18.5%). The Go formatter multiplies our value by 100 for
    display, so we hand back decimal fractions (0.185 for 18.5%).

    Values that already look like decimals (|x| < 1 and no ``%``
    suffix in the source string) are passed through unchanged. This
    keeps the converter idempotent against any future akshare change
    to a decimal representation.
    """
    if isinstance(value, str) and value.strip().endswith("%"):
        # Source explicitly marks percent — always divide by 100.
        f = _to_float(value)
        return None if f is None else f / 100.0
    f = _to_float(value)
    if f is None:
        return None
    return f / 100.0 if abs(f) > 1 else f


def _row_to_dict(row: Any) -> dict[str, Any]:
    """Best-effort conversion of a pandas Series / dict-like row to a
    plain dict so look-ups by Chinese key are uniform across DataFrame
    backends (pandas 1.x vs 2.x vs 3.x).
    """
    try:
        return dict(row)
    except Exception:
        try:
            return row.to_dict()
        except Exception:
            return {}


def _latest_annual_row(df: Any, date_col: str) -> Any | None:
    """Return the most recent annual report row from a DataFrame
    indexed by report date in ``date_col``.

    "Annual" means the date string ends in ``12-31`` (full-year
    report). When the source contains only interim periods
    (e.g. a newly-listed company), we fall back to the lexicographically
    latest row, which under akshare's ``yyyy-MM-dd`` formatting is also
    the most recent period.

    Used for **absolute rate** indicators (ROE, profit margin,
    operating margin). Chinese A-share quarterly reports are
    typically YTD cumulative — Q1 ROE of 1.14% does NOT mean an
    annualized 1.14%, it means 1.14% earned in the first quarter
    alone. Handing that figure to a value-investing persona that
    expects "long-run ROE ≥ 15%" would produce nonsense verdicts.

    Do NOT use this for YoY growth rates — those are comparable
    period-over-period (Q1 vs Q1, FY vs FY) and dropping the latest
    quarter throws away the most timely turnaround / acceleration
    signal. Use ``_latest_period_row`` for those.
    """
    if df is None or df.empty or date_col not in df.columns:
        return None
    # Normalize the date column to string so endswith / max work.
    dates = df[date_col].astype(str)
    annual = df[dates.str.endswith("12-31")]
    if not annual.empty:
        # `dates` is the parallel string view; argmax on the filtered
        # subset would re-index, so just sort the survivors.
        annual_sorted = annual.sort_values(by=date_col)
        return annual_sorted.iloc[-1]
    # Fall back: latest row of any period, sorted lexicographically
    # by the date column.
    sorted_df = df.sort_values(by=date_col)
    return sorted_df.iloc[-1]


def _latest_period_row(df: Any, date_col: str) -> Any | None:
    """Return the most recent row of *any* fiscal period
    (annual or interim) from a DataFrame indexed by ``date_col``.

    Used for YoY growth metrics (revenue_growth, earnings_growth)
    where quarterly data is the most timely signal — a stock whose
    last annual print was -28% but whose latest Q1 print is +35%
    has just turned the corner, and the LLM needs to see both to
    reason about whether this is a one-off or a regime change.

    The Chinese akshare endpoints sort either way (sina ascending,
    THS descending) and may carry duplicate snapshots of the same
    period from different filings; we deduplicate by date and pick
    the lexicographic max, which under ``yyyy-MM-dd`` is the
    chronologically latest.
    """
    if df is None or df.empty or date_col not in df.columns:
        return None
    sorted_df = df.sort_values(by=date_col)
    return sorted_df.iloc[-1]


def _latest_analysis_indicator(symbol: str) -> dict[str, float]:
    """ROE / margins / growth / cash flow from
    ``stock_financial_analysis_indicator``.

    Returns up to 86 columns of historical financial ratios sorted by
    report date ascending. We take the LAST row (most recent period)
    and map the Chinese column names onto the canonical English keys
    the Go parser recognizes. Values in the source are already in
    percent units (e.g. ``27.13`` meaning 27.13%), so ``_pct_to_frac``
    divides by 100 before returning.
    """
    out: dict[str, float] = {}
    try:
        # start_year scopes the upstream query; "2020" is recent enough
        # to keep payloads small while still giving the analysis layer
        # a few periods to validate trajectory (rev growth, ROE trend).
        df = ak.stock_financial_analysis_indicator(symbol=symbol, start_year="2020")
    except Exception as exc:
        log.warning("stock_financial_analysis_indicator failed for %s: %s", symbol, exc)
        return out
    if df is None or df.empty:
        return out

    # ---- Annual snapshot (for absolute rates: ROE, margins) -------------
    annual = _latest_annual_row(df, "日期")
    if annual is None:
        return out
    annual_row = _row_to_dict(annual)
    out["annual_period"] = str(annual_row.get("日期", "")).strip()

    roe = _pct_to_frac(annual_row.get("净资产收益率(%)"))
    if roe is None:
        roe = _pct_to_frac(annual_row.get("加权净资产收益率(%)"))
    if roe is not None:
        out["roe"] = roe

    margin = _pct_to_frac(annual_row.get("销售净利率(%)"))
    if margin is not None:
        out["profit_margin"] = margin

    op_margin = _pct_to_frac(annual_row.get("营业利润率(%)"))
    if op_margin is not None:
        out["operating_margin"] = op_margin

    # Annual YoY growth (kept under the legacy field name for
    # backward compat — Wood / Lynch personas still look at this
    # for "is the long-run trend exponential" judgements).
    rev_growth_annual = _pct_to_frac(annual_row.get("主营业务收入增长率(%)"))
    if rev_growth_annual is not None:
        out["revenue_growth"] = rev_growth_annual

    earnings_growth_annual = _pct_to_frac(annual_row.get("净利润增长率(%)"))
    if earnings_growth_annual is not None:
        out["earnings_growth"] = earnings_growth_annual

    # ---- Latest-period snapshot (for timely YoY growth) -----------------
    # If the latest period is itself the annual report we just used,
    # the two snapshots coincide and we don't need to ship redundant
    # ``_latest`` fields.
    latest = _latest_period_row(df, "日期")
    if latest is None:
        return out
    latest_row = _row_to_dict(latest)
    latest_period = str(latest_row.get("日期", "")).strip()
    if latest_period and latest_period != out.get("annual_period"):
        out["latest_period"] = latest_period
        rev_growth_latest = _pct_to_frac(latest_row.get("主营业务收入增长率(%)"))
        if rev_growth_latest is not None:
            out["revenue_growth_latest"] = rev_growth_latest
        earnings_growth_latest = _pct_to_frac(latest_row.get("净利润增长率(%)"))
        if earnings_growth_latest is not None:
            out["earnings_growth_latest"] = earnings_growth_latest

    return out


def _latest_yjbb_em(symbol: str, ref: datetime | None = None) -> dict[str, Any]:
    """Fetch the most recent 业绩快报 (corporate earnings flash) row
    for ``symbol`` via Eastmoney's ``stock_yjbb_em`` and project the
    fields the LLM prompt needs to produce *citable* numbers.

    Why this exists
    ---------------
    The other endpoints we already use (``stock_financial_analysis_indicator``,
    ``stock_financial_abstract_ths``) ship only RATIO fields — growth %,
    margins, ROE. A reviewer who wants to verify "+27.97% Q1 2026
    revenue growth" against the original company filing has no
    indexable anchor: no announce date, no absolute revenue figure, no
    QoQ delta to spot a momentum reversal. yjbb_em ships exactly that
    cluster of "verification metadata":

      * 营业总收入 (absolute CNY revenue)
      * 净利润 (absolute CNY net income)
      * 同比增长 / 季度环比增长 for both
      * 销售毛利率 (latest gross margin — the price-war detector)
      * 最新公告日期 (when the filing was published — the citation anchor)

    With these in the prompt the master agents are required (by
    rule 8 in master_agent.go) to cite the announce date when
    quoting any *_latest field, and the QoQ deltas surface
    momentum reversals (e.g. 688205 Q1 2026 YoY +27.97% but QoQ
    -9.63%, which is a meaningful trend signal).

    Caching strategy
    ----------------
    Each yjbb_em call downloads ~5000 rows for the full period. The
    FULL DataFrame is cached per period (24h TTL) and then filtered
    by symbol, so a busy advisor session pays the heavy upstream
    fetch at most once per period per day rather than per-symbol
    per-consult.

    Multi-period fallback
    ---------------------
    Filings appear on a known cadence (Q1 ≈ end of Apr, H1 ≈ end of
    Aug, Q3 ≈ end of Oct, FY ≈ end of Apr next year). We try the most
    recent expected period given the calendar date and fall back one
    period at a time, so a stock that hasn't filed Q1 yet still gets
    its Q4 numbers.
    """
    symbol = (symbol or "").strip()
    if not symbol:
        return {}

    now = ref or datetime.now(timezone.utc).replace(tzinfo=None)
    for period in _candidate_yjbb_periods(now):
        df = _yjbb_snapshot(period)
        if df is None or df.empty:
            continue
        # df is the full 5000-row period snapshot; filter to this
        # symbol. Skip rather than 'return empty' so a symbol that
        # hasn't filed for `period` yet falls through to the prior
        # period (cf. multi-period fallback).
        try:
            hit = df[df["股票代码"] == symbol]
        except (KeyError, TypeError):
            continue
        if hit.empty:
            continue
        row = _row_to_dict(hit.iloc[0])
        out: dict[str, Any] = {"latest_source": "eastmoney_yjbb"}
        period_iso = _normalize_iso_date(row.get("报告日") or row.get("报告期") or period)
        if period_iso:
            out["latest_period"] = period_iso
        rev = _to_float(row.get("营业总收入-营业总收入") or row.get("营业总收入"))
        if rev is not None:
            out["latest_revenue"] = rev
        ni = _to_float(row.get("净利润-净利润") or row.get("净利润"))
        if ni is not None:
            out["latest_net_income"] = ni
        # YoY: ratio fields here arrive as bare numbers in PERCENT
        # units (27.9698 = +27.97% YoY), not as the strings with a
        # trailing % we get from THS. _pct_to_frac handles both
        # shapes — divides by 100 when |x| > 1.
        rev_yoy = _pct_to_frac(row.get("营业总收入-同比增长"))
        if rev_yoy is not None:
            out["revenue_growth_latest"] = rev_yoy
        ni_yoy = _pct_to_frac(row.get("净利润-同比增长"))
        if ni_yoy is not None:
            out["earnings_growth_latest"] = ni_yoy
        rev_qoq = _pct_to_frac(row.get("营业总收入-季度环比增长"))
        if rev_qoq is not None:
            out["latest_revenue_qoq"] = rev_qoq
        ni_qoq = _pct_to_frac(row.get("净利润-季度环比增长"))
        if ni_qoq is not None:
            out["latest_net_income_qoq"] = ni_qoq
        gm = _pct_to_frac(row.get("销售毛利率"))
        if gm is not None:
            out["gross_margin_latest"] = gm
        announce = _normalize_iso_date(row.get("最新公告日期"))
        if announce:
            out["latest_announce_date"] = announce
        return out
    return {}


def _candidate_yjbb_periods(now: datetime) -> list[str]:
    """Return the list of `yyyyMMdd` period strings to probe for a
    业绩快报, ordered most-recent-likely-filed first.

    The CSRC reporting calendar is:
      - Q1 (0331) → filed by ~Apr 30
      - H1 (0630) → filed by ~Aug 31
      - Q3 (0930) → filed by ~Oct 31
      - FY (1231) → filed by ~Apr 30 next year

    We give each period a small grace window (the filing deadline
    may be exceeded by a few days) and walk back two prior periods,
    so a stock that hasn't filed the most recent one still has
    something to anchor on.
    """
    y = now.year
    # All quarterly endings within the trailing 18 months,
    # most-recent first.
    periods = [
        datetime(y, 3, 31),
        datetime(y - 1, 12, 31),
        datetime(y - 1, 9, 30),
        datetime(y - 1, 6, 30),
        datetime(y - 1, 3, 31),
    ]
    # Mid-year filers: insert H1 / Q3 / FY of the current year only
    # AFTER the CSRC filing deadline has passed, by which point the
    # bulk yjbb_em snapshot is meaningfully populated. Before the
    # deadline only a handful of companies have filed and the
    # snapshot is too sparse to be useful — fall through to the
    # prior period instead.
    #
    # Deadlines: H1 → Aug 31, Q3 → Oct 31, FY → Apr 30 (next year).
    # We add a small grace (1-2 days) past the deadline to let
    # Eastmoney catch up indexing.
    if now.month >= 9:  # H1 filed by Aug 31
        periods.insert(0, datetime(y, 6, 30))
    if now.month >= 11:  # Q3 filed by Oct 31
        periods.insert(0, datetime(y, 9, 30))
    # FY: filed by Apr 30 next year, so the current-year FY is only
    # reliably bulk-filed from May of *next* year — never insert it
    # into the current-year list (the prior-year FY already covers
    # it via the base periods list above).
    # Drop any periods strictly in the future relative to `now` —
    # those filings cannot exist yet.
    out = []
    for p in periods:
        if p <= now:
            out.append(p.strftime("%Y%m%d"))
    return out


# Per-period snapshot cache. Eastmoney's yjbb_em returns ~5000 rows
# (~500KB) per period, which we want to download at most once per
# period per day. Cache value is (DataFrame or None, cached_at).
_YJBB_CACHE_TTL_SECONDS = 24 * 3600
_yjbb_cache: dict[str, tuple[Any, float]] = {}
_yjbb_cache_lock = threading.Lock()


def _yjbb_snapshot(period: str) -> Any:
    """Return the cached yjbb DataFrame for ``period`` (yyyyMMdd),
    fetching once and serving subsequent calls from memory.

    Returns ``None`` on upstream failure (NOT cached so transient
    Eastmoney blips can self-heal on the next consult).
    """
    now = time.time()
    with _yjbb_cache_lock:
        cached = _yjbb_cache.get(period)
        if cached and (now - cached[1]) < _YJBB_CACHE_TTL_SECONDS:
            return cached[0]
    try:
        df = ak.stock_yjbb_em(date=period)
    except Exception as exc:  # noqa: BLE001
        log.warning("stock_yjbb_em failed for %s: %s", period, exc)
        return None
    with _yjbb_cache_lock:
        _yjbb_cache[period] = (df, now)
    return df


def _latest_abstract_ths(symbol: str) -> dict[str, float]:
    """Fallback / supplement using 同花顺's financial abstract.

    Returns the most recent period (row 0 — THS sorts descending) with
    values formatted as strings carrying a ``%`` suffix
    (``"18.5%"``). ``_pct_to_frac`` handles that shape; missing fields
    appear as the literal ``False`` and are silently skipped.
    """
    out: dict[str, float] = {}
    try:
        df = ak.stock_financial_abstract_ths(symbol=symbol, indicator="按报告期")
    except Exception as exc:
        log.warning("stock_financial_abstract_ths failed for %s: %s", symbol, exc)
        return out
    if df is None or df.empty:
        return out
    # Annual snapshot (absolute rates + annual YoY growth)
    annual = _latest_annual_row(df, "报告期")
    if annual is None:
        return out
    annual_row = _row_to_dict(annual)
    out["annual_period"] = str(annual_row.get("报告期", "")).strip()

    roe = _pct_to_frac(annual_row.get("净资产收益率"))
    if roe is None:
        roe = _pct_to_frac(annual_row.get("净资产收益率-摊薄"))
    if roe is not None:
        out["roe"] = roe

    margin = _pct_to_frac(annual_row.get("销售净利率"))
    if margin is not None:
        out["profit_margin"] = margin

    rev = _pct_to_frac(annual_row.get("营业总收入同比增长率"))
    if rev is not None:
        out["revenue_growth"] = rev

    eg = _pct_to_frac(annual_row.get("净利润同比增长率"))
    if eg is not None:
        out["earnings_growth"] = eg

    # Latest interim snapshot — adds the most timely YoY signal when
    # the company has reported a quarter past the last annual.
    latest = _latest_period_row(df, "报告期")
    if latest is None:
        return out
    latest_row = _row_to_dict(latest)
    latest_period = str(latest_row.get("报告期", "")).strip()
    if latest_period and latest_period != out.get("annual_period"):
        out["latest_period"] = latest_period
        rev_l = _pct_to_frac(latest_row.get("营业总收入同比增长率"))
        if rev_l is not None:
            out["revenue_growth_latest"] = rev_l
        eg_l = _pct_to_frac(latest_row.get("净利润同比增长率"))
        if eg_l is not None:
            out["earnings_growth_latest"] = eg_l
    return out


def fetch_fundamental(symbol: str, market: str) -> dict[str, Any] | None:
    """Return a JSON-serializable dict for the Go side, or ``None``
    when nothing meaningful is available (callers reply 404 so the Go
    provider treats it as ``ErrNoData`` and falls through to the next
    provider in the chain).

    Sources are queried in order of reliability:
      1. ``stock_financial_analysis_indicator`` — primary, numeric.
      2. ``stock_financial_abstract_ths`` — fills gaps the first call
         left behind (some symbols have spotty analysis-indicator
         coverage; THS tends to fill those).
    """
    symbol = (symbol or "").strip()
    if not symbol:
        return None
    market = (market or "a_share").strip().lower()
    if market not in ("a_share", "cn", "china"):
        return None

    metrics: dict[str, Any] = {"symbol": symbol, "currency": "CNY"}
    name = lookup_name(symbol)
    if name:
        metrics["name"] = name
    # Company tenure — emitted as both the raw ISO listing date and a
    # decimal-year tenure so the LLM prompt can apply rule 7 (don't
    # penalise sub-10-year listings as "history.10yr data_unavailable")
    # without having to do the date math itself.
    listing_date = lookup_listing_date(symbol)
    if listing_date:
        metrics["listing_date"] = listing_date
        years = _years_since(listing_date)
        if years is not None:
            metrics["listing_years"] = years
    metrics.update(_latest_analysis_indicator(symbol))

    # Citation metadata — 业绩快报 is the only Akshare source that
    # ships the announce date + absolute revenue/net income alongside
    # the ratios. Surfaces three signals the other sources don't:
    #   1. anchor date for external verification (rule 8 in
    #      master_agent.go forces the LLM to quote it)
    #   2. absolute CNY revenue / net income so the prompt can show
    #      its work, not just the derived %
    #   3. QoQ (季度环比) deltas — early-warning indicator for
    #      momentum reversals that pure YoY can't surface
    # Use setdefault so the existing _latest_period / *_latest fields
    # from the analysis indicator win when both sources cover them;
    # yjbb only fills the gaps (announce_date, absolutes, QoQ, gross
    # margin) it uniquely provides.
    for key, value in _latest_yjbb_em(symbol).items():
        metrics.setdefault(key, value)

    # Only call THS if we're missing key fields; saves an upstream hit
    # for the common case where the analysis indicator covers
    # everything.
    if not all(key in metrics for key in ("roe", "profit_margin", "revenue_growth")):
        for key, value in _latest_abstract_ths(symbol).items():
            metrics.setdefault(key, value)

    if not any(key in metrics for key in ("roe", "profit_margin", "revenue_growth", "earnings_growth")):
        # Even if no metrics, return the name when we have one — it's
        # still useful for the UI header and the LLM prompt. The Go
        # provider treats an empty-metrics row as ErrNoData regardless,
        # so this only matters when *some* metric DOES land.
        return None
    return metrics


@app.route("/health", methods=["GET"])
def health() -> tuple:
    return (
        jsonify({"status": "ok", "ts": datetime.now(timezone.utc).isoformat()}),
        200,
    )


@app.route("/api/fundamental", methods=["GET"])
@app.route("/fundamental", methods=["GET"])
@app.route("/api/key_metrics", methods=["GET"])
@app.route("/key_metrics", methods=["GET"])
def fundamental() -> tuple:
    symbol = request.args.get("symbol", "")
    market = request.args.get("market", "a_share")
    try:
        data = fetch_fundamental(symbol, market)
    except Exception as exc:  # noqa: BLE001 — log + 502 is intentional
        log.exception("fundamental fetch failed symbol=%s market=%s", symbol, market)
        return jsonify({"error": str(exc)}), 502
    if data is None:
        return jsonify({"data": {}}), 404
    log.info(
        "served fundamental symbol=%s market=%s keys=%s",
        symbol, market, sorted(data.keys()),
    )
    return jsonify({"data": data}), 200


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8001"))
    app.run(host="0.0.0.0", port=port)
