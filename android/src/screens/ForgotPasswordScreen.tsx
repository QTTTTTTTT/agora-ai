/**
 * ForgotPasswordScreen — 触发 server 端发送重置邮件。
 *
 * 端策略：成功 / 该邮箱不存在 服务器一律返回 200（防爆破），所以 UI
 * 永远显示"已发送"提示。只在 5xx / 网络故障时显示通用 error。
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

import { apiClient } from '../lib/api';

interface Props {
  onBack: () => void;
}

export default function ForgotPasswordScreen({ onBack }: Props): JSX.Element {
  const { t } = useTranslation();
  const [email, setEmail] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [sent, setSent] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleSubmit() {
    setError(null);
    if (!/.+@.+\..+/.test(email.trim())) {
      setError(t('auth.errorInvalid'));
      return;
    }
    setSubmitting(true);
    try {
      await apiClient.forgotPassword({ email: email.trim() });
      setSent(true);
    } catch {
      setError(t('auth.errorGeneric'));
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
        <Text style={styles.title}>{t('auth.forgotTitle')}</Text>
        <Text style={styles.hint}>{t('auth.forgotHint')}</Text>
        <TextInput
          placeholder={t('auth.email') ?? ''}
          autoCapitalize="none"
          autoCorrect={false}
          keyboardType="email-address"
          style={styles.input}
          value={email}
          onChangeText={setEmail}
          editable={!submitting && !sent}
          accessibilityLabel={t('auth.email') ?? 'email'}
        />
        {error ? <Text style={styles.error}>{error}</Text> : null}
        {sent ? (
          <Text style={styles.success} accessibilityRole="alert">{t('auth.forgotSent')}</Text>
        ) : (
          <TouchableOpacity
            onPress={handleSubmit}
            disabled={submitting}
            accessibilityRole="button"
            accessibilityLabel={t('auth.forgotSubmit') ?? 'send email'}
            style={[styles.button, submitting ? styles.buttonDisabled : null]}
          >
            {submitting ? (
              <ActivityIndicator color="#fff" />
            ) : (
              <Text style={styles.buttonLabel}>{t('auth.forgotSubmit')}</Text>
            )}
          </TouchableOpacity>
        )}
        <TouchableOpacity
          onPress={onBack}
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
  hint: { color: '#6b7280', fontSize: 13, marginBottom: 16, textAlign: 'center' },
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
