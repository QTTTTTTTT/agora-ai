/**
 * HoldingsTrendsGrid — Android RN counterpart to the Web component
 * `web/src/components/HoldingsTrendsGrid.tsx`. Renders a small
 * 2-column grid of mini sparklines, one per fund holding, fetched
 * from GET /api/funds/:fundId/holdings/series. Each card shows
 * symbol, name (when present), the cumulative %-change vs the
 * window start, and an SVG sparkline rebased to start = 100.
 *
 * Why a separate RN component (instead of reusing the Web one):
 *
 *   - Recharts (used by the Web grid) is dom-only and can't be
 *     compiled into RN. We draw the sparkline with react-native-svg
 *     using the same primitives as BenchmarkMiniChart so visual
 *     identity stays consistent.
 *   - The screen is narrower; we cap to 2 columns instead of 4 and
 *     give each card a fixed height so the FlatList virtualization
 *     can do its job without measuring every cell.
 *
 * Lazy-fetches when the panel is expanded so a fund with no
 * holdings (or a deployment without ohlc wired) costs zero
 * bandwidth. We also tolerate `partialFailures`: the server
 * returns a list of holding ids that couldn't be priced, and
 * we surface those as a small amber toast under the grid.
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
import Svg, { Polyline } from 'react-native-svg';
import type { HoldingSeries, HoldingsSeriesResponse } from '@fundai/api-client';

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
  { id: '30', days: 30, i18nKey: 'holdingsSeries.days30' },
  { id: '90', days: 90, i18nKey: 'holdingsSeries.days90' },
  { id: '180', days: 180, i18nKey: 'holdingsSeries.days180' },
];

// Mini-card geometry. Width is determined by 2-column grid in
// the parent; height is fixed so virtualization measures each
// item without re-measuring on data updates.
const CARD_HEIGHT = 96;
const SPARK_HEIGHT = 56;
const SPARK_PADDING_X = 4;
const SPARK_PADDING_Y = 4;

const COLOR_POSITIVE = '#10b981';
const COLOR_NEGATIVE = '#ef4444';

/**
 * sparklinePath computes a polyline points string from a series
 * already normalized to (x: 0..N-1, y: value). We rebase y to the
 * series' own min/max with a tiny padding so neither end touches
 * the card border.
 *
 * Returns null if the series has fewer than 2 points; the caller
 * renders the card without a chart in that case (one-day-old
 * positions, freshly added holdings, etc.).
 */
function sparklinePath(
  points: HoldingSeries['points'],
  width: number,
): { polyline: string; positive: boolean } | null {
  if (!points || points.length < 2) return null;
  let yMin = Infinity;
  let yMax = -Infinity;
  for (const p of points) {
    if (p.value < yMin) yMin = p.value;
    if (p.value > yMax) yMax = p.value;
  }
  if (!Number.isFinite(yMin) || !Number.isFinite(yMax) || yMin === yMax) {
    yMin = (yMin || 100) - 1;
    yMax = (yMax || 100) + 1;
  } else {
    const span = yMax - yMin;
    yMin -= span * 0.05;
    yMax += span * 0.05;
  }
  const innerWidth = Math.max(width - SPARK_PADDING_X * 2, 0);
  const innerHeight = SPARK_HEIGHT - SPARK_PADDING_Y * 2;
  const last = points[points.length - 1].value;
  const positive = last >= 100;
  const segments = points
    .map((p, i) => {
      const x =
        SPARK_PADDING_X +
        (i / (points.length - 1)) * innerWidth;
      const yNorm = (p.value - yMin) / (yMax - yMin);
      const y = SPARK_PADDING_Y + (1 - yNorm) * innerHeight;
      return `${x.toFixed(2)},${y.toFixed(2)}`;
    })
    .join(' ');
  return { polyline: segments, positive };
}

/**
 * HoldingMiniCard — single grid cell rendering one holding.
 * Pure presentational (no fetching of its own); the parent feeds
 * it a fully-loaded HoldingSeries DTO.
 *
 * Layout: top row = symbol (bold) + delta-vs-start (positive
 * green / negative rose). Bottom = sparkline. The card sits
 * inside a flexBasis: '48%' wrapper in the parent so two cards
 * fit per row with a small gap between.
 */
function HoldingMiniCard({
  series,
  cardWidth,
}: {
  series: HoldingSeries;
  cardWidth: number;
}): JSX.Element {
  const { t } = useTranslation();
  const last = series.points.length > 0
    ? series.points[series.points.length - 1].value
    : 100;
  const deltaVsStart = last - 100;
  const positive = deltaVsStart >= 0;
  const path = useMemo(
    () => sparklinePath(series.points, cardWidth),
    [series.points, cardWidth],
  );

  return (
    <View style={[styles.card, { width: cardWidth }]}>
      <View style={styles.cardHeader}>
        <View style={{ flex: 1 }}>
          <Text style={styles.cardSymbol} numberOfLines={1}>
            {series.symbol}
          </Text>
          {series.name ? (
            <Text style={styles.cardName} numberOfLines={1}>
              {series.name}
            </Text>
          ) : null}
        </View>
        <View style={styles.cardDeltaWrap}>
          <Text
            style={[
              styles.cardDelta,
              { color: positive ? COLOR_POSITIVE : COLOR_NEGATIVE },
            ]}
          >
            {positive ? '+' : ''}
            {deltaVsStart.toFixed(2)}
          </Text>
          <Text style={styles.cardDeltaSuffix}>
            {t('holdingsSeries.vsStart')}
          </Text>
        </View>
      </View>
      <View style={styles.cardSparkHost}>
        {path ? (
          <Svg width={cardWidth} height={SPARK_HEIGHT}>
            <Polyline
              points={path.polyline}
              stroke={path.positive ? COLOR_POSITIVE : COLOR_NEGATIVE}
              strokeWidth={1.5}
              fill="none"
            />
          </Svg>
        ) : (
          <View style={styles.cardSparkEmpty}>
            <Text style={styles.cardSparkEmptyText}>—</Text>
          </View>
        )}
      </View>
    </View>
  );
}

/**
 * HoldingsTrendsGrid — main exported component. Mirrors the web
 * component's contract: collapsible, range pills (30 / 90 / 180),
 * lazy fetch, partial-failure toast under the grid.
 */
export function HoldingsTrendsGrid({
  fundId,
  defaultOpen = false,
}: Props): JSX.Element {
  const { t } = useTranslation();
  const [open, setOpen] = useState(defaultOpen);
  const [range, setRange] = useState<RangeOption>(RANGE_OPTIONS[1]); // 90d
  const [containerWidth, setContainerWidth] = useState(320);

  const { data, isLoading, isFetching, isError, refetch } = useQuery<
    HoldingsSeriesResponse,
    unknown
  >({
    queryKey: ['holdings-series', fundId, range.days],
    queryFn: () => apiClient.getHoldingsSeries(fundId, range.days),
    enabled: open,
    staleTime: 5 * 60 * 1000,
    retry: 1,
  });

  // We only render holdings with ≥ 2 points (otherwise the
  // sparkline degenerates to a single dot, which is uglier than
  // an em-dash placeholder).
  const charts = useMemo(() => {
    if (!data) return [];
    return (data.items ?? []).filter((s) => s.points.length >= 2);
  }, [data]);

  // 2-column grid math: card = (container − 2*outerPadding − gap) / 2.
  // We pre-compute it here so HoldingMiniCard doesn't have to.
  const GRID_GAP = 8;
  const OUTER_PADDING = 14;
  const cardWidth = Math.max(
    Math.floor((containerWidth - OUTER_PADDING * 2 - GRID_GAP) / 2),
    100,
  );

  return (
    <View style={styles.outer}>
      <Pressable onPress={() => setOpen((v) => !v)} style={styles.header}>
        <View style={{ flex: 1 }}>
          <Text style={styles.title}>{t('holdingsSeries.title')}</Text>
          <Text style={styles.subtitle}>{t('holdingsSeries.subtitle')}</Text>
        </View>
        <Text style={styles.expandLabel}>
          {open ? t('holdingsSeries.collapse') : t('holdingsSeries.expand')}
        </Text>
      </Pressable>

      {open ? (
        <View
          onLayout={(e) => setContainerWidth(e.nativeEvent.layout.width)}
          style={styles.body}
        >
          <View style={styles.rangeRow}>
            {RANGE_OPTIONS.map((opt) => {
              const active = opt.id === range.id;
              return (
                <Pressable
                  key={opt.id}
                  onPress={() => setRange(opt)}
                  style={[
                    styles.rangePill,
                    active && styles.rangePillActive,
                  ]}
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
              <Text style={styles.stateText}>
                {t('holdingsSeries.loading')}
              </Text>
            </View>
          ) : isError ? (
            <View style={styles.statePane}>
              <Text style={[styles.stateText, { color: '#b91c1c' }]}>
                {t('holdingsSeries.error')}
              </Text>
              <Pressable onPress={() => refetch()} style={styles.retryBtn}>
                <Text style={styles.retryText}>
                  {t('holdingsSeries.retry')}
                </Text>
              </Pressable>
            </View>
          ) : charts.length === 0 ? (
            <View style={styles.statePane}>
              <Text style={styles.stateText}>
                {t('holdingsSeries.empty')}
              </Text>
            </View>
          ) : (
            <View style={styles.grid}>
              {charts.map((s) => (
                <HoldingMiniCard
                  key={s.instrumentKey}
                  series={s}
                  cardWidth={cardWidth}
                />
              ))}
              {data?.partialFailures && data.partialFailures.length > 0 ? (
                <Text style={styles.partialFailureText}>
                  {t('holdingsSeries.partialFailureToast')}
                  {': '}
                  {data.partialFailures.map((f) => f.id).join(', ')}
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
  outer: {
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
    paddingBottom: 14,
  },
  rangeRow: {
    flexDirection: 'row',
    gap: 6,
    marginBottom: 10,
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
  grid: {
    flexDirection: 'row',
    flexWrap: 'wrap',
    gap: 8,
  },
  card: {
    height: CARD_HEIGHT,
    borderRadius: 10,
    borderWidth: 1,
    borderColor: '#e5e7eb',
    backgroundColor: '#fff',
    paddingHorizontal: 8,
    paddingTop: 6,
    paddingBottom: 4,
  },
  cardHeader: {
    flexDirection: 'row',
    alignItems: 'flex-start',
  },
  cardSymbol: {
    fontSize: 12,
    fontWeight: '700',
    color: '#1f2937',
  },
  cardName: {
    fontSize: 10,
    color: '#6b7280',
    marginTop: 1,
  },
  cardDeltaWrap: {
    alignItems: 'flex-end',
    marginLeft: 4,
  },
  cardDelta: {
    fontSize: 12,
    fontWeight: '700',
  },
  cardDeltaSuffix: {
    fontSize: 9,
    color: '#9ca3af',
    marginTop: 1,
  },
  cardSparkHost: {
    marginTop: 4,
  },
  cardSparkEmpty: {
    height: SPARK_HEIGHT,
    alignItems: 'center',
    justifyContent: 'center',
  },
  cardSparkEmptyText: {
    color: '#d1d5db',
    fontSize: 16,
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
  partialFailureText: {
    width: '100%',
    marginTop: 6,
    fontSize: 10,
    color: '#b45309',
  },
});

export default HoldingsTrendsGrid;
