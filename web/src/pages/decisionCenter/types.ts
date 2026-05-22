import type { DecisionTraceTrade } from "../../lib/api";

export type PlanStatus = "pending" | "pending_user" | "approved" | "rejected" | string;
export type ActionType = "buy" | "sell" | "hold" | "reduce" | "add" | "watch" | string;
export type CheckResult = "pass" | "warn" | "fail" | string;

export interface ApiPlanAction {
  id?: string;
  instrumentKey?: string;
  action: ActionType;
  symbol: string;
  market?: string;
  exchange?: string;
  assetClass?: string;
  instrumentType?: string;
  positionSide?: string;
  openClose?: string;
  quantity?: number;
  price?: number;
  amount?: number;
  stopLoss?: number;
  takeProfit?: number;
  reasoning?: string;
  reasoningZh?: string;
  reasoningEn?: string;
  confidence?: number;
  supportedBy?: string[];
  opposedBy?: string[];
  executionStatus?: string;
  sortOrder?: number;
  quoteCurrency?: string;
  settlementCurrency?: string;
  marginMode?: string;
  leverage?: number;
  contractMultiplier?: number;
  expiryDate?: string;
  reduceOnly?: boolean;
}

export interface ApiPlan {
  id: string;
  fundId: string;
  tradingDate?: string;
  status: PlanStatus;
  reasoning?: string;
  reasoningZh?: string;
  reasoningEn?: string;
  riskScore?: number;
  expectedReturn?: number;
  riskReview?: unknown;
  roundtableId?: string;
  pmAgentId?: string;
  actions?: ApiPlanAction[];
  createdAt: string;
  updatedAt: string;
}

export interface RiskCheckView {
  id: string;
  name: string;
  result: CheckResult;
  detail: string;
}

export interface RiskReviewView {
  verdict: string;
  note: string;
  checks: RiskCheckView[];
}

export interface ExecutionTraceView {
  actionId: string;
  symbol: string;
  action: string;
  executionStatus: string;
  trades: DecisionTraceTrade[];
}

export const WORKFLOW_STEP_ORDER = [
  "macro_brief",
  "research_parallel",
  "quant_signals",
  "roundtable",
  "pm_plan",
  "risk_review",
  "user_approval",
  "trade_execution",
  "settlement",
  "daily_review",
] as const;
