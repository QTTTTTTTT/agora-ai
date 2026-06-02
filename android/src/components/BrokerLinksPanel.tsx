/**
 * BrokerLinksPanel — Android (React Native) component.
 *
 * Companion to web's BrokerLinksSection.tsx. Surfaces the broker-
 * link self-service flow inside the Orders screen as a collapsible
 * card. Why "collapsible card on the orders screen" rather than
 * "dedicated screen":
 *   - the orders screen is already the per-fund context;
 *   - each broker-link request is a low-frequency action (set
 *     once, revoke rarely);
 *   - we don't want an extra route + bottom-tab surface for what
 *     is fundamentally a 3-button workflow.
 *
 * State
 *
 *  - `links` is the list returned by GET /broker-links. We sort
 *    active first, then pending, then everything else (newest).
 *  - `expanded` defaults to false to avoid pushing the orders list
 *    down for users who never need this.
 *  - `submitting` / `revokingId` are the only mutation flags. We
 *    deliberately don't optimistically patch the row — the
 *    user-perceived latency on a single row is already <500 ms in
 *    the staging deployment, and an optimistic patch would
 *    complicate the confirm dialog.
 */

import React, { useCallback, useEffect, useMemo, useState } from 'react';
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
import type { BrokerLinkRow } from '@fundai/api-client';

interface Props {
  fundId?: string;
}

const BROKERS: { id: string; label: string }[] = [
  { id: 'ibkr', label: 'Interactive Brokers' },
  { id: 'futu', label: 'Futu' },
  { id: 'alpaca', label: 'Alpaca' },
  { id: 'binance', label: 'Binance' },
  { id: 'mock', label: 'Mock' },
];

export default function BrokerLinksPanel({ fundId }: Props): JSX.Element | null {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(false);
  const [links, setLinks] = useState<BrokerLinkRow[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [refreshKey, setRefreshKey] = useState(0);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [brokerId, setBrokerId] = useState(BROKERS[0].id);
  const [accountId, setAccountId] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [revokingId, setRevokingId] = useState<string | null>(null);

  const refresh = useCallback(() => setRefreshKey((k) => k + 1), []);

  useEffect(() => {
    if (!expanded || !fundId) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    apiClient
      .listBrokerLinks(fundId)
      .then((rows) => {
        if (!cancelled) setLinks(rows);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        // 403/404 → user can't see the fund's broker links. Show
        // empty state rather than error — the parent screen
        // already displays the "permission denied" hint.
        const status = (err as { status?: number })?.status;
        if (status === 403 || status === 404) {
          setLinks([]);
          return;
        }
        setError(`${t('brokerLinks.errorPrefix')}${(err as Error)?.message ?? 'unknown'}`);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [expanded, fundId, refreshKey, t]);

  const onSubmit = async () => {
    if (!fundId || !accountId.trim()) return;
    setSubmitting(true);
    setError(null);
    try {
      await apiClient.requestBrokerLink({
        fundId,
        brokerId,
        accountId: accountId.trim(),
      });
      setAccountId('');
      refresh();
    } catch (err) {
      setError(`${t('brokerLinks.errorPrefix')}${(err as Error)?.message ?? 'unknown'}`);
    } finally {
      setSubmitting(false);
    }
  };

  const onRevoke = (link: BrokerLinkRow) => {
    if (!fundId) return;
    Alert.alert(
      t('brokerLinks.revoke'),
      t('brokerLinks.confirmRevoke'),
      [
        { text: 'Cancel', style: 'cancel' },
        {
          text: t('brokerLinks.revoke'),
          style: 'destructive',
          onPress: async () => {
            setRevokingId(link.id);
            setError(null);
            try {
              await apiClient.revokeBrokerLink({ fundId, linkId: link.id });
              refresh();
            } catch (err) {
              setError(`${t('brokerLinks.errorPrefix')}${(err as Error)?.message ?? 'unknown'}`);
            } finally {
              setRevokingId(null);
            }
          },
        },
      ],
      { cancelable: true },
    );
  };

  // Sort: active first, then pending, then everything else newest first.
  // Mirrors the web component so the priority order is identical.
  const sorted = useMemo(() => {
    if (!links) return [];
    const order = (s: BrokerLinkRow['status']) =>
      s === 'active' ? 0 : s === 'pending' ? 1 : 2;
    return [...links].sort((a, b) => {
      const oa = order(a.status);
      const ob = order(b.status);
      if (oa !== ob) return oa - ob;
      return b.createdAt.localeCompare(a.createdAt);
    });
  }, [links]);

  if (!fundId) return null;

  return (
    <View style={styles.card}>
      <Pressable
        accessibilityRole="button"
        onPress={() => setExpanded((v) => !v)}
        style={styles.header}
      >
        <View style={{ flex: 1 }}>
          <Text style={styles.title}>{t('brokerLinks.title')}</Text>
          {!expanded ? <Text style={styles.subtitle}>{t('brokerLinks.subtitle')}</Text> : null}
        </View>
        <Text style={styles.toggle}>{expanded ? '▲' : '▼'}</Text>
      </Pressable>

      {expanded ? (
        <View style={styles.body}>
          {error ? <Text style={styles.error}>{error}</Text> : null}

          <View style={styles.formCard}>
            <Text style={styles.formTitle}>{t('brokerLinks.formTitle')}</Text>
            <Text style={styles.formLabel}>{t('brokerLinks.formBroker')}</Text>
            <Pressable
              style={styles.dropdown}
              onPress={() => setPickerOpen(true)}
              accessibilityRole="button"
            >
              <Text style={styles.dropdownText}>
                {BROKERS.find((b) => b.id === brokerId)?.label ?? brokerId}
              </Text>
            </Pressable>

            <Text style={[styles.formLabel, { marginTop: 12 }]}>
              {t('brokerLinks.formAccountId')}
            </Text>
            <TextInput
              value={accountId}
              onChangeText={setAccountId}
              placeholder={t('brokerLinks.formAccountIdPlaceholder')}
              placeholderTextColor="#9ca3af"
              autoCapitalize="characters"
              style={styles.input}
              editable={!submitting}
            />
            <Text style={styles.formNote}>{t('brokerLinks.formNote')}</Text>
            <Pressable
              style={[styles.primaryBtn, (!accountId.trim() || submitting) && styles.btnDisabled]}
              disabled={!accountId.trim() || submitting}
              onPress={onSubmit}
            >
              <Text style={styles.primaryBtnText}>
                {submitting ? t('brokerLinks.formSubmitting') : t('brokerLinks.formSubmit')}
              </Text>
            </Pressable>
          </View>

          <View style={styles.listHeader}>
            <Text style={styles.sectionTitle}>
              {t('brokerLinks.title')}
            </Text>
            <Pressable onPress={refresh} disabled={loading} accessibilityRole="button">
              <Text style={[styles.linkBtn, loading && { opacity: 0.5 }]}>
                {loading ? t('brokerLinks.loading') : t('brokerLinks.refresh')}
              </Text>
            </Pressable>
          </View>

          {loading ? (
            <Text style={styles.muted}>{t('brokerLinks.loading')}</Text>
          ) : sorted.length === 0 ? (
            <Text style={styles.muted}>{t('brokerLinks.empty')}</Text>
          ) : (
            sorted.map((link) => (
              <View key={link.id} style={styles.row}>
                <View style={{ flex: 1 }}>
                  <View style={styles.rowHead}>
                    <Text style={styles.rowBroker}>{link.brokerId.toUpperCase()}</Text>
                    <View style={[styles.badge, badgeStyle(link.status)]}>
                      <Text style={styles.badgeText}>{statusLabel(link.status, t)}</Text>
                    </View>
                  </View>
                  <Text style={styles.rowAccount}>{link.accountId}</Text>
                  <Text style={styles.rowMeta}>
                    {new Date(link.createdAt).toLocaleString()}
                  </Text>
                </View>
                {link.status === 'active' || link.status === 'pending' ? (
                  <Pressable
                    style={[styles.dangerBtn, revokingId === link.id && styles.btnDisabled]}
                    onPress={() => onRevoke(link)}
                    disabled={revokingId === link.id}
                  >
                    <Text style={styles.dangerBtnText}>
                      {revokingId === link.id
                        ? t('brokerLinks.revoking')
                        : t('brokerLinks.revoke')}
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
            <Text style={styles.modalTitle}>{t('brokerLinks.formBroker')}</Text>
            <ScrollView>
              {BROKERS.map((b) => (
                <Pressable
                  key={b.id}
                  style={styles.modalItem}
                  onPress={() => {
                    setBrokerId(b.id);
                    setPickerOpen(false);
                  }}
                >
                  <Text
                    style={[
                      styles.modalItemText,
                      brokerId === b.id && { fontWeight: '600', color: '#4f46e5' },
                    ]}
                  >
                    {b.label}
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

function statusLabel(s: BrokerLinkRow['status'], t: (key: string) => string): string {
  switch (s) {
    case 'active':
      return t('brokerLinks.statusActive');
    case 'pending':
      return t('brokerLinks.statusPending');
    case 'suspended':
      return t('brokerLinks.statusSuspended');
    case 'revoked':
      return t('brokerLinks.statusRevoked');
    default:
      return s;
  }
}

function badgeStyle(s: BrokerLinkRow['status']) {
  switch (s) {
    case 'active':
      return { backgroundColor: '#d1fae5' };
    case 'pending':
      return { backgroundColor: '#fef3c7' };
    case 'suspended':
      return { backgroundColor: '#fed7aa' };
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
  body: {
    paddingHorizontal: 14,
    paddingBottom: 14,
    gap: 12,
  },
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
    backgroundColor: '#ffffff',
    borderRadius: 8,
    paddingHorizontal: 12,
    paddingVertical: 10,
    fontSize: 14,
    color: '#111827',
  },
  primaryBtn: {
    marginTop: 10,
    backgroundColor: '#4f46e5',
    borderRadius: 8,
    paddingVertical: 10,
    alignItems: 'center',
  },
  primaryBtnText: { color: '#ffffff', fontSize: 14, fontWeight: '600' },
  btnDisabled: { opacity: 0.6 },
  listHeader: {
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'center',
    marginTop: 4,
  },
  sectionTitle: { fontSize: 13, fontWeight: '600', color: '#111827' },
  linkBtn: { color: '#4f46e5', fontSize: 13, fontWeight: '500' },
  muted: { color: '#6b7280', fontSize: 13 },
  row: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 10,
    paddingVertical: 10,
    borderTopWidth: 1,
    borderTopColor: '#f3f4f6',
  },
  rowHead: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  rowBroker: { fontSize: 14, fontWeight: '600', color: '#111827' },
  rowAccount: { fontFamily: 'monospace', fontSize: 13, color: '#374151', marginTop: 2 },
  rowMeta: { fontSize: 11, color: '#9ca3af', marginTop: 2 },
  badge: { borderRadius: 999, paddingHorizontal: 8, paddingVertical: 2 },
  badgeText: { fontSize: 11, fontWeight: '500', color: '#1f2937' },
  dangerBtn: {
    borderWidth: 1,
    borderColor: '#fca5a5',
    backgroundColor: '#ffffff',
    borderRadius: 8,
    paddingHorizontal: 12,
    paddingVertical: 8,
  },
  dangerBtnText: { color: '#b91c1c', fontSize: 13, fontWeight: '500' },
  modalBackdrop: {
    flex: 1,
    backgroundColor: 'rgba(0,0,0,0.4)',
    justifyContent: 'flex-end',
  },
  modalSheet: {
    backgroundColor: '#ffffff',
    borderTopLeftRadius: 16,
    borderTopRightRadius: 16,
    paddingVertical: 18,
    paddingHorizontal: 16,
    maxHeight: '60%',
  },
  modalTitle: {
    fontSize: 14,
    fontWeight: '600',
    color: '#111827',
    marginBottom: 8,
  },
  modalItem: { paddingVertical: 12 },
  modalItemText: { fontSize: 15, color: '#111827' },
});
