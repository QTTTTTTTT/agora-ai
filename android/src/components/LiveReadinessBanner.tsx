/**
 * LiveReadinessBanner — Android (React Native) component.
 *
 * Renders the per-fund "live trading prerequisites" checklist on
 * top of the Orders screen. Companion to web's
 * src/components/LiveReadinessBanner.tsx — same data source
 * (GET /api/funds/{fundId}/live-readiness, P0-9), same display
 * contract (hide on simulation/paper, render checklist on live).
 *
 * Why a separate file
 *
 * The web banner uses Tailwind classes that don't translate to
 * RN's StyleSheet API; the RN banner additionally needs to feel
 * native (Pressable retry, no hover states). Sharing only the
 * wire shape keeps both surfaces honest.
 *
 * i18n
 *
 * Strings live in shared/api-client/src/i18n.ts under
 * `orders.*` so the same labels can be reused by future surfaces
 * (the WeChat miniapp banner when P0-9 covers it).
 */

import React, { useEffect, useState } from 'react';
import { ActivityIndicator, Pressable, StyleSheet, Text, View } from 'react-native';
import { useTranslation } from 'react-i18next';

import { apiClient } from '../lib/api';
import { peekStepUpToken } from '../lib/stepUp';
import type { LiveReadinessResponse } from '@fundai/api-client';

interface Props {
  fundId?: string;
  // Bumped by the parent after a successful cancel/replace so the
  // banner's StepUpOK can refresh — useful when a step-up token
  // was just minted by the action mutation.
  refreshKey?: number;
}

export default function LiveReadinessBanner({ fundId, refreshKey }: Props): JSX.Element | null {
  const { t } = useTranslation();
  const [data, setData] = useState<LiveReadinessResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    if (!fundId) {
      setData(null);
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    // Forward a cached step-up token if one is in hand — lets the
    // banner light StepUpOK green right after a biometric prompt.
    const cached = peekStepUpToken();
    apiClient
      .liveReadiness({ fundId, stepUpToken: cached ?? undefined })
      .then((resp) => {
        if (cancelled) return;
        setData(resp);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        const status = (err as { status?: number })?.status;
        // Hide on 403/404 — the user simply doesn't own this fund.
        if (status === 403 || status === 404) {
          setData(null);
          return;
        }
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [fundId, refreshKey, tick]);

  if (loading) {
    return (
      <View style={[styles.container, styles.containerLoading]}>
        <ActivityIndicator size="small" color="#92400e" />
      </View>
    );
  }

  if (error) {
    return (
      <View style={[styles.container, styles.containerError]}>
        <Text style={styles.errorText}>{error}</Text>
        <Pressable
          onPress={() => setTick((n) => n + 1)}
          accessibilityRole="button"
          style={styles.retryButton}
        >
          <Text style={styles.retryButtonText}>{t('common.retry') || 'Retry'}</Text>
        </Pressable>
      </View>
    );
  }

  if (!data || data.trading_mode !== 'live') {
    return null;
  }

  return (
    <View style={[styles.container, data.ready ? styles.containerReady : styles.containerBlocked]}>
      <View style={styles.headerRow}>
        <View style={styles.headerText}>
          <Text style={styles.title}>{t('orders.liveBannerTitle')}</Text>
          <Text style={styles.subtitle}>{t('orders.liveBannerSubtitle')}</Text>
        </View>
        <View style={[styles.badge, data.gate_enforced ? styles.badgeEnforced : styles.badgeBypass]}>
          <Text
            style={[
              styles.badgeText,
              data.gate_enforced ? styles.badgeEnforcedText : styles.badgeBypassText,
            ]}
          >
            {data.gate_enforced ? t('orders.liveBannerEnforced') : t('orders.liveBannerBypass')}
          </Text>
        </View>
      </View>
      <PillarRow
        label={t('orders.livePillarKYC')}
        ok={data.kyc_ok}
        okText={t('orders.livePillarOK')}
        pendingText={t('orders.livePillarMissing')}
        hint={t('orders.liveBlockedKYC')}
      />
      <PillarRow
        label={t('orders.livePillarBrokerLink')}
        ok={data.broker_link_ok}
        okText={t('orders.livePillarOK')}
        pendingText={t('orders.livePillarMissing')}
        hint={t('orders.liveBlockedBrokerLink')}
      />
      <PillarRow
        label={t('orders.livePillarTwoFA')}
        ok={data.two_fa_ok}
        okText={t('orders.livePillarOK')}
        pendingText={t('orders.livePillarMissing')}
        hint={t('orders.liveBlockedTwoFA')}
      />
      <PillarRow
        label={t('orders.livePillarStepUp')}
        ok={data.step_up_ok}
        okText={t('orders.livePillarOK')}
        pendingText={t('orders.livePillarMissing')}
        hint={t('orders.liveBlockedStepUp')}
      />
    </View>
  );
}

interface PillarRowProps {
  label: string;
  ok: boolean;
  okText: string;
  pendingText: string;
  hint: string;
}

function PillarRow({ label, ok, okText, pendingText, hint }: PillarRowProps): JSX.Element {
  return (
    <View style={styles.row}>
      <View style={styles.rowLeft}>
        <View style={[styles.dot, ok ? styles.dotOk : styles.dotPending]} />
        <Text style={styles.rowLabel}>{label}</Text>
      </View>
      <View style={styles.rowRight}>
        <Text style={ok ? styles.rowStatusOk : styles.rowStatusPending}>{ok ? okText : pendingText}</Text>
        {!ok ? <Text style={styles.rowHint}>— {hint}</Text> : null}
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  container: {
    margin: 12,
    padding: 14,
    borderRadius: 16,
    borderWidth: 1,
  },
  containerLoading: {
    borderColor: '#fde68a',
    backgroundColor: '#fffbeb',
    alignItems: 'center',
  },
  containerError: {
    borderColor: '#fecaca',
    backgroundColor: '#fef2f2',
  },
  containerReady: {
    borderColor: '#a7f3d0',
    backgroundColor: '#ecfdf5',
  },
  containerBlocked: {
    borderColor: '#fde68a',
    backgroundColor: '#fffbeb',
  },
  headerRow: {
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'flex-start',
    marginBottom: 8,
  },
  headerText: { flex: 1, paddingRight: 12 },
  title: { fontSize: 15, fontWeight: '600', color: '#111827' },
  subtitle: { fontSize: 12, color: '#4b5563', marginTop: 2 },
  badge: {
    paddingHorizontal: 10,
    paddingVertical: 4,
    borderRadius: 999,
  },
  badgeEnforced: { backgroundColor: '#a7f3d0' },
  badgeBypass: { backgroundColor: '#e5e7eb' },
  badgeText: { fontSize: 11, fontWeight: '600' },
  badgeEnforcedText: { color: '#065f46' },
  badgeBypassText: { color: '#374151' },
  row: {
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'flex-start',
    paddingVertical: 6,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: 'rgba(146, 64, 14, 0.2)',
  },
  rowLeft: { flexDirection: 'row', alignItems: 'center', flex: 1 },
  rowRight: { flexShrink: 1, alignItems: 'flex-end' },
  dot: { width: 8, height: 8, borderRadius: 4, marginRight: 8 },
  dotOk: { backgroundColor: '#10b981' },
  dotPending: { backgroundColor: '#f59e0b' },
  rowLabel: { color: '#374151', fontWeight: '500', fontSize: 13 },
  rowStatusOk: { color: '#047857', fontSize: 13 },
  rowStatusPending: { color: '#b45309', fontSize: 13 },
  rowHint: { color: '#6b7280', fontSize: 11, marginTop: 2 },
  errorText: { color: '#991b1b', fontSize: 13, marginBottom: 8 },
  retryButton: {
    alignSelf: 'flex-start',
    paddingHorizontal: 12,
    paddingVertical: 6,
    backgroundColor: '#dc2626',
    borderRadius: 8,
  },
  retryButtonText: { color: '#fff', fontSize: 13, fontWeight: '600' },
});
