/**
 * MemoryScreen — 记忆 + 反思（agent / long-term 两个 tab）。
 *
 * - agent tab: apiClient.getMemory(fundId, 'agent')
 * - reflection tab: apiClient.listReflections(fundId)
 *
 * 每 tab 独立 query key — 切 tab 不会丢另一边的 cache。
 */

import React, { useMemo, useState } from 'react';
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
import type { MemoryItem, ReflectionItem } from '@fundai/api-client';

type Tab = 'agent' | 'reflection';

interface ListRow {
  id: string;
  title: string;
  body: string;
  tags?: string[];
  date?: string;
}

export default function MemoryScreen(): JSX.Element {
  const { t } = useTranslation();
  const [tab, setTab] = useState<Tab>('agent');
  const { fundId } = useActiveFund();

  const memoryQuery = useQuery({
    queryKey: ['memory', fundId, 'agent'],
    enabled: !!fundId && tab === 'agent',
    queryFn: async () => apiClient.getMemory(fundId!, 'agent'),
  });

  const reflectionQuery = useQuery({
    queryKey: ['reflections', fundId],
    enabled: !!fundId && tab === 'reflection',
    queryFn: async () => apiClient.listReflections(fundId!, 20),
  });

  const rows = useMemo<ListRow[]>(() => {
    if (tab === 'agent') {
      const items = memoryQuery.data?.items ?? [];
      return items.map((it: MemoryItem) => ({
        id: it.id,
        title: it.title ?? it.layer,
        body: it.content,
        tags: it.tags,
        date: it.trading_date ?? it.created_at,
      }));
    }
    const items = reflectionQuery.data?.items ?? [];
    return items.map((it: ReflectionItem) => ({
      id: it.id,
      title: it.title || it.theme,
      body: it.content,
      tags: it.tags,
      date: it.trading_date ?? it.created_at,
    }));
  }, [tab, memoryQuery.data, reflectionQuery.data]);

  const isLoading = tab === 'agent' ? memoryQuery.isLoading : reflectionQuery.isLoading;
  const isFetching = tab === 'agent' ? memoryQuery.isFetching : reflectionQuery.isFetching;
  const isError = tab === 'agent' ? memoryQuery.isError : reflectionQuery.isError;
  const refetch = tab === 'agent' ? memoryQuery.refetch : reflectionQuery.refetch;

  return (
    <View style={styles.container}>
      <View style={styles.tabBar}>
        {(['agent', 'reflection'] as Tab[]).map((key) => (
          <Pressable
            key={key}
            onPress={() => setTab(key)}
            accessibilityRole="tab"
            accessibilityState={{ selected: tab === key }}
            accessibilityLabel={t(`memory.tabs.${key}`)}
            style={[styles.tab, tab === key && styles.tabActive]}
          >
            <Text style={[styles.tabText, tab === key && styles.tabTextActive]}>
              {t(`memory.tabs.${key}`)}
            </Text>
          </Pressable>
        ))}
      </View>
      {!fundId ? (
        <View style={styles.center}>
          {/* 真正的 empty 状态：用户还没选基金。和"加载失败"严格区分。 */}
          <Text style={styles.muted}>{t('memory.empty')}</Text>
        </View>
      ) : isLoading ? (
        <View style={styles.center}>
          <ActivityIndicator size="large" color="#4f46e5" />
        </View>
      ) : isError ? (
        <View style={styles.center}>
          <Text style={styles.errorText}>{t('memory.error')}</Text>
          <Pressable
            onPress={() => void refetch()}
            accessibilityRole="button"
            accessibilityLabel={t('memory.retry')}
            style={styles.retryBtn}
          >
            <Text style={styles.retryBtnLabel}>{t('memory.retry')}</Text>
          </Pressable>
        </View>
      ) : (
        <FlatList
          data={rows}
          keyExtractor={(it) => it.id}
          contentContainerStyle={styles.listContainer}
          refreshControl={<RefreshControl refreshing={isFetching} onRefresh={() => void refetch()} />}
          ListEmptyComponent={<Text style={styles.muted}>{t('memory.empty')}</Text>}
          renderItem={({ item }) => (
            <View style={styles.card}>
              <Text style={styles.title}>{item.title}</Text>
              <Text style={styles.body} numberOfLines={4}>
                {item.body}
              </Text>
              {item.tags && item.tags.length > 0 ? (
                <Text style={styles.tags}>{item.tags.slice(0, 4).join(' · ')}</Text>
              ) : null}
              {item.date ? <Text style={styles.date}>{item.date}</Text> : null}
            </View>
          )}
        />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, backgroundColor: '#f9fafb' },
  tabBar: { flexDirection: 'row', backgroundColor: '#ffffff', paddingHorizontal: 16, paddingTop: 8 },
  tab: { paddingVertical: 10, marginRight: 24 },
  tabActive: { borderBottomWidth: 2, borderBottomColor: '#4f46e5' },
  tabText: { fontSize: 14, color: '#6b7280' },
  tabTextActive: { color: '#4f46e5', fontWeight: '600' },
  listContainer: { padding: 16 },
  center: { flex: 1, alignItems: 'center', justifyContent: 'center', padding: 24 },
  muted: { color: '#9ca3af', textAlign: 'center', marginTop: 32 },
  card: { backgroundColor: '#ffffff', borderRadius: 12, padding: 16, marginBottom: 10, elevation: 1 },
  title: { fontSize: 15, fontWeight: '600', color: '#111827' },
  body: { fontSize: 13, color: '#374151', marginTop: 6, lineHeight: 18 },
  tags: { fontSize: 11, color: '#6366f1', marginTop: 8 },
  date: { fontSize: 11, color: '#9ca3af', marginTop: 4 },
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
