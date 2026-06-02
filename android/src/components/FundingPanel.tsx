/**
 * FundingPanel — Android (React Native) component (P1-2).
 *
 * Companion to web's FundingSection.tsx and pairs with the
 * BrokerLinksPanel above it on the orders screen. Surfaces the
 * deposit/withdrawal self-service flow as a collapsible card so
 * the orders list isn't pushed down for users who never need it.
 *
 * UX choices:
 *   - Direction is a two-button toggle (deposit/withdrawal) for
 *     thumb reach on small screens — a dropdown adds an extra tap.
 *   - Method picker is a modal sheet (matches BrokerLinksPanel
 *     style) so the form keeps a tall input area for amount + ref.
 *   - We don't render approve/reject — those live on the web admin
 *     surface only. The user-side panel is "submit + view + cancel".
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  Alert,
  Modal,
  Pressable,
  ScrollView,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native';
import { useTranslation } from 'react-i18next';

import { apiClient } from '../lib/api';
import type {
  FundingDirection,
  FundingMethod,
  FundingRequestRow,
  FundingStatus,
} from '@fundai/api-client';

interface Props {
  fundId?: string;
}

const METHODS: FundingMethod[] = [
  'wire',
  'ach',
  'sepa',
  'check',
  'internal_transfer',
  'manual',
];

export default function FundingPanel({ fundId }: Props): JSX.Element | null {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(false);
  const [rows, setRows] = useState<FundingRequestRow[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [refreshKey, setRefreshKey] = useState(0);

  const [direction, setDirection] = useState<FundingDirection>('deposit');
  const [amount, setAmount] = useState<string>('');
  const [currency, setCurrency] = useState<string>('USD');
  const [method, setMethod] = useState<FundingMethod>('wire');
  const [externalReference, setExternalReference] = useState<string>('');
  const [notes, setNotes] = useState<string>('');

  const [pickerOpen, setPickerOpen] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [cancellingId, setCancellingId] = useState<string | null>(null);

  const refresh = useCallback(() => setRefreshKey((k) => k + 1), []);

  useEffect(() => {
    if (!expanded || !fundId) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    apiClient
      .listFundingRequests({ fundId, limit: 50 })
      .then((data) => {
        if (!cancelled) setRows(data);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        const status = (err as { status?: number })?.status;
        if (status === 403 || status === 404) {
          setRows([]);
          return;
        }
        setError(`${t('funding.errorPrefix')}${(err as Error)?.message ?? 'unknown'}`);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [expanded, fundId, refreshKey, t]);

  const onSubmit = async () => {
    if (!fundId) return;
    const amt = Number(amount);
    if (!Number.isFinite(amt) || amt <= 0) {
      setError(`${t('funding.errorPrefix')}${t('funding.formAmount')}`);
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await apiClient.createFundingRequest({
        fundId,
        direction,
        amount: amt,
        method,
        currency: currency.trim().toUpperCase() || 'USD',
        externalReference: externalReference.trim() || undefined,
        notes: notes.trim() || undefined,
      });
      setAmount('');
      setExternalReference('');
      setNotes('');
      refresh();
    } catch (err) {
      setError(`${t('funding.errorPrefix')}${(err as Error)?.message ?? 'unknown'}`);
    } finally {
      setSubmitting(false);
    }
  };

  const onCancel = (row: FundingRequestRow) => {
    if (!fundId) return;
    Alert.alert(
      t('funding.cancel'),
      t('funding.confirmCancel'),
      [
        { text: 'Cancel', style: 'cancel' },
        {
          text: t('funding.cancel'),
          style: 'destructive',
          onPress: async () => {
            setCancellingId(row.id);
            setError(null);
            try {
              await apiClient.cancelFundingRequest({
                fundId,
                requestId: row.id,
              });
              refresh();
            } catch (err) {
              setError(`${t('funding.errorPrefix')}${(err as Error)?.message ?? 'unknown'}`);
            } finally {
              setCancellingId(null);
            }
          },
        },
      ],
      { cancelable: true },
    );
  };

  const sorted = useMemo(() => {
    if (!rows) return [];
    return [...rows].sort((a, b) => b.createdAt.localeCompare(a.createdAt));
  }, [rows]);

  if (!fundId) return null;

  return (
    <View style={styles.card}>
      <Pressable
        accessibilityRole="button"
        onPress={() => setExpanded((v) => !v)}
        style={styles.header}
      >
        <View style={{ flex: 1 }}>
          <Text style={styles.title}>{t('funding.title')}</Text>
          {!expanded ? (
            <Text style={styles.subtitle}>{t('funding.subtitle')}</Text>
          ) : null}
        </View>
        <Text style={styles.toggle}>{expanded ? '▲' : '▼'}</Text>
      </Pressable>

      {expanded ? (
        <View style={styles.body}>
          {error ? <Text style={styles.error}>{error}</Text> : null}

          <View style={styles.formCard}>
            <Text style={styles.formTitle}>{t('funding.formTitle')}</Text>

            <Text style={styles.formLabel}>{t('funding.formDirection')}</Text>
            <View style={styles.directionRow}>
              <Pressable
                onPress={() => setDirection('deposit')}
                style={[
                  styles.directionBtn,
                  direction === 'deposit' && styles.directionBtnActive,
                ]}
              >
                <Text
                  style={[
                    styles.directionBtnText,
                    direction === 'deposit' && styles.directionBtnTextActive,
                  ]}
                >
                  {t('funding.formDirectionDeposit')}
                </Text>
              </Pressable>
              <Pressable
                onPress={() => setDirection('withdrawal')}
                style={[
                  styles.directionBtn,
                  direction === 'withdrawal' && styles.directionBtnActive,
                ]}
              >
                <Text
                  style={[
                    styles.directionBtnText,
                    direction === 'withdrawal' && styles.directionBtnTextActive,
                  ]}
                >
                  {t('funding.formDirectionWithdrawal')}
                </Text>
              </Pressable>
            </View>

            <View style={styles.amountRow}>
              <View style={{ flex: 2 }}>
                <Text style={styles.formLabel}>{t('funding.formAmount')}</Text>
                <TextInput
                  value={amount}
                  onChangeText={setAmount}
                  placeholder={t('funding.formAmountPlaceholder')}
                  placeholderTextColor="#9ca3af"
                  keyboardType="numeric"
                  style={styles.input}
                  editable={!submitting}
                />
              </View>
              <View style={{ flex: 1 }}>
                <Text style={styles.formLabel}>{t('funding.formCurrency')}</Text>
                <TextInput
                  value={currency}
                  onChangeText={setCurrency}
                  autoCapitalize="characters"
                  maxLength={8}
                  style={styles.input}
                  editable={!submitting}
                />
              </View>
            </View>

            <Text style={[styles.formLabel, { marginTop: 12 }]}>
              {t('funding.formMethod')}
            </Text>
            <Pressable
              style={styles.dropdown}
              onPress={() => setPickerOpen(true)}
              accessibilityRole="button"
            >
              <Text style={styles.dropdownText}>{methodLabel(method, t)}</Text>
            </Pressable>

            <Text style={[styles.formLabel, { marginTop: 12 }]}>
              {t('funding.formExternalReference')}
            </Text>
            <TextInput
              value={externalReference}
              onChangeText={setExternalReference}
              placeholder={t('funding.formExternalReferencePlaceholder')}
              placeholderTextColor="#9ca3af"
              style={styles.input}
              editable={!submitting}
            />

            <Text style={[styles.formLabel, { marginTop: 12 }]}>
              {t('funding.formNotes')}
            </Text>
            <TextInput
              value={notes}
              onChangeText={setNotes}
              placeholder={t('funding.formNotesPlaceholder')}
              placeholderTextColor="#9ca3af"
              multiline
              numberOfLines={2}
              style={[styles.input, { minHeight: 56 }]}
              editable={!submitting}
            />

            <Text style={styles.formNote}>{t('funding.formNote')}</Text>
            <Pressable
              style={[styles.primaryBtn, submitting && styles.btnDisabled]}
              disabled={submitting}
              onPress={onSubmit}
            >
              <Text style={styles.primaryBtnText}>
                {submitting ? t('funding.formSubmitting') : t('funding.formSubmit')}
              </Text>
            </Pressable>
          </View>

          <View style={styles.listHeader}>
            <Text style={styles.sectionTitle}>{t('funding.title')}</Text>
            <Pressable onPress={refresh} disabled={loading} accessibilityRole="button">
              <Text style={[styles.linkBtn, loading && { opacity: 0.5 }]}>
                {loading ? t('funding.loading') : t('funding.refresh')}
              </Text>
            </Pressable>
          </View>

          {loading ? (
            <Text style={styles.muted}>{t('funding.loading')}</Text>
          ) : sorted.length === 0 ? (
            <Text style={styles.muted}>{t('funding.empty')}</Text>
          ) : (
            sorted.map((row) => (
              <View key={row.id} style={styles.row}>
                <View style={{ flex: 1 }}>
                  <View style={styles.rowHead}>
                    <Text style={styles.rowDirection}>
                      {row.direction === 'deposit'
                        ? t('funding.formDirectionDeposit')
                        : t('funding.formDirectionWithdrawal')}
                    </Text>
                    <View style={[styles.badge, badgeStyle(row.status)]}>
                      <Text style={styles.badgeText}>{statusLabel(row.status, t)}</Text>
                    </View>
                  </View>
                  <Text style={styles.rowAmount}>
                    {row.amount.toLocaleString()} {row.currency}
                    {'  '}
                    <Text style={styles.rowMethod}>· {methodLabel(row.method as FundingMethod, t)}</Text>
                  </Text>
                  {row.externalReference ? (
                    <Text style={styles.rowMeta}>ref: {row.externalReference}</Text>
                  ) : null}
                  {row.notes ? <Text style={styles.rowMeta}>{row.notes}</Text> : null}
                  {row.status === 'rejected' && row.rejectionReason ? (
                    <Text style={styles.rowMetaError}>
                      {t('funding.rejectionReasonLabel')}: {row.rejectionReason}
                    </Text>
                  ) : null}
                  {row.status === 'pending' ? (
                    <Text style={styles.rowMetaWarn}>{t('funding.awaitingApproval')}</Text>
                  ) : null}
                  <Text style={styles.rowMeta}>
                    {new Date(row.createdAt).toLocaleString()}
                  </Text>
                </View>
                {row.status === 'pending' ? (
                  <Pressable
                    style={[
                      styles.dangerBtn,
                      cancellingId === row.id && styles.btnDisabled,
                    ]}
                    onPress={() => onCancel(row)}
                    disabled={cancellingId === row.id}
                  >
                    <Text style={styles.dangerBtnText}>
                      {cancellingId === row.id
                        ? t('funding.cancelling')
                        : t('funding.cancel')}
                    </Text>
                  </Pressable>
                ) : null}
              </View>
            ))
          )}
        </View>
      ) : null}

      <Modal
        visible={pickerOpen}
        transparent
        animationType="fade"
        onRequestClose={() => setPickerOpen(false)}
      >
        <Pressable style={styles.modalBackdrop} onPress={() => setPickerOpen(false)}>
          <View style={styles.modalSheet}>
            <Text style={styles.modalTitle}>{t('funding.formMethod')}</Text>
            <ScrollView>
              {METHODS.map((m) => (
                <Pressable
                  key={m}
                  style={styles.modalItem}
                  onPress={() => {
                    setMethod(m);
                    setPickerOpen(false);
                  }}
                >
                  <Text
                    style={[
                      styles.modalItemText,
                      method === m && { fontWeight: '600', color: '#4f46e5' },
                    ]}
                  >
                    {methodLabel(m, t)}
                  </Text>
                </Pressable>
              ))}
            </ScrollView>
          </View>
        </Pressable>
      </Modal>
    </View>
  );
}

function methodLabel(m: FundingMethod, t: (key: string) => string): string {
  switch (m) {
    case 'wire':
      return t('funding.methodWire');
    case 'ach':
      return t('funding.methodACH');
    case 'sepa':
      return t('funding.methodSEPA');
    case 'check':
      return t('funding.methodCheck');
    case 'internal_transfer':
      return t('funding.methodInternal');
    case 'manual':
      return t('funding.methodManual');
    default:
      return m;
  }
}

function statusLabel(s: FundingStatus, t: (key: string) => string): string {
  switch (s) {
    case 'pending':
      return t('funding.statusPending');
    case 'approved':
      return t('funding.statusApproved');
    case 'rejected':
      return t('funding.statusRejected');
    case 'cancelled':
      return t('funding.statusCancelled');
    case 'posted':
      return t('funding.statusPosted');
    default:
      return s;
  }
}

function badgeStyle(s: FundingStatus) {
  switch (s) {
    case 'approved':
      return { backgroundColor: '#d1fae5' };
    case 'pending':
      return { backgroundColor: '#fef3c7' };
    case 'rejected':
      return { backgroundColor: '#fee2e2' };
    case 'cancelled':
      return { backgroundColor: '#e5e7eb' };
    case 'posted':
      return { backgroundColor: '#e0e7ff' };
    default:
      return { backgroundColor: '#e5e7eb' };
  }
}

const styles = StyleSheet.create({
  card: {
    margin: 12,
    backgroundColor: '#ffffff',
    borderRadius: 12,
    borderWidth: 1,
    borderColor: '#e5e7eb',
  },
  header: {
    flexDirection: 'row',
    alignItems: 'center',
    padding: 14,
    gap: 8,
  },
  title: { fontSize: 15, fontWeight: '600', color: '#111827' },
  subtitle: { fontSize: 12, color: '#6b7280', marginTop: 2 },
  toggle: { fontSize: 12, color: '#4f46e5' },
  body: { paddingHorizontal: 14, paddingBottom: 14, gap: 12 },
  error: {
    color: '#b91c1c',
    fontSize: 13,
    backgroundColor: '#fef2f2',
    borderRadius: 8,
    padding: 8,
  },
  formCard: {
    backgroundColor: '#f9fafb',
    borderRadius: 10,
    padding: 12,
    borderWidth: 1,
    borderColor: '#e5e7eb',
  },
  formTitle: { fontSize: 13, fontWeight: '600', color: '#111827', marginBottom: 6 },
  formLabel: { fontSize: 12, color: '#6b7280', marginBottom: 4 },
  formNote: { fontSize: 11, color: '#9ca3af', marginTop: 6 },
  directionRow: { flexDirection: 'row', gap: 8 },
  directionBtn: {
    flex: 1,
    paddingVertical: 10,
    borderRadius: 8,
    borderWidth: 1,
    borderColor: '#d1d5db',
    backgroundColor: '#ffffff',
    alignItems: 'center',
  },
  directionBtnActive: { backgroundColor: '#4f46e5', borderColor: '#4f46e5' },
  directionBtnText: { fontSize: 13, color: '#374151', fontWeight: '500' },
  directionBtnTextActive: { color: '#ffffff' },
  amountRow: { flexDirection: 'row', gap: 8, marginTop: 12 },
  dropdown: {
    borderWidth: 1,
    borderColor: '#d1d5db',
    backgroundColor: '#ffffff',
    borderRadius: 8,
    paddingHorizontal: 12,
    paddingVertical: 10,
  },
  dropdownText: { color: '#111827', fontSize: 14 },
  input: {
    borderWidth: 1,
    borderColor: '#d1d5db',
    borderRadius: 8,
    paddingHorizontal: 12,
    paddingVertical: 8,
    color: '#111827',
    fontSize: 14,
    backgroundColor: '#ffffff',
  },
  primaryBtn: {
    backgroundColor: '#4f46e5',
    borderRadius: 8,
    paddingVertical: 12,
    marginTop: 12,
    alignItems: 'center',
  },
  primaryBtnText: { color: '#ffffff', fontSize: 14, fontWeight: '600' },
  btnDisabled: { opacity: 0.5 },
  listHeader: {
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'center',
    marginTop: 4,
  },
  sectionTitle: { fontSize: 13, fontWeight: '600', color: '#111827' },
  linkBtn: { color: '#4f46e5', fontSize: 12, fontWeight: '500' },
  muted: { color: '#9ca3af', fontSize: 13 },
  row: {
    flexDirection: 'row',
    paddingVertical: 12,
    borderTopWidth: 1,
    borderTopColor: '#f3f4f6',
    gap: 8,
    alignItems: 'flex-start',
  },
  rowHead: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  rowDirection: { fontSize: 14, fontWeight: '600', color: '#111827' },
  rowAmount: { fontSize: 14, color: '#111827', marginTop: 4, fontVariant: ['tabular-nums'] },
  rowMethod: { fontSize: 12, color: '#6b7280', fontWeight: '400' },
  rowMeta: { fontSize: 11, color: '#9ca3af', marginTop: 2 },
  rowMetaError: { fontSize: 11, color: '#b91c1c', marginTop: 2 },
  rowMetaWarn: { fontSize: 11, color: '#b45309', marginTop: 2 },
  badge: {
    paddingHorizontal: 8,
    paddingVertical: 2,
    borderRadius: 999,
  },
  badgeText: { fontSize: 11, color: '#374151' },
  dangerBtn: {
    paddingHorizontal: 12,
    paddingVertical: 8,
    borderRadius: 8,
    borderWidth: 1,
    borderColor: '#fecaca',
    backgroundColor: '#fff',
  },
  dangerBtnText: { color: '#b91c1c', fontSize: 12, fontWeight: '500' },
  modalBackdrop: {
    flex: 1,
    backgroundColor: 'rgba(0,0,0,0.4)',
    justifyContent: 'flex-end',
  },
  modalSheet: {
    backgroundColor: '#ffffff',
    borderTopLeftRadius: 16,
    borderTopRightRadius: 16,
    padding: 16,
    maxHeight: '60%',
  },
  modalTitle: { fontSize: 14, fontWeight: '600', color: '#111827', marginBottom: 8 },
  modalItem: { paddingVertical: 12, borderBottomWidth: 1, borderBottomColor: '#f3f4f6' },
  modalItemText: { fontSize: 14, color: '#374151' },
});
