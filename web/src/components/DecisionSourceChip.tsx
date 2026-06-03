// DecisionSourceChip — Sprint 11.3 transparency chip.
//
// Renders a single inline badge that tells the user where the plan
// came from: a real LLM run (claude-opus, gpt-4o, ...) or the
// deterministic rule-based fallback. Fallback chips carry a short,
// non-technical reason ("AI service rate-limited", "AI context
// length exceeded", ...) and a popover with the suggested next step.
//
// Design guarantees (S11 charter):
//   - We NEVER show the raw provider error string here. The API
//     layer already strips errorclass.Detail.Summary before
//     serialising; the chip surface is restricted to category +
//     provider only.
//   - Categories are a closed enum (see DecisionFallbackCategory in
//     web/src/lib/api.ts). Unknown values fall through to the
//     generic "AI service error" string instead of leaking the raw
//     enum value to users.
//   - The chip is always visible when the field is populated. We
//     deliberately do not hide it on small viewports — knowing the
//     provenance of an investment decision is a baseline regulatory
//     transparency requirement.
//
// Usage:
//   <DecisionSourceChip
//     language={language}
//     source={plan.decisionSource}
//     reason={plan.fallbackReason}
//   />

import { useState } from "react";

import type {
  DecisionFallbackCategory,
  DecisionSource,
  PlanFallbackReason,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Props {
  language: Language;
  source?: DecisionSource;
  reason?: PlanFallbackReason;
  className?: string;
}

interface CategoryCopy {
  title: string;
  hint: string;
}

interface ChipCopy {
  llmPM: string;
  llmThreeStage: string;
  fallback: string;
  fallbackNoLLM: string;
  legacy: string;
  unknownCategory: CategoryCopy;
  unknownLabel: string;
  categories: Record<DecisionFallbackCategory, CategoryCopy>;
  providerLabel: string;
  noActionHint: string;
}

const messages: Record<Language, ChipCopy> = {
  "zh-CN": {
    llmPM: "AI 决策 · 单次 PM",
    llmThreeStage: "AI 决策 · 三段式",
    fallback: "规则兜底",
    fallbackNoLLM: "规则兜底 · 未配置模型",
    legacy: "历史计划",
    unknownLabel: "未知",
    providerLabel: "供应商",
    noActionHint: "如需重新生成，可在决策中心手动重跑工作流。",
    unknownCategory: {
      title: "AI 服务异常",
      hint: "本计划由兜底规则生成而非大模型给出。若反复出现，请联系管理员排查。",
    },
    categories: {
      rate_limited: {
        title: "模型限流",
        hint: "AI 模型 5-10 分钟后通常会恢复，可稍后重新触发本基金的工作流。",
      },
      service_unavailable: {
        title: "模型服务不可用",
        hint: "供应商或代理暂时不可达。可在几分钟后重试；若持续，请联系管理员核查 LLM 健康面板。",
      },
      auth_failed: {
        title: "模型鉴权失败",
        hint: "平台或基金维度的 API 密钥被拒。请到基金设置 → 模型配置或联系管理员核对密钥状态。",
      },
      context_length_exceeded: {
        title: "上下文超长",
        hint: "本次提示超过模型上下文上限。可在基金设置降低 universe 数量或切换更长上下文的模型档位。",
      },
      invalid_request: {
        title: "请求被拒",
        hint: "请求被供应商判定为非法。通常由临时配置错误触发，可重试一次确认是否已恢复。",
      },
      schema_validation_failed: {
        title: "模型输出格式错误",
        hint: "模型返回的 JSON 不符合预期结构，多为偶发抖动。重试通常可恢复。",
      },
      network_timeout: {
        title: "网络超时",
        hint: "到供应商或代理的网络出现超时。重试通常可恢复；若持续请联系平台运维。",
      },
      budget_exceeded: {
        title: "调用预算用尽",
        hint: "本基金或本步骤的日/分钟调用配额已耗尽。可在订阅页面升档或等待窗口重置。",
      },
      empty_response: {
        title: "模型返回为空",
        hint: "模型连续返回空内容，多由提示与模型不匹配导致。重试或更换模型档位通常可恢复。",
      },
      cancelled: {
        title: "调用被取消",
        hint: "调用被上游取消（如工作流超时或人工中止）。可手动重新触发本工作流。",
      },
      unknown: {
        title: "AI 服务异常",
        hint: "未识别的失败原因，已上报后台。可重试，若持续请联系管理员。",
      },
    },
  },
  "en-US": {
    llmPM: "AI decision · single-shot PM",
    llmThreeStage: "AI decision · three-stage pipeline",
    fallback: "Rule-based fallback",
    fallbackNoLLM: "Rule-based fallback · no LLM configured",
    legacy: "Legacy plan",
    unknownLabel: "unknown",
    providerLabel: "provider",
    noActionHint: "You can rerun the workflow from the decision center to regenerate this plan.",
    unknownCategory: {
      title: "AI service error",
      hint: "This plan was produced by the deterministic fallback rather than an LLM. If this persists, contact your administrator.",
    },
    categories: {
      rate_limited: {
        title: "Model rate-limited",
        hint: "The AI model usually recovers within 5–10 minutes. Trigger the workflow again shortly.",
      },
      service_unavailable: {
        title: "Model service unavailable",
        hint: "Provider or proxy is temporarily unreachable. Retry in a few minutes; ask your administrator to check the LLM health dashboard if it persists.",
      },
      auth_failed: {
        title: "Model authentication failed",
        hint: "The platform or fund-scoped API key was rejected. Check your model settings or contact your administrator.",
      },
      context_length_exceeded: {
        title: "Context too long",
        hint: "The prompt exceeded the model's context window. Shrink the universe or switch to a longer-context tier in fund settings.",
      },
      invalid_request: {
        title: "Request rejected",
        hint: "The provider rejected the request as invalid. Usually a transient configuration issue — retry once to confirm.",
      },
      schema_validation_failed: {
        title: "Bad model output",
        hint: "The model returned JSON that didn't match the expected schema. Usually transient — retry should clear it.",
      },
      network_timeout: {
        title: "Network timeout",
        hint: "Network to the provider timed out. Retry usually clears it; contact ops if it persists.",
      },
      budget_exceeded: {
        title: "Call budget exhausted",
        hint: "Your fund or step has hit its per-minute or daily call quota. Upgrade your subscription or wait for the window to reset.",
      },
      empty_response: {
        title: "Empty model response",
        hint: "The model returned an empty response, usually due to a prompt-model mismatch. Retry or switch tiers.",
      },
      cancelled: {
        title: "Call cancelled",
        hint: "Upstream cancelled the call (workflow timeout or manual abort). Re-trigger the workflow to try again.",
      },
      unknown: {
        title: "AI service error",
        hint: "Unrecognised failure — already reported to ops. Retry, and contact your administrator if it persists.",
      },
    },
  },
};

function chipColours(source: DecisionSource | undefined, isFallback: boolean): string {
  if (!source) return "bg-zinc-700/40 text-zinc-300 border-zinc-600";
  if (isFallback) {
    return "bg-amber-500/15 text-amber-200 border-amber-500/40";
  }
  if (source === "legacy") {
    return "bg-zinc-700/40 text-zinc-300 border-zinc-600";
  }
  return "bg-emerald-500/15 text-emerald-200 border-emerald-500/40";
}

export function DecisionSourceChip({ language, source, reason, className }: Props) {
  const t = messages[language];
  const [popoverOpen, setPopoverOpen] = useState(false);

  if (!source) return null;

  const isFallback = source.startsWith("fallback");
  const category = reason?.category;
  const categoryCopy = category && category in t.categories
    ? t.categories[category]
    : t.unknownCategory;

  let primaryText: string;
  switch (source) {
    case "llm_three_stage":
      primaryText = t.llmThreeStage;
      break;
    case "llm_pm":
      primaryText = t.llmPM;
      break;
    case "fallback_no_llm":
      primaryText = t.fallbackNoLLM;
      break;
    case "fallback_after_llm_error":
    case "fallback_empty_plan":
      primaryText = `${t.fallback} · ${categoryCopy.title}`;
      break;
    case "legacy":
      primaryText = t.legacy;
      break;
    default:
      primaryText = source;
  }

  const baseCls = `inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs cursor-pointer ${chipColours(
    source,
    isFallback,
  )} ${className ?? ""}`;

  return (
    <span className="relative inline-block">
      <button
        type="button"
        className={baseCls}
        onClick={() => setPopoverOpen((v) => !v)}
        onBlur={() => setTimeout(() => setPopoverOpen(false), 150)}
        aria-label={primaryText}
      >
        <span>{primaryText}</span>
        {reason?.provider && (
          <span className="text-[10px] opacity-70">· {reason.provider}</span>
        )}
      </button>
      {popoverOpen && (
        <div
          role="tooltip"
          className="absolute left-0 top-full z-30 mt-2 w-72 rounded-md border border-zinc-700 bg-zinc-900 p-3 text-xs text-zinc-200 shadow-xl"
        >
          <p className="font-medium text-zinc-100">{categoryCopy.title}</p>
          <p className="mt-1 leading-relaxed text-zinc-300">{categoryCopy.hint}</p>
          {!isFallback && (
            <p className="mt-1 text-zinc-400">{t.noActionHint}</p>
          )}
          {reason && (
            <div className="mt-2 grid grid-cols-2 gap-x-3 gap-y-0.5 text-[10px] text-zinc-400">
              <span>{t.providerLabel}</span>
              <span className="text-zinc-200">{reason.provider ?? t.unknownLabel}</span>
              {reason.at && (
                <>
                  <span>at</span>
                  <span className="text-zinc-200">{reason.at.slice(0, 19)}</span>
                </>
              )}
            </div>
          )}
        </div>
      )}
    </span>
  );
}
