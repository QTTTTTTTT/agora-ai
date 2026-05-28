/**
 * secureStore — keychain-backed token & biometrics gate.
 *
 * Why not AsyncStorage? On Android, AsyncStorage 默认是 SharedPreferences,
 * 任何 process / root 都能读出。生产侧 token 必须放在 KeyStore + EncryptedSharedPreferences
 * 后面 — react-native-keychain 在 Android 用 Android Keystore，安全等级达标。
 *
 * 我们保留一份"上次成功登录的非敏感 hint" (email) 在 plain storage 里，
 * 这样登录页能 prefill，但 token / refresh token 永远只在 keychain。
 *
 * Biometrics 的角色：
 *   - 每次 app 冷启动时如果 keychain 里有 token，要先要求生物识别才能放行。
 *   - 如果用户拒绝（设备没注册指纹 / face），keychain 直接吐 token，
 *     由后续 401 流处理失效场景。
 *   - 真正的"二次确认"也走 biometrics — 用于换基金 / 大额下单。
 *
 * 全部 API 都 noop-safe：在测试 / Web 路径调用时不会 throw（动态 require
 * 的库缺失会被 catch 成 null）。
 */

const KEYCHAIN_SERVICE = 'com.fundai.platform.auth';

let keychain: typeof import('react-native-keychain') | null = null;
let biometrics: typeof import('react-native-biometrics') | null = null;

function lazyKeychain() {
  if (keychain) return keychain;
  try {
    keychain = require('react-native-keychain');
  } catch {
    keychain = null;
  }
  return keychain;
}

function lazyBiometrics() {
  if (biometrics) return biometrics;
  try {
    biometrics = require('react-native-biometrics');
  } catch {
    biometrics = null;
  }
  return biometrics;
}

export interface StoredCredentials {
  token: string;
  userId?: string;
}

export async function saveToken(token: string, userId?: string): Promise<void> {
  const lib = lazyKeychain();
  if (!lib) return;
  await lib.setGenericPassword(userId ?? 'user', token, {
    service: KEYCHAIN_SERVICE,
    accessible: lib.ACCESSIBLE.WHEN_UNLOCKED_THIS_DEVICE_ONLY,
    securityLevel: lib.SECURITY_LEVEL.SECURE_HARDWARE,
  });
}

export async function loadToken(): Promise<StoredCredentials | null> {
  const lib = lazyKeychain();
  if (!lib) return null;
  try {
    const creds = await lib.getGenericPassword({ service: KEYCHAIN_SERVICE });
    if (!creds) return null;
    return { token: creds.password, userId: creds.username };
  } catch {
    return null;
  }
}

export async function clearToken(): Promise<void> {
  const lib = lazyKeychain();
  if (!lib) return;
  try {
    await lib.resetGenericPassword({ service: KEYCHAIN_SERVICE });
  } catch {
    /* swallow — caller proceeds with logout regardless */
  }
}

/**
 * requireBiometrics 弹一次系统级生物识别 prompt。
 * 返回 true = 用户验证成功，false = 用户拒绝/取消/未启用，
 * null = 库不可用（应当 fall back 到允许进入但加重 audit）。
 */
export async function requireBiometrics(reason: string): Promise<boolean | null> {
  const lib = lazyBiometrics();
  if (!lib) return null;
  const Biometrics = lib.default ?? lib;
  try {
    const instance = new Biometrics();
    const { available } = await instance.isSensorAvailable();
    if (!available) return null;
    const { success } = await instance.simplePrompt({
      promptMessage: reason,
      cancelButtonText: 'Cancel',
    });
    return success;
  } catch {
    return null;
  }
}
