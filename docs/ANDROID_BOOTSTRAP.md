# Android RN bootstrap guide

Sprint 3 / `android-bootstrap` 阶段交付物。本文档把仓库中的 `android/` skeleton 变成一个真正可装机运行的 RN 0.74 应用所需要的所有步骤。

## 前置

在 dev 机上一次性安装：

| 软件 | 版本 | 备注 |
| --- | --- | --- |
| Node.js | 18.18+ | `nvm install 18` |
| JDK | 17 (Temurin/OpenJDK 推荐) | `brew install --cask temurin@17` |
| Android Studio | Hedgehog (2023.1.1) 或更新 | 装 SDK Platform 34 + Build-Tools 34.0.0 |
| Watchman | 最新 | `brew install watchman` |
| CocoaPods | 1.15+ | iOS 用，`sudo gem install cocoapods` |

环境变量（macOS `~/.zshrc`）：

```bash
export JAVA_HOME=/Library/Java/JavaVirtualMachines/temurin-17.jdk/Contents/Home
export ANDROID_HOME=$HOME/Library/Android/sdk
export PATH=$PATH:$ANDROID_HOME/emulator:$ANDROID_HOME/platform-tools
```

## 步骤 1：装 workspace 依赖

```bash
cd <repo-root>
npm install
npm run build:shared    # 编译 @fundai/api-client → dist/
```

## 步骤 2：生成 RN native shell

仓库里的 `android/` 目录目前只有业务 src + 配置文件，缺乏 Gradle / Xcode 工程。我们用官方 init 脚本生成壳，然后把它们搬进来。

```bash
cd <repo-root>
npx react-native init FundAINative --version 0.74.3 --skip-install
# 把原生壳搬进 monorepo 里的 android/
cp -R FundAINative/android <repo-root>/android/android-native
cp -R FundAINative/ios <repo-root>/android/ios
rm -rf FundAINative
```

> **为什么不直接用 `android/` 作目标？**
> RN 官方 init 会要求目录名也叫 `MyApp` 而不是 `android`，而且 Gradle root 必须是 `android-native/` 一层（避免和 monorepo 的 "android/" 上层目录混淆）。所以我们用 **android-native** 作为 Gradle root，metro 里把 `projectRoot=android-native`。

## 步骤 3：在 android-native/ 里启用 Hermes + New Architecture

`android/android-native/gradle.properties`：

```properties
# Hermes JS engine（默认已是 true on RN 0.74）
hermesEnabled=true
# New Architecture (Fabric + TurboModules) — RN 0.74 通过。
newArchEnabled=true
```

`android/android-native/app/build.gradle` 确认 `react.gradle` 配置：

```gradle
react {
    enableHermes = true
}
```

## 步骤 4：配置 monorepo metro

仓库已经写好 `android/metro.config.js`，把 workspace 根 + 根的 `node_modules` 都加进 watchFolders，确保 `@fundai/api-client` 可以被 metro 解析到。

## 步骤 5：第一次构建

```bash
cd <repo-root>/android
npm install   # 装 RN deps（约 200MB）
npm run start &   # 启动 metro
adb devices   # 确认有模拟器
npm run android
```

约 8 分钟首次 Gradle 构建后即可看到 5 tab UI。

## 后续阶段

| 阶段 | 内容 |
| --- | --- |
| `android-core` | 5 页接入真实 apiClient；keychain 存 token；FCM 推送；离线缓存 MMKV |
| `android-production` | SSL pinning；ProGuard；Sentry；GitHub Actions CI 出 APK |
| `qa-launch` | Play Internal Track → Closed Beta → Production |

## 常见问题

- **Metro 起不来 + `@fundai/api-client not found`** — 在 monorepo 根执行 `npm install` 而不是单独在 android/ 里。
- **Gradle build 报 `Could not find tools.jar`** — JDK 17 没装或 JAVA_HOME 没指到 Temurin。
- **APK 太大** — 检查 `gradle.properties` 是否开了 `android.enableShrinkResourcesInReleaseBuilds=true`；release build 会跑 R8/ProGuard。
- **真机 USB 调试连不上** — 先 `adb kill-server && adb start-server`，再开手机的 USB Debugging。
