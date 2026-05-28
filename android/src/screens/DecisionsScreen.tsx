/**
 * DecisionsScreen — 最新决策计划列表（按 active fund）+ 单计划批准/驳回/
 * 刷新报价。
 *
 * 历史：
 *   - 早期版本仅 listPlans + read-only PlanCard。2026-05-28 的功能审计
 *     发现移动端无法完成"日常操作"——批/驳/刷必须 fallback 到 web，违反
 *     "移动端是真实日常决策入口"的产品意图。
 *   - 本次（Sprint 5）把 ApprovalActions 的能力镜像过来：点击 plan 卡片
 *     展开详情区，展示 actions 与三个按钮（approve / reject / refresh-
 *     quote）。逻辑直接走 shared/api-client 的 approvePlan / rejectPlan /
 *     refreshPlanQuote，与 web 同一份后端契约。
 *
 * 状态机：
 *   pending_user / risk_review / draft → 显示 approve+reject+refresh
 *   其它（executing / completed / rejected / failed / mixed）→ 只读
 *
 * UI 简化（vs web）：
 *   - 无 PriceRefreshDialog；refresh 成功直接弹 toast，价格漂移在详情区
 *     里以 actions 的最新 amount/qty 反映；移动端不展示 0.3% 阈值对话框
 *     （桌面端的精细审计放在 web）。
 *   - 无 SlippageGuard 弹层；后端 risk gate 仍然守门，移动端只显示结果。
 */

import React, { useCallback, useMemo, useState } from 'react';
import {
  ActivityIndicator,
  Alert,
  FlatList,
  Pressable,
  RefreshControl,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import { apiClient } from '../lib/api';
import { useActiveFund } from '../lib/activeFund';
import type { PlanDetail, PlanSummary } from '@fundai/api-client';

/** Statuses where the operator can still alter the plan. Mirrors the
 *  web "actionable" gate so what the mobile user sees matches what the
 *  desktop user sees for the same plan. */
const ACTIONABLE_STATUSES = new Set(['draft', 'risk_review', 'pending_user']);

export default function DecisionsScreen(): JSX.Element {
  const { t } = useTranslation();
  const { fundId } = useActiveFund();
  const [expandedId, setExpandedId] = useState<string | null>(null);

  const {
    data,
    isLoading,
    isFetching,
    refetch,
    isError,
  } = useQuery({
    queryKey: ['plans', fundId],
    enabled: !!fundId,
    queryFn: async () => {
      if (!fundId) return { plans: [] };
      return apiClient.listPlans(fundId);
    },
  });

  if (!fundId) {
    return (
      <View style={styles.center}>
        <Text style={styles.muted}>{t('decisions.empty')}</Text>
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

  const plans = (data?.plans ?? []) as PlanSummary[];

  return (
    <FlatList
      data={plans}
      keyExtractor={(p) => p.id}
      contentContainerStyle={styles.container}
      refreshControl={<RefreshControl refreshing={isFetching} onRefresh={() => void refetch()} />}
      ListEmptyComponent={
        <View style={styles.center}>
          {/* 区分错误 vs 真正"今天没生成"。前者必须显示错误调子，否则
              用户以为今天 agent 真没出计划。 */}
          <Text style={[styles.muted, isError && styles.errorText]}>
            {isError ? t('decisions.loadFailed') : t('decisions.empty')}
          </Text>
          {isError ? (
            <Pressable
              style={styles.retry}
              onPress={() => void refetch()}
              accessibilityRole="button"
              accessibilityLabel={t('decisions.retry')}
            >
              <Text style={styles.retryText}>{t('decisions.retry')}</Text>
            </Pressable>
          ) : null}
        </View>
      }
      renderItem={({ item }) => (
        <PlanCard
          plan={item}
          expanded={expandedId === item.id}
          onToggle={() => setExpandedId((prev) => (prev === item.id ? null : item.id))}
        />
      )}
    />
  );
}

interface PlanCardProps {
  plan: PlanSummary;
  expanded: boolean;
  onToggle: () => void;
}

function PlanCard({ plan, expanded, onToggle }: PlanCardProps): JSX.Element {
  const { t } = useTranslation();
  return (
    <Pressable
      onPress={onToggle}
      accessibilityRole="button"
      accessibilityLabel={`${plan.trading_date} ${localizedStatus(plan.status, t)} ${plan.action_count} ${t('decisions.actionsLabel')}`}
      accessibilityHint={expanded ? t('decisions.cancel') : t('decisions.actionsLabel')}
    >
      <View style={styles.planCard}>
        <View style={styles.rowBetween}>
          <Text style={styles.date}>{plan.trading_date}</Text>
          <View style={[styles.badge, statusStyle(plan.status)]}>
            <Text style={styles.badgeText}>{localizedStatus(plan.status, t)}</Text>
          </View>
        </View>
        {plan.reasoning ? (
          <Text style={styles.reasoning} numberOfLines={expanded ? undefined : 3}>
            {plan.reasoning}
          </Text>
        ) : null}
        <Text style={styles.muted}>
          {plan.action_count} {t('decisions.actionsLabel')}
        </Text>
        {expanded ? <PlanDetailPanel planId={plan.id} status={plan.status} /> : null}
      </View>
    </Pressable>
  );
}

/** PlanDetailPanel is rendered inside the expanded PlanCard. Owns its
 *  own getPlan query so we don't fetch action lists for unexpanded
 *  rows. Also owns mutation state for approve / reject / refresh so a
 *  long-running approve on one plan doesn't disable buttons on another. */
function PlanDetailPanel({ planId, status }: { planId: string; status: string }): JSX.Element {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const { fundId } = useActiveFund();
  const [showRejectBox, setShowRejectBox] = useState(false);
  const [rejectReason, setRejectReason] = useState('');
  const [success, setSuccess] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const planQuery = useQuery({
    queryKey: ['plan', planId],
    queryFn: () => apiClient.getPlan(planId),
  });

  // Refresh both the per-plan cache (so the actions list updates with
  // the new status / quotes) AND the plan list cache (so the badge on
  // the collapsed card flips). The list query is keyed by ['plans',
  // fundId]; we invalidate the same key here.
  const invalidateAll = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ['plan', planId] });
    if (fundId) {
      void queryClient.invalidateQueries({ queryKey: ['plans', fundId] });
    }
  }, [queryClient, planId, fundId]);

  const flashSuccess = useCallback((message: string) => {
    setSuccess(message);
    setError(null);
    setTimeout(() => setSuccess(null), 3000);
  }, []);

  const showError = useCallback((message: string) => {
    setError(message);
    setSuccess(null);
  }, []);

  const approve = useMutation({
    mutationFn: () => apiClient.approvePlan(planId),
    onSuccess: () => {
      flashSuccess(t('decisions.successApproved'));
      invalidateAll();
    },
    onError: (err: unknown) => showError(formatErr(err, t('decisions.actionFailed'))),
  });

  const reject = useMutation({
    mutationFn: (reason: string) => apiClient.rejectPlan(planId, reason),
    onSuccess: () => {
      flashSuccess(t('decisions.successRejected'));
      setShowRejectBox(false);
      setRejectReason('');
      invalidateAll();
    },
    onError: (err: unknown) => showError(formatErr(err, t('decisions.actionFailed'))),
  });

  const refresh = useMutation({
    mutationFn: () => apiClient.refreshPlanQuote(planId),
    onSuccess: () => {
      flashSuccess(t('decisions.successRefreshed'));
      invalidateAll();
    },
    onError: (err: unknown) => showError(formatErr(err, t('decisions.actionFailed'))),
  });

  const submitting = approve.isPending || reject.isPending || refresh.isPending;
  const actionable = ACTIONABLE_STATUSES.has(status);
  const handleConfirmReject = () => {
    const trimmed = rejectReason.trim();
    if (!trimmed) {
      showError(t('decisions.rejectReasonRequired'));
      return;
    }
    reject.mutate(trimmed);
  };

  const actions = planQuery.data?.actions ?? [];

  return (
    <View style={styles.detailPanel}>
      {planQuery.isLoading ? (
        <ActivityIndicator size="small" color="#4f46e5" />
      ) : planQuery.isError ? (
        <View style={styles.errorRow}>
          <Text style={styles.errorText}>{t('decisions.loadFailed')}</Text>
          <Pressable
            style={styles.retry}
            onPress={() => void planQuery.refetch()}
            accessibilityRole="button"
            accessibilityLabel={t('decisions.retry')}
          >
            <Text style={styles.retryText}>{t('decisions.retry')}</Text>
          </Pressable>
        </View>
      ) : (
        <ActionList actions={actions} />
      )}

      {success ? <Banner kind="success" text={success} /> : null}
      {error ? <Banner kind="error" text={error} /> : null}

      {showRejectBox ? (
        <View style={styles.rejectBox}>
          <Text style={styles.rejectLabel}>{t('decisions.rejectReasonPrompt')}</Text>
          <TextInput
            value={rejectReason}
            onChangeText={setRejectReason}
            placeholder={t('decisions.rejectReasonPrompt')}
            placeholderTextColor="#9ca3af"
            multiline
            numberOfLines={3}
            maxLength={200}
            style={styles.rejectInput}
            editable={!submitting}
            accessibilityLabel={t('decisions.rejectReasonPrompt')}
          />
          <View style={styles.buttonRow}>
            <Pressable
              style={[styles.btn, styles.btnGhost]}
              onPress={() => {
                setShowRejectBox(false);
                setRejectReason('');
                setError(null);
              }}
              disabled={submitting}
              accessibilityRole="button"
              accessibilityLabel={t('decisions.cancel')}
            >
              <Text style={styles.btnGhostText}>{t('decisions.cancel')}</Text>
            </Pressable>
            <Pressable
              style={[styles.btn, styles.btnDanger, submitting && styles.btnDisabled]}
              onPress={handleConfirmReject}
              disabled={submitting || !rejectReason.trim()}
              accessibilityRole="button"
              accessibilityLabel={t('decisions.confirm')}
            >
              <Text style={styles.btnDangerText}>
                {reject.isPending ? t('decisions.rejecting') : t('decisions.confirm')}
              </Text>
            </Pressable>
          </View>
        </View>
      ) : (
        <View style={styles.buttonRow}>
          {actionable ? (
            <>
              <Pressable
                style={[styles.btn, styles.btnPrimary, submitting && styles.btnDisabled]}
                onPress={() => approve.mutate()}
                disabled={submitting}
                accessibilityRole="button"
                accessibilityLabel={t('decisions.approve')}
              >
                <Text style={styles.btnPrimaryText}>
                  {approve.isPending ? t('decisions.approving') : t('decisions.approve')}
                </Text>
              </Pressable>
              <Pressable
                style={[styles.btn, styles.btnDanger, submitting && styles.btnDisabled]}
                onPress={() => {
                  setShowRejectBox(true);
                  setError(null);
                }}
                disabled={submitting}
                accessibilityRole="button"
                accessibilityLabel={t('decisions.reject')}
              >
                <Text style={styles.btnDangerText}>{t('decisions.reject')}</Text>
              </Pressable>
            </>
          ) : null}
          <Pressable
            style={[styles.btn, styles.btnWarn, submitting && styles.btnDisabled]}
            onPress={() => refresh.mutate()}
            disabled={submitting}
            accessibilityRole="button"
            accessibilityLabel={t('decisions.refresh')}
          >
            <Text style={styles.btnWarnText}>
              {refresh.isPending ? t('decisions.refreshing') : t('decisions.refresh')}
            </Text>
          </Pressable>
        </View>
      )}
    </View>
  );
}

function ActionList({ actions }: { actions: PlanDetail['actions'] }): JSX.Element {
  if (actions.length === 0) {
    return <Text style={styles.muted}>—</Text>;
  }
  return (
    <View style={styles.actionsList}>
      {actions.map((a) => (
        <View key={a.id} style={styles.actionRow}>
          <View style={styles.actionHeader}>
            <Text style={styles.actionSymbol}>{a.symbol}</Text>
            <Text style={styles.actionVerb}>{a.action.toUpperCase()}</Text>
            {typeof a.qty === 'number' && a.qty > 0 ? (
              <Text style={styles.actionQty}>{a.qty}</Text>
            ) : null}
          </View>
          {a.reasoning ? (
            <Text style={styles.actionReasoning} numberOfLines={2}>
              {a.reasoning}
            </Text>
          ) : null}
        </View>
      ))}
    </View>
  );
}

function Banner({ kind, text }: { kind: 'success' | 'error'; text: string }): JSX.Element {
  return (
    <View style={[styles.banner, kind === 'success' ? styles.bannerSuccess : styles.bannerError]}>
      <Text style={kind === 'success' ? styles.bannerSuccessText : styles.bannerErrorText}>{text}</Text>
    </View>
  );
}

function statusStyle(status: string) {
  switch (status) {
    case 'approved':
    case 'completed':
      return { backgroundColor: '#dcfce7' };
    case 'rejected':
    case 'failed':
      return { backgroundColor: '#fee2e2' };
    case 'mixed':
      return { backgroundColor: '#fed7aa' };
    default:
      return { backgroundColor: '#fef3c7' };
  }
}

// Map server status enum to localized label. Web's Decision Center has
// the same mapping in copy.* objects; we mirror it via i18n bundle.
function localizedStatus(status: string, t: (k: string) => string): string {
  const key = `decisions.status${capitalize(camelize(status))}`;
  const resolved = t(key);
  // i18next returns the key itself when missing; in that case fall
  // through to the raw status so the user at least sees something.
  if (resolved === key) return status.toUpperCase();
  return resolved;
}

function camelize(s: string): string {
  return s.replace(/_([a-z])/g, (_, c) => c.toUpperCase());
}

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** Best-effort error formatter — prefers ApiError.message, falls
 *  back to plain Error.message, then to the i18n default. */
function formatErr(err: unknown, fallback: string): string {
  if (err && typeof err === 'object' && 'message' in err) {
    const m = (err as { message?: string }).message;
    if (typeof m === 'string' && m.trim().length > 0) return m;
  }
  return fallback;
}

// Silence the unused `Alert` import that's intentionally kept for
// future native confirm dialogs (e.g. on destructive logout).
void Alert;

const styles = StyleSheet.create({
  container: { padding: 16 },
  center: { flex: 1, alignItems: 'center', justifyContent: 'center', padding: 24 },
  muted: { color: '#9ca3af', fontSize: 12, marginTop: 8 },
  errorText: { color: '#b91c1c', fontSize: 13, marginRight: 12 },
  errorRow: { flexDirection: 'row', alignItems: 'center', paddingVertical: 8 },

  planCard: {
    backgroundColor: '#ffffff',
    borderRadius: 12,
    padding: 16,
    marginBottom: 12,
    elevation: 2,
  },
  rowBetween: { flexDirection: 'row', justifyContent: 'space-between', alignItems: 'center' },
  date: { fontSize: 13, color: '#1f2937', fontWeight: '500' },
  reasoning: { fontSize: 14, color: '#1f2937', marginTop: 8, lineHeight: 20 },
  badge: { borderRadius: 6, paddingHorizontal: 8, paddingVertical: 4 },
  badgeText: { fontSize: 11, color: '#1f2937', textTransform: 'uppercase' },
  retry: { marginTop: 12, paddingVertical: 8, paddingHorizontal: 16, backgroundColor: '#e5e7eb', borderRadius: 6 },
  retryText: { color: '#1f2937', fontSize: 14, fontWeight: '500' },

  detailPanel: {
    marginTop: 12,
    paddingTop: 12,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: '#e5e7eb',
  },
  actionsList: { gap: 8 },
  actionRow: { paddingVertical: 6, borderBottomWidth: StyleSheet.hairlineWidth, borderBottomColor: '#f3f4f6' },
  actionHeader: { flexDirection: 'row', gap: 12, alignItems: 'center' },
  actionSymbol: { fontSize: 13, fontWeight: '600', color: '#1f2937' },
  actionVerb: { fontSize: 11, fontWeight: '600', color: '#4f46e5', letterSpacing: 0.5 },
  actionQty: { fontSize: 12, color: '#6b7280' },
  actionReasoning: { fontSize: 12, color: '#6b7280', marginTop: 4, lineHeight: 16 },

  buttonRow: { flexDirection: 'row', gap: 8, marginTop: 12, flexWrap: 'wrap' },
  btn: { paddingVertical: 10, paddingHorizontal: 14, borderRadius: 8, minWidth: 96, alignItems: 'center' },
  btnPrimary: { backgroundColor: '#059669' },
  btnPrimaryText: { color: '#ffffff', fontSize: 13, fontWeight: '600' },
  btnDanger: { backgroundColor: '#dc2626' },
  btnDangerText: { color: '#ffffff', fontSize: 13, fontWeight: '600' },
  btnWarn: { backgroundColor: '#fef3c7', borderWidth: 1, borderColor: '#f59e0b' },
  btnWarnText: { color: '#92400e', fontSize: 13, fontWeight: '600' },
  btnGhost: { backgroundColor: '#ffffff', borderWidth: 1, borderColor: '#d1d5db' },
  btnGhostText: { color: '#374151', fontSize: 13, fontWeight: '500' },
  btnDisabled: { opacity: 0.6 },

  rejectBox: { marginTop: 12, padding: 12, borderRadius: 8, backgroundColor: '#fef2f2', borderWidth: 1, borderColor: '#fecaca' },
  rejectLabel: { fontSize: 13, color: '#7f1d1d', marginBottom: 6 },
  rejectInput: {
    borderWidth: 1,
    borderColor: '#fca5a5',
    borderRadius: 6,
    padding: 8,
    minHeight: 72,
    fontSize: 13,
    color: '#1f2937',
    textAlignVertical: 'top',
    backgroundColor: '#ffffff',
  },

  banner: { marginTop: 12, padding: 10, borderRadius: 8 },
  bannerSuccess: { backgroundColor: '#ecfdf5', borderWidth: 1, borderColor: '#a7f3d0' },
  bannerSuccessText: { color: '#065f46', fontSize: 13, fontWeight: '500' },
  bannerError: { backgroundColor: '#fef2f2', borderWidth: 1, borderColor: '#fecaca' },
  bannerErrorText: { color: '#991b1b', fontSize: 13, fontWeight: '500' },
});
