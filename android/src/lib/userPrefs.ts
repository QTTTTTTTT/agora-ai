/**
 * userPrefs — MoreScreen 侧的可见用户偏好。
 *
 * 范围：
 *   - biometricEnabled: 启动时是否要求生物识别再放行
 *     (true = require biometrics, false = skip)
 *     默认 true（更安全的 default）
 *   - pushEnabled: 是否接收推送通知 (plan_ready / plan_failed / mixed / reflection_ready)
 *     默认 true（产品默认订阅）
 *
 * 存储复用 theme/activeFund 已用的 react-native-mmkv 'fundai-prefs' store —
 * MMKV mmap-backed, sync, 测试与 web 路径下 require 失败 -> in-memory map
 * fallback，保证 noop-safe（与 theme.tsx 同一处理风格）。
 *
 * 真正生效点：
 *   - push.ts: registerDeviceForPush 检查 pushEnabled，false 时短路
 *               + setPushEnabled(false) 主动 unregister
 *   - auth.tsx: boot 时 biometricEnabled === false → 跳过 requireBiometrics
 *
 * 这两处都 fail-safe：即使 prefs 模块 noop，行为退化为 "default true"，
 * 等价于历史行为，不会回归。
 */

const STORE_ID = 'fundai-prefs';
const KEY_BIOMETRIC = 'biometricEnabled';
const KEY_PUSH = 'pushEnabled';
// P0-7: per-action step-up gating for orders. Default true so the
// out-of-the-box install requires biometric confirmation on
// cancel/replace. Power users on dev devices can flip it off in
// MoreScreen, which voids any cached step-up token immediately.
const KEY_STEP_UP_ORDERS = 'stepUpRequiredForOrders';

interface MmkvLike {
  getString(key: string): string | undefined;
  set(key: string, value: string): void;
}
interface MmkvCtor {
  new (cfg: { id: string }): MmkvLike;
}

let mmkvCtor: MmkvCtor | null = null;
try {
  mmkvCtor = (require('react-native-mmkv') as { MMKV: MmkvCtor }).MMKV;
} catch {
  mmkvCtor = null;
}

let store: MmkvLike | null = null;
const memoryFallback = new Map<string, string>();

function getStore(): MmkvLike {
  if (store) return store;
  if (mmkvCtor) {
    store = new mmkvCtor({ id: STORE_ID });
    return store;
  }
  // 测试 / JSDOM / web 路径 — in-memory map 行为等价。
  store = {
    getString: (k) => memoryFallback.get(k),
    set: (k, v) => {
      memoryFallback.set(k, v);
    },
  };
  return store;
}

function readBool(key: string, defaultValue: boolean): boolean {
  const raw = getStore().getString(key);
  if (raw === '1' || raw === 'true') return true;
  if (raw === '0' || raw === 'false') return false;
  return defaultValue;
}

function writeBool(key: string, value: boolean): void {
  getStore().set(key, value ? '1' : '0');
}

export function isBiometricEnabled(): boolean {
  return readBool(KEY_BIOMETRIC, true);
}

export function setBiometricEnabled(enabled: boolean): void {
  writeBool(KEY_BIOMETRIC, enabled);
}

export function isPushEnabled(): boolean {
  return readBool(KEY_PUSH, true);
}

export function setPushPreference(enabled: boolean): void {
  writeBool(KEY_PUSH, enabled);
}

// isStepUpRequiredForOrders gates the per-action biometric prompt
// on cancel / replace. Defaults to true. The MoreScreen toggle
// flips this and additionally clears the cached step-up token via
// stepUp.clearStepUpCache() — keeping a "stale" token around after
// the user explicitly opted out would defeat the purpose.
export function isStepUpRequiredForOrders(): boolean {
  return readBool(KEY_STEP_UP_ORDERS, true);
}

export function setStepUpRequiredForOrders(enabled: boolean): void {
  writeBool(KEY_STEP_UP_ORDERS, enabled);
}
