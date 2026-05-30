/**
 * BenchmarkMiniChart — Android RN 卡片，挂在 HomeScreen 选中 fund 下方。
 *
 * 用 react-native-svg 画一个最小可用的"基金 vs 基准"折线图：
 *
 *  - 调 GET /api/funds/:fundId/benchmark-history?days=N（不传 series，让
 *    服务端按 fund 的 universe 推荐默认基准）。
 *  - 取 fund 净值 + 第 1 个 benchmark，归一化已经在服务端做完，前端只
 *    负责把 [0, 1] 区间映射到 SVG 坐标系。
 *  - 提供 30d / 90d / 1y 三个 range button，picker 暂留给 Web。
 *  - 折叠默认 false，避免没启动 ohlc 的部署在首屏闪空状态。
 *
 * 与 Web 端的 BenchmarkChart.tsx 一一对应；文案存在
 * shared/api-client/src/i18n.ts 的 benchmark 段。
 */

import React, { useMemo, useState } from 'react';
import {
  ActivityIndicator,
  Pressable,
  StyleSheet,
  Text,
  View,
} from 'react-native';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import Svg, { G, Line, Polyline, Rect, Text as SvgText } from 'react-native-svg';
import type {
  BenchmarkHistoryResponse,
  BenchmarkSeries,
  BenchmarkHoldingOverlap,
} from '@fundai/api-client';

import { apiClient } from '../lib/api';

interface Props {
  fundId: string;
  defaultOpen?: boolean;
}

interface RangeOption {
  id: string;
  days: number;
  i18nKey: string;
}

const RANGE_OPTIONS: RangeOption[] = [
  { id: '30', days: 30, i18nKey: 'benchmark.days30' },
  { id: '90', days: 90, i18nKey: 'benchmark.days90' },
  { id: '365', days: 365, i18nKey: 'benchmark.days365' },
];

// Drawing constants. Width is filled by the parent via flex; we
// pre-allocate a height that matches CorpActionTimelineCard's
// summary block so the home list stays visually balanced.
const CHART_HEIGHT = 160;
const CHART_HORIZONTAL_PADDING = 12;
const CHART_TOP_PADDING = 12;
const CHART_BOTTOM_PADDING = 24;

// Colors picked to match the Web palette — fund line in indigo,
// benchmarks in sky / emerald rotation.
const COLOR_FUND = '#4f46e5';
const COLOR_BENCHMARK = '#0ea5e9';
const COLOR_GRID = '#e5e7eb';
const COLOR_AXIS_LABEL = '#6b7280';

/**
 * HoldingOverlapBanner — RN counterpart to the Web BenchmarkChart
 * banner. Surfaces the server's `holdingOverlap` hint when the
 * fund's positions structurally overlap one of the rendered
 * benchmarks (e.g., a futures fund whose only holding is BTCUSDT
 * while the benchmark line is btc_usdt). In Compare mode the two
 * curves would track each other almost perfectly — uninformative —
 * so we render a small heads-up note. We do NOT have a "switch
 * to Alpha" button on Android because the mini-chart only shows
 * one benchmark line and doesn't expose a Compare/Alpha toggle;
 * the banner's job is just to explain why the lines overlap so
 * the user doesn't think the chart is broken.
 */
function HoldingOverlapBanner({
  overlap,
}: {
  overlap: BenchmarkHoldingOverlap;
}): JSX.Element | null {
  const { t } = useTranslation();
  if (!overlap || !overlap.primaryBenchmark) return null;
  const dominant = overlap.overlapStrength === 'dominant';
  const partial = overlap.overlapStrength === 'partial';
  if (!dominant && !partial) return null;

  // Use the benchmark id as a stable label fallback. The Web
  // version resolves the benchmark.label by walking the rendered
  // list; the RN version keeps that resolution at the parent
  // (we'd need to thread `benchmarks` down). The id is stable and
  // operator-recognisable enough for this small banner.
  const matched = (overlap.matchedSymbols ?? []).join(', ');
  const titleKey = dominant
    ? 'benchmark.holdingOverlapDominantTitle'
    : 'benchmark.holdingOverlapPartialTitle';
  const bodyKey = dominant
    ? 'benchmark.holdingOverlapDominantBody'
    : 'benchmark.holdingOverlapPartialBody';

  return (
    <View
      style={[
        bannerStyles.container,
        dominant ? bannerStyles.containerDominant : bannerStyles.containerPartial,
      ]}
    >
      <Text
        style={[
          bannerStyles.title,
          dominant ? bannerStyles.titleDominant : bannerStyles.titlePartial,
        ]}
      >
        {t(titleKey)}
      </Text>
      <Text
        style={[
          bannerStyles.body,
          dominant ? bannerStyles.bodyDominant : bannerStyles.bodyPartial,
        ]}
      >
        {t(bodyKey)}
        {matched ? `\n(${overlap.primaryBenchmark} ↔ ${matched})` : ''}
      </Text>
    </View>
  );
}

const bannerStyles = StyleSheet.create({
  container: {
    borderWidth: 1,
    borderRadius: 8,
    paddingHorizontal: 10,
    paddingVertical: 8,
    marginBottom: 10,
  },
  containerDominant: {
    borderColor: '#c7d2fe',
    backgroundColor: '#eef2ff',
  },
  containerPartial: {
    borderColor: '#fde68a',
    backgroundColor: '#fffbeb',
  },
  title: {
    fontSize: 12,
    fontWeight: '700',
    marginBottom: 2,
  },
  titleDominant: {
    color: '#3730a3',
  },
  titlePartial: {
    color: '#92400e',
  },
  body: {
    fontSize: 11,
    lineHeight: 15,
  },
  bodyDominant: {
    color: '#312e81',
  },
  bodyPartial: {
    color: '#78350f',
  },
});

/** Map a series' (date, value) sequence into normalized [0, 1] x/y
 *  coords. The benchmark API has already rebased values to 100 at
 *  start, so we just compute min/max across the dataset for the
 *  Y-axis scaling. */
function normalizeForSvg(
  series: BenchmarkSeries[],
  width: number,
  height: number,
): { lines: Array<{ id: string; color: string; points: string }>; yMin: number; yMax: number } {
  // Build a unified date axis so two series share the same x-mapping.
  const allDates = new Set<string>();
  for (const s of series) {
    for (const p of s.points) allDates.add(p.date);
  }
  const sortedDates = Array.from(allDates).sort();
  if (sortedDates.length < 2) {
    return { lines: [], yMin: 0, yMax: 0 };
  }
  const dateIndex = new Map(sortedDates.map((d, i) => [d, i] as const));
  // Pad y-range by 1% so the line doesn't kiss the chart border.
  let yMin = Infinity;
  let yMax = -Infinity;
  for (const s of series) {
    for (const p of s.points) {
      if (p.value < yMin) yMin = p.value;
      if (p.value > yMax) yMax = p.value;
    }
  }
  if (!Number.isFinite(yMin) || !Number.isFinite(yMax) || yMin === yMax) {
    yMin = (yMin || 100) - 1;
    yMax = (yMax || 100) + 1;
  } else {
    const span = yMax - yMin;
    yMin -= span * 0.05;
    yMax += span * 0.05;
  }

  const innerWidth = width - CHART_HORIZONTAL_PADDING * 2;
  const innerHeight = height - CHART_TOP_PADDING - CHART_BOTTOM_PADDING;

  const lines = series.map((s, i) => {
    const color = i === 0 ? COLOR_FUND : COLOR_BENCHMARK;
    const points = s.points
      .map((p) => {
        const xIdx = dateIndex.get(p.date);
        if (xIdx === undefined) return null;
        const x = CHART_HORIZONTAL_PADDING + (xIdx / (sortedDates.length - 1)) * innerWidth;
        const yNorm = (p.value - yMin) / (yMax - yMin);
        const y = CHART_TOP_PADDING + (1 - yNorm) * innerHeight;
        return `${x.toFixed(2)},${y.toFixed(2)}`;
      })
      .filter((v): v is string => v !== null)
      .join(' ');
    return { id: s.id, color, points };
  });

  return { lines, yMin, yMax };
}

export function BenchmarkMiniChart({ fundId, defaultOpen = false }: Props): JSX.Element {
  const { t } = useTranslation();
  const [open, setOpen] = useState(defaultOpen);
  const [range, setRange] = useState<RangeOption>(RANGE_OPTIONS[1]); // 90d default
  const [chartWidth, setChartWidth] = useState(320);

  // Lazy fetch — enabled only when expanded. react-query keeps the
  // last response for 5min, matching CorpActionTimelineCard.
  const { data, isLoading, isError, refetch, isFetching } = useQuery<
    BenchmarkHistoryResponse,
    unknown
  >({
    queryKey: ['benchmark-history', fundId, range.days],
    queryFn: () => apiClient.getBenchmarkHistory(fundId, range.days),
    enabled: open,
    staleTime: 5 * 60 * 1000,
    retry: 1,
  });

  // We render at most fund + 1 benchmark on Android — the small
  // screen makes anything denser unreadable. Picker stays Web-only
  // for now; on RN the user gets the server's recommended[0].
  const seriesToRender = useMemo<BenchmarkSeries[]>(() => {
    if (!data) return [];
    const out: BenchmarkSeries[] = [data.fund];
    if (data.benchmarks.length > 0) {
      out.push(data.benchmarks[0]);
    }
    return out;
  }, [data]);

  const svgGeometry = useMemo(
    () => normalizeForSvg(seriesToRender, chartWidth, CHART_HEIGHT),
    [seriesToRender, chartWidth],
  );

  return (
    <View style={styles.card}>
      <Pressable onPress={() => setOpen((v) => !v)} style={styles.header}>
        <View style={{ flex: 1 }}>
          <Text style={styles.title}>{t('benchmark.title')}</Text>
          <Text style={styles.subtitle}>{t('benchmark.subtitle')}</Text>
        </View>
        <Text style={styles.expandLabel}>
          {open ? t('benchmark.collapse') : t('benchmark.expand')}
        </Text>
      </Pressable>

      {open ? (
        <View style={styles.body}>
          <View style={styles.rangeRow}>
            {RANGE_OPTIONS.map((opt) => {
              const active = opt.id === range.id;
              return (
                <Pressable
                  key={opt.id}
                  onPress={() => setRange(opt)}
                  style={[styles.rangePill, active && styles.rangePillActive]}
                >
                  <Text
                    style={[
                      styles.rangePillText,
                      active && styles.rangePillTextActive,
                    ]}
                  >
                    {t(opt.i18nKey)}
                  </Text>
                </Pressable>
              );
            })}
          </View>

          {isLoading || isFetching ? (
            <View style={styles.statePane}>
              <ActivityIndicator />
              <Text style={styles.stateText}>{t('benchmark.loading')}</Text>
            </View>
          ) : isError ? (
            <View style={styles.statePane}>
              <Text style={[styles.stateText, { color: '#b91c1c' }]}>
                {t('benchmark.error')}
              </Text>
              <Pressable onPress={() => refetch()} style={styles.retryBtn}>
                <Text style={styles.retryText}>{t('benchmark.retry')}</Text>
              </Pressable>
            </View>
          ) : !data || data.fund.points.length < 2 ? (
            <View style={styles.statePane}>
              <Text style={styles.stateText}>{t('benchmark.empty')}</Text>
            </View>
          ) : (
            <View
              onLayout={(e) => setChartWidth(e.nativeEvent.layout.width)}
              style={styles.chartHost}
            >
              {data.holdingOverlap ? (
                <HoldingOverlapBanner overlap={data.holdingOverlap} />
              ) : null}
              <Svg width={chartWidth} height={CHART_HEIGHT}>
                {/* Background card so the chart reads as a panel even
                    on dark themes (RN doesn't have CSS-style rules). */}
                <Rect
                  x={0}
                  y={0}
                  width={chartWidth}
                  height={CHART_HEIGHT}
                  fill="transparent"
                />
                {/* Single horizontal grid at midpoint as a subtle
                    "100" reference. Multiple grid lines clutter at
                    this size. */}
                <G>
                  <Line
                    x1={CHART_HORIZONTAL_PADDING}
                    x2={chartWidth - CHART_HORIZONTAL_PADDING}
                    y1={CHART_TOP_PADDING + (CHART_HEIGHT - CHART_TOP_PADDING - CHART_BOTTOM_PADDING) / 2}
                    y2={CHART_TOP_PADDING + (CHART_HEIGHT - CHART_TOP_PADDING - CHART_BOTTOM_PADDING) / 2}
                    stroke={COLOR_GRID}
                    strokeWidth={1}
                    strokeDasharray="4,4"
                  />
                </G>
                {svgGeometry.lines.map((line) => (
                  <Polyline
                    key={line.id}
                    points={line.points}
                    stroke={line.color}
                    strokeWidth={line.id.startsWith('fund:') ? 2.5 : 1.5}
                    fill="none"
                  />
                ))}
                {/* Y-axis labels: top = max, bottom = min. We don't
                    draw a left axis line because it would compete
                    with the small chart real-estate. */}
                <SvgText
                  x={CHART_HORIZONTAL_PADDING}
                  y={CHART_TOP_PADDING - 2}
                  fontSize={10}
                  fill={COLOR_AXIS_LABEL}
                >
                  {svgGeometry.yMax.toFixed(1)}
                </SvgText>
                <SvgText
                  x={CHART_HORIZONTAL_PADDING}
                  y={CHART_HEIGHT - CHART_BOTTOM_PADDING + 12}
                  fontSize={10}
                  fill={COLOR_AXIS_LABEL}
                >
                  {svgGeometry.yMin.toFixed(1)}
                </SvgText>
                {/* X-axis: just date range bookends to keep things
                    legible on small screens. */}
                <SvgText
                  x={CHART_HORIZONTAL_PADDING}
                  y={CHART_HEIGHT - 4}
                  fontSize={10}
                  fill={COLOR_AXIS_LABEL}
                >
                  {data.from}
                </SvgText>
                <SvgText
                  x={chartWidth - CHART_HORIZONTAL_PADDING}
                  y={CHART_HEIGHT - 4}
                  fontSize={10}
                  fill={COLOR_AXIS_LABEL}
                  textAnchor="end"
                >
                  {data.to}
                </SvgText>
              </Svg>

              <View style={styles.legendRow}>
                <View style={styles.legendItem}>
                  <View style={[styles.legendSwatch, { backgroundColor: COLOR_FUND }]} />
                  <Text style={styles.legendLabel}>{t('benchmark.fund')}</Text>
                </View>
                {data.benchmarks[0] ? (
                  <View style={styles.legendItem}>
                    <View
                      style={[styles.legendSwatch, { backgroundColor: COLOR_BENCHMARK }]}
                    />
                    <Text style={styles.legendLabel}>
                      {data.benchmarks[0].label}
                    </Text>
                  </View>
                ) : null}
              </View>

              {data.partialFailures && data.partialFailures.length > 0 ? (
                <Text style={styles.partialFailureText}>
                  {t('benchmark.partialFailureToast')}
                </Text>
              ) : null}
            </View>
          )}
        </View>
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  card: {
    backgroundColor: '#fff',
    borderRadius: 12,
    borderWidth: 1,
    borderColor: '#e5e7eb',
    marginTop: 12,
    overflow: 'hidden',
  },
  header: {
    flexDirection: 'row',
    alignItems: 'center',
    paddingHorizontal: 14,
    paddingVertical: 12,
  },
  title: {
    fontSize: 13,
    fontWeight: '700',
    color: '#374151',
    letterSpacing: 0.4,
    textTransform: 'uppercase',
  },
  subtitle: {
    fontSize: 11,
    color: '#6b7280',
    marginTop: 2,
  },
  expandLabel: {
    fontSize: 12,
    color: '#4f46e5',
    fontWeight: '600',
  },
  body: {
    paddingHorizontal: 14,
    paddingBottom: 12,
  },
  rangeRow: {
    flexDirection: 'row',
    gap: 6,
    marginBottom: 8,
  },
  rangePill: {
    paddingVertical: 4,
    paddingHorizontal: 10,
    borderRadius: 999,
    borderWidth: 1,
    borderColor: '#e5e7eb',
    backgroundColor: '#f9fafb',
  },
  rangePillActive: {
    borderColor: '#4f46e5',
    backgroundColor: '#eef2ff',
  },
  rangePillText: {
    fontSize: 11,
    color: '#6b7280',
  },
  rangePillTextActive: {
    color: '#4f46e5',
    fontWeight: '600',
  },
  statePane: {
    paddingVertical: 28,
    alignItems: 'center',
    gap: 6,
  },
  stateText: {
    fontSize: 12,
    color: '#6b7280',
  },
  retryBtn: {
    marginTop: 6,
    backgroundColor: '#dc2626',
    paddingHorizontal: 12,
    paddingVertical: 6,
    borderRadius: 8,
  },
  retryText: {
    color: '#fff',
    fontSize: 12,
    fontWeight: '600',
  },
  chartHost: {
    width: '100%',
  },
  legendRow: {
    flexDirection: 'row',
    flexWrap: 'wrap',
    gap: 12,
    marginTop: 6,
  },
  legendItem: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 4,
  },
  legendSwatch: {
    width: 10,
    height: 10,
    borderRadius: 2,
  },
  legendLabel: {
    fontSize: 11,
    color: '#374151',
  },
  partialFailureText: {
    marginTop: 6,
    fontSize: 10,
    color: '#b45309',
  },
});

export default BenchmarkMiniChart;
