# FundAI Android (React Native skeleton)

Sprint 3 / `android-bootstrap` 阶段产出。当前仓库中是 RN 0.74 TypeScript 项目的**业务层 skeleton**：5 个底 tab、5 个 mock screen、i18n、react-query、shared `@fundai/api-client` 接线。

**还没有生成原生 `android/` / `ios/` Gradle / Xcode 项目目录** —— 这一步需要在装好 JDK 17 + Android SDK 的开发机上执行（见下文）。

## 目录结构

```
android/
├── package.json          # RN 0.74 + tamagui + i18next + react-query + victory
├── tsconfig.json
├── babel.config.js       # @react-native/babel-preset + reanimated/plugin
├── metro.config.js       # watchFolders 把 workspace 根 + node_modules 加上
├── app.json              # AppRegistry name = FundAI
├── index.js              # entry: AppRegistry.registerComponent(App)
└── src/
    ├── App.tsx           # NavigationContainer + Tab.Navigator
    ├── i18n/index.ts     # zh-CN / en-US 字典
    ├── lib/api.ts        # createClient(@fundai/api-client) + AsyncStorage token
    └── screens/          # Home / Decisions / Memory / Team / More
```

## 起跑步骤（一次性，需要 JDK + Android SDK）

```bash
# 1) workspace 根装依赖（含 shared/api-client）
cd <repo-root>
npm install

# 2) 在 android 目录下生成原生壳
cd android
npx react-native init FundAINative --template react-native-template-typescript --skip-install
# 复制生成的 FundAINative/android/ 和 FundAINative/ios/ 到当前目录
mv FundAINative/android ./android-native
mv FundAINative/ios ./ios
rm -rf FundAINative

# 3) 把 android-native/ 重命名为 android/ 是反模式（与 monorepo 这个目录冲突），
#    建议保留 android-native/ 命名并在 metro / gradle 里指向它。

# 4) 启动 Metro
npm run start

# 5) 另开 terminal 运行 Android
adb devices  # 确认有模拟器或物理机
npm run android
```

## 已接的依赖

| 依赖 | 用途 |
| --- | --- |
| `@react-navigation/bottom-tabs` + `native` | 5 底 tab + navigation root |
| `@tanstack/react-query` | fetch / cache / pull-to-refresh |
| `react-i18next` + `i18next` | 中英双语 |
| `tamagui` | 跨平台 UI primitive（暂只装，第二阶段接入） |
| `victory-native` | 图表 — NAV 曲线、PnL bar |
| `@react-native-async-storage/async-storage` | dev 阶段 token 存储 |
| `react-native-svg` / `react-native-reanimated` / `react-native-gesture-handler` | 上面几个包的 native peer deps |
| `@fundai/api-client` | 跨端 HTTP 客户端 |

## 与 web 的关系

| 关注点 | web | android |
| --- | --- | --- |
| HTTP API 客户端 | `@fundai/api-client` | `@fundai/api-client` |
| 路由 | react-router-dom | @react-navigation |
| 状态/请求 | 自实现 fetch | react-query |
| i18n | 自实现 PreferencesProvider | react-i18next |
| Token 存储 | localStorage | AsyncStorage → Sprint 4 切 keychain |

## 已知的 skeleton 限制

- 5 个 screen 都用 mock 数据；HomeScreen 已经尝试调真实 API，失败时降级 mock。其他 4 个 screen 全 mock。Sprint `android-core` 阶段会把 4 个 screen 都接通 apiClient。
- 没有登录页 / 登录态守卫；目前打开 App 直接进 5 tab。`android-core` 会加 stack navigator + Login screen + token guard。
- 没有暗色模式 / 主题切换；`android-production` 会加。
- 没接 SSL pinning / Sentry / Detox；`android-production` 阶段。
- 没接生物识别 / FCM 推送；`android-core` 阶段。

## 单元/类型检查

```bash
cd android
npm run typecheck
```

会跑 `tsc --noEmit`。当前 skeleton 通过类型检查（无需安装 native deps 即可校验业务层）。
