/**
 * Push registration — FCM (Firebase Cloud Messaging).
 *
 * 流程：
 *   1. 用户登录后调用 registerDeviceForPush()
 *   2. 请求 POST permission（iOS 强制；Android 13+ POST_NOTIFICATIONS）
 *   3. 拿 messaging().getToken() 得到 FCM device token
 *   4. POST /api/devices/register {token, platform, app_version} 入库
 *   5. 监听 token refresh 事件，自动 re-POST
 *
 * 服务端 device_tokens 表 + 4 类触发事件（plan_ready / plan_failed /
 * mixed / reflection_ready）由 server 一侧推送；本模块只负责注册。
 *
 * 所有 lib import 都用 require + try/catch — Android 上若 google-services.json
 * 没配 / Firebase 未初始化，require 会 throw；我们直接退化为 noop，让
 * dev builds 不被推送依赖阻断。
 */

import { Platform } from 'react-native';

import { apiClient } from './api';
import { isPushEnabled, setPushPreference } from './userPrefs';

interface FirebaseMessagingLib {
  default(): {
    requestPermission(): Promise<number>;
    AuthorizationStatus: { AUTHORIZED: number; PROVISIONAL: number };
    hasPermission(): Promise<number>;
    getToken(): Promise<string>;
    onTokenRefresh(cb: (token: string) => void): () => void;
  };
}

function loadMessaging(): FirebaseMessagingLib | null {
  try {
    return require('@react-native-firebase/messaging');
  } catch {
    return null;
  }
}

let registered = false;

export async function registerDeviceForPush(appVersion: string): Promise<void> {
  if (registered) return;
  // 用户在 MoreScreen 关闭了推送 → 不主动 register。reverse 操作
  // 由 setPushPreference(true) → 调用方再 register 处理。
  if (!isPushEnabled()) return;
  const lib = loadMessaging();
  if (!lib) return;
  const messaging = lib.default();
  try {
    let granted = await messaging.hasPermission();
    if (granted !== messaging.AuthorizationStatus.AUTHORIZED &&
        granted !== messaging.AuthorizationStatus.PROVISIONAL) {
      granted = await messaging.requestPermission();
    }
    if (granted !== messaging.AuthorizationStatus.AUTHORIZED &&
        granted !== messaging.AuthorizationStatus.PROVISIONAL) {
      return;
    }
    const token = await messaging.getToken();
    await postToken(token, appVersion);
    messaging.onTokenRefresh((next) => {
      void postToken(next, appVersion);
    });
    registered = true;
  } catch {
    // best-effort — Sentry / telemetry layer (Sprint 4 / android-production)
    // will log if it's hooked up. Don't block app on push registration.
  }
}

async function postToken(token: string, appVersion: string): Promise<void> {
  try {
    await apiClient.request<{ ok: boolean }>('/api/devices/register', {
      method: 'POST',
      body: {
        token,
        platform: Platform.OS,
        app_version: appVersion,
      },
    });
  } catch {
    /* swallow — registration retries on next token refresh */
  }
}

/**
 * unregisterDeviceForPush 退出登录时调用 — server 端从 device_tokens
 * 表里删除当前 token。
 */
export async function unregisterDeviceForPush(): Promise<void> {
  if (!registered) return;
  const lib = loadMessaging();
  if (!lib) return;
  try {
    const token = await lib.default().getToken();
    await apiClient.request<{ ok: boolean }>('/api/devices/unregister', {
      method: 'POST',
      body: { token },
    });
  } catch {
    /* swallow */
  } finally {
    registered = false;
  }
}

/**
 * setPushEnabled — MoreScreen 用户 toggle 时调用。
 *   - true: 写偏好，若当前未 register 则即刻注册
 *   - false: 写偏好 + 立即 unregister（server 不再向本设备推送）
 *
 * 故意不抛错 — UI 已经显示了 toggle 状态，registration 失败由
 * push.ts 自带 best-effort + telemetry 路径处理。
 */
export async function setPushEnabled(enabled: boolean, appVersion: string): Promise<void> {
  setPushPreference(enabled);
  if (enabled) {
    if (!registered) {
      await registerDeviceForPush(appVersion);
    }
  } else {
    await unregisterDeviceForPush();
  }
}

export { isPushEnabled };
