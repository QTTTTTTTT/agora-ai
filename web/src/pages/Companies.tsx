import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { apiGet, apiPost, formatApiError, getStoredSession, logoutSession } from "../lib/api";
import { formatMoneyForDisplay, formatNumberForLanguage, useAppPreferences } from "../lib/preferences";
import { AutoExecuteInlineToggle, type AutoExecuteConfig } from "../components/AutoExecuteControls";

interface Company {
  id: string;
  ownerUserId: string;
  name: string;
  description?: string;
}

interface FundUniverse {
  mode: string;
  symbols: string[];
  sectors?: string[];
  themes?: string[];
  customFilters?: string[];
}

interface FundTeamSpecialization {
  markets?: string[];
  assetClasses?: string[];
  themes?: string[];
  instruments?: string[];
  styleHints?: string[];
}

interface FundSpecialization {
  team?: FundTeamSpecialization;
}

interface Fund {
  id: string;
  companyId: string;
  name: string;
  description?: string;
  tradingMode: string;
  totalAssets: number;
  nav: number;
  status: string;
  market?: string;
  exchange?: string;
  assetClass?: string;
  baseCurrency?: string;
  benchmarkSymbol?: string;
  primaryDirection?: string;
  calendarCode?: string;
  timeZone?: string;
  universe?: FundUniverse;
  specialization?: FundSpecialization;
  // autoExecute is always populated by the server (the response
  // includes the resolved defaults for funds that haven't opted in),
  // but typed as optional because legacy backend versions or partial
  // PATCH responses may omit it.
  autoExecute?: AutoExecuteConfig | null;
  researchTier?: string;
}

interface CompanyWithFunds extends Company {
  funds: Fund[];
}

interface CreateCompanyFormData {
  name: string;
  description: string;
}

interface CreateFundFormData {
  name: string;
  description: string;
  tradingMode: "simulation" | "live";
  initialCapital: string;
  market: string;
  exchange: string;
  assetClass: string;
  baseCurrency: string;
  benchmarkSymbol: string;
  primaryDirection: string;
  calendarCode: string;
  timeZone: string;
  universeMode: string;
  universeSymbols: string;
  universeThemes: string;
  universeSectors: string;
  universeCustomFilters: string;
  specializationMarkets: string;
  specializationAssetClasses: string;
  specializationThemes: string;
  specializationInstruments: string;
  specializationStyleHints: string;
}

const INITIAL_COMPANY_FORM: CreateCompanyFormData = {
  name: "",
  description: "",
};

const INITIAL_FUND_FORM: CreateFundFormData = {
  name: "",
  description: "",
  tradingMode: "simulation",
  initialCapital: "100000",
  market: "us_equity",
  exchange: "NASDAQ",
  assetClass: "equity",
  baseCurrency: "USD",
  benchmarkSymbol: "SPY",
  primaryDirection: "stocks",
  calendarCode: "US-XNAS",
  timeZone: "America/New_York",
  universeMode: "manual",
  universeSymbols: "",
  universeThemes: "",
  universeSectors: "",
  universeCustomFilters: "",
  specializationMarkets: "",
  specializationAssetClasses: "",
  specializationThemes: "",
  specializationInstruments: "",
  specializationStyleHints: "",
};

function normalizeOptionalText(value: string): string | undefined {
  const normalized = value.trim();
  return normalized || undefined;
}

function parseInitialCapital(value: string): number | null {
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed < 0) {
    return null;
  }
  return parsed;
}

function normalizeListInput(value: string): string[] {
  return value
    .split(/[\n\r,，、;；]/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function normalizeUniverseSymbols(value: string): string[] {
  return normalizeListInput(value);
}

function isKYCErrorMessage(message?: string | null): boolean {
  const normalized = (message ?? "").toLowerCase();
  return normalized.includes("kyc_required") || normalized.includes("kyc_level_insufficient") || normalized.includes("kyc");
}

function kycStatusLabel(status: string | undefined, language: "zh-CN" | "en-US"): string {
  const labels: Record<string, { zh: string; en: string }> = {
    verified: { zh: "KYC 已认证", en: "KYC verified" },
    pending: { zh: "KYC 审核中", en: "KYC pending" },
    rejected: { zh: "KYC 已拒绝", en: "KYC rejected" },
    unverified: { zh: "KYC 未认证", en: "KYC unverified" },
  };
  const normalized = (status ?? "unverified").trim() || "unverified";
  const matched = labels[normalized];
  return matched ? (language === "en-US" ? matched.en : matched.zh) : normalized;
}

function kycStatusTone(status: string | undefined): string {
  switch ((status ?? "unverified").trim()) {
    case "verified":
      return "bg-emerald-50 text-emerald-700 ring-emerald-200";
    case "pending":
      return "bg-amber-50 text-amber-700 ring-amber-200";
    case "rejected":
      return "bg-rose-50 text-rose-700 ring-rose-200";
    default:
      return "bg-gray-50 text-gray-600 ring-gray-200";
  }
}

function suggestCalendarProfile(market: string, exchange: string, assetClass: string): { calendarCode: string; timeZone: string } {
  const normalizedMarket = market.trim().toLowerCase();
  const normalizedExchange = exchange.trim().toUpperCase();
  const normalizedAssetClass = assetClass.trim().toLowerCase();

  if (normalizedExchange === "SSE") {
    return { calendarCode: "CN-SSE", timeZone: "Asia/Shanghai" };
  }
  if (normalizedExchange === "SZSE") {
    return { calendarCode: "CN-SZSE", timeZone: "Asia/Shanghai" };
  }
  if (normalizedExchange === "NYSE" || normalizedExchange === "XNYS") {
    return { calendarCode: "US-XNYS", timeZone: "America/New_York" };
  }
  if (normalizedExchange === "NASDAQ" || normalizedExchange === "XNAS") {
    return { calendarCode: "US-XNAS", timeZone: "America/New_York" };
  }
  if (["CME", "CBOT", "COMEX", "NYMEX"].includes(normalizedExchange)) {
    return { calendarCode: "CME-INDEX", timeZone: "America/Chicago" };
  }
  if (["CFFEX", "SHFE", "DCE", "CZCE", "INE"].includes(normalizedExchange)) {
    return { calendarCode: normalizedExchange, timeZone: "Asia/Shanghai" };
  }
  if (normalizedMarket === "a_share") {
    return { calendarCode: "CN-SSE", timeZone: "Asia/Shanghai" };
  }
  if (normalizedMarket === "us_equity") {
    return { calendarCode: "US-XNAS", timeZone: "America/New_York" };
  }
  if (normalizedMarket === "crypto" || normalizedAssetClass === "crypto") {
    return { calendarCode: "CRYPTO-24X7", timeZone: "UTC" };
  }
  if (normalizedMarket === "futures" || normalizedAssetClass === "futures") {
    return { calendarCode: "CME-INDEX", timeZone: "America/Chicago" };
  }
  return { calendarCode: "US-XNAS", timeZone: "America/New_York" };
}

const CreateCompanyModal: React.FC<{
  open: boolean;
  form: CreateCompanyFormData;
  saving: boolean;
  error: string | null;
  onClose: () => void;
  onChange: <K extends keyof CreateCompanyFormData>(key: K, value: CreateCompanyFormData[K]) => void;
  onSubmit: () => Promise<void>;
}> = ({ open, form, saving, error, onClose, onChange, onSubmit }) => {
  const { language } = useAppPreferences();
  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Create company",
            subtitle: "Create a company first, then add your first fund and continue into the workspace.",
            name: "Company name",
            description: "Company description",
            namePlaceholder: "For example: Horizon Asset Management",
            descriptionPlaceholder: "Optional: describe the company focus, strategy, or team notes",
            cancel: "Cancel",
            saving: "Creating...",
            submit: "Create company",
          }
        : {
            title: "创建公司",
            subtitle: "先创建一家公司，接下来就可以继续创建首只基金并进入系统。",
            name: "公司名称",
            description: "公司描述",
            namePlaceholder: "例如：启航资产管理",
            descriptionPlaceholder: "可选：填写公司定位、策略方向或团队备注",
            cancel: "取消",
            saving: "创建中...",
            submit: "创建公司",
          },
    [language],
  );

  if (!open) {
    return null;
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4">
      <div className="w-full max-w-xl rounded-2xl bg-white p-6 shadow-2xl">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h2 className="text-xl font-bold text-gray-900">{copy.title}</h2>
            <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
          </div>
          <button type="button" onClick={onClose} className="text-2xl leading-none text-gray-400 hover:text-gray-700">
            ×
          </button>
        </div>

        <form
          className="mt-6 space-y-4"
          onSubmit={(event) => {
            event.preventDefault();
            void onSubmit();
          }}
        >
          <div>
            <label htmlFor="company-name" className="mb-2 block text-sm font-medium text-gray-700">{copy.name}</label>
            <input
              id="company-name"
              required
              value={form.name}
              onChange={(event) => onChange("name", event.target.value)}
              className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
              placeholder={copy.namePlaceholder}
            />
          </div>

          <div>
            <label htmlFor="company-description" className="mb-2 block text-sm font-medium text-gray-700">{copy.description}</label>
            <textarea
              id="company-description"
              value={form.description}
              onChange={(event) => onChange("description", event.target.value)}
              rows={3}
              className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
              placeholder={copy.descriptionPlaceholder}
            />
          </div>

          {error ? <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">{error}</div> : null}

          <div className="flex justify-end gap-3 pt-2">
            <button type="button" onClick={onClose} className="rounded-lg border border-gray-300 bg-white px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50">
              {copy.cancel}
            </button>
            <button type="submit" disabled={saving} className="rounded-lg bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60">
              {saving ? copy.saving : copy.submit}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
};

const CreateFundModal: React.FC<{
  open: boolean;
  company: Company | null;
  form: CreateFundFormData;
  saving: boolean;
  error: string | null;
  onClose: () => void;
  onChange: <K extends keyof CreateFundFormData>(key: K, value: CreateFundFormData[K]) => void;
  onSubmit: () => Promise<void>;
}> = ({ open, company, form, saving, error, onClose, onChange, onSubmit }) => {
  const { language } = useAppPreferences();
  const [advancedOpen, setAdvancedOpen] = useState(false);

  useEffect(() => {
    if (!open) {
      setAdvancedOpen(false);
    }
  }, [open]);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Create first fund",
            subtitlePrefix: "Company \"",
            subtitleSuffix: "\" is ready. Add the first fund to enter the working workspace.",
            currentCompany: "Current company",
            fundName: "Fund name",
            fundDescription: "Fund description",
            tradingMode: "Trading mode",
            initialCapital: "Initial capital",
            market: "Market",
            exchange: "Exchange",
            assetClass: "Asset class",
            baseCurrency: "Base currency",
            benchmark: "Benchmark symbol",
            primaryDirection: "Primary direction",
            calendarCode: "Calendar code",
            timeZone: "Time zone",
            universeMode: "Universe mode",
            universeSymbols: "Universe symbols",
            universeThemes: "Theme tags",
            universeSectors: "Sector tags",
            universeCustomFilters: "Custom filters",
            specializationTitle: "Team specialization",
            specializationSubtitle: "Capture the fund team's long-term strengths and preferences. These signals guide runtime context and member routing as soft affinity boosts.",
            specializationMarkets: "Specialization markets",
            specializationAssetClasses: "Specialization asset classes",
            specializationThemes: "Specialization themes",
            specializationInstruments: "Specialization instruments",
            specializationStyleHints: "Specialization style hints",
            specializationPlaceholder: "CPO, us_equity, NVDA, growth, event-driven",
            basicsTitle: "Fund basics",
            basicsHint: "Keep the first screen lightweight. You can refine market profile and universe in advanced settings.",
            advancedTitle: "Advanced settings",
            advancedHint: "Configure market profile, benchmark, flexible universe rules, and team specialization signals.",
            showAdvanced: "Show advanced settings",
            hideAdvanced: "Hide advanced settings",
            marketProfileTitle: "Market profile",
            universeTitle: "Universe and categories",
            fundNamePlaceholder: "For example: Horizon Quant Fund I",
            fundDescriptionPlaceholder: "Optional: describe the fund strategy or current stage",
            exchangePlaceholder: "NASDAQ / SSE / SZSE / BINANCE / CME",
            baseCurrencyPlaceholder: "USD / CNY / USDT",
            benchmarkPlaceholder: "SPY / 000300 / BTCUSDT / ESU2026",
            universePlaceholder: "AAPL, NVDA or 600519, 000858 or BTCUSDT, ETHUSDT",
            universeThemePlaceholder: "CPO, light modules, AI infra",
            universeSectorPlaceholder: "technology, semiconductor, consumer",
            universeFilterPlaceholder: "marketCap>10B, liquidity:high",
            later: "Maybe later",
            completeKYC: "Complete KYC",
            saving: "Creating...",
            submit: "Create fund and enter workspace",
            simulation: "Simulation",
            live: "Live",
            usEquity: "US Equities",
            aShare: "A-shares",
            crypto: "Crypto",
            futures: "Futures",
            stocks: "Stocks",
            multiAsset: "Multi-asset",
            manual: "Manual",
            watchlist: "Watchlist",
          }
        : {
            title: "创建首只基金",
            subtitlePrefix: "公司“",
            subtitleSuffix: "”已创建成功，再补一只基金即可进入实际工作台。",
            currentCompany: "当前公司",
            fundName: "基金名称",
            fundDescription: "基金描述",
            tradingMode: "交易模式",
            initialCapital: "初始资金",
            market: "市场",
            exchange: "交易所",
            assetClass: "资产类别",
            baseCurrency: "基础货币",
            benchmark: "基准标的",
            primaryDirection: "主方向",
            calendarCode: "交易日历代码",
            timeZone: "时区",
            universeMode: "标的池模式",
            universeSymbols: "自选标的",
            universeThemes: "主题标签",
            universeSectors: "行业/板块",
            universeCustomFilters: "自定义筛选条件",
            specializationTitle: "团队专精",
            specializationSubtitle: "描述这只基金团队长期积累的市场、资产、主题、标的与风格偏好，这些信号会作为 runtime 上下文和成员路由的软加分项。",
            specializationMarkets: "专精市场",
            specializationAssetClasses: "专精资产类别",
            specializationThemes: "专精主题",
            specializationInstruments: "专精标的",
            specializationStyleHints: "专精风格提示",
            specializationPlaceholder: "CPO、美股成长、NVDA、growth、event-driven",
            basicsTitle: "基金基础信息",
            basicsHint: "首屏只保留最关键字段，市场画像和标的池可以在高级设置里继续补充。",
            advancedTitle: "高级设置",
            advancedHint: "配置市场画像、基准、更灵活的标的范围，以及团队专精信号。",
            showAdvanced: "展开高级设置",
            hideAdvanced: "收起高级设置",
            marketProfileTitle: "市场画像",
            universeTitle: "标的池与分类",
            fundNamePlaceholder: "例如：启航量化一号",
            fundDescriptionPlaceholder: "可选：填写基金策略或当前阶段说明",
            exchangePlaceholder: "NASDAQ / SSE / SZSE / BINANCE / CME",
            baseCurrencyPlaceholder: "USD / CNY / USDT",
            benchmarkPlaceholder: "SPY / 000300 / BTCUSDT / ESU2026",
            universePlaceholder: "AAPL, NVDA 或 600519, 000858 或 BTCUSDT, ETHUSDT",
            universeThemePlaceholder: "CPO、光模块、AI infra",
            universeSectorPlaceholder: "technology、semiconductor、consumer",
            universeFilterPlaceholder: "marketCap>10B、liquidity:high",
            later: "稍后再说",
            completeKYC: "去实名认证",
            saving: "创建中...",
            submit: "创建基金并进入系统",
            simulation: "模拟",
            live: "实盘",
            usEquity: "美股",
            aShare: "A股",
            crypto: "Crypto",
            futures: "期货",
            stocks: "股票",
            multiAsset: "多资产",
            manual: "manual",
            watchlist: "watchlist",
          },
    [language],
  );

  if (!open || !company) {
    return null;
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4">
      <div className="max-h-[90vh] w-full max-w-3xl overflow-y-auto rounded-2xl bg-white p-6 shadow-2xl">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h2 className="text-xl font-bold text-gray-900">{copy.title}</h2>
            <p className="mt-2 text-sm text-gray-500">{copy.subtitlePrefix}{company.name}{copy.subtitleSuffix}</p>
          </div>
          <button type="button" onClick={onClose} className="text-2xl leading-none text-gray-400 hover:text-gray-700">
            ×
          </button>
        </div>

        <form
          className="mt-6 space-y-4"
          onSubmit={(event) => {
            event.preventDefault();
            void onSubmit();
          }}
        >
          <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3 text-sm text-gray-600">
            {copy.currentCompany}：<span className="font-medium text-gray-900">{company.name}</span>
          </div>

          <section className="rounded-2xl border border-gray-200 bg-white p-5">
            <div className="flex flex-col gap-2 border-b border-gray-100 pb-4 sm:flex-row sm:items-start sm:justify-between">
              <div>
                <h3 className="text-base font-semibold text-gray-900">{copy.basicsTitle}</h3>
                <p className="mt-1 text-sm text-gray-500">{copy.basicsHint}</p>
              </div>
              <span className="rounded-full bg-indigo-50 px-3 py-1 text-xs font-medium text-indigo-700">{copy.market} / {copy.tradingMode}</span>
            </div>

            <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2">
              <div className="md:col-span-2">
                <label htmlFor="fund-name" className="mb-2 block text-sm font-medium text-gray-700">{copy.fundName}</label>
                <input
                  id="fund-name"
                  required
                  value={form.name}
                  onChange={(event) => onChange("name", event.target.value)}
                  className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                  placeholder={copy.fundNamePlaceholder}
                />
              </div>
              <div>
                <label htmlFor="fund-trading-mode" className="mb-2 block text-sm font-medium text-gray-700">{copy.tradingMode}</label>
                <select
                  id="fund-trading-mode"
                  value={form.tradingMode}
                  onChange={(event) => onChange("tradingMode", event.target.value as CreateFundFormData["tradingMode"])}
                  className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                >
                  <option value="simulation">{copy.simulation}</option>
                  <option value="live">{copy.live}</option>
                </select>
              </div>
              <div>
                <label htmlFor="fund-initial-capital" className="mb-2 block text-sm font-medium text-gray-700">{copy.initialCapital}</label>
                <input
                  id="fund-initial-capital"
                  required
                  type="number"
                  min="0"
                  step="1000"
                  value={form.initialCapital}
                  onChange={(event) => onChange("initialCapital", event.target.value)}
                  className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                  placeholder="100000"
                />
              </div>
              <div>
                <label htmlFor="fund-market" className="mb-2 block text-sm font-medium text-gray-700">{copy.market}</label>
                <select
                  id="fund-market"
                  value={form.market}
                  onChange={(event) => {
                    const nextMarket = event.target.value;
                    const suggestion = suggestCalendarProfile(nextMarket, form.exchange, form.assetClass);
                    onChange("market", nextMarket);
                    onChange("calendarCode", suggestion.calendarCode);
                    onChange("timeZone", suggestion.timeZone);
                  }}
                  className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                >
                  <option value="us_equity">{copy.usEquity}</option>
                  <option value="a_share">{copy.aShare}</option>
                  <option value="crypto">{copy.crypto}</option>
                  <option value="futures">{copy.futures}</option>
                </select>
              </div>
              <div className="md:col-span-2">
                <label htmlFor="fund-description" className="mb-2 block text-sm font-medium text-gray-700">{copy.fundDescription}</label>
                <textarea
                  id="fund-description"
                  value={form.description}
                  onChange={(event) => onChange("description", event.target.value)}
                  rows={3}
                  className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                  placeholder={copy.fundDescriptionPlaceholder}
                />
              </div>
            </div>
          </section>

          <section className="rounded-2xl border border-gray-200 bg-white p-5">
            <button
              type="button"
              onClick={() => setAdvancedOpen((current) => !current)}
              className="flex w-full items-center justify-between gap-4 text-left"
            >
              <div>
                <h3 className="text-base font-semibold text-gray-900">{copy.advancedTitle}</h3>
                <p className="mt-1 text-sm text-gray-500">{copy.advancedHint}</p>
              </div>
              <span className="rounded-full bg-gray-100 px-3 py-1 text-xs font-medium text-gray-600">
                {advancedOpen ? copy.hideAdvanced : copy.showAdvanced}
              </span>
            </button>

            {advancedOpen ? (
              <div className="mt-5 space-y-6 border-t border-gray-100 pt-5">
                <div>
                  <h4 className="text-sm font-semibold text-gray-900">{copy.marketProfileTitle}</h4>
                  <div className="mt-3 grid grid-cols-1 gap-4 md:grid-cols-2">
                    <div>
                      <label htmlFor="fund-exchange" className="mb-2 block text-sm font-medium text-gray-700">{copy.exchange}</label>
                      <input
                        id="fund-exchange"
                        value={form.exchange}
                        onChange={(event) => {
                          const nextExchange = event.target.value;
                          const suggestion = suggestCalendarProfile(form.market, nextExchange, form.assetClass);
                          onChange("exchange", nextExchange);
                          onChange("calendarCode", suggestion.calendarCode);
                          onChange("timeZone", suggestion.timeZone);
                        }}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.exchangePlaceholder}
                      />
                    </div>
                    <div>
                      <label htmlFor="fund-asset-class" className="mb-2 block text-sm font-medium text-gray-700">{copy.assetClass}</label>
                      <select
                        id="fund-asset-class"
                        value={form.assetClass}
                        onChange={(event) => {
                          const nextAssetClass = event.target.value;
                          const suggestion = suggestCalendarProfile(form.market, form.exchange, nextAssetClass);
                          onChange("assetClass", nextAssetClass);
                          onChange("calendarCode", suggestion.calendarCode);
                          onChange("timeZone", suggestion.timeZone);
                        }}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                      >
                        <option value="equity">equity</option>
                        <option value="crypto">crypto</option>
                        <option value="futures">futures</option>
                      </select>
                    </div>
                    <div>
                      <label htmlFor="fund-base-currency" className="mb-2 block text-sm font-medium text-gray-700">{copy.baseCurrency}</label>
                      <input
                        id="fund-base-currency"
                        value={form.baseCurrency}
                        onChange={(event) => onChange("baseCurrency", event.target.value)}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.baseCurrencyPlaceholder}
                      />
                    </div>
                    <div>
                      <label htmlFor="fund-benchmark" className="mb-2 block text-sm font-medium text-gray-700">{copy.benchmark}</label>
                      <input
                        id="fund-benchmark"
                        value={form.benchmarkSymbol}
                        onChange={(event) => onChange("benchmarkSymbol", event.target.value)}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.benchmarkPlaceholder}
                      />
                    </div>
                    <div>
                      <label htmlFor="fund-primary-direction" className="mb-2 block text-sm font-medium text-gray-700">{copy.primaryDirection}</label>
                      <select
                        id="fund-primary-direction"
                        value={form.primaryDirection}
                        onChange={(event) => onChange("primaryDirection", event.target.value)}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                      >
                        <option value="stocks">{copy.stocks}</option>
                        <option value="crypto">{copy.crypto}</option>
                        <option value="futures">{copy.futures}</option>
                        <option value="multi_asset">{copy.multiAsset}</option>
                      </select>
                    </div>
                    <div>
                      <label htmlFor="fund-calendar-code" className="mb-2 block text-sm font-medium text-gray-700">{copy.calendarCode}</label>
                      <input
                        id="fund-calendar-code"
                        value={form.calendarCode}
                        onChange={(event) => onChange("calendarCode", event.target.value)}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder="US-XNAS / CN-SSE / CRYPTO-24X7"
                      />
                    </div>
                    <div>
                      <label htmlFor="fund-time-zone" className="mb-2 block text-sm font-medium text-gray-700">{copy.timeZone}</label>
                      <input
                        id="fund-time-zone"
                        value={form.timeZone}
                        onChange={(event) => onChange("timeZone", event.target.value)}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder="America/New_York / Asia/Shanghai / UTC"
                      />
                    </div>
                  </div>
                </div>

                <div>
                  <h4 className="text-sm font-semibold text-gray-900">{copy.universeTitle}</h4>
                  <div className="mt-3 grid grid-cols-1 gap-4 md:grid-cols-2">
                    <div>
                      <label htmlFor="fund-universe-mode" className="mb-2 block text-sm font-medium text-gray-700">{copy.universeMode}</label>
                      <select
                        id="fund-universe-mode"
                        value={form.universeMode}
                        onChange={(event) => onChange("universeMode", event.target.value)}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                      >
                        <option value="manual">{copy.manual}</option>
                        <option value="watchlist">{copy.watchlist}</option>
                      </select>
                    </div>
                    <div />
                    <div className="md:col-span-2">
                      <label htmlFor="fund-universe-symbols" className="mb-2 block text-sm font-medium text-gray-700">{copy.universeSymbols}</label>
                      <textarea
                        id="fund-universe-symbols"
                        value={form.universeSymbols}
                        onChange={(event) => onChange("universeSymbols", event.target.value)}
                        rows={3}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.universePlaceholder}
                      />
                    </div>
                    <div>
                      <label htmlFor="fund-universe-themes" className="mb-2 block text-sm font-medium text-gray-700">{copy.universeThemes}</label>
                      <textarea
                        id="fund-universe-themes"
                        value={form.universeThemes}
                        onChange={(event) => onChange("universeThemes", event.target.value)}
                        rows={3}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.universeThemePlaceholder}
                      />
                    </div>
                    <div>
                      <label htmlFor="fund-universe-sectors" className="mb-2 block text-sm font-medium text-gray-700">{copy.universeSectors}</label>
                      <textarea
                        id="fund-universe-sectors"
                        value={form.universeSectors}
                        onChange={(event) => onChange("universeSectors", event.target.value)}
                        rows={3}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.universeSectorPlaceholder}
                      />
                    </div>
                    <div className="md:col-span-2">
                      <label htmlFor="fund-universe-filters" className="mb-2 block text-sm font-medium text-gray-700">{copy.universeCustomFilters}</label>
                      <textarea
                        id="fund-universe-filters"
                        value={form.universeCustomFilters}
                        onChange={(event) => onChange("universeCustomFilters", event.target.value)}
                        rows={2}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.universeFilterPlaceholder}
                      />
                    </div>
                  </div>
                </div>

                <div className="rounded-2xl border border-violet-100 bg-violet-50/50 p-4">
                  <h4 className="text-sm font-semibold text-violet-950">{copy.specializationTitle}</h4>
                  <p className="mt-1 text-sm text-violet-700">{copy.specializationSubtitle}</p>
                  <div className="mt-3 grid grid-cols-1 gap-4 md:grid-cols-2">
                    <div>
                      <label htmlFor="fund-specialization-markets" className="mb-2 block text-sm font-medium text-gray-700">{copy.specializationMarkets}</label>
                      <textarea
                        id="fund-specialization-markets"
                        value={form.specializationMarkets}
                        onChange={(event) => onChange("specializationMarkets", event.target.value)}
                        rows={2}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.specializationPlaceholder}
                      />
                    </div>
                    <div>
                      <label htmlFor="fund-specialization-asset-classes" className="mb-2 block text-sm font-medium text-gray-700">{copy.specializationAssetClasses}</label>
                      <textarea
                        id="fund-specialization-asset-classes"
                        value={form.specializationAssetClasses}
                        onChange={(event) => onChange("specializationAssetClasses", event.target.value)}
                        rows={2}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.specializationPlaceholder}
                      />
                    </div>
                    <div>
                      <label htmlFor="fund-specialization-themes" className="mb-2 block text-sm font-medium text-gray-700">{copy.specializationThemes}</label>
                      <textarea
                        id="fund-specialization-themes"
                        value={form.specializationThemes}
                        onChange={(event) => onChange("specializationThemes", event.target.value)}
                        rows={2}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.specializationPlaceholder}
                      />
                    </div>
                    <div>
                      <label htmlFor="fund-specialization-instruments" className="mb-2 block text-sm font-medium text-gray-700">{copy.specializationInstruments}</label>
                      <textarea
                        id="fund-specialization-instruments"
                        value={form.specializationInstruments}
                        onChange={(event) => onChange("specializationInstruments", event.target.value)}
                        rows={2}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.specializationPlaceholder}
                      />
                    </div>
                    <div className="md:col-span-2">
                      <label htmlFor="fund-specialization-style-hints" className="mb-2 block text-sm font-medium text-gray-700">{copy.specializationStyleHints}</label>
                      <textarea
                        id="fund-specialization-style-hints"
                        value={form.specializationStyleHints}
                        onChange={(event) => onChange("specializationStyleHints", event.target.value)}
                        rows={2}
                        className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                        placeholder={copy.specializationPlaceholder}
                      />
                    </div>
                  </div>
                </div>
              </div>
            ) : null}
          </section>

          {error ? (
            <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
              <p>{error}</p>
              {isKYCErrorMessage(error) ? (
                <Link to="/kyc" className="mt-3 inline-flex rounded-lg bg-amber-600 px-3 py-2 text-xs font-semibold text-white hover:bg-amber-700">
                  {copy.completeKYC}
                </Link>
              ) : null}
            </div>
          ) : null}

          <div className="flex justify-end gap-3 pt-2">
            <button type="button" onClick={onClose} className="rounded-lg border border-gray-300 bg-white px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50">
              {copy.later}
            </button>
            <button type="submit" disabled={saving} className="rounded-lg bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60">
              {saving ? copy.saving : copy.submit}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
};

const Companies: React.FC = () => {
  const navigate = useNavigate();
  const { language, displayCurrency } = useAppPreferences();
  const [companies, setCompanies] = useState<CompanyWithFunds[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [showCreateCompanyModal, setShowCreateCompanyModal] = useState(false);
  const [companyForm, setCompanyForm] = useState<CreateCompanyFormData>(INITIAL_COMPANY_FORM);
  const [companySaving, setCompanySaving] = useState(false);
  const [companyError, setCompanyError] = useState<string | null>(null);
  const [showCreateFundModal, setShowCreateFundModal] = useState(false);
  const [fundTargetCompany, setFundTargetCompany] = useState<Company | null>(null);
  const [fundForm, setFundForm] = useState<CreateFundFormData>(INITIAL_FUND_FORM);
  const [fundSaving, setFundSaving] = useState(false);
  const [fundError, setFundError] = useState<string | null>(null);
  const currentSession = getStoredSession();

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            notLoggedIn: "Not signed in",
            companiesTitle: "Companies",
            companiesSubtitle: "Choose a company or fund to continue into dashboard, approvals, subscription, model config, and billing.",
            totalLabel: "companies",
            fundsLabel: "funds",
            currentUser: "Current user",
            wallet: "My wallet",
            kyc: "KYC",
            kycLevel: "Level",
            marketplace: "Agent marketplace",
            admin: "Admin",
            createCompany: "Create company",
            logout: "Log out",
            loading: "Loading companies and funds...",
            loadError: "Failed to load companies",
            retry: "Retry",
            emptyTitle: "No companies yet",
            emptyDescription: "New accounts need a company first, then a first fund, before entering the full workspace.",
            createFirstCompany: "Create first company",
            noDescription: "No company description yet.",
            fundCountSuffix: " funds",
            noFundText: "This company has no fund yet. Create the first one to enter the workspace.",
            createFirstFund: "Create first fund",
            addFund: "Add fund",
            openPrimaryFund: "Open primary fund",
            viewDecisions: "View decisions",
            viewSubscription: "View subscription",
            statusUnknown: "Unknown status",
            live: "Live",
            simulation: "Simulation",
            navLabel: "NAV",
            totalAssets: "Total assets",
            marketProfileUnset: "Unconfigured",
            companyNameRequired: "Please enter a company name.",
            createCompanyError: "Failed to create company",
            fundNameRequired: "Please enter a fund name.",
            invalidCapital: "Please enter a valid initial capital greater than or equal to 0.",
            createFundError: "Failed to create fund",
          }
        : {
            notLoggedIn: "未登录",
            companiesTitle: "公司列表",
            companiesSubtitle: "选择一个公司或基金，继续查看投资总览、决策审批、订阅、模型配置与用量账单。",
            totalLabel: "家公司",
            fundsLabel: "只基金",
            currentUser: "当前用户",
            wallet: "我的钱包",
            kyc: "实名认证",
            kycLevel: "等级",
            marketplace: "成员交易市场",
            admin: "管理员后台",
            createCompany: "创建公司",
            logout: "退出登录",
            loading: "正在加载公司与基金信息...",
            loadError: "加载公司列表失败",
            retry: "重试",
            emptyTitle: "还没有可用公司",
            emptyDescription: "新账号需要先创建一家公司，再创建首只基金，之后就可以进入完整的基金工作台继续使用。",
            createFirstCompany: "创建第一家公司",
            noDescription: "暂无公司描述。",
            fundCountSuffix: " 只基金",
            noFundText: "当前公司还没有基金，创建首只基金后即可进入系统。",
            createFirstFund: "创建首只基金",
            addFund: "新增基金",
            openPrimaryFund: "打开主基金",
            viewDecisions: "查看决策",
            viewSubscription: "查看订阅",
            statusUnknown: "未知状态",
            live: "实盘",
            simulation: "模拟",
            navLabel: "单位净值",
            totalAssets: "总资产",
            marketProfileUnset: "未设置",
            companyNameRequired: "请输入公司名称。",
            createCompanyError: "创建公司失败",
            fundNameRequired: "请输入基金名称。",
            invalidCapital: "请输入合法的初始资金，且不能小于 0。",
            createFundError: "创建基金失败",
          },
    [language],
  );

  const currentUserLabel = currentSession?.displayName || currentSession?.email || currentSession?.userId || copy.notLoggedIn;

  const fundStatusMeta = useCallback(
    (status?: string) => {
      if (language === "en-US") {
        const map: Record<string, { label: string; badge: string }> = {
          active: { label: "Active", badge: "bg-emerald-50 text-emerald-700" },
          paused: { label: "Paused", badge: "bg-amber-50 text-amber-700" },
          closed: { label: "Closed", badge: "bg-gray-100 text-gray-600" },
        };
        return map[status ?? ""] ?? { label: status || copy.statusUnknown, badge: "bg-gray-100 text-gray-600" };
      }
      const map: Record<string, { label: string; badge: string }> = {
        active: { label: "运行中", badge: "bg-emerald-50 text-emerald-700" },
        paused: { label: "已暂停", badge: "bg-amber-50 text-amber-700" },
        closed: { label: "已关闭", badge: "bg-gray-100 text-gray-600" },
      };
      return map[status ?? ""] ?? { label: status || copy.statusUnknown, badge: "bg-gray-100 text-gray-600" };
    },
    [copy.statusUnknown, language],
  );

  const renderFundSubtitle = useCallback(
    (fund: Fund): string => {
      const parts = [fund.tradingMode === "live" ? copy.live : copy.simulation];
      if (fund.market) {
        parts.push(fund.market);
      }
      if (fund.exchange) {
        parts.push(fund.exchange);
      }
      parts.push(`${copy.navLabel} ${formatNumberForLanguage(fund.nav, language, { minimumFractionDigits: 4, maximumFractionDigits: 4 })}`);
      parts.push(`${copy.totalAssets} ${formatMoneyForDisplay(fund.totalAssets, fund.baseCurrency, displayCurrency, language)}`);
      return parts.join(" · ");
    },
    [copy.live, copy.navLabel, copy.simulation, copy.totalAssets, displayCurrency, language],
  );

  async function handleLogout() {
    await logoutSession();
    navigate("/login", { replace: true });
  }

  const loadData = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const companiesRes = await apiGet<CompanyWithFunds[]>("/api/companies/overview");
      setCompanies(companiesRes ?? []);
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError]);

  useEffect(() => {
    void loadData();
  }, [loadData]);

  const totalFunds = useMemo(() => companies.reduce((sum, company) => sum + company.funds.length, 0), [companies]);

  const updateCompanyForm = <K extends keyof CreateCompanyFormData>(key: K, value: CreateCompanyFormData[K]) => {
    setCompanyForm((current) => ({ ...current, [key]: value }));
  };

  const updateFundForm = <K extends keyof CreateFundFormData>(key: K, value: CreateFundFormData[K]) => {
    setFundForm((current) => ({ ...current, [key]: value }));
  };

  const openCreateCompanyModal = () => {
    setCompanyForm(INITIAL_COMPANY_FORM);
    setCompanyError(null);
    setShowCreateCompanyModal(true);
  };

  const closeCreateCompanyModal = () => {
    if (companySaving) {
      return;
    }
    setShowCreateCompanyModal(false);
    setCompanyError(null);
  };

  const openCreateFundModal = (company: Company) => {
    setFundTargetCompany(company);
    setFundForm(INITIAL_FUND_FORM);
    setFundError(null);
    setShowCreateFundModal(true);
  };

  const closeCreateFundModal = () => {
    if (fundSaving) {
      return;
    }
    setShowCreateFundModal(false);
    setFundTargetCompany(null);
    setFundError(null);
  };

  const handleCreateCompany = useCallback(async () => {
    const name = companyForm.name.trim();
    if (!name) {
      setCompanyError(copy.companyNameRequired);
      return;
    }

    setCompanySaving(true);
    setCompanyError(null);
    try {
      const created = await apiPost<Company>("/api/companies", {
        name,
        description: normalizeOptionalText(companyForm.description),
      });
      setCompanies((current) => [{ ...created, funds: [] }, ...current.filter((item) => item.id !== created.id)]);
      setShowCreateCompanyModal(false);
      setCompanyForm(INITIAL_COMPANY_FORM);
      openCreateFundModal(created);
    } catch (err) {
      setCompanyError(formatApiError(err, copy.createCompanyError));
    } finally {
      setCompanySaving(false);
    }
  }, [companyForm, copy.companyNameRequired, copy.createCompanyError]);

  const handleCreateFund = useCallback(async () => {
    if (!fundTargetCompany) {
      return;
    }

    const name = fundForm.name.trim();
    if (!name) {
      setFundError(copy.fundNameRequired);
      return;
    }
    const initialCapital = parseInitialCapital(fundForm.initialCapital);
    if (initialCapital === null) {
      setFundError(copy.invalidCapital);
      return;
    }

    setFundSaving(true);
    setFundError(null);
    try {
      const created = await apiPost<Fund>(`/api/companies/${fundTargetCompany.id}/funds`, {
        name,
        description: normalizeOptionalText(fundForm.description),
        tradingMode: fundForm.tradingMode,
        initialCapital,
        market: normalizeOptionalText(fundForm.market),
        exchange: normalizeOptionalText(fundForm.exchange),
        assetClass: normalizeOptionalText(fundForm.assetClass),
        baseCurrency: normalizeOptionalText(fundForm.baseCurrency),
        benchmarkSymbol: normalizeOptionalText(fundForm.benchmarkSymbol),
        primaryDirection: normalizeOptionalText(fundForm.primaryDirection),
        calendarCode: normalizeOptionalText(fundForm.calendarCode),
        timeZone: normalizeOptionalText(fundForm.timeZone),
        universe: {
          mode: normalizeOptionalText(fundForm.universeMode) ?? "manual",
          symbols: normalizeUniverseSymbols(fundForm.universeSymbols),
          themes: normalizeListInput(fundForm.universeThemes),
          sectors: normalizeListInput(fundForm.universeSectors),
          customFilters: normalizeListInput(fundForm.universeCustomFilters),
        },
        specialization: {
          team: {
            markets: normalizeListInput(fundForm.specializationMarkets),
            assetClasses: normalizeListInput(fundForm.specializationAssetClasses),
            themes: normalizeListInput(fundForm.specializationThemes),
            instruments: normalizeListInput(fundForm.specializationInstruments),
            styleHints: normalizeListInput(fundForm.specializationStyleHints),
          },
        },
      });
      setCompanies((current) =>
        current.map((company) =>
          company.id === fundTargetCompany.id
            ? {
                ...company,
                funds: [...company.funds, created],
              }
            : company,
        ),
      );
      setShowCreateFundModal(false);
      setFundTargetCompany(null);
      setFundForm(INITIAL_FUND_FORM);
      navigate(`/funds/${created.id}`);
    } catch (err) {
      setFundError(formatApiError(err, copy.createFundError));
    } finally {
      setFundSaving(false);
    }
  }, [copy.createFundError, copy.fundNameRequired, copy.invalidCapital, fundForm, fundTargetCompany, navigate]);

  return (
    <>
      <div className="min-h-screen bg-gray-50 p-8">
        <div className="mx-auto max-w-6xl">
          <div className="mb-8 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
            <div>
              <h1 className="text-3xl font-bold text-gray-900">{copy.companiesTitle}</h1>
              <p className="mt-2 text-gray-500">{copy.companiesSubtitle}</p>
            </div>
            <div className="flex flex-wrap items-center justify-end gap-3">
              <div className="rounded-xl border border-gray-200 bg-white px-5 py-4 text-sm text-gray-500 shadow-sm">
                <p>
                  {formatNumberForLanguage(companies.length, language)} {copy.totalLabel} · {formatNumberForLanguage(totalFunds, language)} {copy.fundsLabel}
                </p>
                <p className="mt-1 truncate text-xs text-gray-400">{copy.currentUser}：{currentUserLabel}</p>
                <div className="mt-2 flex flex-wrap gap-2">
                  <span className={`rounded-full px-2.5 py-1 text-[11px] font-medium ring-1 ${kycStatusTone(currentSession?.kycStatus)}`}>
                    {kycStatusLabel(currentSession?.kycStatus, language)}
                  </span>
                  {currentSession?.kycLevel ? (
                    <span className="rounded-full bg-gray-100 px-2.5 py-1 text-[11px] font-medium text-gray-600">
                      {copy.kycLevel}: {currentSession.kycLevel}
                    </span>
                  ) : null}
                </div>
              </div>
              <Link
                to="/wallet"
                className="rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm font-medium text-emerald-700 shadow-sm transition hover:bg-emerald-100"
              >
                {copy.wallet}
              </Link>
              <Link
                to="/kyc"
                className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm font-medium text-amber-700 shadow-sm transition hover:bg-amber-100"
              >
                {copy.kyc}
              </Link>
              <Link
                to="/marketplace"
                className="rounded-xl border border-violet-200 bg-violet-50 px-4 py-3 text-sm font-medium text-violet-700 shadow-sm transition hover:bg-violet-100"
              >
                {copy.marketplace}
              </Link>
              {currentSession?.role === "super_admin" ? (
                <Link
                  to="/admin"
                  className="rounded-xl border border-indigo-200 bg-indigo-50 px-4 py-3 text-sm font-medium text-indigo-700 shadow-sm transition hover:bg-indigo-100"
                >
                  {copy.admin}
                </Link>
              ) : null}
              <button
                onClick={openCreateCompanyModal}
                className="rounded-xl bg-indigo-600 px-4 py-3 text-sm font-medium text-white shadow-sm transition hover:bg-indigo-700"
              >
                {copy.createCompany}
              </button>
              <button
                onClick={() => void handleLogout()}
                className="rounded-xl border border-gray-200 bg-white px-4 py-3 text-sm font-medium text-gray-700 shadow-sm transition hover:bg-gray-50"
              >
                {copy.logout}
              </button>
            </div>
          </div>

          {loading ? <div className="rounded-lg border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div> : null}

          {!loading && error ? (
            <div className="rounded-lg border border-red-200 bg-red-50 p-6 text-sm text-red-700">
              <p>{error}</p>
              <button onClick={() => void loadData()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
                {copy.retry}
              </button>
            </div>
          ) : null}

          {!loading && !error && companies.length === 0 ? (
            <div className="rounded-2xl border border-dashed border-gray-200 bg-white p-10 text-center shadow-sm">
              <p className="text-lg font-semibold text-gray-900">{copy.emptyTitle}</p>
              <p className="mx-auto mt-3 max-w-2xl text-sm leading-6 text-gray-500">{copy.emptyDescription}</p>
              <div className="mt-6 flex items-center justify-center gap-3">
                <button onClick={openCreateCompanyModal} className="rounded-lg bg-indigo-600 px-5 py-2.5 text-sm font-medium text-white hover:bg-indigo-700">
                  {copy.createFirstCompany}
                </button>
              </div>
            </div>
          ) : null}

          {!loading && !error && companies.length > 0 ? (
            <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
              {companies.map((company) => {
                const primaryFund = company.funds[0] ?? null;
                return (
                  <div key={company.id} className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                    <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
                      <div>
                        <h2 className="text-xl font-semibold text-gray-900">{company.name}</h2>
                        <p className="mt-2 text-sm text-gray-500">{company.description?.trim() || copy.noDescription}</p>
                      </div>
                      <div className="rounded-full bg-indigo-50 px-3 py-1 text-xs font-medium text-indigo-700">
                        {formatNumberForLanguage(company.funds.length, language)}{copy.fundCountSuffix}
                      </div>
                    </div>

                    {company.funds.length === 0 ? (
                      <div className="mt-6 rounded-xl border border-dashed border-gray-200 bg-gray-50 p-5 text-sm text-gray-500">
                        <p>{copy.noFundText}</p>
                        <button
                          onClick={() => openCreateFundModal(company)}
                          className="mt-4 rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700"
                        >
                          {copy.createFirstFund}
                        </button>
                      </div>
                    ) : (
                      <div className="mt-6 space-y-3">
                        {company.funds.map((fund) => (
                          <div
                            key={fund.id}
                            className="group flex items-center justify-between rounded-xl border border-gray-200 px-4 py-4 transition-colors hover:border-indigo-300 hover:bg-indigo-50/40"
                          >
                            <Link to={`/funds/${fund.id}`} className="flex min-w-0 flex-1 items-center pr-4">
                              <div className="min-w-0">
                                <p className="truncate font-medium text-gray-900">{fund.name}</p>
                                <p className="mt-1 text-xs text-gray-500">{renderFundSubtitle(fund)}</p>
                              </div>
                            </Link>
                            <div className="flex shrink-0 items-center gap-3">
                              <AutoExecuteInlineToggle
                                fund={fund}
                                onUpdated={(updated) =>
                                  setCompanies((prev) =>
                                    prev.map((c) =>
                                      c.id === company.id
                                        ? {
                                            ...c,
                                            funds: c.funds.map((f) => (f.id === updated.id ? { ...f, ...updated } : f)),
                                          }
                                        : c,
                                    ),
                                  )
                                }
                              />
                              <span className={`rounded-full px-2.5 py-1 text-xs font-medium ${fundStatusMeta(fund.status).badge}`}>{fundStatusMeta(fund.status).label}</span>
                            </div>
                          </div>
                        ))}
                      </div>
                    )}

                    {primaryFund ? (
                      <div className="mt-6 flex flex-wrap gap-3">
                        <Link to={`/funds/${primaryFund.id}`} className="rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700">
                          {copy.openPrimaryFund}
                        </Link>
                        <Link to={`/funds/${primaryFund.id}/decisions`} className="rounded-lg border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50">
                          {copy.viewDecisions}
                        </Link>
                        <Link to={`/funds/${primaryFund.id}/subscription`} className="rounded-lg border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50">
                          {copy.viewSubscription}
                        </Link>
                        <button
                          type="button"
                          onClick={() => openCreateFundModal(company)}
                          className="rounded-lg border border-emerald-300 bg-emerald-50 px-4 py-2 text-sm font-medium text-emerald-700 transition hover:bg-emerald-100"
                        >
                          + {copy.addFund}
                        </button>
                      </div>
                    ) : null}
                  </div>
                );
              })}
            </div>
          ) : null}
        </div>
      </div>

      <CreateCompanyModal
        open={showCreateCompanyModal}
        form={companyForm}
        saving={companySaving}
        error={companyError}
        onClose={closeCreateCompanyModal}
        onChange={updateCompanyForm}
        onSubmit={handleCreateCompany}
      />

      <CreateFundModal
        open={showCreateFundModal}
        company={fundTargetCompany}
        form={fundForm}
        saving={fundSaving}
        error={fundError}
        onClose={closeCreateFundModal}
        onChange={updateFundForm}
        onSubmit={handleCreateFund}
      />
    </>
  );
};

export default Companies;
