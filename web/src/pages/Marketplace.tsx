import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { apiDelete, apiGet, apiPost, formatApiError } from "../lib/api";
import { formatDateTimeForLanguage, formatMoneyMinorForDisplay, formatNumberForLanguage, useAppPreferences } from "../lib/preferences";
import { EmptyState } from "../components/EmptyState";

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

interface MarketplaceListing {
  id: string;
  sellerUserId?: string;
  sourceFundId: string;
  sourceAgentId: string;
  agentName: string;
  agentRole: string;
  agentFocus?: string;
  latestLearningSummary?: string;
  askPriceMinor: number;
  currency: string;
  status: string;
  trust?: MarketplaceTrustSignals;
  soldToUserId?: string;
  soldAt?: string;
  createdAt: string;
  updatedAt: string;
}

interface MarketplaceTrustSignals {
  score: number;
  level: string;
  badges?: string[];
  evidence?: string[];
  learningRecords: number;
  publicMemoryRecords: number;
  lastLearningAt?: string;
  lastDailyReturn?: number;
  modelConfigured: boolean;
  profileCompleteness: number;
  listingAgeDays: number;
}

interface MarketplaceBid {
  id: string;
  listingId: string;
  bidderUserId?: string;
  bidPriceMinor: number;
  currency: string;
  status: string;
  createdAt: string;
  updatedAt: string;
}

interface MarketplaceOrder {
  id: string;
  listingId: string;
  sellerUserId?: string;
  buyerUserId?: string;
  buyerFundId?: string;
  sourceAgentId: string;
  deliveredAgentId: string;
  amountMinor: number;
  currency: string;
  status: string;
  createdAt: string;
}

interface CreateListingState {
  fundId: string;
  agentId: string;
  askPriceMinor: string;
  currency: string;
}

const INITIAL_CREATE_FORM: CreateListingState = {
  fundId: "",
  agentId: "",
  askPriceMinor: "",
  currency: "USD",
};

const MARKETPLACE_PAGE_SIZE = 20;
const MARKETPLACE_BID_PAGE_SIZE = 50;

function pagedPath(path: string, limit: number, offset: number): string {
  const params = new URLSearchParams({
    limit: String(limit),
    offset: String(offset),
  });
  return `${path}?${params.toString()}`;
}

function roleLabel(value: string | undefined, language: "zh-CN" | "en-US"): string {
  const labels: Record<string, { zh: string; en: string }> = {
    pm: { zh: "组合经理", en: "PM" },
    researcher: { zh: "研究员", en: "Researcher" },
    trader: { zh: "交易员", en: "Trader" },
    risk: { zh: "风控", en: "Risk" },
  };
  const matched = labels[value ?? ""];
  if (matched) {
    return language === "en-US" ? matched.en : matched.zh;
  }
  return value ?? (language === "en-US" ? "Unknown" : "未知");
}

function statusLabel(value: string | undefined, language: "zh-CN" | "en-US"): string {
  const labels: Record<string, { zh: string; en: string }> = {
    active: { zh: "在售", en: "Active" },
    sold: { zh: "已售出", en: "Sold" },
    cancelled: { zh: "已取消", en: "Cancelled" },
    pending: { zh: "处理中", en: "Pending" },
    completed: { zh: "已完成", en: "Completed" },
  };
  const matched = labels[value ?? ""];
  if (matched) {
    return language === "en-US" ? matched.en : matched.zh;
  }
  return value ?? (language === "en-US" ? "Unknown" : "未知");
}

function trustLevelLabel(value: string | undefined, language: "zh-CN" | "en-US"): string {
  const labels: Record<string, { zh: string; en: string }> = {
    high: { zh: "高可信", en: "High trust" },
    medium: { zh: "中可信", en: "Medium trust" },
    low: { zh: "低可信", en: "Low trust" },
  };
  const matched = labels[value ?? ""];
  return matched ? (language === "en-US" ? matched.en : matched.zh) : (language === "en-US" ? "Trust pending" : "可信度待评估");
}

function trustLevelClass(value: string | undefined): string {
  switch (value) {
    case "high":
      return "bg-emerald-50 text-emerald-700 ring-emerald-100";
    case "medium":
      return "bg-amber-50 text-amber-700 ring-amber-100";
    default:
      return "bg-gray-100 text-gray-600 ring-gray-200";
  }
}

function isKYCErrorMessage(message?: string | null): boolean {
  const normalized = (message ?? "").toLowerCase();
  return normalized.includes("kyc_required") || normalized.includes("kyc_level_insufficient") || normalized.includes("kyc");
}

function agentDisplayName(agent: TeamAgent, language: "zh-CN" | "en-US"): string {
  return agent.name?.trim() || `${roleLabel(agent.role, language)}${language === "en-US" ? " member" : "成员"}`;
}

const Marketplace: React.FC = () => {
  const { language, displayCurrency } = useAppPreferences();
  const [listings, setListings] = useState<MarketplaceListing[]>([]);
  const [myListings, setMyListings] = useState<MarketplaceListing[]>([]);
  const [companyOverviews, setCompanyOverviews] = useState<CompanyOverview[]>([]);
  const [fundAgents, setFundAgents] = useState<Record<string, TeamAgent[]>>({});
  const [selectedTab, setSelectedTab] = useState<"market" | "mine">("market");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [actionSuccess, setActionSuccess] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [createForm, setCreateForm] = useState<CreateListingState>(INITIAL_CREATE_FORM);
  const [loadingAgents, setLoadingAgents] = useState(false);
  const [bidInputs, setBidInputs] = useState<Record<string, string>>({});
  const [bidsByListing, setBidsByListing] = useState<Record<string, MarketplaceBid[]>>({});
  const [loadingBidListingId, setLoadingBidListingId] = useState<string | null>(null);
  const [purchaseListingId, setPurchaseListingId] = useState<string | null>(null);
  const [purchaseFundByListing, setPurchaseFundByListing] = useState<Record<string, string>>({});
  const [marketHasMore, setMarketHasMore] = useState(false);
  const [myHasMore, setMyHasMore] = useState(false);
  const [loadingMoreTab, setLoadingMoreTab] = useState<"market" | "mine" | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            loading: "Loading marketplace...",
            loadError: "Failed to load marketplace",
            retry: "Retry",
            loadMore: "Load more",
            loadingMore: "Loading more...",
            title: "Agent marketplace",
            subtitle: "Browse public listings across the platform, submit bids, or list a trained team member from your own fund.",
            refresh: "Refresh market",
            sourceFund: "Source fund",
            sourceFundPlaceholder: "Select a fund",
            teamMember: "Team member",
            teamMemberLoading: "Loading members...",
            teamMemberPlaceholder: "Select a member",
            askPrice: "Ask price (minor units)",
            currency: "Currency",
            publish: "Publish listing",
            publishing: "Submitting...",
            createTitle: "Create listing",
            createSubtitle: "Choose one of your funds and team members to publish a public marketplace listing.",
            marketTab: "Market listings",
            myTab: "My listings",
            marketEmpty: "No public listings yet",
            marketEmptyDescription: "Once other strategy owners list a member here, this is where you'll discover, bid and recruit them. Switch to your fund's Team Management to package one of your own members for sale.",
            myEmpty: "You haven't published any listings",
            myEmptyDescription: "List a fund member with a public summary so other strategy owners can review their track record and recruit them. You'll keep ownership until a buyer's bid is accepted.",
            directPrice: "Buy now",
            listingCreated: "Listing published successfully.",
            listingCancelled: "Listing cancelled.",
            bidCreated: "Bid submitted.",
            purchaseSuccessBound: "Purchase completed and the member was bound to the selected fund: {id}.",
            purchaseSuccessInventory: "Purchase completed. The member is now in your owned inventory: {id}.",
            createFundRequired: "Please select a source fund.",
            createAgentRequired: "Please select a team member to list.",
            createPriceInvalid: "Please enter a valid ask price greater than 0.",
            bidPriceInvalid: "Please enter a valid bid amount greater than 0.",
            loadAgentsError: "Failed to load listable team members",
            createListingError: "Failed to create listing",
            cancelListingError: "Failed to cancel listing",
            loadBidsError: "Failed to load bids",
            createBidError: "Failed to submit bid",
            purchaseError: "Failed to purchase listing",
            completeKYC: "Complete KYC",
            createdAt: "Created",
            focus: "Focus",
            focusUnset: "Not set",
            summaryEmpty: "No public learning summary yet.",
            previewEmpty: "This member does not have a public learning summary yet.",
            sellerPreview: "Selected member preview",
            marketSource: "Fund",
            marketAgent: "Agent",
            bidLabel: "Bid (minor units)",
            bidButton: "Bid",
            buyerFund: "Bind to fund (optional)",
            buyerFundHint: "Leave empty to buy into owned inventory first, then bind later from Team Management.",
            buyToInventory: "Keep in inventory",
            buyNow: "Buy member",
            buying: "Buying...",
            viewBids: "View bids",
            loadingBids: "Loading bids...",
            cancel: "Cancel listing",
            bidHistory: "Bid history",
            bidder: "Bidder",
            notAvailable: "-",
            trustScore: "Trust score",
            trustEvidence: "Trust evidence",
            trustBadges: {
              learning_summary: "Learning summary",
              learning_history: "Learning history",
              recent_learning: "Recent learning",
              return_trace: "Return trace",
              public_memory: "Public memory",
              model_configured: "Model configured",
              complete_profile: "Complete profile",
            } as Record<string, string>,
            learningRecords: "Learning records",
            publicMemories: "Public memories",
            lastReturn: "Last return",
            profileCompleteness: "Profile completeness",
          }
        : {
            loading: "正在加载交易市场...",
            loadError: "加载交易市场失败",
            retry: "重试",
            loadMore: "加载更多",
            loadingMore: "继续加载中...",
            title: "成员交易市场",
            subtitle: "浏览全平台公开挂牌，提交竞价，或把自己基金里的训练成员打包上架。",
            refresh: "刷新市场",
            sourceFund: "来源基金",
            sourceFundPlaceholder: "请选择基金",
            teamMember: "团队成员",
            teamMemberLoading: "正在加载成员...",
            teamMemberPlaceholder: "请选择成员",
            askPrice: "挂牌价格（分）",
            currency: "币种",
            publish: "发布挂牌",
            publishing: "提交中...",
            createTitle: "创建挂牌",
            createSubtitle: "选择自己的基金和团队成员，生成一个对全体用户可见的挂牌。",
            marketTab: "市场挂牌",
            myTab: "我的挂牌",
            marketEmpty: "暂时没有公开挂牌",
            marketEmptyDescription: "当其他策略主理人把成员公开挂牌后，你可以在此查看历史业绩、出价并吸纳。也可以前往团队管理把自家成员打包上架。",
            myEmpty: "你还没有发布过挂牌",
            myEmptyDescription: "把自家成员打包公开挂牌，附带可见摘要以供其他策略方了解业绩。在买家出价被接受前，你保留成员的所有权与决策权。",
            directPrice: "一口价",
            listingCreated: "成员已上架到交易市场。",
            listingCancelled: "挂牌已取消。",
            bidCreated: "出价已提交。",
            purchaseSuccessBound: "购买成功，成员已直接绑定到所选基金：{id}。",
            purchaseSuccessInventory: "购买成功，成员已进入你的未绑定库存：{id}。",
            createFundRequired: "请选择来源基金。",
            createAgentRequired: "请选择要上架的团队成员。",
            createPriceInvalid: "请输入合法的挂牌价格（分），且必须大于 0。",
            bidPriceInvalid: "请输入合法的出价金额（分），且必须大于 0。",
            loadAgentsError: "加载可上架成员失败",
            createListingError: "创建挂牌失败",
            cancelListingError: "取消挂牌失败",
            loadBidsError: "加载竞价失败",
            createBidError: "提交出价失败",
            purchaseError: "购买挂牌失败",
            completeKYC: "去实名认证",
            createdAt: "创建时间",
            focus: "方向",
            focusUnset: "未设置",
            summaryEmpty: "暂无公开学习摘要。",
            previewEmpty: "该成员当前还没有可展示的最新学习摘要。",
            sellerPreview: "成员预览",
            marketSource: "来源基金",
            marketAgent: "Agent",
            bidLabel: "出价（分）",
            bidButton: "出价",
            buyerFund: "立即绑定基金（可选）",
            buyerFundHint: "留空则先买入未绑定库存，稍后可在团队管理里再绑定到基金。",
            buyToInventory: "先放入未绑定库存",
            buyNow: "购买成员",
            buying: "购买中...",
            viewBids: "查看竞价",
            loadingBids: "加载竞价中...",
            cancel: "取消挂牌",
            bidHistory: "竞价记录",
            bidder: "竞价用户",
            notAvailable: "-",
            trustScore: "可信度评分",
            trustEvidence: "可信证据",
            trustBadges: {
              learning_summary: "学习摘要",
              learning_history: "学习履历",
              recent_learning: "近期学习",
              return_trace: "收益追踪",
              public_memory: "公开记忆",
              model_configured: "模型已配置",
              complete_profile: "履历完整",
            } as Record<string, string>,
            learningRecords: "学习记录",
            publicMemories: "公开记忆",
            lastReturn: "最近收益",
            profileCompleteness: "履历完整度",
          },
    [language],
  );

  const selectedFundAgents = fundAgents[createForm.fundId] ?? [];

  useEffect(() => {
    if (!createForm.fundId) {
      return;
    }
    const cachedAgents = fundAgents[createForm.fundId] ?? [];
    if (cachedAgents.length === 0) {
      return;
    }
    setCreateForm((current) => {
      if (current.fundId !== createForm.fundId || current.agentId) {
        return current;
      }
      return { ...current, agentId: cachedAgents[0].id };
    });
  }, [createForm.fundId, fundAgents]);

  const loadMarketplace = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [marketRes, myRes, companyRes] = await Promise.all([
        apiGet<MarketplaceListing[]>(pagedPath("/api/marketplace/listings", MARKETPLACE_PAGE_SIZE, 0)),
        apiGet<MarketplaceListing[]>(pagedPath("/api/marketplace/my-listings", MARKETPLACE_PAGE_SIZE, 0)),
        apiGet<CompanyOverview[]>("/api/companies/overview"),
      ]);
      const nextMarket = marketRes ?? [];
      const nextMine = myRes ?? [];
      setListings(nextMarket);
      setMyListings(nextMine);
      setMarketHasMore(nextMarket.length === MARKETPLACE_PAGE_SIZE);
      setMyHasMore(nextMine.length === MARKETPLACE_PAGE_SIZE);
      setCompanyOverviews(companyRes ?? []);
      if (!createForm.fundId && companyRes.length > 0) {
        const firstFund = companyRes.flatMap((company) => company.funds ?? [])[0];
        if (firstFund) {
          setCreateForm((current) => ({ ...current, fundId: firstFund.id }));
        }
      }
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError, createForm.fundId]);

  const handleLoadMoreListings = useCallback(async () => {
    const tab = selectedTab;
    const offset = tab === "market" ? listings.length : myListings.length;
    const endpoint = tab === "market" ? "/api/marketplace/listings" : "/api/marketplace/my-listings";
    setLoadingMoreTab(tab);
    setActionError(null);
    try {
      const response = await apiGet<MarketplaceListing[]>(pagedPath(endpoint, MARKETPLACE_PAGE_SIZE, offset));
      const nextPage = response ?? [];
      if (tab === "market") {
        setListings((current) => [...current, ...nextPage]);
        setMarketHasMore(nextPage.length === MARKETPLACE_PAGE_SIZE);
      } else {
        setMyListings((current) => [...current, ...nextPage]);
        setMyHasMore(nextPage.length === MARKETPLACE_PAGE_SIZE);
      }
    } catch (err) {
      setActionError(formatApiError(err, copy.loadError));
    } finally {
      setLoadingMoreTab(null);
    }
  }, [copy.loadError, listings.length, myListings.length, selectedTab]);

  useEffect(() => {
    void loadMarketplace();
  }, [loadMarketplace]);

  const loadFundAgents = useCallback(async (fundId: string) => {
    const normalized = fundId.trim();
    if (!normalized || fundAgents[normalized]) {
      return;
    }
    setLoadingAgents(true);
    setActionError(null);
    try {
      const response = await apiGet<TeamAgent[]>(`/api/funds/${normalized}/team`);
      setFundAgents((current) => ({ ...current, [normalized]: response ?? [] }));
      setCreateForm((current) => {
        if (current.fundId !== normalized) {
          return current;
        }
        const firstAgent = (response ?? [])[0];
        return { ...current, agentId: firstAgent?.id ?? "" };
      });
    } catch (err) {
      setActionError(formatApiError(err, copy.loadAgentsError));
    } finally {
      setLoadingAgents(false);
    }
  }, [copy.loadAgentsError, fundAgents]);

  useEffect(() => {
    if (!createForm.fundId) {
      return;
    }
    void loadFundAgents(createForm.fundId);
  }, [createForm.fundId, loadFundAgents]);

  const handleCreateListing = useCallback(async () => {
    const askPriceMinor = Number(createForm.askPriceMinor);
    if (!createForm.fundId.trim()) {
      setActionError(copy.createFundRequired);
      setActionSuccess(null);
      return;
    }
    if (!createForm.agentId.trim()) {
      setActionError(copy.createAgentRequired);
      setActionSuccess(null);
      return;
    }
    if (!Number.isFinite(askPriceMinor) || askPriceMinor <= 0) {
      setActionError(copy.createPriceInvalid);
      setActionSuccess(null);
      return;
    }

    setSaving(true);
    setActionError(null);
    setActionSuccess(null);
    try {
      await apiPost<MarketplaceListing>("/api/marketplace/listings", {
        fundId: createForm.fundId.trim(),
        agentId: createForm.agentId.trim(),
        askPriceMinor: Math.round(askPriceMinor),
        currency: createForm.currency.trim() || "USD",
      });
      setCreateForm((current) => ({ ...current, askPriceMinor: "" }));
      setActionSuccess(copy.listingCreated);
      await loadMarketplace();
      setSelectedTab("mine");
    } catch (err) {
      setActionError(formatApiError(err, copy.createListingError));
    } finally {
      setSaving(false);
    }
  }, [copy.createAgentRequired, copy.createFundRequired, copy.createListingError, copy.createPriceInvalid, copy.listingCreated, createForm, loadMarketplace]);

  const handleCancelListing = useCallback(async (listingId: string) => {
    setSaving(true);
    setActionError(null);
    setActionSuccess(null);
    try {
      await apiDelete(`/api/marketplace/listings/${listingId}`);
      setActionSuccess(copy.listingCancelled);
      await loadMarketplace();
      setBidsByListing((current) => {
        const next = { ...current };
        delete next[listingId];
        return next;
      });
    } catch (err) {
      setActionError(formatApiError(err, copy.cancelListingError));
    } finally {
      setSaving(false);
    }
  }, [copy.cancelListingError, copy.listingCancelled, loadMarketplace]);

  const handleLoadBids = useCallback(async (listingId: string) => {
    setLoadingBidListingId(listingId);
    setActionError(null);
    try {
      const response = await apiGet<MarketplaceBid[]>(pagedPath(`/api/marketplace/listings/${listingId}/bids`, MARKETPLACE_BID_PAGE_SIZE, 0));
      setBidsByListing((current) => ({ ...current, [listingId]: response ?? [] }));
    } catch (err) {
      setActionError(formatApiError(err, copy.loadBidsError));
    } finally {
      setLoadingBidListingId(null);
    }
  }, [copy.loadBidsError]);

  const handleCreateBid = useCallback(async (listingId: string, currency: string) => {
    const value = bidInputs[listingId] ?? "";
    const bidPriceMinor = Number(value);
    if (!Number.isFinite(bidPriceMinor) || bidPriceMinor <= 0) {
      setActionError(copy.bidPriceInvalid);
      setActionSuccess(null);
      return;
    }
    setSaving(true);
    setActionError(null);
    setActionSuccess(null);
    try {
      await apiPost<MarketplaceBid>("/api/marketplace/bids", {
        listingId,
        bidPriceMinor: Math.round(bidPriceMinor),
        currency,
      });
      setBidInputs((current) => ({ ...current, [listingId]: "" }));
      setActionSuccess(copy.bidCreated);
      await handleLoadBids(listingId);
    } catch (err) {
      setActionError(formatApiError(err, copy.createBidError));
    } finally {
      setSaving(false);
    }
  }, [bidInputs, copy.bidCreated, copy.bidPriceInvalid, copy.createBidError, handleLoadBids]);

  const handlePurchase = useCallback(async (listingId: string) => {
    const buyerFundId = (purchaseFundByListing[listingId] ?? "").trim();
    setPurchaseListingId(listingId);
    setActionError(null);
    setActionSuccess(null);
    try {
      const order = await apiPost<MarketplaceOrder>("/api/marketplace/purchase", buyerFundId ? {
        listingId,
        buyerFundId,
      } : {
        listingId,
      });
      setActionSuccess(
        (order.buyerFundId ? copy.purchaseSuccessBound : copy.purchaseSuccessInventory).replace("{id}", order.deliveredAgentId),
      );
      await loadMarketplace();
      setSelectedTab("mine");
    } catch (err) {
      setActionError(formatApiError(err, copy.purchaseError));
    } finally {
      setPurchaseListingId(null);
    }
  }, [copy.purchaseError, copy.purchaseSuccessBound, copy.purchaseSuccessInventory, loadMarketplace, purchaseFundByListing]);

  if (loading) {
    return <div className="rounded-lg border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div>;
  }

  if (error) {
    return (
      <div className="rounded-lg border border-red-200 bg-red-50 p-6 text-sm text-red-700">
        <p>{error}</p>
        <button onClick={() => void loadMarketplace()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
          {copy.retry}
        </button>
      </div>
    );
  }

  const visibleListings = selectedTab === "market" ? listings : myListings;
  const visibleHasMore = selectedTab === "market" ? marketHasMore : myHasMore;

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-4 rounded-2xl border border-gray-200 bg-white p-6 shadow-sm lg:flex-row lg:items-start lg:justify-between">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
          <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
        </div>
        <button
          onClick={() => void loadMarketplace()}
          className="rounded-xl border border-gray-200 bg-white px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50"
        >
          {copy.refresh}
        </button>
      </div>

      {actionError ? (
        <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
          <p>{actionError}</p>
          {isKYCErrorMessage(actionError) ? (
            <Link to="/kyc" className="mt-3 inline-flex rounded-lg bg-amber-600 px-3 py-2 text-xs font-semibold text-white hover:bg-amber-700">
              {copy.completeKYC}
            </Link>
          ) : null}
        </div>
      ) : null}
      {actionSuccess ? <div className="rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm text-emerald-700">{actionSuccess}</div> : null}

      <div className="grid grid-cols-1 gap-6 xl:grid-cols-[380px_minmax(0,1fr)]">
        <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
          <h2 className="text-lg font-semibold text-gray-900">{copy.createTitle}</h2>
          <p className="mt-2 text-sm text-gray-500">{copy.createSubtitle}</p>

          <div className="mt-5 space-y-4">
            <div>
              <label className="mb-1 block text-sm font-medium text-gray-700">{copy.sourceFund}</label>
              <select
                value={createForm.fundId}
                disabled={saving}
                onChange={(event) => setCreateForm((current) => ({ ...current, fundId: event.target.value, agentId: "" }))}
                className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
              >
                <option value="">{copy.sourceFundPlaceholder}</option>
                {companyOverviews.map((company) => (
                  <optgroup key={company.id} label={company.name}>
                    {(company.funds ?? []).map((fund) => (
                      <option key={fund.id} value={fund.id}>
                        {fund.name}
                      </option>
                    ))}
                  </optgroup>
                ))}
              </select>
            </div>

            <div>
              <label className="mb-1 block text-sm font-medium text-gray-700">{copy.teamMember}</label>
              <select
                value={createForm.agentId}
                disabled={saving || loadingAgents || !createForm.fundId}
                onChange={(event) => setCreateForm((current) => ({ ...current, agentId: event.target.value }))}
                className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none disabled:cursor-not-allowed disabled:bg-gray-50"
              >
                <option value="">{loadingAgents ? copy.teamMemberLoading : copy.teamMemberPlaceholder}</option>
                {selectedFundAgents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agentDisplayName(agent, language)} · {roleLabel(agent.role, language)}
                  </option>
                ))}
              </select>
            </div>

            <div className="grid grid-cols-2 gap-4">
              <div>
                <label className="mb-1 block text-sm font-medium text-gray-700">{copy.askPrice}</label>
                <input
                  type="number"
                  min="1"
                  step="1"
                  value={createForm.askPriceMinor}
                  disabled={saving}
                  onChange={(event) => setCreateForm((current) => ({ ...current, askPriceMinor: event.target.value }))}
                  className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                  placeholder="10000"
                />
              </div>
              <div>
                <label className="mb-1 block text-sm font-medium text-gray-700">{copy.currency}</label>
                <input
                  value={createForm.currency}
                  disabled={saving}
                  onChange={(event) => setCreateForm((current) => ({ ...current, currency: event.target.value.toUpperCase() }))}
                  className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm uppercase focus:border-indigo-500 focus:outline-none"
                  placeholder="USD"
                />
              </div>
            </div>

            {createForm.agentId && selectedFundAgents.length > 0 ? (
              <div className="rounded-xl border border-indigo-100 bg-indigo-50 px-4 py-3 text-sm text-indigo-900">
                {(() => {
                  const agent = selectedFundAgents.find((item) => item.id === createForm.agentId);
                  if (!agent) {
                    return null;
                  }
                  return (
                    <div className="space-y-1">
                      <p className="font-medium">{copy.sellerPreview} · {agentDisplayName(agent, language)}</p>
                      <p>{roleLabel(agent.role, language)}{agent.focus ? ` · ${agent.focus}` : ""}</p>
                      <p className="text-xs text-indigo-700">{agent.latestLearningSummary?.trim() || copy.previewEmpty}</p>
                    </div>
                  );
                })()}
              </div>
            ) : null}

            <button
              onClick={() => void handleCreateListing()}
              disabled={saving}
              className="w-full rounded-lg bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {saving ? copy.publishing : copy.publish}
            </button>
          </div>
        </div>

        <div className="space-y-4">
          <div className="flex flex-wrap gap-2">
            <button
              onClick={() => setSelectedTab("market")}
              className={`rounded-full px-4 py-2 text-sm font-medium ${selectedTab === "market" ? "bg-indigo-600 text-white" : "bg-white text-gray-600 ring-1 ring-gray-200 hover:bg-gray-50"}`}
            >
              {copy.marketTab}
            </button>
            <button
              onClick={() => setSelectedTab("mine")}
              className={`rounded-full px-4 py-2 text-sm font-medium ${selectedTab === "mine" ? "bg-indigo-600 text-white" : "bg-white text-gray-600 ring-1 ring-gray-200 hover:bg-gray-50"}`}
            >
              {copy.myTab}
            </button>
          </div>

          {visibleListings.length === 0 ? (
            <EmptyState
              kind="marketplace"
              title={selectedTab === "market" ? copy.marketEmpty : copy.myEmpty}
              description={selectedTab === "market" ? copy.marketEmptyDescription : copy.myEmptyDescription}
            />
          ) : (
            <div className="space-y-4">
              {visibleListings.map((listing) => {
                const bids = bidsByListing[listing.id] ?? [];
                const selectedBuyerFundId = purchaseFundByListing[listing.id] ?? "";
                return (
                  <div key={listing.id} className="rounded-2xl border border-gray-200 bg-white p-5 shadow-sm">
                    <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
                      <div className="space-y-2">
                        <div className="flex flex-wrap items-center gap-2">
                          <h2 className="text-lg font-semibold text-gray-900">{listing.agentName}</h2>
                          <span className="rounded-full bg-indigo-50 px-2.5 py-1 text-xs font-medium text-indigo-700">{roleLabel(listing.agentRole, language)}</span>
                          <span className={`rounded-full px-2.5 py-1 text-xs font-medium ${listing.status === "active" ? "bg-emerald-50 text-emerald-700" : "bg-gray-100 text-gray-600"}`}>
                            {statusLabel(listing.status, language)}
                          </span>
                          {listing.trust ? (
                            <span className={`rounded-full px-2.5 py-1 text-xs font-semibold ring-1 ${trustLevelClass(listing.trust.level)}`}>
                              {trustLevelLabel(listing.trust.level, language)} · {listing.trust.score}/100
                            </span>
                          ) : null}
                        </div>
                        <p className="text-sm text-gray-500">{copy.marketSource}：{listing.sourceFundId} · {copy.marketAgent}：{listing.sourceAgentId}</p>
                        <p className="text-sm text-gray-500">{copy.focus}：{listing.agentFocus?.trim() || copy.focusUnset}</p>
                        <p className="text-sm text-gray-700">{listing.latestLearningSummary?.trim() || copy.summaryEmpty}</p>
                        <p className="text-xs text-gray-400">{copy.createdAt}：{formatDateTimeForLanguage(listing.createdAt, language)}</p>
                        {listing.trust ? (
                          <div className="rounded-xl border border-gray-100 bg-gray-50 p-3">
                            <div className="flex flex-wrap gap-2">
                              {(listing.trust.badges ?? []).map((badge) => (
                                <span key={badge} className="rounded-full bg-white px-2 py-1 text-[11px] font-medium text-gray-600 ring-1 ring-gray-200">
                                  {copy.trustBadges[badge] ?? badge}
                                </span>
                              ))}
                            </div>
                            <div className="mt-3 grid gap-2 text-xs text-gray-600 sm:grid-cols-2 lg:grid-cols-4">
                              <span>{copy.learningRecords}: {listing.trust.learningRecords}</span>
                              <span>{copy.publicMemories}: {listing.trust.publicMemoryRecords}</span>
                              <span>{copy.profileCompleteness}: {formatNumberForLanguage(listing.trust.profileCompleteness * 100, language, { maximumFractionDigits: 0 })}%</span>
                              {listing.trust.lastDailyReturn !== undefined ? (
                                <span>{copy.lastReturn}: {formatNumberForLanguage(listing.trust.lastDailyReturn * 100, language, { maximumFractionDigits: 2 })}%</span>
                              ) : null}
                            </div>
                            {(listing.trust.evidence ?? []).length > 0 ? (
                              <details className="mt-2 text-xs text-gray-500">
                                <summary className="cursor-pointer font-medium text-gray-600">{copy.trustEvidence}</summary>
                                <ul className="mt-2 list-disc space-y-1 pl-4">
                                  {(listing.trust.evidence ?? []).map((item, index) => <li key={`${item}-${index}`}>{item}</li>)}
                                </ul>
                              </details>
                            ) : null}
                          </div>
                        ) : null}
                      </div>

                      <div className="min-w-[220px] space-y-3 rounded-xl bg-gray-50 p-4">
                        <div>
                          <p className="text-xs text-gray-500">{copy.directPrice}</p>
                          <p className="mt-1 text-2xl font-bold text-gray-900">
                            {formatMoneyMinorForDisplay(listing.askPriceMinor, listing.currency, displayCurrency, language)}
                          </p>
                        </div>

                        {selectedTab === "mine" ? (
                          <div className="space-y-2">
                            <button
                              onClick={() => void handleLoadBids(listing.id)}
                              disabled={loadingBidListingId === listing.id}
                              className="w-full rounded-lg border border-gray-200 bg-white px-4 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-60"
                            >
                              {loadingBidListingId === listing.id ? copy.loadingBids : `${copy.viewBids}${bids.length ? ` (${bids.length})` : ""}`}
                            </button>
                            {listing.status === "active" ? (
                              <button
                                onClick={() => void handleCancelListing(listing.id)}
                                disabled={saving}
                                className="w-full rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700 disabled:cursor-not-allowed disabled:opacity-60"
                              >
                                {copy.cancel}
                              </button>
                            ) : null}
                          </div>
                        ) : (
                          <div className="space-y-3">
                            <div>
                              <label className="mb-1 block text-xs font-medium text-gray-600">{copy.bidLabel}</label>
                              <div className="flex gap-2">
                                <input
                                  type="number"
                                  min="1"
                                  step="1"
                                  value={bidInputs[listing.id] ?? ""}
                                  onChange={(event) => setBidInputs((current) => ({ ...current, [listing.id]: event.target.value }))}
                                  className="min-w-0 flex-1 rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                                  placeholder="9000"
                                />
                                <button
                                  onClick={() => void handleCreateBid(listing.id, listing.currency)}
                                  disabled={saving || listing.status !== "active"}
                                  className="rounded-lg border border-indigo-200 bg-indigo-50 px-4 py-2 text-sm font-medium text-indigo-700 hover:bg-indigo-100 disabled:cursor-not-allowed disabled:opacity-60"
                                >
                                  {copy.bidButton}
                                </button>
                              </div>
                            </div>

                            <div>
                              <label className="mb-1 block text-xs font-medium text-gray-600">{copy.buyerFund}</label>
                              <select
                                value={selectedBuyerFundId}
                                onChange={(event) => setPurchaseFundByListing((current) => ({ ...current, [listing.id]: event.target.value }))}
                                className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                              >
                                <option value="">{copy.buyToInventory}</option>
                                {companyOverviews.map((company) => (
                                  <optgroup key={company.id} label={company.name}>
                                    {(company.funds ?? []).map((fund) => (
                                      <option key={fund.id} value={fund.id}>{fund.name}</option>
                                    ))}
                                  </optgroup>
                                ))}
                              </select>
                              <p className="mt-1 text-xs text-gray-500">{copy.buyerFundHint}</p>
                            </div>

                            <button
                              onClick={() => void handlePurchase(listing.id)}
                              disabled={purchaseListingId === listing.id || listing.status !== "active"}
                              className="w-full rounded-lg bg-emerald-600 px-4 py-2 text-sm font-medium text-white hover:bg-emerald-700 disabled:cursor-not-allowed disabled:opacity-60"
                            >
                              {purchaseListingId === listing.id ? copy.buying : copy.buyNow}
                            </button>
                          </div>
                        )}
                      </div>
                    </div>

                    {selectedTab === "mine" && bids.length > 0 ? (
                      <div className="mt-4 rounded-xl border border-gray-200 bg-gray-50 p-4">
                        <h3 className="text-sm font-semibold text-gray-900">{copy.bidHistory}</h3>
                        <div className="mt-3 space-y-2">
                          {bids.map((bid) => (
                            <div key={bid.id} className="flex flex-col gap-1 rounded-lg bg-white px-3 py-2 text-sm text-gray-700 sm:flex-row sm:items-center sm:justify-between">
                              <span>{copy.bidder}：{bid.bidderUserId || copy.notAvailable}</span>
                              <span>{formatMoneyMinorForDisplay(bid.bidPriceMinor, bid.currency, displayCurrency, language)}</span>
                              <span className="text-xs text-gray-500">{formatDateTimeForLanguage(bid.createdAt, language)}</span>
                            </div>
                          ))}
                        </div>
                      </div>
                    ) : null}
                  </div>
                );
              })}
              {visibleHasMore ? (
                <div className="flex justify-center pt-2">
                  <button
                    onClick={() => void handleLoadMoreListings()}
                    disabled={loadingMoreTab === selectedTab}
                    className="rounded-xl border border-gray-200 bg-white px-5 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-60"
                  >
                    {loadingMoreTab === selectedTab ? copy.loadingMore : copy.loadMore}
                  </button>
                </div>
              ) : null}
            </div>
          )}
        </div>
      </div>
    </div>
  );
};

export default Marketplace;
