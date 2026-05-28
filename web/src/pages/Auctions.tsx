import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  apiGet,
  ApiError,
  formatApiError,
  AuctionListing,
  PlaceAuctionBidResponse,
  CreateAuctionInput,
  AuctionSettlementResult,
  fetchAuctions,
  fetchAuction,
  createAuction,
  placeAuctionBid,
  settleAuction,
} from "../lib/api";
import {
  AppLanguage,
  DisplayCurrency,
  formatDateTimeForLanguage,
  formatMoneyMinorForDisplay,
  formatNumberForLanguage,
  useAppPreferences,
} from "../lib/preferences";

interface CompanyOverview {
  id: string;
  name: string;
  funds: FundSummary[];
}

interface FundSummary {
  id: string;
  companyId: string;
  name: string;
}

interface TeamAgent {
  id: string;
  agentId?: string;
  name?: string;
  role: string;
  focus?: string;
  latestLearningSummary?: string;
}

const COPY = {
  zh: {
    title: "拍卖市场",
    subtitle: "英式递增 + anti-sniping + 钱包冻结。卖方设定起拍价 / 保留价 / 最小递增 / 反狙击窗口，竞拍者出价时钱包资金被冻结，被超出后自动退款，最高价在截止时间结算并克隆 agent。",
    refresh: "刷新",
    listLoading: "正在加载拍卖…",
    listEmpty: "暂无进行中的拍卖。点击右上角发起一场。",
    createTab: "发起拍卖",
    listTab: "进行中",
    fund: "基金",
    agent: "Agent",
    startingPrice: "起拍价",
    reserve: "保留价（可选）",
    minIncrement: "最小递增",
    antiSnipe: "反狙击窗口（秒）",
    currency: "币种",
    endsAt: "截止时间",
    createCta: "发起拍卖",
    creating: "创建中…",
    detailTitle: "拍卖详情",
    statusLabel: "状态",
    timeRemaining: "剩余时间",
    closedLabel: "已截止",
    minNextBid: "下一口最低出价",
    bidPlaceholder: "出价金额（分）",
    bidCta: "出价",
    bidding: "出价中…",
    settleCta: "结算（卖方）",
    settling: "结算中…",
    backToList: "← 返回列表",
    outcome: {
      sold: "已成交",
      reserve_not_met: "未达保留价 - 已退款",
      no_bids: "无人出价 - 已下架",
    } as Record<string, string>,
    fields: {
      currentBid: "当前最高",
      yourPosition: "你的状态",
      seller: "卖方",
      winner: "中标者",
    },
  },
  en: {
    title: "Auction Marketplace",
    subtitle: "English-ascending auctions with wallet-hold escrow and anti-sniping. Sellers set a starting price, optional reserve, minimum increment and anti-snipe window; bidders' wallets are held on each top bid and refunded automatically when outbid.",
    refresh: "Refresh",
    listLoading: "Loading auctions…",
    listEmpty: "No live auctions yet. Open one from the right panel.",
    createTab: "Create",
    listTab: "Live",
    fund: "Fund",
    agent: "Agent",
    startingPrice: "Starting price (minor)",
    reserve: "Reserve (optional, minor)",
    minIncrement: "Min increment (minor)",
    antiSnipe: "Anti-snipe window (s)",
    currency: "Currency",
    endsAt: "Ends at",
    createCta: "Open auction",
    creating: "Creating…",
    detailTitle: "Auction detail",
    statusLabel: "Status",
    timeRemaining: "Time remaining",
    closedLabel: "Closed",
    minNextBid: "Min next bid (minor)",
    bidPlaceholder: "Your bid (minor)",
    bidCta: "Place bid",
    bidding: "Placing…",
    settleCta: "Settle (seller)",
    settling: "Settling…",
    backToList: "← Back",
    outcome: {
      sold: "Sold",
      reserve_not_met: "Reserve not met — refunded",
      no_bids: "No bids — withdrawn",
    } as Record<string, string>,
    fields: {
      currentBid: "Current top",
      yourPosition: "Your position",
      seller: "Seller",
      winner: "Winner",
    },
  },
};

interface AuctionFormState {
  companyId: string;
  fundId: string;
  agentId: string;
  startingPriceMinor: string;
  reserveMinor: string;
  minIncrementMinor: string;
  antiSnipeSeconds: string;
  currency: string;
  endsAt: string;
}

function defaultFormState(): AuctionFormState {
  // Default end time: 2 hours from now, ISO local string for the
  // datetime-local input.
  const ends = new Date(Date.now() + 2 * 60 * 60 * 1000);
  const pad = (n: number) => String(n).padStart(2, "0");
  const local = `${ends.getFullYear()}-${pad(ends.getMonth() + 1)}-${pad(ends.getDate())}T${pad(ends.getHours())}:${pad(ends.getMinutes())}`;
  return {
    companyId: "",
    fundId: "",
    agentId: "",
    startingPriceMinor: "10000",
    reserveMinor: "",
    minIncrementMinor: "100",
    antiSnipeSeconds: "60",
    currency: "USD",
    endsAt: local,
  };
}

export default function Auctions() {
  const { language, displayCurrency } = useAppPreferences();
  const copy = useMemo(() => (language === "zh-CN" ? COPY.zh : COPY.en), [language]);

  const [companies, setCompanies] = useState<CompanyOverview[]>([]);
  const [teamAgents, setTeamAgents] = useState<TeamAgent[]>([]);
  const [auctions, setAuctions] = useState<AuctionListing[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [selectedDetail, setSelectedDetail] = useState<AuctionListing | null>(null);
  const [recentSettlement, setRecentSettlement] = useState<AuctionSettlementResult | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState<AuctionFormState>(defaultFormState);
  const [bidAmount, setBidAmount] = useState<string>("");
  const [busy, setBusy] = useState<null | "create" | "bid" | "settle">(null);

  // Load companies + funds once to feed the auction-creation dropdowns.
  useEffect(() => {
    apiGet<CompanyOverview[]>("/api/companies/overview")
      .then((data) => setCompanies(Array.isArray(data) ? data : []))
      .catch(() => setCompanies([]));
  }, []);

  const reloadList = useCallback(() => {
    setLoading(true);
    fetchAuctions(50)
      .then((data) => {
        setAuctions(data);
        setError(null);
      })
      .catch((err) => {
        if (err instanceof ApiError && err.status === 503) {
          setAuctions([]);
          setError(language === "zh-CN" ? "拍卖服务尚未启用" : "Auction service is not enabled");
        } else {
          setError(formatApiError(err, language === "zh-CN" ? "拉取拍卖失败" : "Failed to load auctions"));
        }
      })
      .finally(() => setLoading(false));
  }, [language]);

  useEffect(() => {
    reloadList();
  }, [reloadList]);

  // Auto-refresh the selected auction every 5s so bid + ends_at stay live.
  useEffect(() => {
    if (!selectedId) {
      setSelectedDetail(null);
      return;
    }
    let cancelled = false;
    const tick = () => {
      fetchAuction(selectedId)
        .then((data) => {
          if (!cancelled) setSelectedDetail(data);
        })
        .catch((err) => {
          if (!cancelled) {
            setError(formatApiError(err, language === "zh-CN" ? "拉取拍卖详情失败" : "Failed to load auction detail"));
          }
        });
    };
    tick();
    const id = window.setInterval(tick, 5_000);
    return () => {
      cancelled = true;
      window.clearInterval(id);
    };
  }, [selectedId, language]);

  // When the user picks a fund, load that fund's team so they can pick
  // the agent to auction.
  useEffect(() => {
    if (!form.fundId) {
      setTeamAgents([]);
      return;
    }
    apiGet<TeamAgent[]>(`/api/funds/${encodeURIComponent(form.fundId)}/team`)
      .then((data) => setTeamAgents(Array.isArray(data) ? data : []))
      .catch(() => setTeamAgents([]));
  }, [form.fundId]);

  const handleCreate = async () => {
    if (!form.fundId || !form.agentId) {
      setError(language === "zh-CN" ? "请选择基金和 agent" : "Pick a fund and an agent");
      return;
    }
    const starting = Number.parseInt(form.startingPriceMinor, 10);
    if (!Number.isFinite(starting) || starting <= 0) {
      setError(language === "zh-CN" ? "起拍价必须为正" : "Starting price must be positive");
      return;
    }
    const endsAt = new Date(form.endsAt);
    if (Number.isNaN(endsAt.getTime()) || endsAt.getTime() <= Date.now()) {
      setError(language === "zh-CN" ? "截止时间必须在未来" : "End time must be in the future");
      return;
    }
    const payload: CreateAuctionInput = {
      fundId: form.fundId,
      agentId: form.agentId,
      startingPriceMinor: starting,
      reserveMinor: form.reserveMinor ? Number.parseInt(form.reserveMinor, 10) : undefined,
      minIncrementMinor: form.minIncrementMinor ? Number.parseInt(form.minIncrementMinor, 10) : undefined,
      antiSnipeSeconds: form.antiSnipeSeconds ? Number.parseInt(form.antiSnipeSeconds, 10) : undefined,
      currency: form.currency || "USD",
      endsAt: endsAt.toISOString(),
    };
    setBusy("create");
    setError(null);
    try {
      const created = await createAuction(payload);
      reloadList();
      setSelectedId(created.id);
      setForm(defaultFormState());
    } catch (err) {
      setError(formatApiError(err, language === "zh-CN" ? "创建拍卖失败" : "Failed to create auction"));
    } finally {
      setBusy(null);
    }
  };

  const handleBid = async () => {
    if (!selectedDetail) return;
    const amount = Number.parseInt(bidAmount, 10);
    if (!Number.isFinite(amount) || amount <= 0) {
      setError(language === "zh-CN" ? "请输入出价" : "Enter a bid amount");
      return;
    }
    if (amount < selectedDetail.minNextBidMinor) {
      setError(
        language === "zh-CN"
          ? `出价必须 >= 下一口最低 ${selectedDetail.minNextBidMinor}`
          : `Bid must be >= min next ${selectedDetail.minNextBidMinor}`,
      );
      return;
    }
    setBusy("bid");
    setError(null);
    try {
      const result: PlaceAuctionBidResponse = await placeAuctionBid(
        selectedDetail.id,
        amount,
        selectedDetail.currency,
      );
      if (result.auction) {
        setSelectedDetail(result.auction);
      }
      setBidAmount("");
      reloadList();
    } catch (err) {
      setError(formatApiError(err, language === "zh-CN" ? "出价失败" : "Failed to place bid"));
    } finally {
      setBusy(null);
    }
  };

  const handleSettle = async () => {
    if (!selectedDetail) return;
    setBusy("settle");
    setError(null);
    try {
      const result = await settleAuction(selectedDetail.id);
      setRecentSettlement(result);
      reloadList();
      const refreshed = await fetchAuction(selectedDetail.id);
      setSelectedDetail(refreshed);
    } catch (err) {
      setError(formatApiError(err, language === "zh-CN" ? "结算失败" : "Failed to settle"));
    } finally {
      setBusy(null);
    }
  };

  const allFunds = useMemo(() => companies.flatMap((c) => c.funds.map((f) => ({ ...f, companyName: c.name }))), [companies]);

  return (
    <div className="mx-auto max-w-7xl space-y-6 p-6">
      <header className="flex items-start justify-between">
        <div>
          <h1 className="text-2xl font-bold text-slate-900">{copy.title}</h1>
          <p className="mt-1 max-w-3xl text-sm text-slate-600">{copy.subtitle}</p>
          <div className="mt-2 text-xs">
            <Link className="text-indigo-600 hover:underline" to="/marketplace">
              ← Marketplace (buyout / subscribe)
            </Link>
          </div>
        </div>
        <button onClick={reloadList} className="rounded-md border border-slate-300 px-3 py-1.5 text-sm text-slate-700 hover:bg-slate-50">
          {copy.refresh}
        </button>
      </header>

      {error && (
        <div className="rounded-md border border-red-200 bg-red-50 px-4 py-2 text-sm text-red-700">{error}</div>
      )}

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
        <section className="rounded-lg border border-slate-200 bg-white p-4 shadow-sm">
          <div className="mb-3 flex items-center justify-between">
            <h2 className="text-lg font-semibold text-slate-900">{copy.listTab}</h2>
            <span className="text-xs text-slate-500">{auctions.length}</span>
          </div>
          {loading ? (
            <div className="py-12 text-center text-sm text-slate-500">{copy.listLoading}</div>
          ) : auctions.length === 0 ? (
            <div className="py-12 text-center text-sm text-slate-500">{copy.listEmpty}</div>
          ) : (
            <ul className="space-y-2">
              {auctions.map((auction) => (
                <li
                  key={auction.id}
                  onClick={() => setSelectedId(auction.id)}
                  className={`cursor-pointer rounded-md border px-3 py-2 text-sm ${
                    selectedId === auction.id ? "border-indigo-300 bg-indigo-50" : "border-slate-200 hover:bg-slate-50"
                  }`}
                >
                  <div className="flex items-center justify-between">
                    <span className="font-medium">{auction.agentName || auction.id}</span>
                    <span className="text-xs uppercase text-slate-500">{auction.status}</span>
                  </div>
                  <div className="mt-1 flex flex-wrap gap-3 text-xs text-slate-600">
                    <span>{auction.agentRole}</span>
                    <span>
                      {copy.fields.currentBid}:{" "}
                      {auction.currentBidMinor != null
                        ? formatMoneyMinorForDisplay(auction.currentBidMinor, auction.currency, displayCurrency, language)
                        : "—"}
                    </span>
                    <span>
                      {copy.minNextBid}:{" "}
                      {formatMoneyMinorForDisplay(auction.minNextBidMinor, auction.currency, displayCurrency, language)}
                    </span>
                    <span>
                      {copy.endsAt}: {auction.endsAt ? formatDateTimeForLanguage(auction.endsAt, language) : "—"}
                    </span>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </section>

        <section className="space-y-4">
          {selectedDetail ? (
            <AuctionDetail
              auction={selectedDetail}
              copy={copy}
              language={language}
              displayCurrency={displayCurrency}
              bidAmount={bidAmount}
              onBidAmountChange={setBidAmount}
              onBid={handleBid}
              onSettle={handleSettle}
              busy={busy}
              recentSettlement={recentSettlement}
              onClose={() => {
                setSelectedId(null);
                setRecentSettlement(null);
              }}
            />
          ) : (
            <CreateAuctionForm
              copy={copy}
              form={form}
              setForm={setForm}
              allFunds={allFunds}
              teamAgents={teamAgents}
              onSubmit={handleCreate}
              busy={busy === "create"}
            />
          )}
        </section>
      </div>
    </div>
  );
}

interface AuctionDetailProps {
  auction: AuctionListing;
  copy: typeof COPY.en;
  language: AppLanguage;
  displayCurrency: DisplayCurrency;
  bidAmount: string;
  onBidAmountChange: (v: string) => void;
  onBid: () => void;
  onSettle: () => void;
  busy: null | "create" | "bid" | "settle";
  recentSettlement: AuctionSettlementResult | null;
  onClose: () => void;
}

function AuctionDetail({
  auction,
  copy,
  language,
  displayCurrency,
  bidAmount,
  onBidAmountChange,
  onBid,
  onSettle,
  busy,
  recentSettlement,
  onClose,
}: AuctionDetailProps) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, []);
  const endsMs = auction.endsAt ? new Date(auction.endsAt).getTime() : 0;
  const remainingMs = endsMs > 0 ? endsMs - now : 0;
  const remaining =
    remainingMs > 0
      ? `${Math.floor(remainingMs / 1000)}s`
      : copy.closedLabel;

  const outcomeLabel =
    recentSettlement && recentSettlement.outcome
      ? copy.outcome[recentSettlement.outcome] ?? recentSettlement.outcome
      : null;

  const canBid = auction.status === "active" && remainingMs > 0;
  const canSettle = auction.status === "active" && remainingMs <= 0;

  return (
    <div className="rounded-lg border border-slate-200 bg-white p-5 shadow-sm">
      <div className="mb-3 flex items-center justify-between">
        <button onClick={onClose} className="text-xs text-slate-500 hover:underline">
          {copy.backToList}
        </button>
        <h2 className="text-lg font-semibold text-slate-900">{auction.agentName || auction.id}</h2>
      </div>
      <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
        <dt className="text-slate-500">{copy.statusLabel}</dt>
        <dd className="font-medium uppercase">{auction.status}</dd>

        <dt className="text-slate-500">{copy.fields.currentBid}</dt>
        <dd>
          {auction.currentBidMinor != null
            ? formatMoneyMinorForDisplay(auction.currentBidMinor, auction.currency, displayCurrency, language)
            : "—"}
        </dd>

        <dt className="text-slate-500">{copy.minNextBid}</dt>
        <dd>{formatMoneyMinorForDisplay(auction.minNextBidMinor, auction.currency, displayCurrency, language)}</dd>

        <dt className="text-slate-500">{copy.timeRemaining}</dt>
        <dd>{remaining}</dd>

        <dt className="text-slate-500">{copy.endsAt}</dt>
        <dd>{auction.endsAt ? formatDateTimeForLanguage(auction.endsAt, language) : "—"}</dd>

        <dt className="text-slate-500">{copy.antiSnipe}</dt>
        <dd>{formatNumberForLanguage(auction.antiSnipeSeconds, language)}s</dd>

        {auction.reserveMinor != null && (
          <>
            <dt className="text-slate-500">{copy.reserve}</dt>
            <dd>{formatMoneyMinorForDisplay(auction.reserveMinor, auction.currency, displayCurrency, language)}</dd>
          </>
        )}

        {auction.winnerUserId && (
          <>
            <dt className="text-slate-500">{copy.fields.winner}</dt>
            <dd className="font-mono text-xs">{auction.winnerUserId}</dd>
          </>
        )}
      </dl>

      <div className="mt-4 flex items-end gap-2">
        <div className="flex-1">
          <label className="block text-xs text-slate-500">{copy.bidPlaceholder}</label>
          <input
            type="number"
            min={auction.minNextBidMinor}
            step={auction.minIncrementMinor}
            value={bidAmount}
            onChange={(e) => onBidAmountChange(e.target.value)}
            className="mt-1 w-full rounded-md border border-slate-300 px-3 py-1.5 text-sm"
            disabled={!canBid}
          />
        </div>
        <button
          onClick={onBid}
          disabled={!canBid || busy === "bid"}
          className="rounded-md bg-indigo-600 px-4 py-1.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:bg-slate-300"
        >
          {busy === "bid" ? copy.bidding : copy.bidCta}
        </button>
        {canSettle && (
          <button
            onClick={onSettle}
            disabled={busy === "settle"}
            className="rounded-md bg-amber-500 px-4 py-1.5 text-sm font-medium text-white hover:bg-amber-600 disabled:bg-slate-300"
          >
            {busy === "settle" ? copy.settling : copy.settleCta}
          </button>
        )}
      </div>

      {outcomeLabel && (
        <div className="mt-4 rounded-md border border-emerald-200 bg-emerald-50 px-3 py-2 text-sm text-emerald-800">
          {outcomeLabel}
          {recentSettlement?.finalBidMinor != null && (
            <span className="ml-2 font-mono">
              {formatMoneyMinorForDisplay(recentSettlement.finalBidMinor, auction.currency, displayCurrency, language)}
            </span>
          )}
        </div>
      )}
    </div>
  );
}

interface CreateAuctionFormProps {
  copy: typeof COPY.en;
  form: AuctionFormState;
  setForm: React.Dispatch<React.SetStateAction<AuctionFormState>>;
  allFunds: Array<FundSummary & { companyName: string }>;
  teamAgents: TeamAgent[];
  onSubmit: () => void;
  busy: boolean;
}

function CreateAuctionForm({ copy, form, setForm, allFunds, teamAgents, onSubmit, busy }: CreateAuctionFormProps) {
  const updateField = (field: keyof AuctionFormState) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm((prev) => ({ ...prev, [field]: e.target.value }));

  return (
    <div className="rounded-lg border border-slate-200 bg-white p-5 shadow-sm">
      <h2 className="mb-3 text-lg font-semibold text-slate-900">{copy.createTab}</h2>
      <div className="grid grid-cols-2 gap-3 text-sm">
        <label className="col-span-2">
          <span className="text-xs text-slate-500">{copy.fund}</span>
          <select
            value={form.fundId}
            onChange={(e) => setForm((prev) => ({ ...prev, fundId: e.target.value, agentId: "" }))}
            className="mt-1 w-full rounded-md border border-slate-300 px-3 py-1.5"
          >
            <option value="">--</option>
            {allFunds.map((f) => (
              <option key={f.id} value={f.id}>
                {f.companyName} / {f.name}
              </option>
            ))}
          </select>
        </label>

        <label className="col-span-2">
          <span className="text-xs text-slate-500">{copy.agent}</span>
          <select
            value={form.agentId}
            onChange={updateField("agentId")}
            disabled={!form.fundId || teamAgents.length === 0}
            className="mt-1 w-full rounded-md border border-slate-300 px-3 py-1.5"
          >
            <option value="">--</option>
            {teamAgents.map((a) => (
              <option key={a.id} value={a.agentId ?? a.id}>
                {(a.name ?? a.id)} ({a.role})
              </option>
            ))}
          </select>
        </label>

        <label>
          <span className="text-xs text-slate-500">{copy.startingPrice}</span>
          <input
            type="number"
            value={form.startingPriceMinor}
            onChange={updateField("startingPriceMinor")}
            className="mt-1 w-full rounded-md border border-slate-300 px-3 py-1.5"
          />
        </label>

        <label>
          <span className="text-xs text-slate-500">{copy.reserve}</span>
          <input
            type="number"
            value={form.reserveMinor}
            onChange={updateField("reserveMinor")}
            className="mt-1 w-full rounded-md border border-slate-300 px-3 py-1.5"
          />
        </label>

        <label>
          <span className="text-xs text-slate-500">{copy.minIncrement}</span>
          <input
            type="number"
            value={form.minIncrementMinor}
            onChange={updateField("minIncrementMinor")}
            className="mt-1 w-full rounded-md border border-slate-300 px-3 py-1.5"
          />
        </label>

        <label>
          <span className="text-xs text-slate-500">{copy.antiSnipe}</span>
          <input
            type="number"
            value={form.antiSnipeSeconds}
            onChange={updateField("antiSnipeSeconds")}
            className="mt-1 w-full rounded-md border border-slate-300 px-3 py-1.5"
          />
        </label>

        <label>
          <span className="text-xs text-slate-500">{copy.currency}</span>
          <input
            type="text"
            value={form.currency}
            onChange={updateField("currency")}
            className="mt-1 w-full rounded-md border border-slate-300 px-3 py-1.5"
          />
        </label>

        <label className="col-span-2">
          <span className="text-xs text-slate-500">{copy.endsAt}</span>
          <input
            type="datetime-local"
            value={form.endsAt}
            onChange={updateField("endsAt")}
            className="mt-1 w-full rounded-md border border-slate-300 px-3 py-1.5"
          />
        </label>
      </div>
      <button
        onClick={onSubmit}
        disabled={busy}
        className="mt-4 w-full rounded-md bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700 disabled:bg-slate-300"
      >
        {busy ? copy.creating : copy.createCta}
      </button>
    </div>
  );
}
