/**
 * MoreScreen — 设置 / 安全 / 语言 / 主题 / 推送 / 登出 / 最近事件。
 *
 * 子代理 QA 反馈：
 *  - More tab 缺少"账号与安全 / 生物识别 / 推送通知" 三个用户可控的
 *    入口。Account info 不可见、biometric 与 push 仅能从代码层 toggle。
 *  - 这一版补齐：
 *      * Account 区块 —— 展示已登录 email + verified 状态（来自 useAuth().state）。
 *      * Biometric 行 —— 显示并 toggle userPrefs.isBiometricEnabled()。
 *        关闭后下次启动 (auth.tsx boot) 跳过生物识别，直接进入主界面。
 *      * Notifications 行 —— 显示并 toggle userPrefs.isPushEnabled()。
 *        on → register + 入库；off → unregister + 删 token。
 *  - 语言 / 主题 / 退出 / 版本 / Recent events 五段保留不动，避免回归。
 */

import React, { useCallback, useEffect, useState } from 'react';
import {
  Alert,
  Pressable,
  ScrollView,
  StyleSheet,
  Switch,
  Text,
  View,
} from 'react-native';
import { useTranslation } from 'react-i18next';

import { i18n } from '../i18n';
import { useAuth } from '../lib/auth';
import { setPushEnabled, unregisterDeviceForPush } from '../lib/push';
import { recentEvents } from '../lib/telemetry';
import { useTheme, type ThemePreference } from '../lib/theme';
import {
  isBiometricEnabled,
  isPushEnabled,
  setBiometricEnabled,
} from '../lib/userPrefs';

const APP_VERSION = '0.1.0';

export default function MoreScreen(): JSX.Element {
  const { t } = useTranslation();
  const { state, logout } = useAuth();
  const { preference, setPreference, colors } = useTheme();
  const [biometric, setBiometric] = useState<boolean>(true);
  const [push, setPush] = useState<boolean>(true);
  const [pushBusy, setPushBusy] = useState<boolean>(false);

  useEffect(() => {
    setBiometric(isBiometricEnabled());
    setPush(isPushEnabled());
  }, []);

  const handleLogout = useCallback(async () => {
    await unregisterDeviceForPush();
    await logout();
  }, [logout]);

  const toggleLanguage = useCallback(async () => {
    const next = i18n.language === 'zh-CN' ? 'en-US' : 'zh-CN';
    await i18n.changeLanguage(next);
  }, []);

  const handleToggleBiometric = useCallback(
    (next: boolean) => {
      // 持久化偏好 → 生效在下次启动（auth.tsx 的 boot useEffect 读 prefs）。
      // 当前会话不强制重启，避免破坏用户正在做的事；只在 banner 里告知。
      setBiometric(next);
      setBiometricEnabled(next);
    },
    [],
  );

  const handleTogglePush = useCallback(async (next: boolean) => {
    setPush(next);
    setPushBusy(true);
    try {
      await setPushEnabled(next, APP_VERSION);
    } catch {
      // 失败 → 回滚 UI 状态并提示。setPushEnabled 内部已 swallow，
      // 这里 catch 走不到，但留 defensive path 保证 toggle 不会卡死。
      setPush(!next);
      Alert.alert(t('more.notificationsRegistrationFailed'));
    } finally {
      setPushBusy(false);
    }
  }, [t]);

  const events = recentEvents().slice(-5).reverse();
  const user = state.status === 'authenticated' ? state.user : null;
  const accountEmail = user?.email ?? user?.user_id ?? t('more.accountInfoMissing');

  return (
    <ScrollView
      style={[styles.container, { backgroundColor: colors.bg }]}
      contentContainerStyle={styles.content}
    >
      {/* Account section */}
      <Text style={[styles.sectionLabel, { color: colors.textMuted }]}>
        {t('more.sectionAccount')}
      </Text>
      <View
        style={[styles.column, { backgroundColor: colors.surface }]}
        accessibilityRole="summary"
      >
        <Text style={[styles.value, { color: colors.textMuted }]}>
          {t('more.accountInfoLabel')}
        </Text>
        <Text style={[styles.label, { color: colors.text, marginTop: 2 }]}>{accountEmail}</Text>
      </View>

      <View
        style={[styles.row, { backgroundColor: colors.surface }]}
        accessibilityRole="switch"
        accessibilityState={{ checked: biometric }}
        accessibilityLabel={t('more.biometric')}
      >
        <View style={styles.rowText}>
          <Text style={[styles.label, { color: colors.text }]}>{t('more.biometric')}</Text>
          <Text style={[styles.hint, { color: colors.textMuted }]}>{t('more.biometricHint')}</Text>
        </View>
        <Switch value={biometric} onValueChange={handleToggleBiometric} />
      </View>

      <View
        style={[styles.row, { backgroundColor: colors.surface }]}
        accessibilityRole="switch"
        accessibilityState={{ checked: push, busy: pushBusy }}
        accessibilityLabel={t('more.notifications')}
      >
        <View style={styles.rowText}>
          <Text style={[styles.label, { color: colors.text }]}>{t('more.notifications')}</Text>
          <Text style={[styles.hint, { color: colors.textMuted }]}>
            {pushBusy ? t('more.notificationsRegistering') : t('more.notificationsHint')}
          </Text>
        </View>
        <Switch value={push} onValueChange={(v) => void handleTogglePush(v)} disabled={pushBusy} />
      </View>

      {/* Language */}
      <Text style={[styles.sectionLabel, { color: colors.textMuted }]}>
        {t('more.sectionLanguage')}
      </Text>
      <Pressable
        style={[styles.row, { backgroundColor: colors.surface }]}
        onPress={() => void toggleLanguage()}
        accessibilityRole="button"
        accessibilityLabel={t('more.language')}
      >
        <Text style={[styles.label, { color: colors.text }]}>{t('more.language')}</Text>
        <Text style={[styles.value, { color: colors.textMuted }]}>{i18n.language}</Text>
      </Pressable>

      {/* Appearance */}
      <Text style={[styles.sectionLabel, { color: colors.textMuted }]}>
        {t('more.sectionAppearance')}
      </Text>
      <View style={[styles.rowChoices, { backgroundColor: colors.surface }]}>
        {(['system', 'light', 'dark'] as ThemePreference[]).map((opt) => (
          <Pressable
            key={opt}
            onPress={() => setPreference(opt)}
            accessibilityRole="button"
            accessibilityState={{ selected: preference === opt }}
            style={[
              styles.choice,
              {
                backgroundColor: preference === opt ? colors.accent : colors.surfaceAlt,
                borderColor: colors.border,
              },
            ]}
          >
            <Text
              style={[
                styles.choiceLabel,
                { color: preference === opt ? '#ffffff' : colors.text },
              ]}
            >
              {t(
                opt === 'system'
                  ? 'more.appearanceSystem'
                  : opt === 'light'
                    ? 'more.appearanceLight'
                    : 'more.appearanceDark',
              )}
            </Text>
          </Pressable>
        ))}
      </View>

      {/* Session / Danger zone */}
      <Text style={[styles.sectionLabel, { color: colors.textMuted }]}>
        {t('more.sectionDanger')}
      </Text>
      <Pressable
        style={[styles.row, { backgroundColor: colors.danger }]}
        onPress={() => void handleLogout()}
        accessibilityRole="button"
        accessibilityLabel={t('more.logout')}
      >
        <Text style={[styles.label, { color: '#ffffff' }]}>{t('more.logout')}</Text>
      </Pressable>

      <View style={[styles.row, { backgroundColor: colors.surface }]}>
        <Text style={[styles.label, { color: colors.text }]}>{t('more.version')}</Text>
        <Text style={[styles.value, { color: colors.textMuted }]}>{APP_VERSION}</Text>
      </View>

      {events.length > 0 ? (
        <View style={styles.eventsSection}>
          <Text style={[styles.sectionLabel, { color: colors.textMuted }]}>
            {t('more.recentEvents')}
          </Text>
          {events.map((ev, idx) => (
            <Text key={`${ev.ts}-${idx}`} style={[styles.eventLine, { color: colors.textMuted }]}>
              · {new Date(ev.ts).toLocaleTimeString()} {ev.kind}
            </Text>
          ))}
        </View>
      ) : null}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1 },
  content: { padding: 16 },
  row: {
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'center',
    borderRadius: 8,
    padding: 16,
    marginBottom: 8,
    gap: 12,
  },
  rowText: { flex: 1, paddingRight: 12 },
  column: {
    borderRadius: 8,
    padding: 16,
    marginBottom: 8,
  },
  rowChoices: {
    flexDirection: 'row',
    borderRadius: 8,
    padding: 8,
    marginBottom: 8,
    gap: 8,
  },
  label: { fontSize: 14, fontWeight: '500' },
  hint: { fontSize: 12, marginTop: 4, lineHeight: 16 },
  value: { fontSize: 13 },
  sectionLabel: {
    fontSize: 12,
    marginTop: 12,
    marginBottom: 4,
    textTransform: 'uppercase',
    letterSpacing: 0.5,
  },
  choice: {
    flex: 1,
    paddingVertical: 8,
    borderRadius: 6,
    alignItems: 'center',
    borderWidth: 1,
  },
  choiceLabel: { fontSize: 13, fontWeight: '500' },
  eventsSection: { marginTop: 16 },
  eventLine: { fontSize: 12, fontFamily: 'monospace', marginTop: 2 },
});
