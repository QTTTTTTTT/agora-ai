/**
 * LoginScreen — Email/password 登录。
 *
 * - 输入: email + password
 * - 提交后调用 apiClient.login，成功则更新 AuthContext → 自动切换到主 tab
 * - 失败 401 显示"邮箱或密码错误"，其它错显示通用错误
 * - "忘记密码" 跳 ForgotPasswordScreen
 *
 * 设计取舍：
 *   * 不做客户端 password 复杂度校验 — server 是单一权威。客户端只
 *     做"非空 + 邮箱基本格式"以减少废 RPC。
 *   * 没有 register 流（产品定位 internal/invite-only），这与现有
 *     web 端一致。
 */

import React, { useState } from 'react';
import {
  ActivityIndicator,
  KeyboardAvoidingView,
  Platform,
  StyleSheet,
  Text,
  TextInput,
  TouchableOpacity,
  View,
} from 'react-native';
import { useTranslation } from 'react-i18next';

import { ApiError } from '@fundai/api-client';
import { useAuth } from '../lib/auth';

interface Props {
  onForgotPassword: () => void;
}

export default function LoginScreen({ onForgotPassword }: Props): JSX.Element {
  const { t } = useTranslation();
  const { state, login } = useAuth();
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  function isProbablyEmail(value: string): boolean {
    return /.+@.+\..+/.test(value.trim());
  }

  // 把 AuthProvider 的最近 unauthenticated 原因 surface 出来。
  // biometrics: 生物识别失败/取消 → 引导用户改用密码。
  // session_error: 启动时 /api/session 网络/5xx → 提示离线，不强制重新登录。
  const reason = state.status === 'unauthenticated' ? state.reason : undefined;
  const reasonBanner = (() => {
    if (reason === 'biometrics') {
      return { tone: 'warn' as const, text: t('auth.biometricsBlockedHint') };
    }
    if (reason === 'session_error') {
      return { tone: 'info' as const, text: t('auth.sessionErrorHint') };
    }
    return null;
  })();

  async function handleSubmit() {
    setError(null);
    if (!isProbablyEmail(email) || password.length === 0) {
      setError(t('auth.errorInvalid'));
      return;
    }
    setSubmitting(true);
    try {
      await login(email.trim(), password);
    } catch (err) {
      if (err instanceof ApiError && err.code === 401) {
        setError(t('auth.errorInvalid'));
      } else {
        setError(t('auth.errorGeneric'));
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <KeyboardAvoidingView
      behavior={Platform.OS === 'ios' ? 'padding' : undefined}
      style={styles.container}
    >
      <View style={styles.card}>
        <Text style={styles.title}>{t('auth.loginTitle')}</Text>
        {reasonBanner ? (
          <View
            style={[
              styles.reasonBanner,
              reasonBanner.tone === 'warn' ? styles.reasonBannerWarn : styles.reasonBannerInfo,
            ]}
            accessibilityRole="alert"
          >
            <Text
              style={
                reasonBanner.tone === 'warn' ? styles.reasonBannerWarnText : styles.reasonBannerInfoText
              }
            >
              {reasonBanner.text}
            </Text>
          </View>
        ) : null}
        <TextInput
          placeholder={t('auth.email') ?? ''}
          autoCapitalize="none"
          autoCorrect={false}
          keyboardType="email-address"
          textContentType="emailAddress"
          style={styles.input}
          value={email}
          onChangeText={setEmail}
          editable={!submitting}
          accessibilityLabel={t('auth.email') ?? 'email'}
        />
        <TextInput
          placeholder={t('auth.password') ?? ''}
          autoCapitalize="none"
          autoCorrect={false}
          secureTextEntry
          textContentType="password"
          style={styles.input}
          value={password}
          onChangeText={setPassword}
          editable={!submitting}
          accessibilityLabel={t('auth.password') ?? 'password'}
        />
        {error ? <Text style={styles.error}>{error}</Text> : null}
        <TouchableOpacity
          onPress={handleSubmit}
          disabled={submitting}
          accessibilityRole="button"
          accessibilityLabel={t('auth.submit') ?? 'sign in'}
          style={[styles.button, submitting ? styles.buttonDisabled : null]}
        >
          {submitting ? (
            <ActivityIndicator color="#fff" />
          ) : (
            <Text style={styles.buttonLabel}>{t('auth.submit')}</Text>
          )}
        </TouchableOpacity>
        <TouchableOpacity
          onPress={onForgotPassword}
          accessibilityRole="link"
          accessibilityLabel={t('auth.forgot') ?? 'forgot password'}
          style={styles.linkRow}
        >
          <Text style={styles.link}>{t('auth.forgot')}</Text>
        </TouchableOpacity>
      </View>
    </KeyboardAvoidingView>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
    backgroundColor: '#f3f4f6',
    justifyContent: 'center',
    padding: 24,
  },
  card: {
    backgroundColor: '#fff',
    borderRadius: 12,
    padding: 24,
    shadowColor: '#000',
    shadowOpacity: 0.06,
    shadowRadius: 6,
    shadowOffset: { width: 0, height: 2 },
    elevation: 2,
  },
  title: {
    fontSize: 22,
    fontWeight: '600',
    color: '#111827',
    marginBottom: 16,
    textAlign: 'center',
  },
  input: {
    borderWidth: 1,
    borderColor: '#d1d5db',
    borderRadius: 8,
    padding: 12,
    marginBottom: 12,
    fontSize: 15,
    color: '#111827',
    backgroundColor: '#fff',
  },
  button: {
    backgroundColor: '#4f46e5',
    paddingVertical: 14,
    borderRadius: 8,
    alignItems: 'center',
    marginTop: 4,
  },
  buttonDisabled: { opacity: 0.6 },
  buttonLabel: { color: '#fff', fontSize: 15, fontWeight: '600' },
  linkRow: { alignItems: 'center', marginTop: 16 },
  link: { color: '#4f46e5', fontSize: 14 },
  error: { color: '#dc2626', fontSize: 13, marginBottom: 8, textAlign: 'center' },
  reasonBanner: {
    borderRadius: 8,
    padding: 12,
    marginBottom: 12,
    borderWidth: 1,
  },
  reasonBannerWarn: {
    backgroundColor: '#fef3c7',
    borderColor: '#facc15',
  },
  reasonBannerInfo: {
    backgroundColor: '#dbeafe',
    borderColor: '#3b82f6',
  },
  reasonBannerWarnText: { color: '#92400e', fontSize: 13, lineHeight: 18 },
  reasonBannerInfoText: { color: '#1e3a8a', fontSize: 13, lineHeight: 18 },
});
