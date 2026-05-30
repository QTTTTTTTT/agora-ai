/**
 * HomeScreen — 公司 + 基金概览，并触发当前 activeFund 选择。
 *
 * - 首次拿到 fund 列表时如果没有 activeFundId，自动选第一支。
 * - 用户点 fund 卡片时切换 activeFund（高亮当前选中）。
 * - 任何 setSession token = null 后，react-query cache 仍保留（用 mmkv 持久化）—
 *   下次登录用同一账号能"瞬时"看到上一会话的数据。
 */

import React, { useEffect } from 'react';
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
import type { CompanyWithFunds, FundSummary } from '@fundai/api-client';

import { apiClient } from '../lib/api';
import { useActiveFund } from '../lib/activeFund';
import { CorpActionTimelineCard } from '../components/CorpActionTimelineCard';
import { BenchmarkMiniChart } from '../components/BenchmarkMiniChart';
import { HoldingsTrendsGrid } from '../components/HoldingsTrendsGrid';

export default function HomeScreen(): JSX.Element {
  const { t } = useTranslation();
  const { fundId, setFund } = useActiveFund();
  const { data, isLoading, isError, refetch, isFetching } = useQuery({
    queryKey: ['companies'],
    queryFn: async () => {
      const resp = await apiClient.listCompanies();
      return resp.companies;
    },
    retry: 1,
  });

  useEffect(() => {
    if (!fundId && data && data.length > 0 && data[0].funds.length > 0) {
      setFund(data[0].funds[0].id);
    }
  }, [data, fundId, setFund]);

  if (isLoading) {
    return (
      <View style={styles.center}>
        <ActivityIndicator size="large" color="#4f46e5" />
        <Text style={styles.muted}>{t('home.loading')}</Text>
      </View>
    );
  }

  if (isError || !data) {
    return (
      <View style={styles.center}>
        <Text style={styles.errorText}>{t('home.error')}</Text>
        <Pressable
          style={styles.retry}
          onPress={() => void refetch()}
          accessibilityRole="button"
          accessibilityLabel={t('home.retry')}
        >
          {/* 之前这里显示 t('home.loading') —— 用户看到"加载中"但其实
              是请求失败，按钮文案与行为对不上。改回正确的 retry 文案。 */}
          <Text style={styles.retryText}>{t('home.retry')}</Text>
        </Pressable>
      </View>
    );
  }

  return (
    <FlatList
      data={data}
      keyExtractor={(c) => c.id}
      contentContainerStyle={styles.container}
      refreshControl={<RefreshControl refreshing={isFetching} onRefresh={() => void refetch()} />}
      ListEmptyComponent={<Text style={styles.muted}>{t('home.empty')}</Text>}
      renderItem={({ item }) => <CompanyCard company={item} activeFundId={fundId} setFund={setFund} />}
    />
  );
}

function CompanyCard({
  company,
  activeFundId,
  setFund,
}: {
  company: CompanyWithFunds;
  activeFundId: string | null;
  setFund: (id: string | null) => void;
}): JSX.Element {
  return (
    <View style={styles.companyCard}>
      <Text style={styles.companyName}>{company.name}</Text>
      {company.description ? <Text style={styles.muted}>{company.description}</Text> : null}
      {company.funds.map((fund) => (
        <FundCard key={fund.id} fund={fund} active={fund.id === activeFundId} onPress={() => setFund(fund.id)} />
      ))}
    </View>
  );
}

function FundCard({
  fund,
  active,
  onPress,
}: {
  fund: FundSummary;
  active: boolean;
  onPress: () => void;
}): JSX.Element {
  const { t } = useTranslation();
  return (
    <View>
      <Pressable
        style={[styles.fundCard, active ? styles.fundCardActive : null]}
        onPress={onPress}
        accessibilityRole="button"
        accessibilityState={{ selected: active }}
        accessibilityLabel={`${fund.name} ${active ? 'active' : ''}`}
      >
        <View style={styles.row}>
          <Text style={[styles.fundName, active ? styles.fundNameActive : null]}>{fund.name}</Text>
          {active ? <Text style={styles.activeChip}>•</Text> : null}
        </View>
        <View style={styles.row}>
          <Text style={styles.mutedSmall}>{t('home.navLabel')}</Text>
          <Text style={styles.fundValue}>{fund.nav.toFixed(3)}</Text>
        </View>
        <View style={styles.row}>
          <Text style={styles.mutedSmall}>{t('home.assetsLabel')}</Text>
          <Text style={styles.fundValue}>
            {fund.total_assets.toLocaleString()} {fund.base_currency ?? ''}
          </Text>
        </View>
      </Pressable>
      {/* Timeline only renders when this fund is the active one,
          to keep the home list short and focused. The card itself
          starts collapsed and is gated by react-query's `enabled`
          flag, so inactive funds never trigger network calls. */}
      {active ? <BenchmarkMiniChart fundId={fund.id} /> : null}
      {active ? <HoldingsTrendsGrid fundId={fund.id} /> : null}
      {active ? <CorpActionTimelineCard fundId={fund.id} /> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  container: { padding: 16, paddingBottom: 32 },
  center: { flex: 1, alignItems: 'center', justifyContent: 'center', padding: 24 },
  muted: { color: '#6b7280', fontSize: 14, marginTop: 8 },
  mutedSmall: { color: '#9ca3af', fontSize: 12 },
  companyCard: {
    backgroundColor: '#ffffff',
    borderRadius: 12,
    padding: 16,
    marginBottom: 16,
    shadowColor: '#000',
    shadowOpacity: 0.06,
    shadowRadius: 6,
    shadowOffset: { width: 0, height: 2 },
    elevation: 2,
  },
  companyName: { fontSize: 18, fontWeight: '600', color: '#111827', marginBottom: 4 },
  fundCard: {
    backgroundColor: '#f9fafb',
    borderRadius: 8,
    padding: 12,
    marginTop: 10,
    borderWidth: 1,
    borderColor: 'transparent',
  },
  fundCardActive: { borderColor: '#4f46e5', backgroundColor: '#eef2ff' },
  fundName: { fontSize: 15, fontWeight: '500', color: '#1f2937' },
  fundNameActive: { color: '#3730a3' },
  fundValue: { fontSize: 14, color: '#111827', fontWeight: '500' },
  row: { flexDirection: 'row', justifyContent: 'space-between', alignItems: 'center', marginTop: 4 },
  retry: { marginTop: 12, paddingVertical: 8, paddingHorizontal: 16, backgroundColor: '#e5e7eb', borderRadius: 6 },
  retryText: { color: '#1f2937' },
  errorText: { color: '#dc2626', fontSize: 14, marginTop: 8, textAlign: 'center' },
  activeChip: { color: '#4f46e5', fontSize: 18, fontWeight: '700' },
});
