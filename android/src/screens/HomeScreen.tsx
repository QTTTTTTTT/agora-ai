/**
 * HomeScreen — 公司 + 基金概览，并触发当前 activeFund 选择。
 *
 * - 首次拿到 fund 列表时如果没有 activeFundId，自动选第一支。
 * - 用户点 fund 卡片时切换 activeFund（高亮当前选中）。
 * - 任何 setSession token = null 后，react-query cache 仍保留（用 mmkv 持久化）—
 *   下次登录用同一账号能"瞬时"看到上一会话的数据。
 *
 * 视觉风格已切换到 cream / sage / 黑胶囊 设计系统（与 web /style-preview
 * 一致），通过 ThemedCard / PillTag / BlackPillButton / MetricBlock 复用。
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
import { useTheme } from '../lib/theme';
import { CorpActionTimelineCard } from '../components/CorpActionTimelineCard';
import { BenchmarkMiniChart } from '../components/BenchmarkMiniChart';
import { HoldingsTrendsGrid } from '../components/HoldingsTrendsGrid';
import {
  ThemedCard,
  PillTag,
  BlackPillButton,
  MetricBlock,
  SectionLabel,
} from '../components/ThemedCard';

export default function HomeScreen(): JSX.Element {
  const { t } = useTranslation();
  const { fundId, setFund } = useActiveFund();
  const { colors } = useTheme();
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
      <View style={[styles.center, { backgroundColor: colors.bg }]}>
        <ActivityIndicator size="large" color={colors.accent} />
        <Text style={[styles.muted, { color: colors.textMuted }]}>{t('home.loading')}</Text>
      </View>
    );
  }

  if (isError || !data) {
    return (
      <View style={[styles.center, { backgroundColor: colors.bg }]}>
        <Text style={[styles.errorText, { color: colors.danger }]}>{t('home.error')}</Text>
        <BlackPillButton
          label={t('home.retry')}
          onPress={() => void refetch()}
          variant="ink"
          size="md"
          style={{ marginTop: 16 }}
          accessibilityLabel={t('home.retry')}
        />
      </View>
    );
  }

  return (
    <FlatList
      data={data}
      keyExtractor={(c) => c.id}
      style={{ backgroundColor: colors.bg }}
      contentContainerStyle={styles.container}
      refreshControl={<RefreshControl refreshing={isFetching} onRefresh={() => void refetch()} tintColor={colors.accent} />}
      ListHeaderComponent={
        <SectionLabel style={{ marginTop: 8 }} trailing={`${data.length}`}>
          组合驾驶舱
        </SectionLabel>
      }
      ListEmptyComponent={<Text style={[styles.muted, { color: colors.textMuted }]}>{t('home.empty')}</Text>}
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
  const { colors } = useTheme();
  return (
    <ThemedCard>
      <View style={{ flexDirection: 'row', justifyContent: 'space-between', alignItems: 'flex-start' }}>
        <View style={{ flex: 1, paddingRight: 8 }}>
          <Text style={{ color: colors.textMuted, fontSize: 11, fontWeight: '600', letterSpacing: 1, textTransform: 'uppercase' }}>
            团队驾驶舱
          </Text>
          <Text style={{ color: colors.text, fontSize: 20, fontWeight: '800', marginTop: 4 }}>
            {company.name}
          </Text>
          {company.description ? (
            <Text style={{ color: colors.textMuted, fontSize: 13, marginTop: 6 }}>
              {company.description}
            </Text>
          ) : null}
        </View>
        <PillTag tone="sage" size="sm">{`${company.funds.length} 支`}</PillTag>
      </View>
      <View style={{ height: 14 }} />
      {company.funds.map((fund) => (
        <FundCard key={fund.id} fund={fund} active={fund.id === activeFundId} onPress={() => setFund(fund.id)} />
      ))}
    </ThemedCard>
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
  const { colors } = useTheme();
  return (
    <View>
      <Pressable
        onPress={onPress}
        accessibilityRole="button"
        accessibilityState={{ selected: active }}
        accessibilityLabel={`${fund.name} ${active ? 'active' : ''}`}
        style={{
          backgroundColor: active ? colors.ink : colors.surfaceAlt,
          borderRadius: 18,
          padding: 14,
          marginTop: 10,
          borderWidth: 1,
          borderColor: active ? colors.ink : colors.border,
        }}
      >
        <View style={[styles.row, { marginTop: 0 }]}>
          <Text
            style={{
              color: active ? '#ffffff' : colors.text,
              fontSize: 15,
              fontWeight: '700',
              flex: 1,
            }}
          >
            {fund.name}
          </Text>
          <PillTag
            tone={active ? 'sage' : fund.status === 'active' ? 'info' : 'muted'}
            size="sm"
          >
            {active ? '当前' : fund.status}
          </PillTag>
        </View>
        <View style={{ flexDirection: 'row', justifyContent: 'space-between', marginTop: 12, gap: 12 }}>
          <View style={{ flex: 1 }}>
            <MetricBlock
              label={t('home.navLabel') ?? '组合净值'}
              value={fund.nav.toFixed(3)}
              tone="neutral"
            />
          </View>
          <View style={{ flex: 1 }}>
            <MetricBlock
              label={t('home.assetsLabel') ?? '总资产'}
              value={`${fund.total_assets.toLocaleString()} ${fund.base_currency ?? ''}`.trim()}
              tone="neutral"
            />
          </View>
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
  muted: { fontSize: 14, marginTop: 8 },
  row: { flexDirection: 'row', justifyContent: 'space-between', alignItems: 'center', marginTop: 4 },
  errorText: { fontSize: 14, marginTop: 8, textAlign: 'center' },
});
