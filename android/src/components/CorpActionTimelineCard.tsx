/**
 * CorpActionTimelineCard — RN 卡片，挂在 HomeScreen 选中的 active fund
 * 下方，列出近期分红 / 拆股 / 配股事件。
 *
 * 数据来自 GET /api/funds/:fundId/corp-actions（fund-membership 已在
 * 服务端做了，401/403 会被全局 onUnauthorized 处理）。默认折叠，避免
 * 没事件的 fund 把 home list 撑得过长；展开后用 react-query 拉数据。
 *
 * 与 web 端的 CorpActionTimeline.tsx 一一对应；i18n 文案存在
 * shared/api-client/src/i18n.ts 的 corpActions 段，两端共用。
 */

import React, { useState } from 'react';
import {
  ActivityIndicator,
  Pressable,
  StyleSheet,
  Text,
  View,
} from 'react-native';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import type { CorpActionApplication } from '@fundai/api-client';

import { apiClient } from '../lib/api';

interface Props {
  fundId: string;
  /** Default false — keep the home list short for funds with no events. */
  defaultOpen?: boolean;
  limit?: number;
}

const TYPE_LABEL_KEY: Record<CorpActionApplication['actionType'], string> = {
  split: 'corpActions.typeSplit',
  cash_dividend: 'corpActions.typeCashDividend',
  stock_dividend: 'corpActions.typeStockDividend',
  combined: 'corpActions.typeCombined',
};

const TYPE_BADGE_BG: Record<CorpActionApplication['actionType'], string> = {
  split: '#dbeafe',
  cash_dividend: '#d1fae5',
  stock_dividend: '#ede9fe',
  combined: '#fef3c7',
};

const TYPE_BADGE_FG: Record<CorpActionApplication['actionType'], string> = {
  split: '#1d4ed8',
  cash_dividend: '#047857',
  stock_dividend: '#6d28d9',
  combined: '#92400e',
};

function formatNumber(n: number, fractionDigits = 2): string {
  if (!Number.isFinite(n)) return '—';
  return n.toLocaleString(undefined, {
    minimumFractionDigits: fractionDigits,
    maximumFractionDigits: fractionDigits,
  });
}

export function CorpActionTimelineCard({
  fundId,
  defaultOpen = false,
  limit = 50,
}: Props): JSX.Element {
  const { t } = useTranslation();
  const [open, setOpen] = useState(defaultOpen);

  // Lazy fetch: enabled flag means we only hit the network when the
  // user actually expands the card. react-query's cache keeps the
  // result for 5 minutes so collapsing + reopening doesn't refetch.
  const { data, isLoading, isError, refetch, isFetching } = useQuery({
    queryKey: ['corp-actions', fundId, limit],
    queryFn: () => apiClient.getCorpActions(fundId, limit),
    enabled: open,
    staleTime: 5 * 60 * 1000,
    retry: 1,
  });

  return (
    <View style={styles.card}>
      <Pressable
        onPress={() => setOpen((v) => !v)}
        accessibilityRole="button"
        accessibilityState={{ expanded: open }}
        style={styles.header}
      >
        <View style={styles.headerText}>
          <Text style={styles.title}>{t('corpActions.title')}</Text>
          <Text style={styles.subtitle}>{t('corpActions.subtitle')}</Text>
        </View>
        <Text style={styles.toggle}>
          {open ? t('corpActions.collapse') : t('corpActions.expand')}
        </Text>
      </Pressable>

      {open ? (
        <View style={styles.body}>
          {isLoading || isFetching ? (
            <View style={styles.center}>
              <ActivityIndicator size="small" color="#4f46e5" />
              <Text style={styles.muted}>{t('corpActions.loading')}</Text>
            </View>
          ) : isError ? (
            <View style={styles.center}>
              <Text style={styles.errorText}>{t('corpActions.error')}</Text>
              <Pressable onPress={() => void refetch()} style={styles.retry}>
                <Text style={styles.retryText}>{t('corpActions.retry')}</Text>
              </Pressable>
            </View>
          ) : !data || data.items.length === 0 ? (
            <Text style={styles.muted}>{t('corpActions.empty')}</Text>
          ) : (
            data.items.map((item, idx) => (
              <CorpActionRow key={`${item.instrumentKey}-${item.exDate}-${idx}`} item={item} />
            ))
          )}
        </View>
      ) : null}
    </View>
  );
}

function CorpActionRow({ item }: { item: CorpActionApplication }): JSX.Element {
  const { t } = useTranslation();
  const sharesDelta = item.postQuantity - item.preQuantity;
  const costDelta = item.postCostPrice - item.preCostPrice;

  return (
    <View style={styles.row}>
      <View style={styles.rowHeader}>
        <Text style={styles.instrument}>{item.instrumentKey}</Text>
        <Text style={styles.exDate}>{item.exDate.slice(0, 10)}</Text>
      </View>
      <View style={styles.rowBadge}>
        <View
          style={[
            styles.badge,
            { backgroundColor: TYPE_BADGE_BG[item.actionType] },
          ]}
        >
          <Text style={[styles.badgeText, { color: TYPE_BADGE_FG[item.actionType] }]}>
            {t(TYPE_LABEL_KEY[item.actionType])}
          </Text>
        </View>
      </View>
      <View style={styles.metrics}>
        <Metric
          label={t('corpActions.sharesLabel')}
          value={`${formatNumber(item.preQuantity)} → ${formatNumber(item.postQuantity)}`}
          delta={sharesDelta}
          deltaSuffix=""
        />
        <Metric
          label={t('corpActions.costLabel')}
          value={`${formatNumber(item.preCostPrice, 4)} → ${formatNumber(item.postCostPrice, 4)}`}
          delta={costDelta}
          deltaSuffix=""
          invertColor
        />
        {item.cashCredit > 0 ? (
          <Metric
            label={t('corpActions.cashLabel')}
            value={formatNumber(item.cashCredit, 2)}
            delta={item.cashCredit}
            deltaSuffix=""
            positiveOnly
          />
        ) : null}
      </View>
    </View>
  );
}

function Metric({
  label,
  value,
  delta,
  deltaSuffix,
  invertColor = false,
  positiveOnly = false,
}: {
  label: string;
  value: string;
  delta: number;
  deltaSuffix: string;
  invertColor?: boolean;
  positiveOnly?: boolean;
}): JSX.Element {
  // Cost basis convention: a NEGATIVE delta is GOOD (lower cost basis
  // post-split / post-dividend), so `invertColor` flips the colour.
  // Cash credit is "positiveOnly" — it can never go negative, so we
  // colour it green or muted regardless of sign.
  const positive = positiveOnly
    ? delta > 0
    : invertColor
      ? delta < 0
      : delta > 0;
  const negative = positiveOnly
    ? false
    : invertColor
      ? delta > 0
      : delta < 0;
  return (
    <View style={styles.metric}>
      <Text style={styles.metricLabel}>{label}</Text>
      <Text style={styles.metricValue}>{value}</Text>
      {delta !== 0 ? (
        <Text
          style={[
            styles.metricDelta,
            positive ? styles.deltaPositive : null,
            negative ? styles.deltaNegative : null,
          ]}
        >
          {delta > 0 ? '+' : ''}
          {formatNumber(delta, 2)}
          {deltaSuffix}
        </Text>
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  card: {
    backgroundColor: '#ffffff',
    borderRadius: 8,
    padding: 12,
    marginTop: 10,
    borderWidth: 1,
    borderColor: '#e5e7eb',
  },
  header: {
    flexDirection: 'row',
    alignItems: 'flex-start',
    justifyContent: 'space-between',
  },
  headerText: { flex: 1, paddingRight: 12 },
  title: { fontSize: 14, fontWeight: '600', color: '#111827' },
  subtitle: { fontSize: 11, color: '#6b7280', marginTop: 2 },
  toggle: { fontSize: 12, color: '#4f46e5', fontWeight: '600' },
  body: { marginTop: 10, paddingTop: 10, borderTopWidth: 1, borderTopColor: '#f3f4f6' },
  center: { alignItems: 'center', paddingVertical: 12 },
  muted: { color: '#6b7280', fontSize: 13, marginTop: 6 },
  errorText: { color: '#dc2626', fontSize: 13, marginBottom: 8 },
  retry: { paddingVertical: 6, paddingHorizontal: 12, backgroundColor: '#e5e7eb', borderRadius: 6 },
  retryText: { color: '#1f2937', fontSize: 12 },
  row: {
    paddingVertical: 8,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: '#f3f4f6',
  },
  rowHeader: { flexDirection: 'row', justifyContent: 'space-between', alignItems: 'center' },
  rowBadge: { flexDirection: 'row', marginTop: 4 },
  instrument: { fontSize: 13, fontWeight: '600', color: '#111827' },
  exDate: { fontSize: 11, color: '#6b7280', fontFamily: 'monospace' },
  badge: { paddingHorizontal: 8, paddingVertical: 2, borderRadius: 12 },
  badgeText: { fontSize: 11, fontWeight: '500' },
  metrics: { marginTop: 8 },
  metric: {
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'center',
    marginTop: 4,
  },
  metricLabel: { fontSize: 11, color: '#6b7280' },
  metricValue: { fontSize: 12, color: '#1f2937', fontVariant: ['tabular-nums'] },
  metricDelta: { fontSize: 11, fontVariant: ['tabular-nums'], minWidth: 56, textAlign: 'right' },
  deltaPositive: { color: '#047857' },
  deltaNegative: { color: '#dc2626' },
});

export default CorpActionTimelineCard;
