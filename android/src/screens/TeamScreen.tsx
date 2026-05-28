/**
 * TeamScreen — agent 团队列表（按 active fund）。
 *
 * 接入 apiClient.listTeam(fundId)；mock 路径已去掉 — fallback 是
 * 直接显示 empty 提示并允许下拉刷新重试。
 */

import React from 'react';
import {
  ActivityIndicator,
  FlatList,
  Pressable,
  RefreshControl,
  StyleSheet,
  Text,
  View,
} from 'react-native';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import { apiClient } from '../lib/api';
import { useActiveFund } from '../lib/activeFund';
import type { TeamMemberSummary } from '@fundai/api-client';

const roleColors: Record<string, string> = {
  pm: '#dbeafe',
  risk: '#fee2e2',
  researcher: '#fef3c7',
  trader: '#dcfce7',
};

export default function TeamScreen(): JSX.Element {
  const { t } = useTranslation();
  const { fundId } = useActiveFund();
  const { data, isLoading, isFetching, isError, refetch } = useQuery({
    queryKey: ['team', fundId],
    enabled: !!fundId,
    queryFn: async () => apiClient.listTeam(fundId!),
  });

  if (!fundId) {
    // No active fund yet (fresh install / user has no funds).
    // 真正的 empty 状态 — 与"加载失败"严格区分。
    return (
      <View style={styles.center}>
        <Text style={styles.muted}>{t('team.empty')}</Text>
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

  // 网络/5xx 失败：明确显示错误 + retry。之前会退化为 empty 文案，
  // 用户无法分辨"今天还没人"与"接口挂了"。
  if (isError) {
    return (
      <View style={styles.center}>
        <Text style={styles.errorText}>{t('team.error')}</Text>
        <Pressable
          onPress={() => void refetch()}
          accessibilityRole="button"
          accessibilityLabel={t('team.retry')}
          style={styles.retryBtn}
        >
          <Text style={styles.retryBtnLabel}>{t('team.retry')}</Text>
        </Pressable>
      </View>
    );
  }

  const members = data?.members ?? [];

  return (
    <FlatList
      data={members}
      keyExtractor={(m: TeamMemberSummary) => m.member_id}
      contentContainerStyle={styles.container}
      refreshControl={<RefreshControl refreshing={isFetching} onRefresh={() => void refetch()} />}
      ListEmptyComponent={<Text style={styles.muted}>{t('team.empty')}</Text>}
      renderItem={({ item }) => (
        <View style={[styles.card, { backgroundColor: roleColors[item.role] ?? '#ffffff' }]}>
          <Text style={styles.name}>{item.name ?? item.agent_id}</Text>
          <Text style={styles.role}>{item.role.toUpperCase()}</Text>
          {item.focus ? <Text style={styles.focus}>{item.focus}</Text> : null}
          {item.model_provider || item.model_name ? (
            <Text style={styles.model}>
              {[item.model_provider, item.model_name].filter(Boolean).join(' · ')}
            </Text>
          ) : null}
        </View>
      )}
    />
  );
}

const styles = StyleSheet.create({
  container: { padding: 16 },
  center: { flex: 1, alignItems: 'center', justifyContent: 'center', padding: 24 },
  card: { borderRadius: 12, padding: 16, marginBottom: 12 },
  name: { fontSize: 16, fontWeight: '600', color: '#111827' },
  role: { fontSize: 11, color: '#1f2937', marginTop: 2, fontWeight: '500' },
  focus: { fontSize: 13, color: '#374151', marginTop: 8 },
  model: { fontSize: 11, color: '#6b7280', marginTop: 4, fontStyle: 'italic' },
  muted: { color: '#9ca3af', textAlign: 'center', marginTop: 32 },
  errorText: { color: '#dc2626', fontSize: 14, textAlign: 'center' },
  retryBtn: {
    marginTop: 16,
    backgroundColor: '#4f46e5',
    paddingHorizontal: 24,
    paddingVertical: 10,
    borderRadius: 8,
  },
  retryBtnLabel: { color: '#ffffff', fontWeight: '600', fontSize: 14 },
});
