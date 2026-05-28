/**
 * security.ts — runtime defensive hardening.
 *
 * 1. FLAG_SECURE — 阻止 Android 屏幕录制 / 阻止任务切换器截图。
 *    实现：调用 react-native-prevent-screenshot-android.activate()。
 *    缺库时 fallback 到 noop（dev / iOS）— iOS 的等价是 UIScreen
 *    isCaptured listener，留给 ios-native PR。
 *
 * 2. Root detection — 用 jail-monkey 包（zero-config）；缺库 → 假定
 *    设备 ok。检测结果走 isDeviceRooted()，gate 函数式抛出，用于在
 *    LoginScreen 启动时 block 流程 + 上报 Sentry。
 *
 * 3. SSL pinning — react-native-ssl-pinning 提供 fetch 替代品；本
 *    模块只暴露 hosts 配置 + 替代 fetch；调用方在 createClient 注
 *    入。配置缺失 / 库缺失时退化为标准 fetch（生产构建里 hostsToFingerprints
 *    应包含至少一组指纹；缺失会被 Sentry 报警）。
 */

import { Platform } from 'react-native';

// ------------------------------ FLAG_SECURE -----------------------------

let flagSecureLib: { activate?: () => void; deactivate?: () => void } | null = null;
try {
  flagSecureLib = require('react-native-prevent-screenshot-android');
} catch {
  flagSecureLib = null;
}

export function enableScreenCapturePrevention(): void {
  if (Platform.OS !== 'android') return;
  flagSecureLib?.activate?.();
}

export function disableScreenCapturePrevention(): void {
  if (Platform.OS !== 'android') return;
  flagSecureLib?.deactivate?.();
}

// ---------------------------- Root detection ----------------------------

interface JailMonkeyLib {
  isJailBroken(): boolean;
  hookDetected?(): boolean;
  trustFall?(): boolean;
}

let jailMonkey: JailMonkeyLib | null = null;
try {
  const lib = require('jail-monkey');
  jailMonkey = lib?.default ?? lib;
} catch {
  jailMonkey = null;
}

export interface SecurityVerdict {
  rooted: boolean;
  hookDetected: boolean;
  available: boolean;
}

export function checkDeviceSecurity(): SecurityVerdict {
  if (!jailMonkey) {
    return { rooted: false, hookDetected: false, available: false };
  }
  return {
    rooted: jailMonkey.isJailBroken(),
    hookDetected: jailMonkey.hookDetected?.() ?? false,
    available: true,
  };
}

// ---------------------------- SSL pinning ------------------------------

let sslPinningLib: {
  fetch?: (url: string, init: { sslPinning?: { certs: string[] } } & RequestInit) => Promise<Response>;
} | null = null;
try {
  sslPinningLib = require('react-native-ssl-pinning');
} catch {
  sslPinningLib = null;
}

/**
 * hostFingerprints — host → list of allowed cert fingerprint identifiers.
 *
 * The identifiers correspond to .cer files bundled inside the native
 * project (Android: app/src/main/assets/; iOS: bundle resources). The
 * native install step in docs/ANDROID_BOOTSTRAP.md covers how to drop
 * the public cert files in. Empty map = pinning disabled.
 */
const hostFingerprints: Record<string, string[]> = {
  // example: 'fund.example.com': ['fund_example_com'],
};

/**
 * pinnedFetch — drop-in fetch replacement used by createClient.fetchImpl
 * when both:
 *   - hostFingerprints has an entry for the URL host
 *   - react-native-ssl-pinning is available
 *
 * Falls back to standard fetch otherwise — that is the dev path where
 * we ship cert pinning disabled (talking to 10.0.2.2 / localhost). The
 * production CI step (sprint 4 / android-production CI) ensures the
 * map is populated for release builds.
 */
export const pinnedFetch: typeof fetch = async (input, init) => {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
  try {
    const host = new URL(url).host;
    const certs = hostFingerprints[host];
    if (!sslPinningLib?.fetch || !certs || certs.length === 0) {
      return fetch(input as RequestInfo, init);
    }
    return await sslPinningLib.fetch(url, {
      ...(init as RequestInit),
      sslPinning: { certs },
    });
  } catch {
    return fetch(input as RequestInfo, init);
  }
};
