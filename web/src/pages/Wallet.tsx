import React, { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
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
  // W4-26 — react-i18next migration. Catalog lives in
  // web/src/i18n/locales/{en-US,zh-CN}/wallet.ts.
  const { t } = useTranslation("wallet");
  const [wallet, setWallet] = useState<WalletAccount | null>(null);
  const [ledger, setLedger] = useState<WalletLedgerEntry[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

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
      setError(formatApiError(err, t("loadError")));
    } finally {
      setLoading(false);
    }
  }, [t]);

  useEffect(() => {
    void loadData();
  }, [loadData]);

  const baseCurrency = wallet?.base_currency || "USD";
  const balanceMinor = wallet?.balance_minor ?? 0;
  const convertedHint = t("convertedHint", {
    currency: displayCurrency,
    baseCurrency,
  });

  if (loading) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-bold text-gray-900">{t("title")}</h1>
        <div className="rounded-lg border border-gray-200 bg-white p-6 text-sm text-gray-500">{t("loading")}</div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-bold text-gray-900">{t("title")}</h1>
        <div className="rounded-lg border border-red-200 bg-red-50 p-6 text-sm text-red-700">
          <p>{error}</p>
          <button onClick={() => void loadData()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
            {t("retry")}
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
            <h1 className="text-2xl font-bold text-gray-900">{t("title")}</h1>
            <p className="mt-2 text-sm text-gray-500">{t("subtitle")}</p>
            <p className="mt-2 text-xs text-gray-400">{convertedHint}</p>
          </div>
          <button
            onClick={() => void loadData()}
            className="rounded-xl border border-gray-200 bg-white px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50"
          >
            {t("refresh")}
          </button>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-4 md:grid-cols-4">
        <div className="rounded-xl border border-emerald-200 bg-emerald-50 p-5 shadow-sm">
          <p className="text-sm text-emerald-700">{t("balance")}</p>
          <p className="mt-3 text-3xl font-bold text-emerald-900">
            {formatMoneyMinorForDisplay(balanceMinor, baseCurrency, displayCurrency, language)}
          </p>
        </div>
        <div className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
          <p className="text-sm text-gray-500">{t("ledgerCurrency")}</p>
          <p className="mt-3 text-2xl font-bold text-gray-900">{baseCurrency}</p>
        </div>
        <div className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
          <p className="text-sm text-gray-500">{t("displayCurrencyLabel")}</p>
          <p className="mt-3 text-2xl font-bold text-gray-900">{displayCurrency}</p>
        </div>
        <div className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
          <p className="text-sm text-gray-500">{t("entries")}</p>
          <p className="mt-3 text-2xl font-bold text-gray-900">{total}</p>
        </div>
      </div>

      <div className="rounded-2xl border border-gray-200 bg-white shadow-sm">
        <div className="border-b border-gray-200 px-6 py-4">
          <h2 className="text-lg font-semibold text-gray-900">{t("ledgerTitle")}</h2>
        </div>
        {ledger.length === 0 ? (
          <div className="p-6 text-sm text-gray-500">{t("emptyLedger")}</div>
        ) : (
          <div className="overflow-x-auto">
            <table className="min-w-full divide-y divide-gray-200 text-sm">
              <thead className="bg-gray-50">
                <tr>
                  <th className="px-6 py-3 text-left font-medium text-gray-500">{t("time")}</th>
                  <th className="px-6 py-3 text-left font-medium text-gray-500">{t("type")}</th>
                  <th className="px-6 py-3 text-left font-medium text-gray-500">{t("amount")}</th>
                  <th className="px-6 py-3 text-left font-medium text-gray-500">{t("balanceAfter")}</th>
                  <th className="px-6 py-3 text-left font-medium text-gray-500">{t("reference")}</th>
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
