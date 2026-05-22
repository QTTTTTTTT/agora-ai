import type { DecisionTraceStep } from "../../lib/api";
import type { ApiPlanAction, RiskCheckView, RiskReviewView } from "./types";
import { WORKFLOW_STEP_ORDER } from "./types";

export function humanizeValue(value?: string, emptyLabel = "-"): string {
  if (!value) {
    return emptyLabel;
  }
  return value
    .replace(/[_-]+/g, " ")
    .trim()
    .replace(/\b\w/g, (char) => char.toUpperCase());
}

export function pickLocalizedText(language: string, value?: string, valueZh?: string, valueEn?: string): string {
  const base = value?.trim() ?? "";
  const zh = valueZh?.trim() ?? "";
  const en = valueEn?.trim() ?? "";
  return language === "zh-CN" ? zh || base || en : en || base || zh;
}

export function pickLocalizedList(language: string, value?: string[], valueZh?: string[], valueEn?: string[]): string[] {
  const base = (value ?? []).map((item) => item.trim()).filter(Boolean);
  const zh = (valueZh ?? []).map((item) => item.trim()).filter(Boolean);
  const en = (valueEn ?? []).map((item) => item.trim()).filter(Boolean);
  if (language === "zh-CN") {
    return zh.length ? zh : base.length ? base : en;
  }
  return en.length ? en : base.length ? base : zh;
}

function shouldCompactStrategyReasoning(value: string): boolean {
  const normalized = value.toLowerCase();
  return (
    value.split(/\r?\n/).filter((line) => line.trim()).length > 14 ||
    normalized.includes("fund focus:") ||
    normalized.includes("specialization context:") ||
    normalized.includes("market data snapshot") ||
    normalized.includes("stock research") ||
    normalized.includes("macro brief")
  );
}

function normalizeReasoningLine(value: string): string {
  let line = value.trim().replace(/^[-•]\s*/, "");
  if (line.toLowerCase().startsWith("news:")) {
    line = line.slice(5).trim();
  }
  return line.length > 180 ? `${line.slice(0, 180)}…` : line;
}

function isNoisyReasoningLine(value: string): boolean {
  const line = value.trim().toLowerCase();
  if (!line) return true;
  return [
    "macro brief",
    "stock research",
    "fundamental research",
    "quant signals",
    "fund focus",
    "specialization context",
    "market data snapshot",
    "benchmark:",
    "spy:",
    "market:",
    "asset class:",
    "primary direction:",
    "universe mode:",
    "universe themes:",
    "universe sectors:",
    "team specialization",
    "member specialization",
  ].some((prefix) => line.startsWith(prefix));
}

function actionVerbLabel(action: string | undefined, language: string): string {
  const normalized = (action ?? "").trim().toLowerCase();
  if (language === "zh-CN") {
    if (normalized === "buy" || normalized === "add") return "买入/增配";
    if (normalized === "sell" || normalized === "reduce") return "卖出/降仓";
    if (normalized === "hold") return "持有观察";
    if (normalized === "watch") return "仅观察";
  }
  return humanizeValue(normalized || action || "-");
}

export function compactStrategyReasoning(raw: string, actions: ApiPlanAction[] | undefined, language: string): string {
  const source = raw.trim();
  if (!source || !shouldCompactStrategyReasoning(source)) {
    return source;
  }

  const labels =
    language === "zh-CN"
      ? { title: "策略摘要", current: "当前建议", basis: "主要依据", risk: "执行与风险提示", noAction: "暂不执行自动交易，等待更完整的标的、报价或风控输入。" }
      : { title: "Strategy summary", current: "Current recommendation", basis: "Main rationale", risk: "Execution and risk notes", noAction: "Do not auto-trade yet; wait for more complete ticker, quote, or risk inputs." };

  const actionItems = (actions ?? [])
    .slice(0, 5)
    .map((action) => {
      const symbol = action.symbol?.trim() || action.instrumentKey?.trim() || (language === "zh-CN" ? "未指定标的" : "unspecified instrument");
      const amount = action.amount && action.amount > 0 ? (language === "zh-CN" ? ` · 名义金额 ${action.amount.toFixed(2)}` : ` · notional ${action.amount.toFixed(2)}`) : "";
      const confidence = action.confidence && action.confidence > 0 ? ` · ${Math.round(action.confidence * 100)}%` : "";
      return `${actionVerbLabel(action.action, language)} ${symbol}${amount}${confidence}`;
    })
    .filter(Boolean);

  const evidence: string[] = [];
  const seen = new Set<string>();
  source.split(/\r?\n/).forEach((line) => {
    const normalized = normalizeReasoningLine(line);
    if (!normalized || isNoisyReasoningLine(normalized)) return;
    const key = normalized.toLowerCase();
    if (seen.has(key) || evidence.length >= 4) return;
    seen.add(key);
    evidence.push(normalized);
  });

  const risks = (actions ?? [])
    .flatMap((action) => {
      const reasoning = (action.reasoning ?? "").toLowerCase();
      const notes: string[] = [];
      if (reasoning.includes("quote unavailable")) {
        const symbol = (action.symbol ?? "").trim().toUpperCase();
        notes.push(
          language === "zh-CN"
            ? `未能从行情源取到 ${symbol || "标的"} 的实时报价；可点击 "刷新报价并重报" 后再下单。`
            : `Live quote for ${symbol || "the instrument"} could not be retrieved; click "Refresh quote and rerun" before placing the order.`,
        );
      }
      if (reasoning.includes("structured ticker configuration is missing")) notes.push(language === "zh-CN" ? "标的配置不完整，建议先补充 universe symbols 或 specialization instruments。" : "Ticker configuration is incomplete; add universe symbols or specialization instruments first.");
      return notes;
    })
    .filter((item, index, all) => all.indexOf(item) === index)
    .slice(0, 3);

  const sections = [`${labels.title}:`, `- ${labels.current}: ${actionItems.length ? actionItems.join(language === "zh-CN" ? "；" : "; ") : labels.noAction}`];
  if (evidence.length) sections.push(`\n${labels.basis}:\n${evidence.map((item) => `- ${item}`).join("\n")}`);
  if (risks.length) sections.push(`\n${labels.risk}:\n${risks.map((item) => `- ${item}`).join("\n")}`);
  return sections.join("\n");
}

function toSentenceCase(value: string): string {
  return value
    .replace(/[_-]+/g, " ")
    .trim()
    .replace(/\b\w/g, (char) => char.toUpperCase());
}

export function isPendingPlan(status: string): boolean {
  return status === "pending" || status === "pending_user";
}

// extractAutoExecuteReasonCode pulls the autoExecute.reasonCode value
// from a plan's risk_review JSON. The server writes this field on
// every plan that went through the auto-execute gate; downstream UI
// uses it to disambiguate semantically different "completed" plans
// (e.g. a PM "watch only today" verdict vs an actual filled trade)
// without needing a new DB enum. Returns "" when the field is missing
// or the JSON is shaped differently.
export function extractAutoExecuteReasonCode(riskReview: unknown): string {
  if (!riskReview || typeof riskReview !== "object") {
    return "";
  }
  const auto = (riskReview as Record<string, unknown>).autoExecute;
  if (!auto || typeof auto !== "object") {
    return "";
  }
  const value = (auto as Record<string, unknown>).reasonCode;
  return typeof value === "string" ? value : "";
}

// planEffectiveStatusKey collapses (plan.status, autoExecute.reasonCode)
// into a single key that the i18n label map and badge class map can
// look up directly. The only synthetic key today is "watch_only"
// (status=completed + reasonCode=no_actionable_trade), which lets the
// Decision Center render a PM "observe-only" verdict as its own row
// instead of being lumped in with real filled trades under "已完成".
// Add more synthetic keys here if/when the gate gains other reason
// codes the UI needs to distinguish (e.g. partial fills, halted).
export function planEffectiveStatusKey(status: string, riskReview: unknown): string {
  const normalized = (status ?? "").toLowerCase();
  if (normalized === "completed" && extractAutoExecuteReasonCode(riskReview) === "no_actionable_trade") {
    return "watch_only";
  }
  return normalized;
}

export function canReviewPlan(status: string): boolean {
  return isPendingPlan(status);
}

export function normalizeTraceStepKey(step?: string): string {
  return (step ?? "").trim().toLowerCase();
}

export function normalizeTradeActionKey(side?: string): string {
  const normalized = (side ?? "").trim().toLowerCase();
  if (normalized === "long" || normalized === "buy") {
    return "buy";
  }
  if (normalized === "short" || normalized === "sell") {
    return "sell";
  }
  return normalized;
}

export function isEmptyObject(value: unknown): boolean {
  return Boolean(value) && typeof value === "object" && !Array.isArray(value) && Object.keys(value as Record<string, unknown>).length === 0;
}

export function normalizeTradingDateParam(value?: string): string | undefined {
  const trimmed = value?.trim();
  if (!trimmed) {
    return undefined;
  }
  const matched = trimmed.match(/^(\d{4}-\d{2}-\d{2})/);
  return matched?.[1];
}

export function metaItems(...values: Array<string | undefined>): string[] {
  return values.filter((value): value is string => Boolean(value?.trim()));
}

export function expandWorkflowSteps(steps: DecisionTraceStep[], currentStep?: string): DecisionTraceStep[] {
  if (!steps.length) {
    return [];
  }
  const normalizedCurrentStep = normalizeTraceStepKey(currentStep);
  const byStep = new Map(steps.map((step) => [normalizeTraceStepKey(step.step), step] as const));
  return WORKFLOW_STEP_ORDER.map((stepKey) => {
    const existing = byStep.get(stepKey);
    if (existing) {
      return existing;
    }
    return {
      step: stepKey,
      status: normalizedCurrentStep === stepKey ? "running" : "pending",
    };
  });
}

export function parseRiskReview(
  raw: unknown,
  fallbackCheckName: (index: number) => string,
  fallbackDetail: string,
  fallbackNote: string,
): RiskReviewView | null {
  if (!raw || typeof raw !== "object") {
    return null;
  }

  const source = raw as Record<string, unknown>;
  const verdictValue = source.verdict ?? source.Verdict;
  const commentaryValue = source.overallNote ?? source.Commentary;
  const warningsValue = source.warnings ?? source.Warnings;
  const rejectionsValue = source.rejections ?? source.Rejections;
  const suggestionsValue = source.suggestions ?? source.Suggestions;
  const checksValue = source.checks ?? source.Checks;

  const checks: RiskCheckView[] = Array.isArray(checksValue)
    ? checksValue.flatMap((entry, index) => {
        if (!entry || typeof entry !== "object") {
          return [];
        }
        const item = entry as Record<string, unknown>;
        const name =
          typeof item.name === "string"
            ? item.name
            : typeof item.rule === "string"
              ? toSentenceCase(item.rule)
              : fallbackCheckName(index + 1);
        const result =
          typeof item.result === "string"
            ? item.result.toLowerCase()
            : typeof item.status === "string"
              ? item.status.toLowerCase()
              : "pass";
        const detail =
          typeof item.detail === "string"
            ? item.detail
            : typeof item.message === "string"
              ? item.message
              : fallbackDetail;
        const id = typeof item.id === "string" ? item.id : `${name}-${index}`;
        return [{ id, name, result, detail }];
      })
    : [];

  const notes = [
    typeof commentaryValue === "string" ? commentaryValue : "",
    ...(Array.isArray(warningsValue) ? warningsValue.filter((item): item is string => typeof item === "string") : []),
    ...(Array.isArray(rejectionsValue) ? rejectionsValue.filter((item): item is string => typeof item === "string") : []),
    ...(Array.isArray(suggestionsValue) ? suggestionsValue.filter((item): item is string => typeof item === "string") : []),
  ].filter(Boolean);

  const verdict = typeof verdictValue === "string" ? verdictValue.toLowerCase() : "pending";
  if (!checks.length && notes.length === 0 && !verdict) {
    return null;
  }

  return {
    verdict,
    note: notes.join("\n") || fallbackNote,
    checks,
  };
}
