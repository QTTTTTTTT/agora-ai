// AdminBrinsonCompositionsSection — admin CRUD for the benchmark
// side of the Brinson model (S7 / P3-4).
//
// One composition row per (benchmark_id, dimension, asof). The
// inline form supports adding multiple buckets at once. Validation
// (weights ≈ 1, ASOF parseable, etc.) is enforced server-side; the
// UI surfaces the API error inline.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  deleteAdminBrinsonComposition,
  formatApiError,
  listAdminBrinsonCompositions,
  upsertAdminBrinsonComposition,
  type BrinsonBucket,
  type BrinsonBucketDimension,
  type BrinsonComposition,
} from "../lib/api";
import { ALL_BRINSON_DIMENSIONS } from "@fundai/api-client";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  refresh: string;
  loading: string;
  empty: string;
  error: string;
  upsertTitle: string;
  upsertSubmit: string;
  upsertSubmitting: string;
  deleteButton: string;
  deleteConfirm: string;
  benchmarkId: string;
  dimension: string;
  asof: string;
  note: string;
  buckets: string;
  bucketKey: string;
  bucketWeight: string;
  bucketReturn: string;
  addBucket: string;
  removeBucket: string;
  dimensionAssetClass: string;
  dimensionMarket: string;
  dimensionSector: string;
  dimensionAll: string;
  colBenchmark: string;
  colDimension: string;
  colAsof: string;
  colBucketCount: string;
  colUpdated: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "Brinson 基准成分库",
    panelSubtitle: "维护 brinson_benchmark_compositions 表。每行对应一个 (benchmark_id, dimension, asof)；buckets 数组的 weight 是分数（合计 ≈ 1.0），return_pct 是该桶基准当期收益（如 0.05 = 5%）。",
    refresh: "刷新",
    loading: "加载中…",
    empty: "暂无基准成分，请新增",
    error: "加载失败",
    upsertTitle: "新增 / 更新基准成分",
    upsertSubmit: "保存",
    upsertSubmitting: "保存中…",
    deleteButton: "删除",
    deleteConfirm: "确认删除该基准成分？引用它的归因快照会被级联删除。",
    benchmarkId: "基准 ID (e.g. spx, csi300)",
    dimension: "维度",
    asof: "截至日期",
    note: "备注",
    buckets: "分桶",
    bucketKey: "Key (如 equity / US / hk_equity)",
    bucketWeight: "权重 (0-1)",
    bucketReturn: "收益 (如 0.05 = 5%)",
    addBucket: "添加分桶",
    removeBucket: "移除",
    dimensionAssetClass: "资产类别",
    dimensionMarket: "市场",
    dimensionSector: "行业",
    dimensionAll: "全部维度",
    colBenchmark: "基准",
    colDimension: "维度",
    colAsof: "截至",
    colBucketCount: "分桶数",
    colUpdated: "更新时间",
  },
  "en-US": {
    panelTitle: "Brinson benchmark compositions",
    panelSubtitle: "Maintain the brinson_benchmark_compositions table. One row per (benchmark_id, dimension, asof). Bucket weight is a fraction (sum ≈ 1.0); return_pct is the benchmark's realised return for that bucket (e.g. 0.05 = 5%).",
    refresh: "Refresh",
    loading: "Loading…",
    empty: "No compositions yet — add the first one",
    error: "Failed to load",
    upsertTitle: "Add / update composition",
    upsertSubmit: "Save",
    upsertSubmitting: "Saving…",
    deleteButton: "Delete",
    deleteConfirm: "Delete this composition? Attribution snapshots referencing it will cascade-delete.",
    benchmarkId: "Benchmark ID (e.g. spx, csi300)",
    dimension: "Dimension",
    asof: "As-of date",
    note: "Note",
    buckets: "Buckets",
    bucketKey: "Key (e.g. equity / US / hk_equity)",
    bucketWeight: "Weight (0–1)",
    bucketReturn: "Return (e.g. 0.05 = 5%)",
    addBucket: "Add bucket",
    removeBucket: "Remove",
    dimensionAssetClass: "Asset class",
    dimensionMarket: "Market",
    dimensionSector: "Sector",
    dimensionAll: "All dimensions",
    colBenchmark: "Benchmark",
    colDimension: "Dim.",
    colAsof: "As-of",
    colBucketCount: "Buckets",
    colUpdated: "Updated",
  },
};

interface Props {
  language?: Language;
}

function dimensionLabel(m: Messages, d: BrinsonBucketDimension): string {
  if (d === "asset_class") return m.dimensionAssetClass;
  if (d === "market") return m.dimensionMarket;
  if (d === "sector") return m.dimensionSector;
  return d;
}

function todayISO(): string {
  return new Date().toISOString().slice(0, 10);
}

export default function AdminBrinsonCompositionsSection({ language = "zh-CN" }: Props) {
  const m = useMemo(() => messages[language], [language]);
  const [rows, setRows] = useState<BrinsonComposition[]>([]);
  const [filterDimension, setFilterDimension] = useState<BrinsonBucketDimension | "">("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const [benchmarkId, setBenchmarkId] = useState("");
  const [dimension, setDimension] = useState<BrinsonBucketDimension>("asset_class");
  const [asof, setAsof] = useState(todayISO());
  const [note, setNote] = useState("");
  const [buckets, setBuckets] = useState<BrinsonBucket[]>([
    { key: "equity", weight: 0.6, return_pct: 0.05 },
    { key: "bond", weight: 0.4, return_pct: 0.02 },
  ]);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await listAdminBrinsonCompositions(
        filterDimension ? { dimension: filterDimension } : {},
      );
      setRows(resp.compositions ?? []);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoading(false);
    }
  }, [filterDimension, m.error]);

  useEffect(() => {
    load().catch(() => {});
  }, [load]);

  const submit = useCallback(async () => {
    if (!benchmarkId.trim() || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await upsertAdminBrinsonComposition({
        benchmark_id: benchmarkId.trim(),
        dimension,
        asof,
        buckets,
        note,
      });
      setBenchmarkId("");
      setNote("");
      await load();
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setSubmitting(false);
    }
  }, [benchmarkId, dimension, asof, buckets, note, submitting, m.error, load]);

  const addBucket = useCallback(() => {
    setBuckets((prev) => [...prev, { key: "", weight: 0, return_pct: 0 }]);
  }, []);

  const updateBucket = useCallback((idx: number, patch: Partial<BrinsonBucket>) => {
    setBuckets((prev) => prev.map((b, i) => (i === idx ? { ...b, ...patch } : b)));
  }, []);

  const removeBucket = useCallback((idx: number) => {
    setBuckets((prev) => prev.filter((_, i) => i !== idx));
  }, []);

  const handleDelete = useCallback(
    async (id: string) => {
      if (!window.confirm(m.deleteConfirm)) return;
      try {
        await deleteAdminBrinsonComposition(id);
        await load();
      } catch (err) {
        setError(formatApiError(err, m.error));
      }
    },
    [m.deleteConfirm, m.error, load],
  );

  return (
    <section className="space-y-4 rounded-2xl border border-gray-100 bg-white p-6 shadow-sm">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-base font-semibold text-gray-900">{m.panelTitle}</h2>
          <p className="mt-1 text-sm text-gray-500">{m.panelSubtitle}</p>
        </div>
        <div className="flex items-center gap-2">
          <select
            value={filterDimension}
            onChange={(e) => setFilterDimension(e.target.value as BrinsonBucketDimension | "")}
            className="rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
          >
            <option value="">{m.dimensionAll}</option>
            {ALL_BRINSON_DIMENSIONS.map((d) => (
              <option key={d} value={d}>
                {dimensionLabel(m, d)}
              </option>
            ))}
          </select>
          <button
            type="button"
            onClick={() => load().catch(() => {})}
            className="rounded-md border border-gray-200 px-3 py-1.5 text-xs font-medium text-gray-600 hover:bg-gray-50"
          >
            {m.refresh}
          </button>
        </div>
      </header>

      {error && (
        <div className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          {error}
        </div>
      )}

      <div className="rounded-md border border-gray-100 p-4">
        <h3 className="mb-3 text-sm font-semibold text-gray-800">{m.upsertTitle}</h3>
        <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
          <label className="text-xs text-gray-600">
            <span className="block">{m.benchmarkId}</span>
            <input
              type="text"
              value={benchmarkId}
              onChange={(e) => setBenchmarkId(e.target.value)}
              className="mt-1 w-full rounded-md border border-gray-200 bg-white px-2 py-1 text-sm text-gray-800"
            />
          </label>
          <label className="text-xs text-gray-600">
            <span className="block">{m.dimension}</span>
            <select
              value={dimension}
              onChange={(e) => setDimension(e.target.value as BrinsonBucketDimension)}
              className="mt-1 w-full rounded-md border border-gray-200 bg-white px-2 py-1 text-sm text-gray-800"
            >
              {ALL_BRINSON_DIMENSIONS.map((d) => (
                <option key={d} value={d}>
                  {dimensionLabel(m, d)}
                </option>
              ))}
            </select>
          </label>
          <label className="text-xs text-gray-600">
            <span className="block">{m.asof}</span>
            <input
              type="date"
              value={asof}
              onChange={(e) => setAsof(e.target.value)}
              className="mt-1 w-full rounded-md border border-gray-200 bg-white px-2 py-1 text-sm text-gray-800"
            />
          </label>
          <label className="text-xs text-gray-600 md:col-span-3">
            <span className="block">{m.note}</span>
            <textarea
              value={note}
              onChange={(e) => setNote(e.target.value)}
              rows={2}
              className="mt-1 w-full rounded-md border border-gray-200 bg-white px-2 py-1 text-sm text-gray-800"
            />
          </label>
        </div>

        <div className="mt-4">
          <div className="mb-2 flex items-center justify-between">
            <span className="text-xs font-medium text-gray-700">{m.buckets}</span>
            <button
              type="button"
              onClick={addBucket}
              className="rounded-md border border-gray-200 px-2 py-0.5 text-xs text-gray-600 hover:bg-gray-50"
            >
              + {m.addBucket}
            </button>
          </div>
          <div className="space-y-2">
            {buckets.map((b, idx) => (
              <div
                key={idx}
                className="grid grid-cols-1 gap-2 rounded-md border border-gray-100 bg-gray-50 p-2 md:grid-cols-[2fr_1fr_1fr_auto]"
              >
                <input
                  type="text"
                  value={b.key}
                  onChange={(e) => updateBucket(idx, { key: e.target.value })}
                  placeholder={m.bucketKey}
                  className="rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
                />
                <input
                  type="number"
                  step="0.01"
                  value={b.weight}
                  onChange={(e) => updateBucket(idx, { weight: Number(e.target.value) })}
                  placeholder={m.bucketWeight}
                  className="rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
                />
                <input
                  type="number"
                  step="0.001"
                  value={b.return_pct}
                  onChange={(e) => updateBucket(idx, { return_pct: Number(e.target.value) })}
                  placeholder={m.bucketReturn}
                  className="rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
                />
                <button
                  type="button"
                  onClick={() => removeBucket(idx)}
                  className="rounded-md border border-rose-200 bg-rose-50 px-2 py-1 text-xs text-rose-600 hover:bg-rose-100"
                >
                  {m.removeBucket}
                </button>
              </div>
            ))}
          </div>
        </div>

        <div className="mt-4">
          <button
            type="button"
            disabled={!benchmarkId.trim() || submitting}
            onClick={() => submit().catch(() => {})}
            className="rounded-md border border-blue-200 bg-blue-50 px-3 py-1.5 text-xs font-medium text-blue-700 hover:bg-blue-100 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {submitting ? m.upsertSubmitting : m.upsertSubmit}
          </button>
        </div>
      </div>

      <div>
        {loading && rows.length === 0 ? (
          <p className="rounded-md border border-gray-100 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
            {m.loading}
          </p>
        ) : rows.length === 0 ? (
          <p className="rounded-md border border-gray-100 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
            {m.empty}
          </p>
        ) : (
          <div className="overflow-x-auto rounded-md border border-gray-100">
            <table className="min-w-full text-xs">
              <thead className="bg-gray-50 text-gray-500">
                <tr>
                  <th className="px-3 py-2 text-left">{m.colBenchmark}</th>
                  <th className="px-3 py-2 text-left">{m.colDimension}</th>
                  <th className="px-3 py-2 text-left">{m.colAsof}</th>
                  <th className="px-3 py-2 text-right">{m.colBucketCount}</th>
                  <th className="px-3 py-2 text-left">{m.colUpdated}</th>
                  <th className="px-3 py-2"></th>
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-100">
                {rows.map((r) => (
                  <tr key={r.id}>
                    <td className="px-3 py-2 font-mono text-gray-800">{r.benchmark_id}</td>
                    <td className="px-3 py-2 text-gray-700">{dimensionLabel(m, r.dimension)}</td>
                    <td className="px-3 py-2 font-mono text-gray-700">{r.asof}</td>
                    <td className="px-3 py-2 text-right font-mono text-gray-700">{r.buckets.length}</td>
                    <td className="px-3 py-2 text-gray-500">{new Date(r.updated_at).toLocaleString()}</td>
                    <td className="px-3 py-2 text-right">
                      <button
                        type="button"
                        onClick={() => handleDelete(r.id).catch(() => {})}
                        className="rounded-md border border-rose-200 bg-rose-50 px-2 py-1 text-xs text-rose-600 hover:bg-rose-100"
                      >
                        {m.deleteButton}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </section>
  );
}
