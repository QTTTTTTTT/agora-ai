// AdminLockupSection — admin UI for the S6.3 IPO / restricted-
// share lock-up store.
//
// Capability surface
//
//   - List + filter by fund / instrument / status (active /
//     expired / released).
//   - Create new lock-up (typically called after an IPO
//     allocation lot has been booked).
//   - Edit qty / locked_until / reason / note for active rows.
//   - Early-release with a required reason → audit-logged.
//   - Hard-delete (typo fix); the dialog actively warns the
//     operator that release is the safer path.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  createAdminLockup,
  deleteAdminLockup,
  formatApiError,
  listAdminLockups,
  releaseAdminLockup,
  updateAdminLockup,
  type CreateLockupInput,
  type UpdateLockupInput,
} from "../lib/api";
import type {
  LockupReason,
  LockupRecord,
  LockupStatus,
} from "@fundai/api-client";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  refresh: string;
  listTitle: string;
  listEmpty: string;
  fieldFund: string;
  fieldInstrument: string;
  fieldSymbol: string;
  fieldQty: string;
  fieldUntil: string;
  fieldReason: string;
  fieldNote: string;
  fieldStatus: string;
  fieldSourceLot: string;
  fieldReleasedAt: string;
  fieldReleasedReason: string;
  statusActive: string;
  statusExpired: string;
  statusReleased: string;
  reasonIPO: string;
  reasonPrivatePlacement: string;
  reasonRSU: string;
  reasonRestricted: string;
  reasonEmployeeGrant: string;
  reasonBlockSale: string;
  reasonOther: string;
  filterAll: string;
  createButton: string;
  createDialogTitle: string;
  editButton: string;
  editDialogTitle: string;
  deleteButton: string;
  deleteConfirm: string;
  releaseButton: string;
  releaseDialogTitle: string;
  releaseReasonLabel: string;
  saveButton: string;
  saveSubmitting: string;
  cancelButton: string;
  error: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "S6.3 · IPO / 受限股 lock-up",
    panelSubtitle:
      "为 IPO 配售、定增、RSU、限售股等受限持仓登记锁定期。撮合时若 SELL 数量超过 (持仓 - 活跃锁定 qty)，模拟器会拒单。可在到期前手工提前释放，操作会写入审计。",
    refresh: "刷新",
    listTitle: "Lock-up 记录",
    listEmpty: "暂无 lock-up 记录",
    fieldFund: "基金",
    fieldInstrument: "instrument_key",
    fieldSymbol: "代码",
    fieldQty: "锁定数量",
    fieldUntil: "锁定至",
    fieldReason: "原因",
    fieldNote: "备注",
    fieldStatus: "状态",
    fieldSourceLot: "关联 lot",
    fieldReleasedAt: "提前释放时间",
    fieldReleasedReason: "释放原因",
    statusActive: "生效中",
    statusExpired: "已到期",
    statusReleased: "已提前释放",
    reasonIPO: "IPO 配售",
    reasonPrivatePlacement: "定向增发",
    reasonRSU: "RSU",
    reasonRestricted: "限售股",
    reasonEmployeeGrant: "员工股权",
    reasonBlockSale: "大宗交易锁定",
    reasonOther: "其他",
    filterAll: "全部",
    createButton: "新增",
    createDialogTitle: "登记 Lock-up",
    editButton: "编辑",
    editDialogTitle: "编辑 Lock-up",
    deleteButton: "删除",
    deleteConfirm: "直接删除会丢失审计痕迹，确认要删除而不是「提前释放」吗？",
    releaseButton: "提前释放",
    releaseDialogTitle: "提前释放 Lock-up",
    releaseReasonLabel: "释放原因（必填，会写入审计日志）",
    saveButton: "保存",
    saveSubmitting: "保存中…",
    cancelButton: "取消",
    error: "加载失败",
  },
  "en-US": {
    panelTitle: "S6.3 · IPO / restricted-share lock-up",
    panelSubtitle:
      "Records hold-periods on positions acquired via IPO allocation, private placement, RSU vest, or other restricted channels. The simulator rejects sells whose qty exceeds (position - sum of active locked qty). Operators can early-release with an audited reason.",
    refresh: "Refresh",
    listTitle: "Lock-up records",
    listEmpty: "No lock-up records yet",
    fieldFund: "Fund",
    fieldInstrument: "instrument_key",
    fieldSymbol: "Symbol",
    fieldQty: "Locked qty",
    fieldUntil: "Locked until",
    fieldReason: "Reason",
    fieldNote: "Note",
    fieldStatus: "Status",
    fieldSourceLot: "Source lot",
    fieldReleasedAt: "Released at",
    fieldReleasedReason: "Released reason",
    statusActive: "Active",
    statusExpired: "Expired",
    statusReleased: "Released",
    reasonIPO: "IPO allocation",
    reasonPrivatePlacement: "Private placement",
    reasonRSU: "RSU vest",
    reasonRestricted: "Restricted",
    reasonEmployeeGrant: "Employee grant",
    reasonBlockSale: "Block sale",
    reasonOther: "Other",
    filterAll: "All",
    createButton: "Add",
    createDialogTitle: "Record lock-up",
    editButton: "Edit",
    editDialogTitle: "Edit lock-up",
    deleteButton: "Delete",
    deleteConfirm:
      'Hard-deleting loses the audit trail. Are you sure you want to delete instead of "early release"?',
    releaseButton: "Early release",
    releaseDialogTitle: "Release lock-up early",
    releaseReasonLabel: "Release reason (required, will be audit-logged)",
    saveButton: "Save",
    saveSubmitting: "Saving…",
    cancelButton: "Cancel",
    error: "Failed to load",
  },
};

interface Props {
  language?: Language;
}

interface CreateForm {
  open: boolean;
  fund_id: string;
  instrument_key: string;
  symbol: string;
  locked_qty: string;
  locked_until: string;
  reason: LockupReason;
  source_lot_id: string;
  note: string;
}

const emptyCreate: CreateForm = {
  open: false,
  fund_id: "",
  instrument_key: "",
  symbol: "",
  locked_qty: "",
  locked_until: "",
  reason: "ipo",
  source_lot_id: "",
  note: "",
};

interface EditForm {
  open: boolean;
  id: string;
  locked_qty: string;
  locked_until: string;
  reason: LockupReason;
  note: string;
}

const emptyEdit: EditForm = {
  open: false,
  id: "",
  locked_qty: "",
  locked_until: "",
  reason: "ipo",
  note: "",
};

interface ReleaseForm {
  open: boolean;
  id: string;
  reason: string;
}

const emptyRelease: ReleaseForm = { open: false, id: "", reason: "" };

export function AdminLockupSection({ language = "zh-CN" }: Props) {
  const m = useMemo<Messages>(
    () => messages[language] ?? messages["zh-CN"],
    [language],
  );

  const [rows, setRows] = useState<LockupRecord[]>([]);
  const [filterFund, setFilterFund] = useState("");
  const [filterInstrument, setFilterInstrument] = useState("");
  const [filterStatus, setFilterStatus] = useState<LockupStatus | "">("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const [createForm, setCreateForm] = useState<CreateForm>(emptyCreate);
  const [editForm, setEditForm] = useState<EditForm>(emptyEdit);
  const [releaseForm, setReleaseForm] = useState<ReleaseForm>(emptyRelease);
  const [submitting, setSubmitting] = useState(false);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await listAdminLockups({
        fundId: filterFund || undefined,
        instrumentKey: filterInstrument || undefined,
        status: filterStatus || undefined,
      });
      setRows(res.lockups);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoading(false);
    }
  }, [filterFund, filterInstrument, filterStatus, m.error]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const reasonLabel = (r: LockupReason): string => {
    switch (r) {
      case "ipo":
        return m.reasonIPO;
      case "private_placement":
        return m.reasonPrivatePlacement;
      case "rsu":
        return m.reasonRSU;
      case "restricted":
        return m.reasonRestricted;
      case "employee_grant":
        return m.reasonEmployeeGrant;
      case "block_sale":
        return m.reasonBlockSale;
      case "other":
        return m.reasonOther;
      default:
        return r;
    }
  };

  const statusLabel = (s: LockupStatus): string => {
    switch (s) {
      case "active":
        return m.statusActive;
      case "expired":
        return m.statusExpired;
      case "released":
        return m.statusReleased;
      default:
        return s;
    }
  };

  const handleCreate = async () => {
    setSubmitting(true);
    setError(null);
    try {
      const input: CreateLockupInput = {
        fund_id: createForm.fund_id.trim(),
        instrument_key: createForm.instrument_key.trim(),
        symbol: createForm.symbol.trim(),
        locked_qty: Number(createForm.locked_qty) || 0,
        locked_until: createForm.locked_until.trim(),
        reason: createForm.reason,
        source_lot_id: createForm.source_lot_id.trim() || undefined,
        note: createForm.note.trim() || undefined,
      };
      await createAdminLockup(input);
      setCreateForm(emptyCreate);
      await reload();
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setSubmitting(false);
    }
  };

  const handleEdit = async () => {
    setSubmitting(true);
    setError(null);
    try {
      const input: UpdateLockupInput = {};
      if (editForm.locked_qty.trim() !== "") {
        input.locked_qty = Number(editForm.locked_qty) || 0;
      }
      if (editForm.locked_until.trim() !== "") {
        input.locked_until = editForm.locked_until.trim();
      }
      input.reason = editForm.reason;
      if (editForm.note.trim() !== "") {
        input.note = editForm.note.trim();
      }
      await updateAdminLockup(editForm.id, input);
      setEditForm(emptyEdit);
      await reload();
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setSubmitting(false);
    }
  };

  const handleRelease = async () => {
    if (!releaseForm.reason.trim()) return;
    setSubmitting(true);
    setError(null);
    try {
      await releaseAdminLockup(releaseForm.id, releaseForm.reason);
      setReleaseForm(emptyRelease);
      await reload();
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setSubmitting(false);
    }
  };

  const handleDelete = async (id: string) => {
    if (!window.confirm(m.deleteConfirm)) return;
    setError(null);
    try {
      await deleteAdminLockup(id);
      await reload();
    } catch (err) {
      setError(formatApiError(err, m.error));
    }
  };

  return (
    <section className="admin-section admin-lockup">
      <header className="admin-section__header">
        <div>
          <h2>{m.panelTitle}</h2>
          <p className="admin-section__subtitle">{m.panelSubtitle}</p>
        </div>
        <div className="admin-section__actions">
          <button type="button" onClick={() => void reload()} disabled={loading}>
            {m.refresh}
          </button>
          <button
            type="button"
            onClick={() => setCreateForm({ ...emptyCreate, open: true })}
          >
            {m.createButton}
          </button>
        </div>
      </header>

      {error && <div className="admin-section__error">{error}</div>}

      <div className="admin-section__filters">
        <input
          placeholder={m.fieldFund}
          value={filterFund}
          onChange={(e) => setFilterFund(e.target.value)}
        />
        <input
          placeholder={m.fieldInstrument}
          value={filterInstrument}
          onChange={(e) => setFilterInstrument(e.target.value)}
        />
        <select
          value={filterStatus}
          onChange={(e) => setFilterStatus(e.target.value as LockupStatus | "")}
        >
          <option value="">{m.filterAll}</option>
          <option value="active">{m.statusActive}</option>
          <option value="expired">{m.statusExpired}</option>
          <option value="released">{m.statusReleased}</option>
        </select>
      </div>

      <h3>{m.listTitle}</h3>
      {rows.length === 0 ? (
        <p className="admin-section__empty">{m.listEmpty}</p>
      ) : (
        <div className="admin-table-wrap">
          <table className="admin-table">
            <thead>
              <tr>
                <th>{m.fieldFund}</th>
                <th>{m.fieldSymbol}</th>
                <th>{m.fieldQty}</th>
                <th>{m.fieldUntil}</th>
                <th>{m.fieldReason}</th>
                <th>{m.fieldStatus}</th>
                <th>{m.fieldNote}</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.id}>
                  <td>{shortFundID(r.fund_id)}</td>
                  <td>{r.symbol}</td>
                  <td>{formatNumber(r.locked_qty)}</td>
                  <td>{formatTimestamp(r.locked_until)}</td>
                  <td>{reasonLabel(r.reason)}</td>
                  <td>{statusLabel(r.status)}</td>
                  <td>{r.note ?? ""}</td>
                  <td>
                    {r.status === "active" && (
                      <>
                        <button
                          type="button"
                          onClick={() =>
                            setEditForm({
                              open: true,
                              id: r.id,
                              locked_qty: String(r.locked_qty),
                              locked_until: r.locked_until,
                              reason: r.reason,
                              note: r.note ?? "",
                            })
                          }
                        >
                          {m.editButton}
                        </button>{" "}
                        <button
                          type="button"
                          onClick={() =>
                            setReleaseForm({ open: true, id: r.id, reason: "" })
                          }
                        >
                          {m.releaseButton}
                        </button>{" "}
                      </>
                    )}
                    <button type="button" onClick={() => void handleDelete(r.id)}>
                      {m.deleteButton}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {createForm.open && (
        <div className="admin-modal">
          <div className="admin-modal__inner">
            <h3>{m.createDialogTitle}</h3>
            <div className="admin-grid">
              <label>
                <span>{m.fieldFund}</span>
                <input
                  value={createForm.fund_id}
                  onChange={(e) =>
                    setCreateForm({ ...createForm, fund_id: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldInstrument}</span>
                <input
                  value={createForm.instrument_key}
                  onChange={(e) =>
                    setCreateForm({
                      ...createForm,
                      instrument_key: e.target.value,
                    })
                  }
                />
              </label>
              <label>
                <span>{m.fieldSymbol}</span>
                <input
                  value={createForm.symbol}
                  onChange={(e) =>
                    setCreateForm({ ...createForm, symbol: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldQty}</span>
                <input
                  value={createForm.locked_qty}
                  onChange={(e) =>
                    setCreateForm({ ...createForm, locked_qty: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldUntil}</span>
                <input
                  placeholder="2026-12-01T00:00:00Z"
                  value={createForm.locked_until}
                  onChange={(e) =>
                    setCreateForm({
                      ...createForm,
                      locked_until: e.target.value,
                    })
                  }
                />
              </label>
              <label>
                <span>{m.fieldReason}</span>
                <select
                  value={createForm.reason}
                  onChange={(e) =>
                    setCreateForm({
                      ...createForm,
                      reason: e.target.value as LockupReason,
                    })
                  }
                >
                  <option value="ipo">{m.reasonIPO}</option>
                  <option value="private_placement">
                    {m.reasonPrivatePlacement}
                  </option>
                  <option value="rsu">{m.reasonRSU}</option>
                  <option value="restricted">{m.reasonRestricted}</option>
                  <option value="employee_grant">{m.reasonEmployeeGrant}</option>
                  <option value="block_sale">{m.reasonBlockSale}</option>
                  <option value="other">{m.reasonOther}</option>
                </select>
              </label>
              <label>
                <span>{m.fieldSourceLot}</span>
                <input
                  value={createForm.source_lot_id}
                  onChange={(e) =>
                    setCreateForm({
                      ...createForm,
                      source_lot_id: e.target.value,
                    })
                  }
                />
              </label>
              <label className="admin-grid__full">
                <span>{m.fieldNote}</span>
                <input
                  value={createForm.note}
                  onChange={(e) =>
                    setCreateForm({ ...createForm, note: e.target.value })
                  }
                />
              </label>
            </div>
            <div className="admin-modal__actions">
              <button
                type="button"
                onClick={() => setCreateForm(emptyCreate)}
                disabled={submitting}
              >
                {m.cancelButton}
              </button>
              <button
                type="button"
                onClick={() => void handleCreate()}
                disabled={submitting}
              >
                {submitting ? m.saveSubmitting : m.saveButton}
              </button>
            </div>
          </div>
        </div>
      )}

      {editForm.open && (
        <div className="admin-modal">
          <div className="admin-modal__inner">
            <h3>{m.editDialogTitle}</h3>
            <div className="admin-grid">
              <label>
                <span>{m.fieldQty}</span>
                <input
                  value={editForm.locked_qty}
                  onChange={(e) =>
                    setEditForm({ ...editForm, locked_qty: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldUntil}</span>
                <input
                  placeholder="2026-12-01T00:00:00Z"
                  value={editForm.locked_until}
                  onChange={(e) =>
                    setEditForm({ ...editForm, locked_until: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldReason}</span>
                <select
                  value={editForm.reason}
                  onChange={(e) =>
                    setEditForm({
                      ...editForm,
                      reason: e.target.value as LockupReason,
                    })
                  }
                >
                  <option value="ipo">{m.reasonIPO}</option>
                  <option value="private_placement">
                    {m.reasonPrivatePlacement}
                  </option>
                  <option value="rsu">{m.reasonRSU}</option>
                  <option value="restricted">{m.reasonRestricted}</option>
                  <option value="employee_grant">{m.reasonEmployeeGrant}</option>
                  <option value="block_sale">{m.reasonBlockSale}</option>
                  <option value="other">{m.reasonOther}</option>
                </select>
              </label>
              <label className="admin-grid__full">
                <span>{m.fieldNote}</span>
                <input
                  value={editForm.note}
                  onChange={(e) =>
                    setEditForm({ ...editForm, note: e.target.value })
                  }
                />
              </label>
            </div>
            <div className="admin-modal__actions">
              <button
                type="button"
                onClick={() => setEditForm(emptyEdit)}
                disabled={submitting}
              >
                {m.cancelButton}
              </button>
              <button
                type="button"
                onClick={() => void handleEdit()}
                disabled={submitting}
              >
                {submitting ? m.saveSubmitting : m.saveButton}
              </button>
            </div>
          </div>
        </div>
      )}

      {releaseForm.open && (
        <div className="admin-modal">
          <div className="admin-modal__inner">
            <h3>{m.releaseDialogTitle}</h3>
            <div className="admin-grid">
              <label className="admin-grid__full">
                <span>{m.releaseReasonLabel}</span>
                <input
                  value={releaseForm.reason}
                  onChange={(e) =>
                    setReleaseForm({ ...releaseForm, reason: e.target.value })
                  }
                />
              </label>
            </div>
            <div className="admin-modal__actions">
              <button
                type="button"
                onClick={() => setReleaseForm(emptyRelease)}
                disabled={submitting}
              >
                {m.cancelButton}
              </button>
              <button
                type="button"
                onClick={() => void handleRelease()}
                disabled={submitting || !releaseForm.reason.trim()}
              >
                {submitting ? m.saveSubmitting : m.saveButton}
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}

function formatNumber(n: number | null | undefined): string {
  if (n === null || n === undefined) return "—";
  if (Math.abs(n) >= 1e6) return n.toExponential(2);
  return String(n);
}

function formatTimestamp(s: string | null | undefined): string {
  if (!s) return "—";
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return s;
  return d.toLocaleString();
}

function shortFundID(id: string): string {
  if (!id) return "—";
  if (id.length <= 12) return id;
  return `${id.slice(0, 8)}…`;
}

export default AdminLockupSection;
