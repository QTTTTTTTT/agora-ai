/**
 * TeamScreen — agent 团队列表（按 active fund）。
 *
 * 接入 apiClient.listTeam(fundId)；mock 路径已去掉 — fallback 是
 * 直接显示 empty 提示并允许下拉刷新重试。
 *
 * 视觉风格已切换到 cream / sage / 黑胶囊 设计系统：每个 agent
 * 用像素风 MascotAvatar 呈现，与 web 端 /style-preview 的"阵容"
 * 卡片一致；roleColors 不再决定底色，转而由 PillTag tone 表达。
 */

import React, { useMemo } from 'react';
import {
  ActivityIndicator,
  FlatList,
  RefreshControl,
  StyleSheet,
  Text,
  View,
} from 'react-native';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import { apiClient } from '../lib/api';
import { useActiveFund } from '../lib/activeFund';
import { useTheme } from '../lib/theme';
import {
  ThemedCard,
  PillTag,
  BlackPillButton,
  MascotAvatar,
  type MascotRole,
  SectionLabel,
} from '../components/ThemedCard';
import type { TeamMemberSummary } from '@fundai/api-client';

// Map an agent role onto the closest mascot persona. PMs sit in
// the "captain" silhouette (overall lead), risk officers wear the
// red shield, researchers carry the magnifier, traders carry the
// BUY/SELL screen, and anything unrecognised gets the analyst.
function roleToMascot(role: string): MascotRole {
  const r = role.toLowerCase();
  if (r.includes('pm') || r.includes('lead') || r.includes('captain')) return 'captain';
  if (r.includes('risk')) return 'risk';
  if (r.includes('research') || r.includes('analyst') || r.includes('intel')) return 'intel';
  if (r.includes('pick') || r.includes('select')) return 'picker';
  if (r.includes('trade') || r.includes('execu')) return 'trader';
  return 'analyst';
}

function rolePillTone(role: string): 'sage' | 'coral' | 'risk' | 'ink' | 'muted' | 'info' {
  const r = role.toLowerCase();
  if (r.includes('risk')) return 'risk';
  if (r.includes('pm') || r.includes('lead')) return 'ink';
  if (r.includes('research') || r.includes('analyst')) return 'info';
  if (r.includes('pick') || r.includes('select')) return 'sage';
  if (r.includes('trade') || r.includes('execu')) return 'coral';
  return 'muted';
}

export default function TeamScreen(): JSX.Element {
  const { t } = useTranslation();
  const { fundId } = useActiveFund();
  const { colors } = useTheme();
  const { data, isLoading, isFetching, isError, refetch } = useQuery({
    queryKey: ['team', fundId],
    enabled: !!fundId,
    queryFn: async () => apiClient.listTeam(fundId!),
  });

  const members = useMemo(() => data?.members ?? [], [data]);

  if (!fundId) {
    return (
      <View style={[styles.center, { backgroundColor: colors.bg }]}>
        <Text style={[styles.muted, { color: colors.textMuted }]}>{t('team.empty')}</Text>
      </View>
    );
  }

  if (isLoading) {
    return (
      <View style={[styles.center, { backgroundColor: colors.bg }]}>
        <ActivityIndicator size="large" color={colors.accent} />
      </View>
    );
  }

  if (isError) {
    return (
      <View style={[styles.center, { backgroundColor: colors.bg }]}>
        <Text style={[styles.errorText, { color: colors.danger }]}>{t('team.error')}</Text>
        <BlackPillButton
          label={t('team.retry')}
          onPress={() => void refetch()}
          variant="ink"
          size="md"
          style={{ marginTop: 16 }}
          accessibilityLabel={t('team.retry')}
        />
      </View>
    );
  }

  return (
    <FlatList
      data={members}
      keyExtractor={(m: TeamMemberSummary) => m.member_id}
      style={{ backgroundColor: colors.bg }}
      contentContainerStyle={styles.container}
      ListHeaderComponent={
        <View>
          <SectionLabel style={{ marginTop: 8 }} trailing={`${members.length} / ${members.length}`}>
            阵容
          </SectionLabel>
          <ThemedCard>
            <Text style={{ color: colors.text, fontSize: 22, fontWeight: '800' }}>阵容</Text>
            <Text style={{ color: colors.textMuted, fontSize: 13, marginTop: 4 }}>
              看准方向，跟住强势股
            </Text>
            <View style={{ height: 8 }} />
            <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 6 }}>
              <PillTag tone="ink"   size="sm">官方</PillTag>
              <PillTag tone="coral" size="sm">需处理</PillTag>
              <PillTag tone="sage"  size="sm">已整备</PillTag>
              <PillTag tone="risk"  size="sm">风控优先</PillTag>
            </View>
          </ThemedCard>
        </View>
      }
      refreshControl={<RefreshControl refreshing={isFetching} onRefresh={() => void refetch()} tintColor={colors.accent} />}
      ListEmptyComponent={<Text style={[styles.muted, { color: colors.textMuted }]}>{t('team.empty')}</Text>}
      renderItem={({ item }) => {
        const mascot = roleToMascot(item.role);
        const tone = rolePillTone(item.role);
        return (
          <ThemedCard>
            <View style={{ flexDirection: 'row', alignItems: 'center', gap: 14 }}>
              <MascotAvatar role={mascot} size={72} />
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ color: colors.text, fontSize: 16, fontWeight: '800' }}>
                  {item.name ?? item.agent_id}
                </Text>
                <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 6, marginTop: 6 }}>
                  <PillTag tone={tone} size="sm">{item.role.toUpperCase()}</PillTag>
                  {item.model_provider || item.model_name ? (
                    <PillTag tone="muted" size="sm">
                      {[item.model_provider, item.model_name].filter(Boolean).join(' · ')}
                    </PillTag>
                  ) : null}
                </View>
                {item.focus ? (
                  <Text style={{ color: colors.textMuted, fontSize: 13, marginTop: 8 }}>
                    {item.focus}
                  </Text>
                ) : null}
              </View>
            </View>
          </ThemedCard>
        );
      }}
    />
  );
}

const styles = StyleSheet.create({
  container: { padding: 16 },
  center: { flex: 1, alignItems: 'center', justifyContent: 'center', padding: 24 },
  muted: { textAlign: 'center', marginTop: 32 },
  errorText: { fontSize: 14, textAlign: 'center' },
});
