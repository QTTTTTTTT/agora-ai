// lib/lazyWithRetry.ts
//
// `Failed to fetch dynamically imported module` 是 SPA + lazy-route 一个出名
// 的 race condition。它**不是**服务端 bug —— 服务端返回的 chunk 实际是 200 +
// text/javascript。它发生在三种典型场景：
//
//   1) **网络抖动**：单次 chunk fetch 偶发 TCP 重置、丢包重传超时、Wi-Fi 切换、
//      VPN 断开。下一秒重试就成功。
//   2) **用户行为打断**：chunk 还在下载，用户已经路由跳走 / 关闭 tab / 点了后退，
//      浏览器取消请求；React.lazy 的 Promise 被 reject。
//   3) **跨 deploy 拿到 stale entry**：用户保持 tab 多天没关，期间我们重建了
//      frontend → entry chunk 引用的 hashed page chunk 已经被新 build 删掉。
//      旧 entry 触发 import 旧 hash → 服务端真的 404。
//
// 单次原生 `lazy(() => import(...))` 完全没保护：任意一次失败 → 整个路由 crash
// 到 ErrorBoundary，用户看到的就是裸的 "Failed to fetch dynamically imported
// module"。这个 wrapper 提供两层防线：
//
//   A) **重试**：失败 → 等 `delays[i]` ms → 重试。覆盖 (1)(2) 偶发抖动。
//   B) **兜底刷新**：所有重试都耗尽 → 标记一个 sessionStorage key 防止
//      reload 死循环 → `location.reload()` 拉取最新 index.html（新 entry 会
//      pin 新 hash 的 chunk）。覆盖 (3) stale entry 场景。
//
// 实现注意点：
//
//   - **Promise 缓存**：原生 `lazy` 缓存 import 返回的 Promise。如果第一次
//     reject 了，第二次访问该路由仍拿到同一个 rejected Promise，永远不会重试。
//     我们必须包一层 factory，在每次重试时**重新调用 importFn()**，拿到新的
//     fresh Promise。
//   - **不污染失败错误**：兜底刷新走的是 `sessionStorage.hasReloaded` 标志位
//     —— 同一 session 只 reload 一次，避免新 chunk 也坏的极端场景把用户卡进
//     永久刷新循环。第二次失败时让原生错误冒泡到 ErrorBoundary，用户至少能看
//     到 "重试"按钮和导航。
//   - **类型对齐 React.lazy**：返回签名与 `lazy()` 一致，调用方零侵入替换。

import { ComponentType, lazy, LazyExoticComponent } from "react";

// Use `ComponentType<any>` instead of `ComponentType<unknown>` to match
// the exact constraint React.lazy itself uses. With `unknown` the prop
// types of strict `React.FC<{}>` exports (no-props pages like `Wallet`)
// are not assignable to the importer's promise shape and the call site
// gets a wall of "FC<{}> is not assignable to ComponentType<unknown>"
// errors. `any` is the standard escape hatch here and matches @types/react.
//
type Importer<T extends ComponentType<any>> = () => Promise<{ default: T }>;

const DEFAULT_DELAYS_MS = [200, 600, 1200];
const RELOAD_FLAG = "fundai:chunk-reload-attempted";

function isChunkLoadError(err: unknown): boolean {
  if (!err) return false;
  const msg = typeof err === "string" ? err : (err as Error).message || "";
  const name = (err as Error).name || "";
  return (
    /Failed to fetch dynamically imported module/i.test(msg) ||
    /Importing a module script failed/i.test(msg) ||
    /error loading dynamically imported module/i.test(msg) ||
    // The "MIME mismatch" failure mode: when the SPA fallback or a
    // misconfigured CDN serves text/html for a chunk URL, V8 / Webkit
    // surface a TypeError whose message is exactly the line below.
    // It is morally a stale-deploy failure (the chunk URL no longer
    // resolves to a real .js file), so we want to retry/reload —
    // but the bare regex above misses it because there's no "fetch"
    // verb in the message.
    /Cannot read propert(?:y|ies) of undefined \(reading 'default'\)/i.test(msg) ||
    name === "ChunkLoadError"
  );
}

// hasModuleDefault returns true iff `mod` looks like a real ESM module
// namespace with a `default` export. This is the second line of defence
// against "server returned HTML/JSON for a chunk URL": the network call
// succeeds, but the resulting module's namespace is empty (or shaped
// like `{}` from a misparsed JSON 404 body), so React.lazy then trips
// on `module.default` later and surfaces a TypeError. We catch the
// shape problem at import time so retry/reload kicks in immediately
// instead of after the lazy boundary already committed.
function hasModuleDefault<T>(mod: unknown): mod is { default: T } {
  return (
    typeof mod === "object" &&
    mod !== null &&
    typeof (mod as { default?: unknown }).default !== "undefined"
  );
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

async function importWithRetry<T extends ComponentType<any>>(
  importer: Importer<T>,
  delays: number[],
): Promise<{ default: T }> {
  let lastErr: unknown;
  for (let attempt = 0; attempt <= delays.length; attempt += 1) {
    try {
      const mod = await importer();
      // Shape guard: a successful await but an empty namespace
      // means we got a non-JS payload (HTML SPA fallback, JSON
      // 404 body, …). Treat it as a chunk-load failure so we
      // burn a retry / reload instead of leaking a TypeError out
      // to the ErrorBoundary.
      if (!hasModuleDefault<T>(mod)) {
        throw new Error("Importing a module script failed: namespace missing default export");
      }
      return mod;
    } catch (err) {
      lastErr = err;
      if (!isChunkLoadError(err)) {
        // Real bugs (TypeError inside the module, SyntaxError, …) should
        // not be papered over with retries — they'll never recover and
        // we'd just hide the stack trace from the developer.
        throw err;
      }
      if (attempt < delays.length) {
        await delay(delays[attempt]);
      }
    }
  }

  // All retries exhausted. If we haven't already tried a hard reload this
  // session, do one now — typically resolves the "stale entry across
  // deploy" case where the user has been on the tab for hours and we
  // shipped a new bundle.
  if (typeof window !== "undefined" && typeof sessionStorage !== "undefined") {
    try {
      const tried = sessionStorage.getItem(RELOAD_FLAG);
      if (!tried) {
        sessionStorage.setItem(RELOAD_FLAG, String(Date.now()));
        window.location.reload();
        // Block forever so React doesn't render an error boundary in the
        // split second before the navigation actually happens.
        return await new Promise<{ default: T }>(() => {});
      }
    } catch {
      // sessionStorage may throw in private mode + iframe + storage quota.
      // Fall through to the rethrow so the ErrorBoundary at least renders.
    }
  }

  throw lastErr;
}

/**
 * Drop-in replacement for `React.lazy(() => import('./Page'))` that
 * survives transient chunk-load failures. Same return type and same
 * usage; no other call-site changes required.
 */
export function lazyWithRetry<T extends ComponentType<any>>(
  importer: Importer<T>,
  delays: number[] = DEFAULT_DELAYS_MS,
): LazyExoticComponent<T> {
  return lazy(() => importWithRetry(importer, delays));
}

/**
 * Mark the current session as "reload not yet attempted" so the next
 * chunk-load failure is allowed to trigger one. Call this on successful
 * route renders if you want the reload escape hatch to remain available
 * for the rest of the session (default: a single reload per tab).
 */
export function resetChunkReloadFlag(): void {
  if (typeof sessionStorage === "undefined") return;
  try {
    sessionStorage.removeItem(RELOAD_FLAG);
  } catch {
    // ignore
  }
}
