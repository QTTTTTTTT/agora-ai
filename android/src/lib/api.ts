/**
 * RN-side wiring for the shared @fundai/api-client.
 *
 * Sprint 4 / android-core: token 走 react-native-keychain（Android Keystore /
 * iOS Keychain Services），不再用 AsyncStorage。AsyncStorage 在 Android 上是
 * SharedPreferences — root / debugger / 其他 app 可读，不合规。
 *
 * baseUrl 走环境变量（metro 在 build 时把 process.env 注入），缺省值
 *   - Android emulator: 10.0.2.2:8080 (访问 host 机的 localhost)
 *   - iOS sim: localhost:8080
 *   - Production: 通过 react-native-config / metro-define 注入
 */

import { Platform } from 'react-native';
import { createClient, type ApiClient } from '@fundai/api-client';

import { clearToken, loadToken, saveToken } from './secureStore';

let cachedToken: string | null = null;
let unauthHandler: (() => void) | null = null;

export async function bootstrapApi(): Promise<void> {
  const creds = await loadToken();
  cachedToken = creds?.token ?? null;
}

export function getInMemoryToken(): string | null {
  return cachedToken;
}

export async function setSessionToken(token: string | null, userId?: string): Promise<void> {
  cachedToken = token;
  if (token === null) {
    await clearToken();
  } else {
    await saveToken(token, userId);
  }
}

export function onUnauthorized(handler: () => void): void {
  unauthHandler = handler;
}

declare const process: { env: Record<string, string | undefined> } | undefined;

function defaultBaseUrl(): string {
  const fromEnv = typeof process !== 'undefined' ? process.env.FUND_API_URL : undefined;
  if (fromEnv) return fromEnv;
  if (Platform.OS === 'android') return 'http://10.0.2.2:8080';
  return 'http://localhost:8080';
}

export const apiClient: ApiClient = createClient({
  baseUrl: defaultBaseUrl(),
  getToken: () => cachedToken,
  onUnauthorized: async () => {
    cachedToken = null;
    await clearToken();
    unauthHandler?.();
  },
  timeoutSeconds: 30,
});
