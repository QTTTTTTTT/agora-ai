/**
 * App.tsx — RN navigation shell.
 *
 * Sprint 4 / android-core 改动：
 *  - 增加 AuthProvider — 未登录显示 LoginScreen，登录后才挂主 tab
 *  - 接入 react-query persistence (MMKV) — 离线缓存
 *  - 登录成功后注册 FCM device token
 *
 * 5 个底部 tab：Home / Decisions / Memory / Team / More
 */

import React, { useCallback, useEffect, useState } from 'react';
import { ActivityIndicator, Linking, StyleSheet, View } from 'react-native';
import { NavigationContainer } from '@react-navigation/native';
import { createBottomTabNavigator } from '@react-navigation/bottom-tabs';
import { QueryClientProvider } from '@tanstack/react-query';
import { SafeAreaProvider } from 'react-native-safe-area-context';
import { I18nextProvider } from 'react-i18next';

import { i18n } from './i18n';
import HomeScreen from './screens/HomeScreen';
import DecisionsScreen from './screens/DecisionsScreen';
import OrdersScreen from './screens/OrdersScreen';
import MemoryScreen from './screens/MemoryScreen';
import TeamScreen from './screens/TeamScreen';
import MoreScreen from './screens/MoreScreen';
import LoginScreen from './screens/LoginScreen';
import ForgotPasswordScreen from './screens/ForgotPasswordScreen';
import ResetPasswordScreen from './screens/ResetPasswordScreen';

import { AuthProvider, useAuth } from './lib/auth';
import { initQueryPersistence, queryClient } from './lib/queryClient';
import { registerDeviceForPush } from './lib/push';
import { ThemeProvider } from './lib/theme';
import { checkDeviceSecurity, enableScreenCapturePrevention } from './lib/security';
import { initTelemetry, reportEvent } from './lib/telemetry';

const Tab = createBottomTabNavigator();
const APP_VERSION = '0.1.0';

function MainTabs(): JSX.Element {
  return (
    <Tab.Navigator
      screenOptions={{
        headerShown: true,
        tabBarActiveTintColor: '#4f46e5',
        tabBarInactiveTintColor: '#6b7280',
      }}
    >
      <Tab.Screen name="Home" component={HomeScreen} options={{ title: i18n.t('tabs.home') }} />
      <Tab.Screen name="Decisions" component={DecisionsScreen} options={{ title: i18n.t('tabs.decisions') }} />
      <Tab.Screen name="Orders" component={OrdersScreen} options={{ title: i18n.t('tabs.orders') }} />
      <Tab.Screen name="Memory" component={MemoryScreen} options={{ title: i18n.t('tabs.memory') }} />
      <Tab.Screen name="Team" component={TeamScreen} options={{ title: i18n.t('tabs.team') }} />
      <Tab.Screen name="More" component={MoreScreen} options={{ title: i18n.t('tabs.more') }} />
    </Tab.Navigator>
  );
}

/**
 * extractResetToken — 解析重置密码深链。
 *   - `fundai://reset?token=abc`            (custom scheme)
 *   - `https://app.fundai.com/reset?token=` (universal link / app link)
 * 兼容这两种以及 query-string 在 hash 段的写法 (#token=)，
 * 后端在 042 migration 里固定参数名是 `token`。
 */
export function extractResetToken(url: string | null | undefined): string | null {
  if (!url) return null;
  try {
    const m = /[?#&]token=([^&]+)/.exec(url);
    if (!m) return null;
    const raw = decodeURIComponent(m[1]);
    return raw.length > 0 ? raw : null;
  } catch {
    return null;
  }
}

function Gate(): JSX.Element {
  const { state } = useAuth();
  const [showForgot, setShowForgot] = useState(false);
  const [resetToken, setResetToken] = useState<string | null>(null);

  useEffect(() => {
    if (state.status === 'authenticated') {
      void registerDeviceForPush(APP_VERSION);
    }
  }, [state.status]);

  // Deep-link listener：邮件 link → app 启动 / 已运行 → 解析 token。
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const url = await Linking.getInitialURL();
        const token = extractResetToken(url);
        if (token && !cancelled) setResetToken(token);
      } catch {
        /* swallow — Linking 不可用就不进入 reset 流 */
      }
    })();
    const sub = Linking.addEventListener('url', (event) => {
      const token = extractResetToken(event.url);
      if (token) setResetToken(token);
    });
    return () => {
      cancelled = true;
      sub.remove();
    };
  }, []);

  const handleResetSuccess = useCallback(() => {
    setResetToken(null);
    // 服务端已 invalidate session；本地 token 由 auth.tsx 的 401 hook
    // 处理。这里只清掉 deep-link state 让用户回 LoginScreen。
  }, []);

  if (state.status === 'loading') {
    return (
      <View style={styles.center}>
        <ActivityIndicator size="large" color="#4f46e5" />
      </View>
    );
  }

  // 优先级最高：reset 深链。即使已登录，也允许用户走重置流（与 web 端一致）。
  if (resetToken) {
    return (
      <ResetPasswordScreen
        token={resetToken}
        onCancel={() => setResetToken(null)}
        onSuccess={handleResetSuccess}
      />
    );
  }

  if (state.status === 'unauthenticated') {
    if (showForgot) {
      return <ForgotPasswordScreen onBack={() => setShowForgot(false)} />;
    }
    return <LoginScreen onForgotPassword={() => setShowForgot(true)} />;
  }

  return (
    <NavigationContainer>
      <MainTabs />
    </NavigationContainer>
  );
}

export default function App(): JSX.Element {
  useEffect(() => {
    initTelemetry({ release: APP_VERSION, environment: __DEV__ ? 'development' : 'production' });
    initQueryPersistence();
    enableScreenCapturePrevention();
    const verdict = checkDeviceSecurity();
    if (verdict.rooted || verdict.hookDetected) {
      reportEvent('security.compromised', verdict as unknown as Record<string, unknown>);
    }
  }, []);

  return (
    <SafeAreaProvider>
      <I18nextProvider i18n={i18n}>
        <QueryClientProvider client={queryClient}>
          <ThemeProvider>
            <AuthProvider>
              <Gate />
            </AuthProvider>
          </ThemeProvider>
        </QueryClientProvider>
      </I18nextProvider>
    </SafeAreaProvider>
  );
}

const styles = StyleSheet.create({
  center: { flex: 1, alignItems: 'center', justifyContent: 'center', backgroundColor: '#f3f4f6' },
});
