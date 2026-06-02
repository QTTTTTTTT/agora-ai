// BrokerLinksSection — P1-6 web component.
//
// Self-service broker-link management for the fund owner. Sits
// inside FundSettings (per-fund context). Three actions:
//
//   - request a new link (POST /broker-links)
//   - view existing links + their status badges
//   - revoke an active link (POST /broker-links/{id}/revoke)
//
// Approval flow: a freshly-requested link starts in 'pending' and
// only becomes 'active' after a super_admin approves it via the
// admin UI (4-eye check enforced server-side). The fund owner
// sees the status badge change on the next refresh.
//
// Why per-fund (not under AccountSecurity)
//
// Each fund is a routing target — same user can have IBKR for
// fund A and Alpaca for fund B. Putting this UI on the fund
// settings page keeps the mental model local.

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  ApiError,
  formatApiError,
  listBrokerLinks,
  requestBrokerLink,
  revokeBrokerLink,
  type BrokerLinkRow,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Props {
  fundId: string;
  language: Language;
  // Optional: when the parent (FundSettings) detects trading_mode='live'
  // we render the section as expanded by default; otherwise it's
  // a collapsible block to avoid noise on simulation funds.
  defaultExpanded?: boolean;
}

// Closed broker vocabulary mirroring server allowedBrokerIDs.
// Update both sides in lock-step when a new integration ships.
const BROKER_OPTIONS: { id: string; label: string }[] = [
  { id: "ibkr", label: "Interactive Brokers" },
  { id: "futu", label: "Futu" },
  { id: "alpaca", label: "Alpaca" },
  { id: "binance", label: "Binance (crypto)" },
  { id: "mock", label: "Mock (development)" },
];

const messages: Record<Language, {
  title: string;
  subtitle: string;
  expand: string;
  collapse: string;
  empty: string;
  loading: string;
  refresh: string;
  formTitle: string;
  formBroker: string;
  formAccountId: string;
  formAccountIdPlaceholder: string;
  formSubmit: string;
  formSubmitting: string;
  formNote: string;
  errorPrefix: string;
  status: { pending: string; active: string; suspended: string; revoked: string };
  revoke: string;
  revoking: string;
  confirmRevoke: string;
  approvedBy: string;
  approvedAt: string;
  createdAt: string;
}> = {
  "zh-CN": {
    title: "券商账户绑定",
    subtitle:
      "为该基金绑定一个券商账户。新建请求会进入待审批状态，需另一位 super_admin 完成 4-eye 审核后才能用于实盘下单。",
    expand: "展开",
    collapse: "收起",
    empty: "暂无绑定记录",
    loading: "加载中…",
    refresh: "刷新",
    formTitle: "新建绑定请求",
    formBroker: "券商",
    formAccountId: "券商账号",
    formAccountIdPlaceholder: "如 U1234567",
    formSubmit: "提交申请",
    formSubmitting: "提交中…",
    formNote: "提交后请等待管理员 4-eye 审批；已通过的绑定才会被实盘门禁认可。",
    errorPrefix: "操作失败：",
    status: { pending: "待审批", active: "已生效", suspended: "已暂停", revoked: "已注销" },
    revoke: "注销",
    revoking: "注销中…",
    confirmRevoke: "注销该绑定后，实盘下单将被门禁拦截，确定继续？",
    approvedBy: "审批人",
    approvedAt: "审批时间",
    createdAt: "创建时间",
  },
  "en-US": {
    title: "Broker account link",
    subtitle:
      "Link an external broker account to this fund. New requests start as pending and require a different super_admin to approve (4-eye check) before live trading is unlocked.",
    expand: "Expand",
    collapse: "Collapse",
    empty: "No broker links yet",
    loading: "Loading…",
    refresh: "Refresh",
    formTitle: "Submit a new link request",
    formBroker: "Broker",
    formAccountId: "Broker account ID",
    formAccountIdPlaceholder: "e.g. U1234567",
    formSubmit: "Submit request",
    formSubmitting: "Submitting…",
    formNote:
      "After submitting, wait for an admin 4-eye approval. Only approved links count toward the live-trading gate.",
    errorPrefix: "Action failed: ",
    status: {
      pending: "Pending approval",
      active: "Active",
      suspended: "Suspended",
      revoked: "Revoked",
    },
    revoke: "Revoke",
    revoking: "Revoking…",
    confirmRevoke:
      "Revoking will immediately block live cancel/replace until a new link is approved. Continue?",
    approvedBy: "Approved by",
    approvedAt: "Approved at",
    createdAt: "Created at",
  },
};

export default function BrokerLinksSection({ fundId, language, defaultExpanded = false }: Props) {
  const m = messages[language];
  const [expanded, setExpanded] = useState(defaultExpanded);
  const [links, setLinks] = useState<BrokerLinkRow[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [revokingId, setRevokingId] = useState<string | null>(null);
  const [brokerId, setBrokerId] = useState(BROKER_OPTIONS[0].id);
  const [accountId, setAccountId] = useState("");
  const [refreshKey, setRefreshKey] = useState(0);

  const refresh = useCallback(() => setRefreshKey((k) => k + 1), []);

  useEffect(() => {
    if (!expanded || !fundId) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    listBrokerLinks(fundId)
      .then((rows) => {
        if (!cancelled) setLinks(rows);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
          setLinks([]); // not authorised — show empty state, no error
          return;
        }
        setError(formatApiError(err, m.errorPrefix));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [expanded, fundId, refreshKey]);

  const onSubmit = async (event: React.FormEvent) => {
    event.preventDefault();
    if (!accountId.trim()) return;
    setSubmitting(true);
    setError(null);
    try {
      await requestBrokerLink(fundId, { brokerId, accountId: accountId.trim() });
      setAccountId("");
      refresh();
    } catch (err) {
      setError(formatApiError(err, m.errorPrefix));
    } finally {
      setSubmitting(false);
    }
  };

  const onRevoke = async (link: BrokerLinkRow) => {
    if (!window.confirm(m.confirmRevoke)) return;
    setRevokingId(link.id);
    setError(null);
    try {
      await revokeBrokerLink(fundId, link.id);
      refresh();
    } catch (err) {
      setError(formatApiError(err, m.errorPrefix));
    } finally {
      setRevokingId(null);
    }
  };

  // Sort: active first, then pending, then everything else newest-first.
  // Same fund may have multiple historical revoked rows; the
  // operator most cares about active + pending so we surface them.
  const sortedLinks = useMemo(() => {
    if (!links) return [];
    const order = (s: string) => (s === "active" ? 0 : s === "pending" ? 1 : 2);
    return [...links].sort((a, b) => {
      const oa = order(a.status);
      const ob = order(b.status);
      if (oa !== ob) return oa - ob;
      return b.createdAt.localeCompare(a.createdAt);
    });
  }, [links]);

  return (
    <section className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-gray-900">{m.title}</h2>
          <p className="mt-1 text-sm text-gray-500">{m.subtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          className="rounded-lg border border-gray-300 bg-white px-3 py-1.5 text-sm font-medium text-gray-700 hover:bg-gray-50"
        >
          {expanded ? m.collapse : m.expand}
        </button>
      </div>

      {expanded ? (
        <div className="mt-4 space-y-5">
          {error ? (
            <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">{error}</div>
          ) : null}

          <form onSubmit={onSubmit} className="rounded-xl border border-gray-200 bg-gray-50 p-4">
            <h3 className="text-sm font-semibold text-gray-800">{m.formTitle}</h3>
            <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
              <label className="text-sm text-gray-600">
                <span className="mb-1 block">{m.formBroker}</span>
                <select
                  value={brokerId}
                  onChange={(e) => setBrokerId(e.target.value)}
                  className="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
                  disabled={submitting}
                >
                  {BROKER_OPTIONS.map((b) => (
                    <option key={b.id} value={b.id}>
                      {b.label}
                    </option>
                  ))}
                </select>
              </label>
              <label className="text-sm text-gray-600">
                <span className="mb-1 block">{m.formAccountId}</span>
                <input
                  type="text"
                  value={accountId}
                  onChange={(e) => setAccountId(e.target.value)}
                  placeholder={m.formAccountIdPlaceholder}
                  className="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
                  disabled={submitting}
                  required
                />
              </label>
            </div>
            <p className="mt-3 text-xs text-gray-500">{m.formNote}</p>
            <div className="mt-3">
              <button
                type="submit"
                disabled={submitting || !accountId.trim()}
                className="rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {submitting ? m.formSubmitting : m.formSubmit}
              </button>
            </div>
          </form>

          <div>
            <div className="flex items-center justify-between">
              <h3 className="text-sm font-semibold text-gray-800">
                {language === "zh-CN" ? "现有绑定" : "Existing links"}
              </h3>
              <button
                type="button"
                onClick={refresh}
                className="text-sm font-medium text-indigo-600 hover:text-indigo-700"
                disabled={loading}
              >
                {m.refresh}
              </button>
            </div>
            {loading ? (
              <p className="mt-3 text-sm text-gray-500">{m.loading}</p>
            ) : sortedLinks.length === 0 ? (
              <p className="mt-3 text-sm text-gray-500">{m.empty}</p>
            ) : (
              <ul className="mt-3 divide-y divide-gray-200 rounded-xl border border-gray-200 bg-white">
                {sortedLinks.map((link) => (
                  <li key={link.id} className="flex items-center justify-between gap-3 px-4 py-3">
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2">
                        <span className="font-medium text-gray-900">{link.brokerId.toUpperCase()}</span>
                        <StatusBadge status={link.status} label={m.status[link.status]} />
                      </div>
                      <p className="mt-1 font-mono text-sm text-gray-600">{link.accountId}</p>
                      <p className="mt-1 text-xs text-gray-400">
                        {m.createdAt}: {formatTimestamp(link.createdAt)}
                        {link.approvedBy ? ` · ${m.approvedBy}: ${shortenId(link.approvedBy)}` : ""}
                      </p>
                    </div>
                    {link.status === "active" || link.status === "pending" ? (
                      <button
                        type="button"
                        onClick={() => void onRevoke(link)}
                        disabled={revokingId === link.id}
                        className="rounded-lg border border-red-300 bg-white px-3 py-1.5 text-sm font-medium text-red-700 hover:bg-red-50 disabled:cursor-not-allowed disabled:opacity-60"
                      >
                        {revokingId === link.id ? m.revoking : m.revoke}
                      </button>
                    ) : null}
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      ) : null}
    </section>
  );
}

function StatusBadge({ status, label }: { status: BrokerLinkRow["status"]; label: string }) {
  const tone =
    status === "active"
      ? "bg-emerald-100 text-emerald-800"
      : status === "pending"
        ? "bg-amber-100 text-amber-800"
        : status === "suspended"
          ? "bg-orange-100 text-orange-800"
          : "bg-gray-100 text-gray-600";
  return (
    <span className={`whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium ${tone}`}>{label}</span>
  );
}

function formatTimestamp(iso: string): string {
  if (!iso) return "—";
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

function shortenId(uuid: string): string {
  if (!uuid || uuid.length < 8) return uuid;
  return uuid.slice(0, 8);
}
