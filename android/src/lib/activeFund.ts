/**
 * activeFund — RN-wide "which fund am I looking at right now".
 *
 * 平台允许一个用户/公司管多支基金；移动端 UX 上只显示一支当前选中的，
 * 一切 list（Decisions / Memory / Team / Portfolio）都用这个 id。
 *
 * 实现：
 *   - useActiveFund() — react hook，订阅当前 active fund id 变化
 *   - setActiveFund(id) — 程序触发切换；持久化到 MMKV（启动恢复）
 *   - 启动时若 MMKV 没值，则在 HomeScreen 首次拿到 fund 列表后自动 pick 第一支
 *
 * 选择 MMKV 而不是 AsyncStorage —— activeFund 不是密钥，但访问极频
 * （每个 hook、navigation focus、refetch 都查一次），MMKV 同步 sync
 * read 比 async getItem 更友好。
 */

import React, { useEffect, useState } from 'react';

interface MmkvLike {
  getString(key: string): string | undefined;
  set(key: string, value: string): void;
  delete(key: string): void;
}

interface MmkvCtor {
  new (config: { id: string }): MmkvLike;
}

let mmkvCtor: MmkvCtor | null = null;
try {
  mmkvCtor = (require('react-native-mmkv') as { MMKV: MmkvCtor }).MMKV;
} catch {
  mmkvCtor = null;
}

const KEY = 'activeFundId';
const subscribers = new Set<(value: string | null) => void>();
let cached: string | null | undefined = undefined;
let store: MmkvLike | null = null;

function ensureStore(): MmkvLike | null {
  if (store !== null) return store;
  if (!mmkvCtor) return null;
  store = new mmkvCtor({ id: 'fundai-active-fund' });
  return store;
}

function loadFromStore(): string | null {
  if (cached !== undefined) return cached;
  const s = ensureStore();
  cached = s?.getString(KEY) ?? null;
  return cached ?? null;
}

export function getActiveFundId(): string | null {
  return loadFromStore();
}

export function setActiveFund(id: string | null): void {
  cached = id;
  const s = ensureStore();
  if (s) {
    if (id === null) {
      s.delete(KEY);
    } else {
      s.set(KEY, id);
    }
  }
  subscribers.forEach((cb) => cb(id));
}

export function useActiveFund(): { fundId: string | null; setFund: (id: string | null) => void } {
  const [value, setValue] = useState<string | null>(() => loadFromStore());
  useEffect(() => {
    const cb = (next: string | null) => setValue(next);
    subscribers.add(cb);
    return () => {
      subscribers.delete(cb);
    };
  }, []);
  return { fundId: value, setFund: setActiveFund };
}

// 单元测试 / dev tooling 可用 — 重置内部缓存。
export const __testing__ = {
  reset: () => {
    cached = undefined;
    store = null;
    subscribers.clear();
  },
};

// 兼容 ESM import { React } 不需要的小桥 — TS strict 模式会嫌 unused
// import；保留显式 React 导入让 hooks 编译通过。
void React;
