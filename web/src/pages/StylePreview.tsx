// StylePreview.tsx — design-system showcase page.
//
// This page exists as a single-screen reference for the 2026
// cream / sage / black-pill design refresh. It mirrors the
// three "team dashboard" mockups from the design spec so the
// new components can be verified in-situ without wiring real
// fund data through every consuming page.
//
// Routed at /style-preview behind the auth gate. Not linked
// from any nav by default — operators land here intentionally
// (e.g. via the command palette or by typing the URL) to
// double-check that a code change to a theme component still
// matches the source-of-truth screenshot.
//
// Three vertical sections, each a self-contained "screen":
//   1. 团队总览 — driver's-seat card with metric blocks.
//   2. 阵容 — pixel-art mascot grid for the agent roster.
//   3. 技能面板 — skill catalog rows with "已装" / "查看" tags.

import React, { useState } from "react";
import { Link } from "react-router-dom";
import {
  EnvelopeCard,
  EnvelopeSection,
  PillTag,
  BlackPillButton,
  GhostPillButton,
  MascotAvatar,
  TabPills,
  MetricBlock,
  SectionLabel,
} from "../theme";
import type { MascotRole } from "../theme/MascotAvatar";

type TeamTab = "overview" | "config";

const teamTabs: ReadonlyArray<{ key: TeamTab; label: string }> = [
  { key: "overview", label: "团队总览" },
  { key: "config",   label: "编辑配置" },
];

interface RosterMember {
  role: MascotRole;
  name: string;
  badge?: string;
  description: string;
  // Coverage hint (0-1) drives the bottom progress bar.
  coverage: number;
}

const roster: ReadonlyArray<RosterMember> = [
  { role: "captain", name: "策略队长", badge: "统一规则", description: "负责设定团队门槛与调仓节奏，是整支队伍的指挥位。", coverage: 0.85 },
  { role: "intel",   name: "情报",     description: "环境判断", coverage: 0.7 },
  { role: "picker",  name: "选股",     description: "候选生成", coverage: 0.6 },
  { role: "trader",  name: "交易",     description: "动作执行", coverage: 0.55 },
  { role: "risk",    name: "风控",     description: "防守止损", coverage: 0.9 },
];

interface SkillRow {
  emoji: string;
  name: string;
  desc: string;
  equipped?: boolean;
  weight?: number;
}

const skills: ReadonlyArray<SkillRow> = [
  { emoji: "🔬", name: "因子过滤",     desc: "按量化条件过滤候选股票池，剔除不达标…" },
  { emoji: "🚀", name: "动量排名选股", desc: "按价格动量排名，选择近期涨势最强的 N…" },
  { emoji: "🧮", name: "多因子评分选股", desc: "综合动量、均线位置、成交量、波动率等…" },
  { emoji: "🏆", name: "相对强弱选股", desc: "已装配 · 权重 1.0", equipped: true, weight: 1.0 },
  { emoji: "😌", name: "低波动选股",   desc: "选出波动率最低的股票。低波动因子在 A 股…" },
  { emoji: "⛰️", name: "突破新高过滤", desc: "已装配 · 权重 1.0", equipped: true, weight: 1.0 },
  { emoji: "🔄", name: "均值回归选股", desc: "选出短期过度下跌、偏离均线较多的股票…" },
];

const StylePreview: React.FC = () => {
  const [tab, setTab] = useState<TeamTab>("overview");

  return (
    <div className="min-h-screen pb-32">
      {/* Page-level header — keeps the showcase anchored so a
          stakeholder dropping in via URL has context. */}
      <div className="mx-auto max-w-3xl px-4 pt-6 sm:pt-10">
        <div className="mb-6 flex items-center justify-between">
          <Link
            to="/masters"
            className="inline-flex h-10 w-10 items-center justify-center rounded-full bg-cream-0 text-ink-700 shadow-pill ring-1 ring-ink-100/70 transition hover:bg-cream-50"
            aria-label="返回"
          >
            <span aria-hidden="true">←</span>
          </Link>
          <div className="flex items-center gap-1.5">
            <span className="h-2 w-2 rounded-full bg-sage-300" />
            <span className="h-2 w-6 rounded-full bg-coral-300" />
            <span className="h-2 w-2 rounded-full bg-ink-100" />
            <span className="h-2 w-2 rounded-full bg-ink-100" />
          </div>
          <BlackPillButton size="sm" withArrow>装备</BlackPillButton>
        </div>

        {/* ──────────────────────────────────────────────────
            SCREEN 1 — Team driver's seat (WechatIMG180)
            ────────────────────────────────────────────────── */}
        <header className="mb-6">
          <div className="flex items-center gap-3">
            <div className="text-2xl font-extrabold text-ink-900">
              官方热点主线团队
            </div>
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-ink-300">
            <PillTag tone="ink"   size="sm">官方</PillTag>
            <PillTag tone="coral" size="sm">需处理</PillTag>
            <PillTag tone="sage"  size="sm">已整备</PillTag>
            <PillTag tone="muted" size="sm">未验证</PillTag>
            <span className="ml-1">最近同步 04/21 · 5 位 Agent</span>
          </div>
        </header>

        <div className="mb-5">
          <TabPills tabs={teamTabs} active={tab} onChange={setTab} />
        </div>

        {tab === "overview" ? (
          <>
            <EnvelopeCard className="mb-5 animate-fade-up">
              <div className="mb-4 flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="text-xs font-medium uppercase tracking-wide text-ink-300">
                    团队驾驶舱
                  </div>
                  <div className="mt-1 text-2xl font-extrabold text-ink-900">
                    官方热点主线团队
                  </div>
                  <div className="mt-1 text-sm text-ink-300">
                    风控要求处理 · 通威股份
                  </div>
                </div>
                <div className="flex flex-col items-end gap-2">
                  <GhostPillButton size="sm" leadingIcon={<span>↻</span>}>去回测</GhostPillButton>
                  <GhostPillButton size="sm">暂停</GhostPillButton>
                </div>
              </div>

              <div className="mb-5 flex flex-wrap gap-2">
                <PillTag tone="coral">需处理</PillTag>
                <PillTag tone="sage">已整备</PillTag>
                <PillTag tone="risk">风控优先</PillTag>
                <span className="ml-1 self-center text-xs text-ink-300">
                  同步 04/21
                </span>
              </div>

              <div className="grid grid-cols-2 gap-x-6 gap-y-4">
                <MetricBlock label="组合净值" value="¥122,582.22" />
                <MetricBlock label="滚动收益" value="+22.58%" tone="positive" />
                <MetricBlock label="待执行"   value="1 项" />
                <MetricBlock label="持仓 / 交易" value="0 / 0" />
              </div>
            </EnvelopeCard>

            <EnvelopeSection
              eyebrow="数据整备"
              title="新能源赛道池"
              subtitle="整备完成，可用于运行与回测。"
              action={<GhostPillButton size="sm" leadingIcon={<span>📊</span>}>整备详情</GhostPillButton>}
              className="mb-6 animate-fade-up"
            >
              <div className="grid grid-cols-3 gap-3 text-xs text-ink-300">
                <div className="rounded-xl bg-cream-50 p-3 ring-1 ring-ink-100/60">
                  <div className="font-semibold text-ink-700">有效股票池</div>
                  <div className="mt-1 text-ink-300">28 支</div>
                </div>
                <div className="rounded-xl bg-cream-50 p-3 ring-1 ring-ink-100/60">
                  <div className="font-semibold text-ink-700">股票池技能</div>
                  <div className="mt-1 text-ink-300">3 项</div>
                </div>
                <div className="rounded-xl bg-cream-50 p-3 ring-1 ring-ink-100/60">
                  <div className="font-semibold text-ink-700">历史窗口</div>
                  <div className="mt-1 text-ink-300">120 日</div>
                </div>
              </div>
            </EnvelopeSection>
          </>
        ) : (
          <EnvelopeCard className="mb-6 animate-fade-up">
            <div className="text-sm text-ink-300">
              切换到「编辑配置」视图。这里通常出现成员卡片、规则编辑器、回测开关等表单。
            </div>
          </EnvelopeCard>
        )}

        {/* ──────────────────────────────────────────────────
            SCREEN 2 — Roster (WechatIMG182)
            ────────────────────────────────────────────────── */}
        <SectionLabel className="mb-3 mt-10" trailing="5 / 5">
          阵容
        </SectionLabel>
        <EnvelopeCard className="mb-6 animate-fade-up">
          <div className="mb-2 flex items-end justify-between">
            <div>
              <div className="text-xl font-extrabold text-ink-900">阵容</div>
              <div className="mt-0.5 text-sm text-ink-300">看准方向，跟住强势股</div>
            </div>
          </div>

          {/* Captain — full-width hero card */}
          <div className="mb-4 mt-3 rounded-2xl bg-cream-50/90 p-5 ring-1 ring-ink-100/60">
            <div className="flex flex-col items-center text-center">
              <MascotAvatar role="captain" size={96} animated className="mb-3" />
              <div className="text-base font-extrabold text-ink-900">
                {roster[0].name}
              </div>
              <div className="mt-2">
                <PillTag tone="muted">{roster[0].badge}</PillTag>
              </div>
              <p className="mt-3 max-w-[18rem] text-sm leading-relaxed text-ink-300">
                {roster[0].description}
              </p>
              <div className="mt-4 h-1 w-16 rounded-full bg-coral-300" />
            </div>
          </div>

          {/* 4 lieutenants in a 2×2 grid */}
          <div className="grid grid-cols-2 gap-3">
            {roster.slice(1).map((m) => (
              <div
                key={m.role}
                className="flex flex-col items-center rounded-2xl bg-cream-0 p-4 text-center shadow-pill ring-1 ring-ink-100/60"
              >
                <MascotAvatar role={m.role} size={72} className="mb-2" />
                <div className="text-sm font-extrabold text-ink-900">{m.name}</div>
                <div className="mt-0.5 text-xs text-ink-300">{m.description}</div>
                <div className="mt-3 h-1 w-12 rounded-full bg-sage-300" style={{ width: `${24 + m.coverage * 24}px` }} />
              </div>
            ))}
          </div>
        </EnvelopeCard>

        {/* ──────────────────────────────────────────────────
            SCREEN 3 — Skill catalog (WechatIMG181)
            ────────────────────────────────────────────────── */}
        <SectionLabel className="mb-3 mt-10">技能面板</SectionLabel>

        {/* tab strip across the four cabinets */}
        <div className="mb-4 grid grid-cols-4 gap-2">
          {[
            { k: "intel",  label: "情报", icon: "📡" },
            { k: "picker", label: "选股", icon: "🔍", active: true },
            { k: "trade",  label: "交易", icon: "⚡" },
            { k: "risk",   label: "风控", icon: "🛡️" },
          ].map((c) => (
            <div
              key={c.k}
              className={[
                "flex flex-col items-center rounded-2xl px-3 py-3 ring-1 transition",
                c.active
                  ? "bg-cream-0 ring-ink-200/80 shadow-pill"
                  : "bg-cream-50/60 ring-ink-100/40 text-ink-300",
              ].join(" ")}
            >
              <div className="text-2xl">{c.icon}</div>
              <div className={["mt-1 text-xs font-semibold", c.active ? "text-ink-900" : ""].join(" ")}>
                {c.label}
              </div>
              {c.active ? (
                <div className="mt-1 h-1 w-6 rounded-full bg-coral-300" />
              ) : null}
            </div>
          ))}
        </div>

        <EnvelopeCard className="mb-6 animate-fade-up">
          <div className="mb-3 flex items-center justify-between">
            <div className="text-xl font-extrabold text-ink-900">选股官 的技能面板</div>
            <button
              type="button"
              className="inline-flex h-9 w-9 items-center justify-center rounded-full bg-cream-100 text-ink-700"
              aria-label="关闭"
            >
              ×
            </button>
          </div>
          <div className="mb-4 flex items-center gap-4 text-sm">
            <span className="text-ink-300">股票池技能 3</span>
            <span className="rounded-full bg-cream-50 px-3 py-1 text-xs font-semibold text-ink-700 ring-1 ring-ink-100">
              过滤 / 排序技能 7
            </span>
          </div>

          <ul className="space-y-3">
            {skills.map((s) => (
              <li
                key={s.name}
                className="flex items-center gap-3 rounded-2xl bg-cream-50/80 px-3 py-3 ring-1 ring-ink-100/60"
              >
                <div className="flex h-12 w-12 shrink-0 items-center justify-center rounded-full bg-cream-0 text-2xl shadow-pill ring-1 ring-ink-100/60">
                  {s.emoji}
                </div>
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm font-semibold text-ink-900">
                    {s.name}
                  </div>
                  <div className="mt-0.5 truncate text-xs text-ink-300">
                    {s.desc}
                  </div>
                </div>
                {s.equipped ? (
                  <PillTag tone="sage" size="sm">已装</PillTag>
                ) : (
                  <button
                    type="button"
                    className="rounded-full bg-cream-0 px-3 py-1.5 text-xs font-semibold text-ink-700 ring-1 ring-ink-100"
                  >
                    查看
                  </button>
                )}
              </li>
            ))}
          </ul>
        </EnvelopeCard>
      </div>

      {/* Sticky bottom CTA — anchors the "去回测验证" button at
          the foot of the page so the user always has an obvious
          escape hatch into the next workflow. */}
      <div className="fixed inset-x-0 bottom-0 z-30 px-4 pb-5 pt-3">
        <div className="mx-auto max-w-3xl">
          <div className="flex items-center gap-3 rounded-full bg-cream-0/90 p-2 shadow-envelope ring-1 ring-ink-100/70 backdrop-blur">
            <button
              type="button"
              className="ml-3 text-sm font-semibold text-ink-300"
            >
              官方团队已锁定
            </button>
            <BlackPillButton block size="lg" leadingIcon={<span>📈</span>}>
              去回测验证
            </BlackPillButton>
          </div>
        </div>
      </div>
    </div>
  );
};

export default StylePreview;
