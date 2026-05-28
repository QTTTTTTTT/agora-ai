# Sprint 4 / android-production — ProGuard / R8 rules for release builds.
#
# This file is referenced from android/app/build.gradle once you run the
# `npx react-native init` bootstrap (see docs/ANDROID_BOOTSTRAP.md) —
# copy it into android/app/ alongside the generated proguard-rules.pro
# (or replace it entirely; the bootstrap version is empty).
#
# Goals:
#   1. Strip unused classes / methods to keep the APK under the 30MB
#      MASVS-aligned size budget (Sprint 4 / qa-launch).
#   2. Preserve the native bridges React Native + each library expects
#      to find at runtime. Stripping these breaks `new SomeModule()` in
#      Java land — debugging is a nightmare without these keep rules.

# --- React Native core (Hermes / Fabric / TurboModules) -------------
-keep class com.facebook.react.** { *; }
-keep interface com.facebook.react.** { *; }
-keep class com.facebook.hermes.** { *; }
-keep class com.facebook.jni.** { *; }
-keep class com.facebook.fbreact.** { *; }

# --- JSC / Hermes intrinsics ---------------------------------------
-keepclassmembers class * {
    @com.facebook.react.uimanager.annotations.ReactProp <methods>;
    @com.facebook.react.uimanager.annotations.ReactPropGroup <methods>;
}
-dontwarn com.facebook.react.**

# --- Project-specific RN libraries ---------------------------------
# Keep classes the bridge looks up reflectively.
-keep class com.swmansion.reanimated.** { *; }
-keep class com.th3rdwave.safeareacontext.** { *; }
-keep class com.oblador.keychain.** { *; }
-keep class io.invertase.firebase.** { *; }
-keep class com.tencent.mmkv.** { *; }
-keep class com.rnssl.** { *; }
-keep class com.rnjailmonkey.** { *; }

# --- OkHttp / Retrofit (used by some RN libs) ----------------------
-dontwarn okhttp3.**
-dontwarn okio.**
-dontwarn javax.annotation.**

# --- Sentry --------------------------------------------------------
-keep class io.sentry.** { *; }
-dontwarn io.sentry.**

# --- App entry point + JS bridges ----------------------------------
# Replace com.fundai.platform with the actual package id you ship.
-keep class com.fundai.platform.MainActivity { *; }
-keep class com.fundai.platform.MainApplication { *; }

# --- Defensive: silence warnings on Java 8+ features -------------
-dontwarn java.lang.invoke.**
-dontwarn java.lang.management.**

# --- Optional: keep line numbers for crash reports -----------------
# Sentry uses these to map stack frames back to source.
-keepattributes SourceFile,LineNumberTable
-renamesourcefileattribute SourceFile
