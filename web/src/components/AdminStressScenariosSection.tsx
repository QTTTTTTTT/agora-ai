// AdminStressScenariosSection — admin CRUD for stress scenarios
// (S7 / P3-3). Compact list + inline upsert + per-row delete.
//
// Scenarios are typically created by hand for historical events
// ("2008 Lehman", "COVID Mar 2020") or for regulatory standards;
// the inline form supports adding multiple shocks at once. A
// real "scenario designer" with charts and previews can land in
// a later sprint — for now we ship the minimum admin surface.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  formatApiError,
  listAdminStressScenarios,
  upsertAdminStressScenario,
  deleteAdminStressScenario,
  type StressCategory,
  type StressScenario,
  type StressShock,
  type StressShockTargetType,
} from "../lib/api";
import {
  ALL_STRESS_CATEGORIES,
  ALL_STRESS_TARGET_TYPES,
} from "@fundai/api-client";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  refresh: string;
  loading: string;
  empty: string;
  error: string;
  scenarioName: string;
  scenarioCategory: string;
  scenarioDescription: string;
  scenarioShocks: string;
  scenarioCreatedBy: string;
  scenarioUpdatedAt: string;
  upsertTitle: string;
  upsertSubmit: string;
  upsertSubmitting: string;
  deleteButton: string;
  deleteConfirm: string;
  targetInstrument: string;
  targetMarket: string;
  targetAssetClass: string;
  targetFactor: string;
  targetWildcard: string;
  shockTargetType: string;
  shockTargetKey: string;
  shockValue: string;
  addShock: string;
  removeShock: string;
  categoryHistorical: string;
  categoryHypothetical: string;
  categoryRegulatory: string;
  categoryAll: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "压力情景库",
    panelSubtitle: "维护 stress_scenarios 表。情景定义里的 shock 数组按 instrument > market > asset_class > factor > wildcard 的特异性匹配持仓；factor 类冲击会与 instrument_factor_loadings 复合相加。",
    refresh: "刷新",
    loading: "加载中…",
    empty: "暂无情景，请新增",
    error: "加载失败",
    scenarioName: "名称",
    scenarioCategory: "类别",
    scenarioDescription: "说明",
    scenarioShocks: "冲击",
    scenarioCreatedBy: "作者",
    scenarioUpdatedAt: "更新时间",
    upsertTitle: "新增 / 更新场景",
    upsertSubmit: "保存",
    upsertSubmitting: "保存中…",
    deleteButton: "删除",
    deleteConfirm: "确认删除该场景？历史结果会被级联删除。",
    targetInstrument: "标的级",
    targetMarket: "市场级",
    targetAssetClass: "资产类别级",
    targetFactor: "因子级",
    targetWildcard: "通配",
    shockTargetType: "类型",
    shockTargetKey: "目标键",
    shockValue: "冲击值 (小数 ; -0.20 = -20%)",
    addShock: "添加一条冲击",
    removeShock: "移除",
    categoryHistorical: "历史复刻",
    categoryHypothetical: "假设情景",
    categoryRegulatory: "监管标准",
    categoryAll: "全部类别",
  },
  "en-US": {
    panelTitle: "Stress scenario library",
    panelSubtitle: "Maintain the stress_scenarios table. Shocks match holdings by specificity (instrument > market > asset_class > factor > wildcard); factor shocks combine additively with instrument_factor_loadings.",
    refresh: "Refresh",
    loading: "Loading…",
    empty: "No scenarios on file — add the first one",
    error: "Failed to load",
    scenarioName: "Name",
    scenarioCategory: "Category",
    scenarioDescription: "Description",
    scenarioShocks: "Shocks",
    scenarioCreatedBy: "Author",
    scenarioUpdatedAt: "Updated",
    upsertTitle: "Add / update scenario",
    upsertSubmit: "Save",
    upsertSubmitting: "Saving…",
    deleteButton: "Delete",
    deleteConfirm: "Delete this scenario? Historical results will cascade-delete.",
    targetInstrument: "Instrument",
    targetMarket: "Market",
    targetAssetClass: "Asset class",
    targetFactor: "Factor",
    targetWildcard: "Wildcard",
    shockTargetType: "Type",
    shockTargetKey: "Target key",
    shockValue: "Value (decimal; -0.20 = -20%)",
    addShock: "Add shock",
    removeShock: "Remove",
    categoryHistorical: "Historical",
    categoryHypothetical: "Hypothetical",
    categoryRegulatory: "Regulatory",
    categoryAll: "All categories",
  },
};

interface Props {
  language?: Language;
}

function categoryLabel(m: Messages, c: StressCategory): string {
  if (c === "historical") return m.categoryHistorical;
  if (c === "hypothetical") return m.categoryHypothetical;
  if (c === "regulatory") return m.categoryRegulatory;
  return c;
}

function targetLabel(m: Messages, t: StressShockTargetType): string {
  if (t === "instrument") return m.targetInstrument;
  if (t === "market") return m.targetMarket;
  if (t === "asset_class") return m.targetAssetClass;
  if (t === "factor") return m.targetFactor;
  if (t === "wildcard") return m.targetWildcard;
  return t;
}

export default function AdminStressScenariosSection({ language = "zh-CN" }: Props) {
  const m = useMemo(() => messages[language], [language]);
  const [scenarios, setScenarios] = useState<StressScenario[]>([]);
  const [filterCategory, setFilterCategory] = useState<StressCategory | "">("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const [name, setName] = useState("");
  const [category, setCategory] = useState<StressCategory>("historical");
  const [description, setDescription] = useState("");
  const [shocks, setShocks] = useState<StressShock[]>([
    { target_type: "wildcard", target_key: "*", value: -0.10 },
  ]);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await listAdminStressScenarios(filterCategory ? { category: filterCategory } : {});
      setScenarios(resp.scenarios ?? []);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoading(false);
    }
  }, [filterCategory, m.error]);

  useEffect(() => {
    load().catch(() => {});
  }, [load]);

  const submit = useCallback(async () => {
    if (!name.trim() || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await upsertAdminStressScenario({
        name: name.trim(),
        category,
        description,
        shocks,
      });
      setName("");
      setDescription("");
      setShocks([{ target_type: "wildcard", target_key: "*", value: -0.10 }]);
      await load();
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setSubmitting(false);
    }
  }, [name, category, description, shocks, submitting, m.error, load]);

  const removeShock = useCallback((idx: number) => {
    setShocks((prev) => prev.filter((_, i) => i !== idx));
  }, []);

  const updateShock = useCallback((idx: number, patch: Partial<StressShock>) => {
    setShocks((prev) => prev.map((s, i) => (i === idx ? { ...s, ...patch } : s)));
  }, []);

  const addShock = useCallback(() => {
    setShocks((prev) => [...prev, { target_type: "asset_class", target_key: "equity", value: -0.10 }]);
  }, []);

  const handleDelete = useCallback(
    async (id: string) => {
      if (!window.confirm(m.deleteConfirm)) return;
      try {
        await deleteAdminStressScenario(id);
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
            value={filterCategory}
            onChange={(e) => setFilterCategory(e.target.value as StressCategory | "")}
            className="rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
          >
            <option value="">{m.categoryAll}</option>
            {ALL_STRESS_CATEGORIES.map((c) => (
              <option key={c} value={c}>
                {categoryLabel(m, c)}
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
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
          <label className="text-xs text-gray-600">
            <span className="block">{m.scenarioName}</span>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              className="mt-1 w-full rounded-md border border-gray-200 bg-white px-2 py-1 text-sm text-gray-800"
            />
          </label>
          <label className="text-xs text-gray-600">
            <span className="block">{m.scenarioCategory}</span>
            <select
              value={category}
              onChange={(e) => setCategory(e.target.value as StressCategory)}
              className="mt-1 w-full rounded-md border border-gray-200 bg-white px-2 py-1 text-sm text-gray-800"
            >
              {ALL_STRESS_CATEGORIES.map((c) => (
                <option key={c} value={c}>
                  {categoryLabel(m, c)}
                </option>
              ))}
            </select>
          </label>
          <label className="text-xs text-gray-600 md:col-span-2">
            <span className="block">{m.scenarioDescription}</span>
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={3}
              className="mt-1 w-full rounded-md border border-gray-200 bg-white px-2 py-1 text-sm text-gray-800"
            />
          </label>
        </div>

        <div className="mt-4">
          <div className="mb-2 flex items-center justify-between">
            <span className="text-xs font-medium text-gray-700">{m.scenarioShocks}</span>
            <button
              type="button"
              onClick={addShock}
              className="rounded-md border border-gray-200 px-2 py-0.5 text-xs text-gray-600 hover:bg-gray-50"
            >
              + {m.addShock}
            </button>
          </div>
          <div className="space-y-2">
            {shocks.map((s, idx) => (
              <div
                key={idx}
                className="grid grid-cols-1 gap-2 rounded-md border border-gray-100 bg-gray-50 p-2 md:grid-cols-[1fr_2fr_1fr_auto]"
              >
                <select
                  value={s.target_type}
                  onChange={(e) => updateShock(idx, { target_type: e.target.value as StressShockTargetType })}
                  className="rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
                >
                  {ALL_STRESS_TARGET_TYPES.map((tt) => (
                    <option key={tt} value={tt}>
                      {targetLabel(m, tt)}
                    </option>
                  ))}
                </select>
                <input
                  type="text"
                  value={s.target_key}
                  onChange={(e) => updateShock(idx, { target_key: e.target.value })}
                  placeholder={m.shockTargetKey}
                  className="rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
                />
                <input
                  type="number"
                  step="0.01"
                  value={s.value}
                  onChange={(e) => updateShock(idx, { value: Number(e.target.value) })}
                  className="rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
                />
                <button
                  type="button"
                  onClick={() => removeShock(idx)}
                  className="rounded-md border border-rose-200 bg-rose-50 px-2 py-1 text-xs text-rose-600 hover:bg-rose-100"
                >
                  {m.removeShock}
                </button>
              </div>
            ))}
          </div>
        </div>

        <div className="mt-4">
          <button
            type="button"
            disabled={!name.trim() || submitting}
            onClick={() => submit().catch(() => {})}
            className="rounded-md border border-blue-200 bg-blue-50 px-3 py-1.5 text-xs font-medium text-blue-700 hover:bg-blue-100 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {submitting ? m.upsertSubmitting : m.upsertSubmit}
          </button>
        </div>
      </div>

      <div>
        {loading && scenarios.length === 0 ? (
          <p className="rounded-md border border-gray-100 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
            {m.loading}
          </p>
        ) : scenarios.length === 0 ? (
          <p className="rounded-md border border-gray-100 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
            {m.empty}
          </p>
        ) : (
          <div className="overflow-x-auto rounded-md border border-gray-100">
            <table className="min-w-full text-xs">
              <thead className="bg-gray-50 text-gray-500">
                <tr>
                  <th className="px-3 py-2 text-left">{m.scenarioName}</th>
                  <th className="px-3 py-2 text-left">{m.scenarioCategory}</th>
                  <th className="px-3 py-2 text-right">{m.scenarioShocks}</th>
                  <th className="px-3 py-2 text-left">{m.scenarioUpdatedAt}</th>
                  <th className="px-3 py-2"></th>
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-100">
                {scenarios.map((s) => (
                  <tr key={s.id}>
                    <td className="px-3 py-2 font-mono text-gray-800">{s.name}</td>
                    <td className="px-3 py-2 text-gray-700">{categoryLabel(m, s.category)}</td>
                    <td className="px-3 py-2 text-right font-mono text-gray-700">{s.shocks.length}</td>
                    <td className="px-3 py-2 text-gray-500">{new Date(s.updated_at).toLocaleString()}</td>
                    <td className="px-3 py-2 text-right">
                      <button
                        type="button"
                        onClick={() => handleDelete(s.id).catch(() => {})}
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
