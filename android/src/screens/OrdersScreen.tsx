/**
 * OrdersScreen — open-order list with cancel + replace actions (P0-5).
 *
 * Why this screen exists
 * ----------------------
 * The Sprint-1 mobile build only had Decision/Plan-level controls;
 * once a plan is approved and trades are persisted, the operator had
 * no way to react to changing market conditions on mobile (cancel a
 * resting limit, raise a stop). This screen closes that loop using
 * the same /api/funds/{fundId}/orders/{tradeId}/cancel & /replace
 * endpoints the web side calls, keeping behaviour identical across
 * surfaces.
 *
 * Scope
 * -----
 * - Lists the most recent 200 trades for the active fund, filters
 *   client-side to the open subset (pending / working / triggered /
 *   partial). Other statuses are kept hidden so the action surface
 *   is uncluttered; full history is part of S8's TradeHistory work.
 * - Cancel: native Alert.alert confirmation, then apiClient.cancelOrder.
 * - Replace: modal with the per-order-type input fields, runs
 *   apiClient.replaceOrder. Returns the post-mutation OrderActionResponse
 *   which we splice back into the cached list.
 *
 * Concurrency: react-query owns the cache + refetch. Each mutation
 * patches the cache via setQueryData rather than triggering a full
 * refetch — keeps the perceived latency snappy on a slow network.
 */

import React, { useCallback, useMemo, useState } from 'react';
import {
  ActivityIndicator,
  Alert,
  FlatList,
  KeyboardAvoidingView,
  Modal,
  Platform,
  Pressable,
  RefreshControl,
  ScrollView,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import { apiClient } from '../lib/api';
import { useActiveFund } from '../lib/activeFund';
import { isStepUpCancelled, withStepUp } from '../lib/stepUp';
import { isStepUpRequiredForOrders } from '../lib/userPrefs';
import LiveReadinessBanner from '../components/LiveReadinessBanner';
import BrokerLinksPanel from '../components/BrokerLinksPanel';
import FundingPanel from '../components/FundingPanel';
import type { OrderActionResponse, ReplaceOrderPayload, TradeRecord } from '@fundai/api-client';

const OPEN_STATUSES = new Set(['pending', 'working', 'triggered', 'partial']);

function isOpenStatus(s: string): boolean {
  return OPEN_STATUSES.has(s);
}

export default function OrdersScreen(): JSX.Element {
  const { t } = useTranslation();
  const { fundId } = useActiveFund();
  const queryClient = useQueryClient();
  const [replaceTarget, setReplaceTarget] = useState<TradeRecord | null>(null);

  const queryKey = useMemo(() => ['orders', fundId], [fundId]);

  const {
    data,
    isLoading,
    isFetching,
    refetch,
    isError,
  } = useQuery({
    queryKey,
    enabled: !!fundId,
    queryFn: async () => {
      if (!fundId) return { trades: [] };
      return apiClient.listTrades(fundId, 200);
    },
  });

  const trades = (data?.trades ?? []).filter((t) => isOpenStatus(t.status));

  // patchCache mutates the cached list in place using the post-mutation
  // OrderActionResponse. We deliberately keep the legacy fields the
  // server didn't include in the trim shape (executedAt, fees, etc.)
  // so the row stays renderable.
  const patchCache = useCallback(
    (updated: OrderActionResponse) => {
      queryClient.setQueryData<{ trades: TradeRecord[] } | undefined>(queryKey, (old) => {
        if (!old) return old;
        return {
          trades: old.trades.map((t) =>
            t.id === updated.id
              ? {
                  ...t,
                  status: updated.status,
                  quantity: updated.quantity,
                  filledQty: updated.filledQty,
                  price: updated.limitPrice ?? t.price,
                  stopPrice: updated.stopPrice ?? t.stopPrice,
                  trailAmount: updated.trailAmount ?? t.trailAmount,
                  trailPercent: updated.trailPercent ?? t.trailPercent,
                  displayQty: updated.displayQty ?? t.displayQty,
                  cancelReason: updated.cancelReason ?? t.cancelReason,
                  replaceCount: updated.replaceCount,
                }
              : t,
          ),
        };
      });
    },
    [queryKey, queryClient],
  );

  const cancelMutation = useMutation({
    mutationFn: async (trade: TradeRecord) => {
      if (!fundId) throw new Error('no active fund');
      // P0-7 — When the user has step-up gating enabled (default
      // true), prompt biometrics before dispatching the cancel.
      // The token is scoped to this single request via
      // X-Step-Up-Token; cache reuse is handled inside withStepUp.
      if (isStepUpRequiredForOrders()) {
        return withStepUp(
          t('orders.stepUpCancelReason'),
          (token) => apiClient.cancelOrder(fundId, trade.id, { reason: 'user_requested', stepUpToken: token }),
          { biometricKind: 'fingerprint' },
        );
      }
      return apiClient.cancelOrder(fundId, trade.id, { reason: 'user_requested' });
    },
    onSuccess: (updated) => {
      patchCache(updated);
      Alert.alert(t('orders.cancelSuccess'));
    },
    onError: (err: unknown) => {
      // Biometric cancel is a quiet abort — the user already
      // saw the system prompt and decided not to confirm.
      if (isStepUpCancelled(err)) return;
      Alert.alert(t('orders.actionFailed'), describeError(err));
    },
  });

  const replaceMutation = useMutation({
    mutationFn: async (args: { trade: TradeRecord; payload: ReplaceOrderPayload }) => {
      if (!fundId) throw new Error('no active fund');
      if (isStepUpRequiredForOrders()) {
        return withStepUp(
          t('orders.stepUpReplaceReason'),
          (token) => apiClient.replaceOrder(fundId, args.trade.id, args.payload, { stepUpToken: token }),
          { biometricKind: 'fingerprint' },
        );
      }
      return apiClient.replaceOrder(fundId, args.trade.id, args.payload);
    },
    onSuccess: (updated) => {
      patchCache(updated);
      setReplaceTarget(null);
      Alert.alert(t('orders.replaceSuccess'));
    },
    onError: (err: unknown) => {
      if (isStepUpCancelled(err)) return;
      Alert.alert(t('orders.actionFailed'), describeError(err));
    },
  });

  const handleCancel = useCallback(
    (trade: TradeRecord) => {
      Alert.alert(
        t('orders.cancelConfirmTitle'),
        t('orders.cancelConfirmBody'),
        [
          { text: t('orders.cancelDismiss'), style: 'cancel' },
          {
            text: t('orders.cancelOkConfirm'),
            style: 'destructive',
            onPress: () => cancelMutation.mutate(trade),
          },
        ],
      );
    },
    [cancelMutation, t],
  );

  if (!fundId) {
    return (
      <View style={styles.center}>
        <Text style={styles.muted}>{t('orders.empty')}</Text>
      </View>
    );
  }

  if (isLoading) {
    return (
      <View style={styles.center}>
        <ActivityIndicator size="large" color="#4f46e5" />
      </View>
    );
  }

  return (
    <>
      <FlatList
        data={trades}
        keyExtractor={(t) => t.id}
        contentContainerStyle={styles.container}
        refreshControl={<RefreshControl refreshing={isFetching} onRefresh={() => void refetch()} />}
        ListHeaderComponent={
          fundId ? (
            <View>
              <LiveReadinessBanner fundId={fundId} />
              <BrokerLinksPanel fundId={fundId} />
              <FundingPanel fundId={fundId} />
            </View>
          ) : null
        }
        ListEmptyComponent={
          <View style={styles.center}>
            <Text style={[styles.muted, isError && styles.errorText]}>
              {isError ? t('orders.loadFailed') : t('orders.empty')}
            </Text>
            {isError ? (
              <Pressable
                style={styles.retry}
                onPress={() => void refetch()}
                accessibilityRole="button"
                accessibilityLabel={t('orders.retry')}
              >
                <Text style={styles.retryText}>{t('orders.retry')}</Text>
              </Pressable>
            ) : null}
          </View>
        }
        renderItem={({ item }) => (
          <OrderCard
            trade={item}
            onCancel={() => handleCancel(item)}
            onReplace={() => setReplaceTarget(item)}
            isBusy={
              (cancelMutation.isPending && cancelMutation.variables?.id === item.id) ||
              (replaceMutation.isPending && replaceMutation.variables?.trade.id === item.id)
            }
          />
        )}
      />
      {replaceTarget ? (
        <ReplaceOrderModal
          trade={replaceTarget}
          submitting={replaceMutation.isPending}
          onCancel={() => setReplaceTarget(null)}
          onSubmit={(payload) => replaceMutation.mutate({ trade: replaceTarget, payload })}
        />
      ) : null}
    </>
  );
}

interface OrderCardProps {
  trade: TradeRecord;
  onCancel: () => void;
  onReplace: () => void;
  isBusy: boolean;
}

const OrderCard: React.FC<OrderCardProps> = ({ trade, onCancel, onReplace, isBusy }) => {
  const { t } = useTranslation();
  const sideColor = trade.side === 'sell' ? '#dc2626' : '#059669';
  return (
    <View style={styles.card}>
      <View style={styles.cardHead}>
        <Text style={styles.symbol}>{trade.symbol}</Text>
        <Text style={[styles.side, { color: sideColor }]}>{trade.side.toUpperCase()}</Text>
      </View>
      <View style={styles.metaRow}>
        <Text style={styles.metaCell}>
          {t('orders.columns.qty')}: {trade.quantity.toFixed(2)}
        </Text>
        <Text style={styles.metaCell}>
          {t('orders.columns.price')}: {trade.price?.toFixed(2) ?? '-'}
        </Text>
        <Text style={styles.metaCell}>
          {t('orders.columns.status')}: {trade.status}
        </Text>
      </View>
      {trade.stopPrice ? (
        <Text style={styles.metaCell}>stop: {trade.stopPrice.toFixed(2)}</Text>
      ) : null}
      <View style={styles.cardActions}>
        <Pressable
          onPress={onReplace}
          disabled={isBusy}
          style={[styles.actionBtn, styles.actionReplace, isBusy && styles.actionDisabled]}
          accessibilityRole="button"
          accessibilityLabel={t('orders.replace')}
        >
          <Text style={styles.actionText}>
            {isBusy ? t('orders.replacing') : t('orders.replace')}
          </Text>
        </Pressable>
        <Pressable
          onPress={onCancel}
          disabled={isBusy}
          style={[styles.actionBtn, styles.actionCancel, isBusy && styles.actionDisabled]}
          accessibilityRole="button"
          accessibilityLabel={t('orders.cancel')}
        >
          <Text style={styles.actionText}>
            {isBusy ? t('orders.cancelling') : t('orders.cancel')}
          </Text>
        </Pressable>
      </View>
    </View>
  );
};

interface ReplaceOrderModalProps {
  trade: TradeRecord;
  submitting: boolean;
  onCancel: () => void;
  onSubmit: (payload: ReplaceOrderPayload) => void;
}

const ReplaceOrderModal: React.FC<ReplaceOrderModalProps> = ({ trade, submitting, onCancel, onSubmit }) => {
  const { t } = useTranslation();
  const [quantity, setQuantity] = useState('');
  const [limitPrice, setLimitPrice] = useState('');
  const [stopPrice, setStopPrice] = useState('');
  const [trailAmount, setTrailAmount] = useState('');
  const [trailPercent, setTrailPercent] = useState('');
  const [displayQty, setDisplayQty] = useState('');
  const [note, setNote] = useState('');

  const supportsLimit = trade.orderType === 'limit' || trade.orderType === 'stop_limit' || trade.orderType === 'iceberg';
  const supportsStop = trade.orderType === 'stop' || trade.orderType === 'stop_limit' || trade.orderType === 'trailing_stop';
  const supportsTrail = trade.orderType === 'trailing_stop';
  const supportsDisplayQty = trade.orderType === 'iceberg';

  const handleSubmit = () => {
    const payload: ReplaceOrderPayload = {};
    const parseNum = (v: string): number | undefined => {
      const trimmed = v.trim();
      if (!trimmed) return undefined;
      const n = Number(trimmed);
      return Number.isFinite(n) && n > 0 ? n : undefined;
    };
    const q = parseNum(quantity);
    if (q !== undefined) payload.quantity = q;
    if (supportsLimit) {
      const lp = parseNum(limitPrice);
      if (lp !== undefined) payload.limitPrice = lp;
    }
    if (supportsStop) {
      const sp = parseNum(stopPrice);
      if (sp !== undefined) payload.stopPrice = sp;
    }
    if (supportsTrail) {
      const ta = parseNum(trailAmount);
      if (ta !== undefined) payload.trailAmount = ta;
      const tp = parseNum(trailPercent);
      if (tp !== undefined && tp < 1) payload.trailPercent = tp;
    }
    if (supportsDisplayQty) {
      const dq = parseNum(displayQty);
      if (dq !== undefined) payload.displayQty = dq;
    }
    const n = note.trim();
    if (n) payload.note = n;
    const numericChange =
      payload.quantity !== undefined ||
      payload.limitPrice !== undefined ||
      payload.stopPrice !== undefined ||
      payload.trailAmount !== undefined ||
      payload.trailPercent !== undefined ||
      payload.displayQty !== undefined;
    if (!numericChange) {
      Alert.alert(t('orders.actionFailed'), t('orders.replaceLeaveBlankHint'));
      return;
    }
    onSubmit(payload);
  };

  return (
    <Modal animationType="slide" transparent visible onRequestClose={onCancel}>
      <KeyboardAvoidingView
        behavior={Platform.OS === 'ios' ? 'padding' : undefined}
        style={styles.modalRoot}
      >
        <View style={styles.modalCard}>
          <ScrollView contentContainerStyle={styles.modalScroll} keyboardShouldPersistTaps="handled">
            <Text style={styles.modalTitle}>{t('orders.replaceTitle')}</Text>
            <Text style={styles.modalSub}>{trade.symbol} · {trade.side.toUpperCase()} · {trade.orderType}</Text>
            <Text style={styles.modalHint}>{t('orders.replaceLeaveBlankHint')}</Text>
            <Field
              label={t('orders.replaceQuantity')}
              value={quantity}
              onChange={setQuantity}
              placeholder={String(trade.quantity)}
            />
            {supportsLimit ? (
              <Field
                label={t('orders.replaceLimit')}
                value={limitPrice}
                onChange={setLimitPrice}
                placeholder={String(trade.price ?? '')}
              />
            ) : null}
            {supportsStop ? (
              <Field
                label={t('orders.replaceStop')}
                value={stopPrice}
                onChange={setStopPrice}
              />
            ) : null}
            {supportsTrail ? (
              <>
                <Field
                  label={t('orders.replaceTrailAmount')}
                  value={trailAmount}
                  onChange={setTrailAmount}
                />
                <Field
                  label={t('orders.replaceTrailPercent')}
                  value={trailPercent}
                  onChange={setTrailPercent}
                />
              </>
            ) : null}
            {supportsDisplayQty ? (
              <Field
                label={t('orders.replaceDisplayQty')}
                value={displayQty}
                onChange={setDisplayQty}
              />
            ) : null}
            <Field
              label={t('orders.replaceNote')}
              value={note}
              onChange={setNote}
              keyboardType="default"
            />
            <View style={styles.modalActions}>
              <Pressable
                onPress={onCancel}
                style={[styles.modalBtn, styles.modalBtnSecondary]}
                accessibilityRole="button"
                accessibilityLabel={t('orders.replaceCancel')}
              >
                <Text style={styles.modalBtnSecondaryText}>{t('orders.replaceCancel')}</Text>
              </Pressable>
              <Pressable
                onPress={handleSubmit}
                disabled={submitting}
                style={[styles.modalBtn, styles.modalBtnPrimary, submitting && styles.actionDisabled]}
                accessibilityRole="button"
                accessibilityLabel={t('orders.replaceSubmit')}
              >
                <Text style={styles.modalBtnPrimaryText}>
                  {submitting ? t('orders.replacing') : t('orders.replaceSubmit')}
                </Text>
              </Pressable>
            </View>
          </ScrollView>
        </View>
      </KeyboardAvoidingView>
    </Modal>
  );
};

interface FieldProps {
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  keyboardType?: 'default' | 'numeric' | 'decimal-pad';
}

const Field: React.FC<FieldProps> = ({ label, value, onChange, placeholder, keyboardType }) => (
  <View style={styles.field}>
    <Text style={styles.fieldLabel}>{label}</Text>
    <TextInput
      value={value}
      onChangeText={onChange}
      placeholder={placeholder}
      placeholderTextColor="#9ca3af"
      keyboardType={keyboardType ?? 'decimal-pad'}
      style={styles.fieldInput}
    />
  </View>
);

function describeError(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}

const styles = StyleSheet.create({
  container: { padding: 16, gap: 12 },
  center: { flex: 1, alignItems: 'center', justifyContent: 'center', paddingTop: 32, paddingHorizontal: 16 },
  muted: { color: '#6b7280', fontSize: 14, textAlign: 'center' },
  errorText: { color: '#dc2626' },
  retry: { marginTop: 12, paddingHorizontal: 16, paddingVertical: 8, backgroundColor: '#4f46e5', borderRadius: 6 },
  retryText: { color: '#fff', fontWeight: '600' },
  card: {
    backgroundColor: '#ffffff',
    borderRadius: 10,
    padding: 14,
    marginBottom: 4,
    borderColor: '#e5e7eb',
    borderWidth: 1,
  },
  cardHead: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between' },
  symbol: { fontSize: 16, fontWeight: '700', color: '#111827' },
  side: { fontSize: 13, fontWeight: '700' },
  metaRow: { flexDirection: 'row', flexWrap: 'wrap', marginTop: 8, gap: 12 },
  metaCell: { fontSize: 12, color: '#6b7280' },
  cardActions: { flexDirection: 'row', justifyContent: 'flex-end', gap: 8, marginTop: 12 },
  actionBtn: {
    paddingHorizontal: 14,
    paddingVertical: 8,
    borderRadius: 6,
    borderWidth: 1,
  },
  actionReplace: { backgroundColor: '#eef2ff', borderColor: '#c7d2fe' },
  actionCancel: { backgroundColor: '#fee2e2', borderColor: '#fecaca' },
  actionDisabled: { opacity: 0.5 },
  actionText: { fontSize: 13, fontWeight: '600' },
  // modal
  modalRoot: { flex: 1, backgroundColor: 'rgba(0,0,0,0.4)', justifyContent: 'flex-end' },
  modalCard: { backgroundColor: '#ffffff', borderTopLeftRadius: 16, borderTopRightRadius: 16, maxHeight: '85%' },
  modalScroll: { padding: 20 },
  modalTitle: { fontSize: 18, fontWeight: '700', color: '#111827', marginBottom: 4 },
  modalSub: { fontSize: 12, color: '#6b7280', marginBottom: 12 },
  modalHint: { fontSize: 12, color: '#6b7280', marginBottom: 12 },
  field: { marginBottom: 12 },
  fieldLabel: { fontSize: 13, color: '#374151', marginBottom: 4 },
  fieldInput: {
    borderWidth: 1,
    borderColor: '#d1d5db',
    borderRadius: 6,
    paddingHorizontal: 12,
    paddingVertical: 8,
    fontSize: 14,
    color: '#111827',
  },
  modalActions: { flexDirection: 'row', justifyContent: 'flex-end', gap: 8, marginTop: 12 },
  modalBtn: { paddingHorizontal: 14, paddingVertical: 10, borderRadius: 6 },
  modalBtnPrimary: { backgroundColor: '#4f46e5' },
  modalBtnPrimaryText: { color: '#fff', fontWeight: '600' },
  modalBtnSecondary: { borderWidth: 1, borderColor: '#d1d5db', backgroundColor: '#fff' },
  modalBtnSecondaryText: { color: '#374151', fontWeight: '500' },
});
