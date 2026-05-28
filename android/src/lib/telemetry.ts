/**
 * telemetry — thin wrapper around @sentry/react-native + a local audit
 * sink for events we want to keep even when Sentry isn't configured.
 *
 * Sentry init 故意在运行时 lazy require — 没装 / dev 没填 DSN 时
 * silently no-op，整个 app 仍然能跑。生产 build 在 release CI 步骤
 * 注入 DSN（SENTRY_DSN env），然后通过 metro-define 写到运行时常量。
 *
 * 我们暴露 5 个事件：
 *   - rootedDevice           → 启动后 root 检测命中
 *   - biometricsFailed       → 解锁 prompt 被用户拒绝
 *   - networkError           → fetch 失败（非 401）
 *   - pushPermissionDenied   → FCM permission 拒绝
 *   - pushRegistered         → device token POST 成功
 *
 * "自建 telemetry" 部分：所有事件同时写一份到内存 audit ring buffer
 * （32 entries）— Sprint 4 的 "More" 页能看 last events，无需后端配
 * 合即可帮 ops 远程排障（用户截图 → 看 audit 列表 → 定位）。
 */

declare const process: { env: Record<string, string | undefined> } | undefined;

interface SentryLib {
  init(opts: { dsn?: string; tracesSampleRate?: number; debug?: boolean; environment?: string; release?: string }): void;
  captureException(err: unknown): void;
  captureMessage(msg: string, level?: 'fatal' | 'error' | 'warning' | 'info' | 'debug'): void;
  addBreadcrumb(crumb: { category?: string; message?: string; level?: string; data?: Record<string, unknown> }): void;
}

let sentry: SentryLib | null = null;
try {
  sentry = require('@sentry/react-native');
} catch {
  sentry = null;
}

export interface TelemetryEvent {
  ts: number;
  kind: string;
  data?: Record<string, unknown>;
}

const ringBuffer: TelemetryEvent[] = [];
const RING_LIMIT = 32;

export function initTelemetry(opts: { release: string; environment: string }): void {
  if (!sentry) return;
  const dsn = typeof process !== 'undefined' ? process.env.SENTRY_DSN : undefined;
  if (!dsn) return;
  sentry.init({
    dsn,
    tracesSampleRate: 0.1,
    environment: opts.environment,
    release: opts.release,
    debug: opts.environment !== 'production',
  });
}

export function reportEvent(kind: string, data?: Record<string, unknown>): void {
  const entry: TelemetryEvent = { ts: Date.now(), kind, data };
  ringBuffer.push(entry);
  if (ringBuffer.length > RING_LIMIT) ringBuffer.shift();
  sentry?.addBreadcrumb({ category: 'app', message: kind, data });
}

export function reportError(err: unknown, context?: Record<string, unknown>): void {
  const kind = err instanceof Error ? err.name : 'error';
  reportEvent(kind, context);
  if (sentry) {
    sentry.captureException(err);
  }
}

export function recentEvents(): TelemetryEvent[] {
  // Return a defensive copy so caller mutations don't corrupt the buffer.
  return [...ringBuffer];
}
