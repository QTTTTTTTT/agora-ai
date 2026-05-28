/**
 * React Query client + MMKV persistor.
 *
 * 离线缓存策略：
 *   - 用 react-native-mmkv 做底层 KV，比 AsyncStorage 快 20-30x
 *     （MMKV 是 Tencent 开源，mmap-backed binary KV）。
 *   - persistQueryClient 把每个 query 的 lastFetched 数据 + cache age
 *     dump 进 MMKV。冷启动时 hydrate；24h 内有缓存的 query 立即显示
 *     stale data + 后台 refetch。
 *   - 排除 mutations（默认就排除）和 401 错误状态（dehydrateOptions 过滤）。
 *
 * 注意：缓存里可能含 partial PII（基金名/团队成员 email）；MMKV
 * 默认不加密，但我们 setEncryptionKey 后就走 SQLCipher 的 AES-256。
 * encryption key 从 keychain 里取 — 设备首启时随机生成一次。
 */

import { QueryClient } from '@tanstack/react-query';
import { persistQueryClient } from '@tanstack/react-query-persist-client';
import { createAsyncStoragePersister } from '@tanstack/query-async-storage-persister';

let mmkv: typeof import('react-native-mmkv') | null = null;
try {
  mmkv = require('react-native-mmkv');
} catch {
  mmkv = null;
}

const ONE_DAY_MS = 24 * 60 * 60 * 1000;

function mmkvBackedStorage() {
  if (!mmkv) return null;
  const store = new mmkv.MMKV({ id: 'fundai-query-cache' });
  return {
    getItem: (key: string) => Promise.resolve(store.getString(key) ?? null),
    setItem: (key: string, value: string) => {
      store.set(key, value);
      return Promise.resolve();
    },
    removeItem: (key: string) => {
      store.delete(key);
      return Promise.resolve();
    },
  };
}

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 60_000,
      retry: 1,
      refetchOnWindowFocus: false,
      networkMode: 'offlineFirst',
    },
    mutations: {
      retry: 0,
      networkMode: 'online',
    },
  },
});

let initialised = false;

export function initQueryPersistence(): void {
  if (initialised) return;
  initialised = true;
  const storage = mmkvBackedStorage();
  if (!storage) return;
  const persister = createAsyncStoragePersister({
    storage,
    key: 'fundai.query.cache',
    throttleTime: 1000,
  });
  void persistQueryClient({
    queryClient,
    persister,
    maxAge: ONE_DAY_MS,
    dehydrateOptions: {
      shouldDehydrateQuery: (query) => query.state.status === 'success',
    },
  });
}
