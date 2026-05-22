import React, { useCallback, useEffect, useMemo, useState } from "react";
import { apiGet, formatApiError } from "../lib/api";
import { formatDateTimeForLanguage, formatMoneyMinorForDisplay, useAppPreferences } from "../lib/preferences";

interface WalletAccount {
  id?: string;
  user_id: string;
  base_currency: string;
  balance_minor: number;
  created_at?: string;
  updated_at?: string;
}

interface WalletLedgerEntry {
  id: string;
  account_id: string;
  entry_type: string;
  amount_minor: number;
  balance_after_minor: number;
  currency: string;
  reference_type?: string;
  reference_id?: string;
  created_by_user_id?: string;
  metadata?: Record<string, unknown> | null;
  created_at: string;
}

interface WalletResponse {
  wallet: WalletAccount;
}

interface WalletLedgerResponse {
  entries: WalletLedgerEntry[];
  total: number;
  offset: number;
  limit: number;
}

function entryLabel(value: string, language: "zh-CN" | "en-US"): string {
  const labels: Record<string, { zh: string; en: string }> = {
    recharge: { zh: "充值入账", en: "Recharge" },
    marketplace_purchase: { zh: "市场购买", en: "Marketplace purchase" },
    marketplace_sale: { zh: "市场售出", en: "Marketplace sale" },
  };
  const matched = labels[value];
  if (matched) {
    return language === "en-US" ? matched.en : matched.zh;
  }
  return value.replace(/[_-]+/g, " ");
}

const Wallet: React.FC = () => {
  const { language, displayCurrency } = useAppPreferences();
  const [wallet, setWallet] = useState<WalletAccount | null>(null);
  const [ledger, setLedger] = useState<WalletLedgerEntry[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "My wallet",
            loading: "Loading wallet balance and ledger...",
            loadError: "Failed to load wallet",
            retry: "Retry",
            subtitle: "View your current balance and recent recharge and settlement entries. The underlying ledger still settles in USD.",
            refresh: "Refresh",
            balance: "Current balance",
            ledgerCurrency: "Ledger currency",
            displayCurrencyLabel: "Display currency",
            entries: "Ledger entries",
            ledgerTitle: "Ledger details",
            emptyLedger: "There are no wallet entries yet.",
            time: "Time",
            type: "Type",
            amount: "Amount",
            balanceAfter: "Balance",
            reference: "Reference",
            convertedHint: "Shown in {currency}; ledger settlement remains in {baseCurrency}.",
          }
        : {
            title: "我的钱包",
            loading: "正在加载钱包余额与流水...",
            loadError: "加载钱包失败",
            retry: "重试",
            subtitle: "查看当前余额和最近的充值、结算流水。底层账本仍按美元结算。",
            refresh: "刷新",
            balance: "当前余额",
            ledgerCurrency: "账本币种",
            displayCurrencyLabel: "显示币种",
            entries: "流水条数",
            ledgerTitle: "流水明细",
            emptyLedger: "当前还没有钱包流水。",
            time: "时间",
            type: "类型",
            amount: "金额",
            balanceAfter: "余额",
            reference: "引用",
            convertedHint: "当前按 {currency} 展示，底层账本结算币种仍为 {baseCurrency}。",
          },
    [language],
  );

  const loadData = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [walletRes, ledgerRes] = await Promise.all([
        apiGet<WalletResponse>("/api/wallet"),
        apiGet<WalletLedgerResponse>("/api/wallet/ledger?limit=50"),
      ]);
      setWallet(walletRes.wallet);
      setLedger(ledgerRes.entries ?? []);
      setTotal(ledgerRes.total ?? 0);
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError]);

  useEffect(() => {
    void loadData();
  }, [loadData]);

  const baseCurrency = wallet?.base_currency || "USD";
  const balanceMinor = wallet?.balance_minor ?? 0;
  const convertedHint = copy.convertedHint
    .replace("{currency}", displayCurrency)
    .replace("{baseCurrency}", baseCurrency);

  if (loading) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <div className="rounded-lg border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <div className="rounded-lg border border-red-200 bg-red-50 p-6 text-sm text-red-700">
          <p>{error}</p>
          <button onClick={() => void loadData()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
            {copy.retry}
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <div className="flex flex-col gap-4 lg:flex-row lg:items-end lg:justify-between">
          <div>
            <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
            <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
            <p className="mt-2 text-xs text-gray-400">{convertedHint}</p>
          </div>
          <button
            onClick={() => void loadData()}
            className="rounded-xl border border-gray-200 bg-white px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50"
          >
            {copy.refresh}
          </button>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-4 md:grid-cols-4">
        <div className="rounded-xl border border-emerald-200 bg-emerald-50 p-5 shadow-sm">
          <p className="text-sm text-emerald-700">{copy.balance}</p>
          <p className="mt-3 text-3xl font-bold text-emerald-900">
            {formatMoneyMinorForDisplay(balanceMinor, baseCurrency, displayCurrency, language)}
          </p>
        </div>
        <div className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
          <p className="text-sm text-gray-500">{copy.ledgerCurrency}</p>
          <p className="mt-3 text-2xl font-bold text-gray-900">{baseCurrency}</p>
        </div>
        <div className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
          <p className="text-sm text-gray-500">{copy.displayCurrencyLabel}</p>
          <p className="mt-3 text-2xl font-bold text-gray-900">{displayCurrency}</p>
        </div>
        <div className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
          <p className="text-sm text-gray-500">{copy.entries}</p>
          <p className="mt-3 text-2xl font-bold text-gray-900">{total}</p>
        </div>
      </div>

      <div className="rounded-2xl border border-gray-200 bg-white shadow-sm">
        <div className="border-b border-gray-200 px-6 py-4">
          <h2 className="text-lg font-semibold text-gray-900">{copy.ledgerTitle}</h2>
        </div>
        {ledger.length === 0 ? (
          <div className="p-6 text-sm text-gray-500">{copy.emptyLedger}</div>
        ) : (
          <div className="overflow-x-auto">
            <table className="min-w-full divide-y divide-gray-200 text-sm">
              <thead className="bg-gray-50">
                <tr>
                  <th className="px-6 py-3 text-left font-medium text-gray-500">{copy.time}</th>
                  <th className="px-6 py-3 text-left font-medium text-gray-500">{copy.type}</th>
                  <th className="px-6 py-3 text-left font-medium text-gray-500">{copy.amount}</th>
                  <th className="px-6 py-3 text-left font-medium text-gray-500">{copy.balanceAfter}</th>
                  <th className="px-6 py-3 text-left font-medium text-gray-500">{copy.reference}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-100 bg-white">
                {ledger.map((entry) => (
                  <tr key={entry.id}>
                    <td className="px-6 py-4 text-gray-600">{formatDateTimeForLanguage(entry.created_at, language)}</td>
                    <td className="px-6 py-4 text-gray-900">{entryLabel(entry.entry_type, language)}</td>
                    <td className="px-6 py-4 font-medium text-gray-900">
                      {formatMoneyMinorForDisplay(entry.amount_minor, entry.currency, displayCurrency, language)}
                    </td>
                    <td className="px-6 py-4 text-gray-600">
                      {formatMoneyMinorForDisplay(entry.balance_after_minor, entry.currency, displayCurrency, language)}
                    </td>
                    <td className="px-6 py-4 text-xs text-gray-500">
                      {entry.reference_type || entry.reference_id ? `${entry.reference_type || "-"} / ${entry.reference_id || "-"}` : "-"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
};

export default Wallet;
