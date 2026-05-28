#!/usr/bin/env bash
# =============================================================================
# scripts/fetch-yahoo-ohlc.sh — pull daily OHLC CSVs (NASDAQ.com API)
# =============================================================================
#
# G1 #3 companion: builds a real-data fixture for `cmd/factorlab/`.
# The script tries Yahoo's keyless v8/chart endpoint FIRST and
# falls back to NASDAQ.com's public API when Yahoo rate-limits
# this IP (which it does aggressively for any caller hitting the
# endpoint more than a few times per minute from the same /24).
#
# Output: one CSV per symbol with header
#   date,open,high,low,close,volume
# Benchmark goes to `_benchmark/{BENCH}.csv`.
#
# Usage:
#   ./scripts/fetch-yahoo-ohlc.sh                       # default 20 US large-caps
#   ./scripts/fetch-yahoo-ohlc.sh --out /tmp/fixture    # custom output dir
#   ./scripts/fetch-yahoo-ohlc.sh --years 3             # 3-year window
#   ./scripts/fetch-yahoo-ohlc.sh --symbols AAPL,MSFT   # custom universe
#
# Requires: curl + python3.
# =============================================================================
set -euo pipefail

OUTDIR="${PWD}/testdata/factorlab/us_largecap"
YEARS=2
BENCH="SPY"
SYMBOLS="AAPL,MSFT,NVDA,GOOGL,META,AMZN,TSLA,JPM,XOM,JNJ,V,PG,UNH,HD,MA,KO,PEP,WMT,DIS,COST"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out) OUTDIR="$2"; shift 2 ;;
    --years) YEARS="$2"; shift 2 ;;
    --symbols) SYMBOLS="$2"; shift 2 ;;
    --bench) BENCH="$2"; shift 2 ;;
    --help|-h)
      sed -n '2,25p' "$0"
      exit 0
      ;;
    *) echo "unknown flag $1" >&2; exit 2 ;;
  esac
done

mkdir -p "$OUTDIR" "$OUTDIR/_benchmark"

UA='Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123 Safari/537.36'
RANGE="${YEARS}y"
FROM=$(date -u -v-"${YEARS}"y +%Y-%m-%d 2>/dev/null || date -u -d "${YEARS} years ago" +%Y-%m-%d)
TO=$(date -u +%Y-%m-%d)
LIMIT=$((YEARS * 260))

fetch_yahoo() {
  local sym="$1"
  local out="$2"
  local url="https://query1.finance.yahoo.com/v8/finance/chart/${sym}?range=${RANGE}&interval=1d"
  local tmp
  tmp=$(mktemp -t yfetch.XXXXXX)
  trap "rm -f $tmp" RETURN

  local http
  http=$(curl -sS -A "$UA" -o "$tmp" -w "%{http_code}" "$url" || echo 000)
  if [[ "$http" != "200" ]]; then
    return 1
  fi
  python3 - "$tmp" "$out" <<'PY' 2>/dev/null
import json, sys, datetime
src, dst = sys.argv[1], sys.argv[2]
with open(src) as f:
    data = json.load(f)
result = data.get('chart', {}).get('result', [])
if not result:
    sys.exit(2)
r = result[0]
ts = r.get('timestamp') or []
quote = (r.get('indicators', {}).get('quote') or [{}])[0]
opens = quote.get('open') or []
highs = quote.get('high') or []
lows = quote.get('low') or []
closes = quote.get('close') or []
vols = quote.get('volume') or []
with open(dst, 'w') as f:
    f.write("date,open,high,low,close,volume\n")
    n = 0
    for i, t in enumerate(ts):
        c = closes[i] if i < len(closes) else None
        if c is None:
            continue
        d = datetime.datetime.utcfromtimestamp(t).strftime('%Y-%m-%d')
        f.write(f"{d},{opens[i] or ''},{highs[i] or ''},{lows[i] or ''},{c},{int(vols[i] or 0)}\n")
        n += 1
print(n)
PY
}

fetch_nasdaq() {
  local sym="$1"
  local out="$2"
  # NASDAQ's public historical API. Same shape across all
  # tickers it knows about (US-listed common stock + ETFs).
  local url="https://api.nasdaq.com/api/quote/${sym}/historical?assetclass=stocks&fromdate=${FROM}&todate=${TO}&limit=${LIMIT}&offset=0"
  local tmp
  tmp=$(mktemp -t nfetch.XXXXXX)
  trap "rm -f $tmp" RETURN

  local http
  http=$(curl -sS -o "$tmp" -w "%{http_code}" "$url" \
    -H "User-Agent: $UA" -H "Accept: application/json" || echo 000)
  if [[ "$http" != "200" ]]; then
    # NASDAQ uses asset class 'etf' for SPY/QQQ etc.
    url="https://api.nasdaq.com/api/quote/${sym}/historical?assetclass=etf&fromdate=${FROM}&todate=${TO}&limit=${LIMIT}&offset=0"
    http=$(curl -sS -o "$tmp" -w "%{http_code}" "$url" \
      -H "User-Agent: $UA" -H "Accept: application/json" || echo 000)
    if [[ "$http" != "200" ]]; then
      return 1
    fi
  fi
  python3 - "$tmp" "$out" <<'PY' 2>/dev/null
import json, sys, datetime
src, dst = sys.argv[1], sys.argv[2]
with open(src) as f:
    data = json.load(f)
table = (data.get('data') or {}).get('tradesTable') or {}
rows = table.get('rows') or []
if not rows:
    sys.exit(2)
# Normalise "$308.82" / "43,670,220" / "05/22/2026" / "N/A"
def num(s):
    s = (s or '').replace('$','').replace(',','').strip()
    if s in ('', 'N/A'): return 0
    try: return float(s)
    except ValueError: return 0
def vol(s):
    s = (s or '').replace(',','').strip()
    if s in ('', 'N/A'): return 0
    try: return int(float(s))
    except ValueError: return 0
parsed = []
for r in rows:
    d = datetime.datetime.strptime(r['date'], '%m/%d/%Y').strftime('%Y-%m-%d')
    parsed.append((d, num(r.get('open','')), num(r.get('high','')),
                   num(r.get('low','')), num(r.get('close','')),
                   vol(r.get('volume',''))))
parsed.sort(key=lambda x: x[0])
with open(dst, 'w') as f:
    f.write("date,open,high,low,close,volume\n")
    for row in parsed:
        f.write("%s,%g,%g,%g,%g,%d\n" % row)
print(len(parsed))
PY
}

fetch_symbol() {
  local sym="$1"
  local out="$2"
  local rows
  if rows=$(fetch_yahoo "$sym" "$out") && [[ -n "$rows" && "$rows" != "0" ]]; then
    echo "yahoo  rows=${rows}"
    return 0
  fi
  if rows=$(fetch_nasdaq "$sym" "$out") && [[ -n "$rows" && "$rows" != "0" ]]; then
    echo "nasdaq rows=${rows}"
    return 0
  fi
  echo "FAIL"
  return 1
}

echo "fetching universe to ${OUTDIR}"
IFS=',' read -ra arr <<< "$SYMBOLS"
ok=0; skip=0
for sym in "${arr[@]}"; do
  sym=$(echo "$sym" | tr -d '[:space:]')
  [[ -z "$sym" ]] && continue
  printf "  %-8s " "$sym"
  if fetch_symbol "$sym" "${OUTDIR}/${sym}.csv"; then
    ok=$((ok+1))
  else
    skip=$((skip+1))
  fi
  sleep 0.5
done

echo
echo "fetching benchmark ${BENCH}"
printf "  %-8s " "$BENCH"
if fetch_symbol "$BENCH" "${OUTDIR}/_benchmark/${BENCH}.csv"; then
  echo "  benchmark OK"
fi

echo
echo "fixture written to ${OUTDIR}"
echo "  symbols fetched: ${ok}, skipped: ${skip}"
echo "  next step:  cd server && go run ./cmd/factorlab/ --fixture ${OUTDIR}"
