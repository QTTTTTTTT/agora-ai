/**
 * ResetPasswordScreen — 通过邮件深链 (`fundai://reset?token=...`) 进入。
 *
 * 流程：
 *   1. ForgotPasswordScreen 触发邮件
 *   2. 用户点邮件里的 link → 系统打开 app → App.tsx Linking 解析 token
 *   3. 渲染本 screen，用户输入新密码 → POST /api/auth/reset-password
 *   4. 成功后回到 LoginScreen，提示重新登录
 *
 * 不在 React Navigation stack 里（项目目前用 Gate 切换），所以本 screen
 * 由 Gate 直接 render，props 拿 token + onCancel + onSuccess。这样无须
 * 引入额外的 stack navigator。
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
import { apiClient } from '../lib/api';

interface Props {
  token: string;
  onCancel: () => void;
  onSuccess: () => void;
}

export default function ResetPasswordScreen({ token, onCancel, onSuccess }: Props): JSX.Element {
  const { t } = useTranslation();
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);

  async function handleSubmit(): Promise<void> {
    setError(null);
    setSuccess(null);
    if (password.length < 8) {
      setError(t('auth.errorInvalid'));
      return;
    }
    if (password !== confirm) {
      setError(t('auth.resetPasswordMismatch'));
      return;
    }
    setSubmitting(true);
    try {
      await apiClient.resetPassword({ token, new_password: password });
      setSuccess(t('auth.resetSuccess'));
      // 服务端清掉 token + 旧 session 后 onSuccess 把用户带回 LoginScreen
      // 让他们用新密码登录。
      setTimeout(onSuccess, 1200);
    } catch (err) {
      if (err instanceof ApiError && (err.code === 400 || err.code === 401 || err.code === 410)) {
        setError(t('auth.resetTokenInvalid'));
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
        <Text style={styles.title}>{t('auth.resetTitle')}</Text>
        <Text style={styles.hint}>{t('auth.resetHint')}</Text>
        <TextInput
          placeholder={t('auth.resetNewPassword') ?? ''}
          autoCapitalize="none"
          autoCorrect={false}
          secureTextEntry
          style={styles.input}
          value={password}
          onChangeText={setPassword}
          editable={!submitting && !success}
          accessibilityLabel={t('auth.resetNewPassword') ?? 'new password'}
        />
        <TextInput
          placeholder={t('auth.resetConfirmPassword') ?? ''}
          autoCapitalize="none"
          autoCorrect={false}
          secureTextEntry
          style={styles.input}
          value={confirm}
          onChangeText={setConfirm}
          editable={!submitting && !success}
          accessibilityLabel={t('auth.resetConfirmPassword') ?? 'confirm password'}
        />
        {error ? <Text style={styles.error}>{error}</Text> : null}
        {success ? <Text style={styles.success}>{success}</Text> : null}
        {!success ? (
          <TouchableOpacity
            onPress={() => void handleSubmit()}
            disabled={submitting}
            accessibilityRole="button"
            accessibilityLabel={t('auth.resetSubmit') ?? 'reset password'}
            style={[styles.button, submitting ? styles.buttonDisabled : null]}
          >
            {submitting ? (
              <ActivityIndicator color="#fff" />
            ) : (
              <Text style={styles.buttonLabel}>{t('auth.resetSubmit')}</Text>
            )}
          </TouchableOpacity>
        ) : null}
        <TouchableOpacity
          onPress={onCancel}
          accessibilityRole="link"
          accessibilityLabel={t('auth.backToLogin') ?? 'back to sign-in'}
          style={styles.linkRow}
        >
          <Text style={styles.link}>{t('auth.backToLogin')}</Text>
        </TouchableOpacity>
      </View>
    </KeyboardAvoidingView>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, backgroundColor: '#f3f4f6', justifyContent: 'center', padding: 24 },
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
  title: { fontSize: 20, fontWeight: '600', color: '#111827', marginBottom: 8, textAlign: 'center' },
  hint: { color: '#6b7280', fontSize: 13, marginBottom: 16, textAlign: 'center', lineHeight: 18 },
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
  button: { backgroundColor: '#4f46e5', paddingVertical: 14, borderRadius: 8, alignItems: 'center' },
  buttonDisabled: { opacity: 0.6 },
  buttonLabel: { color: '#fff', fontSize: 15, fontWeight: '600' },
  linkRow: { alignItems: 'center', marginTop: 16 },
  link: { color: '#4f46e5', fontSize: 14 },
  error: { color: '#dc2626', fontSize: 13, marginBottom: 8, textAlign: 'center' },
  success: { color: '#059669', fontSize: 14, textAlign: 'center', marginVertical: 12 },
});
