var util = require('../../../utils/util.js');
var api = require('../../../utils/api.js').api;

function formatDate(value) {
  if (!value) return '-';
  var date = new Date(value);
  if (isNaN(date.getTime())) return String(value);
  return util.formatDate(date);
}

function modeIndex(mode, modes) {
  var idx = modes.indexOf(mode);
  return idx >= 0 ? idx : 0;
}

// Defaults must mirror server/cmd/server/wiring_adapters.go
// (DefaultAutoExecute* constants). Keep them in sync — the server is
// the source of truth at runtime, but the modal previews defaults
// before save when the fund has never opted in.
var DEFAULT_AUTO_EXEC = {
  enabled: false,
  maxOrderPctOfAssets: 0.05,
  maxDailyPctOfAssets: 0.20,
  minConfidence: 0.60,
  slippageBouncePolicy: 'bounce_to_user',
  allowedMarkets: []
};

function normalizeAutoExecute(raw) {
  if (!raw || typeof raw !== 'object') {
    return Object.assign({}, DEFAULT_AUTO_EXEC);
  }
  return {
    enabled: !!raw.enabled,
    maxOrderPctOfAssets: typeof raw.maxOrderPctOfAssets === 'number' ? raw.maxOrderPctOfAssets : DEFAULT_AUTO_EXEC.maxOrderPctOfAssets,
    maxDailyPctOfAssets: typeof raw.maxDailyPctOfAssets === 'number' ? raw.maxDailyPctOfAssets : DEFAULT_AUTO_EXEC.maxDailyPctOfAssets,
    minConfidence: typeof raw.minConfidence === 'number' ? raw.minConfidence : DEFAULT_AUTO_EXEC.minConfidence,
    slippageBouncePolicy: raw.slippageBouncePolicy || DEFAULT_AUTO_EXEC.slippageBouncePolicy,
    allowedMarkets: Array.isArray(raw.allowedMarkets) ? raw.allowedMarkets.slice() : []
  };
}

// Phase 2B research tier toggle. Mirrors the web component: only two
// canonical values are accepted; anything else collapses to
// "standard" so a stale or hand-edited config can't silently turn the
// (expensive) debate path on.
function normalizeResearchTier(raw) {
  if (typeof raw !== 'string') return 'standard';
  var trimmed = raw.trim().toLowerCase();
  return trimmed === 'advanced' ? 'advanced' : 'standard';
}

function formatPct(fraction) {
  if (!Number.isFinite(fraction)) return '0%';
  return (fraction * 100).toFixed(fraction < 0.1 ? 1 : 0) + '%';
}

function normalizeFund(raw, modes) {
  var nav = Number(raw.nav || 0);
  var initialCapital = Number(raw.initialCapital || 0);
  var totalAssets = Number(raw.totalAssets || raw.currentCapital || 0);
  var totalReturn = initialCapital > 0 ? (totalAssets / initialCapital - 1) * 100 : 0;
  var autoExec = normalizeAutoExecute(raw.autoExecute);
  return {
    id: raw.id,
    name: raw.name || '--',
    nav: nav.toFixed(4),
    navDate: formatDate(raw.updatedAt || raw.createdAt),
    dailyChange: '0.0000',
    dailyChangePercent: '0.00',
    isUp: true,
    initialCapital: util.formatMoney(initialCapital),
    aum: util.formatMoney(totalAssets),
    tradingMode: raw.tradingMode || 'simulation',
    tradingModeIndex: modeIndex(raw.tradingMode || 'simulation', modes),
    createdDate: formatDate(raw.createdAt),
    autoExecute: autoExec,
    autoExecuteSummary: {
      enabled: autoExec.enabled,
      enabledLabel: autoExec.enabled ? '已开启' : '已关闭',
      maxOrderPctText: formatPct(autoExec.maxOrderPctOfAssets),
      maxDailyPctText: formatPct(autoExec.maxDailyPctOfAssets),
      minConfidenceText: (autoExec.minConfidence * 100).toFixed(0) + '%'
    },
    researchTier: normalizeResearchTier(raw.researchTier),
    metrics: {
      totalReturn: +totalReturn.toFixed(2),
      annualizedReturn: 0,
      sharpeRatio: 0,
      maxDrawdown: 0,
      volatility: 0,
      winRate: 0
    }
  };
}

function normalizeAgent(row) {
  var agent = row.agent || row;
  return {
    id: agent.id || row.agentId,
    emoji: '🤖',
    name: agent.name || row.role || '--',
    role: row.role || agent.role || '--'
  };
}

Page({
  data: {
    fund: null,
    agents: [],
    tradingModes: ['simulation', 'paper', 'live'],
    currentModeIndex: 0,
    showModeSelector: false,
    // Auto-execute settings modal local state. We keep a working
    // draft (autoExecuteDraft) separate from fund.autoExecute so the
    // user can edit fields without committing until they tap "保存".
    showAutoExecuteSheet: false,
    autoExecuteDraft: Object.assign({}, DEFAULT_AUTO_EXEC),
    autoExecuteAllowedMarketsText: '',
    autoExecutePolicies: [
      { value: 'bounce_to_user', label: '退回人工审批（默认）' },
      { value: 'reject', label: '直接拒绝该方案' },
      { value: 'force_execute', label: '强制按实时价成交' }
    ],
    autoExecuteSlippageIndex: 0,
    researchTierDraft: 'standard',
    // Order MUST match: index 0 -> standard, index 1 -> advanced. The
    // picker in the WXML uses this for both display labels and the
    // value lookup on change.
    researchTierOptions: [
      { value: 'standard', label: '标准（快、便宜，文本汇总）' },
      { value: 'advanced', label: '深度辩论（Bull/Bear/Quant 多轮）' }
    ],
    researchTierIndex: 0
  },

  onLoad(options) {
    const fundId = options.fundId || options.id || wx.getStorageSync('currentFundId') || '';
    this.loadFundDetail(fundId);
  },

  loadFundDetail(fundId) {
    if (!fundId) {
      this.setData({ fund: null, agents: [] });
      return;
    }
    api.getFund(fundId).then((fund) => {
      var normalized = normalizeFund(fund || {}, this.data.tradingModes);
      wx.setStorageSync('currentFundId', normalized.id);
      wx.setStorageSync('currentFund', { id: normalized.id, name: normalized.name });
      this.setData({
        fund: normalized,
        currentModeIndex: normalized.tradingModeIndex
      });
      return api.getTeam(fundId);
    }).then((team) => {
      var agents = Array.isArray(team) ? team.map(normalizeAgent) : [];
      this.setData({ agents: agents });
    }).catch(() => {
      this.setData({ fund: null, agents: [] });
      wx.showToast({ title: '基金详情加载失败', icon: 'none' });
    });
  },

  onShowModeSelector() {
    this.setData({ showModeSelector: true });
  },

  onHideModeSelector() {
    this.setData({ showModeSelector: false });
  },

  onSwitchMode(e) {
    const mode = e.currentTarget.dataset.mode;
    const index = this.data.tradingModes.indexOf(mode);
    wx.showModal({
      title: '切换交易模式',
      content: `确认将交易模式切换为 ${mode}？`,
      success: (res) => {
        if (res.confirm && this.data.fund) {
          api.updateFund(this.data.fund.id, { tradingMode: mode }).then(() => {
            this.setData({
              currentModeIndex: index,
              'fund.tradingMode': mode,
              'fund.tradingModeIndex': index,
              showModeSelector: false
            });
            wx.showToast({ title: '已切换至 ' + mode, icon: 'success' });
          }).catch(() => {
            wx.showToast({ title: '切换失败', icon: 'none' });
          });
        }
      }
    });
  },

  onAgentTap(e) {
    const agentId = e.currentTarget.dataset.id;
    wx.navigateTo({
      url: '/packageB/pages/agent-detail/agent-detail?agentId=' + agentId
    });
  },

  // Auto-execute toggle on the main card: a quick flip without
  // opening the settings sheet. Sends the full current config (or
  // defaults) so the server never sees a partial patch that would
  // leave guardrails undefined.
  onAutoExecuteQuickToggle(e) {
    if (!this.data.fund) return;
    var next = !!e.detail.value;
    var current = normalizeAutoExecute(this.data.fund.autoExecute);
    current.enabled = next;
    this.persistAutoExecute(current);
  },

  // Long-press / "更多" entry: open the bottom sheet with the full
  // form. We prime the draft from the current persisted state so
  // tapping Cancel restores cleanly.
  onAutoExecuteOpenSheet() {
    if (!this.data.fund) return;
    var cfg = normalizeAutoExecute(this.data.fund.autoExecute);
    var tier = normalizeResearchTier(this.data.fund.researchTier);
    this.setData({
      showAutoExecuteSheet: true,
      autoExecuteDraft: cfg,
      autoExecuteAllowedMarketsText: cfg.allowedMarkets.join(', '),
      autoExecuteSlippageIndex: Math.max(0, this.data.autoExecutePolicies.findIndex(function (p) { return p.value === cfg.slippageBouncePolicy; })),
      researchTierDraft: tier,
      researchTierIndex: Math.max(0, this.data.researchTierOptions.findIndex(function (o) { return o.value === tier; }))
    });
  },

  onAutoExecuteCloseSheet() {
    this.setData({ showAutoExecuteSheet: false });
  },

  onAutoExecuteDraftToggle(e) {
    this.setData({ 'autoExecuteDraft.enabled': !!e.detail.value });
  },

  onAutoExecuteDraftMaxOrderInput(e) {
    var raw = Number(e.detail.value);
    if (!Number.isFinite(raw)) raw = 0;
    var fraction = Math.min(1, Math.max(0, raw / 100));
    this.setData({ 'autoExecuteDraft.maxOrderPctOfAssets': fraction });
  },

  onAutoExecuteDraftMaxDailyInput(e) {
    var raw = Number(e.detail.value);
    if (!Number.isFinite(raw)) raw = 0;
    var fraction = Math.min(1, Math.max(0, raw / 100));
    this.setData({ 'autoExecuteDraft.maxDailyPctOfAssets': fraction });
  },

  onAutoExecuteDraftMinConfidenceInput(e) {
    var raw = Number(e.detail.value);
    if (!Number.isFinite(raw)) raw = 0;
    var clamped = Math.min(1, Math.max(0, raw));
    this.setData({ 'autoExecuteDraft.minConfidence': clamped });
  },

  onAutoExecuteDraftPolicyChange(e) {
    var idx = Number(e.detail.value);
    var policy = this.data.autoExecutePolicies[idx];
    if (!policy) return;
    this.setData({
      autoExecuteSlippageIndex: idx,
      'autoExecuteDraft.slippageBouncePolicy': policy.value
    });
  },

  onAutoExecuteDraftMarketsInput(e) {
    var text = e.detail.value || '';
    var markets = text.split(/[\s,，]+/).map(function (s) { return s.trim().toLowerCase(); }).filter(Boolean);
    this.setData({
      autoExecuteAllowedMarketsText: text,
      'autoExecuteDraft.allowedMarkets': markets
    });
  },

  // Phase 2B research tier picker. The WXML uses a wx:picker bound to
  // researchTierOptions; here we map the chosen index back to the
  // canonical value and stash it on the draft. We do NOT auto-save —
  // the change is committed alongside the rest of the autoExecute
  // payload when the user taps "保存".
  onResearchTierDraftChange(e) {
    var idx = Number(e.detail.value);
    var option = this.data.researchTierOptions[idx];
    if (!option) return;
    this.setData({
      researchTierIndex: idx,
      researchTierDraft: option.value
    });
  },

  onAutoExecuteSave() {
    var draft = Object.assign({}, this.data.autoExecuteDraft);
    draft.allowedMarkets = (draft.allowedMarkets || []).slice();
    this.persistAutoExecute(draft, true, this.data.researchTierDraft);
  },

  persistAutoExecute(cfg, closeSheetAfter, researchTier) {
    var self = this;
    var fundId = this.data.fund && this.data.fund.id;
    if (!fundId) return;
    var payload = { autoExecute: cfg };
    if (researchTier === 'standard' || researchTier === 'advanced') {
      payload.researchTier = researchTier;
    }
    wx.showLoading({ title: '保存中', mask: true });
    api.updateFund(fundId, payload).then(function (updated) {
      wx.hideLoading();
      // Server returns the canonical Fund, including the resolved
      // autoExecute block (with defaults backfilled). Re-normalize so
      // the summary card uses the same numbers the server will apply.
      var normalized = normalizeFund(updated || {}, self.data.tradingModes);
      var nextState = {
        fund: normalized,
        currentModeIndex: normalized.tradingModeIndex
      };
      if (closeSheetAfter) {
        nextState.showAutoExecuteSheet = false;
      }
      self.setData(nextState);
      wx.showToast({ title: cfg.enabled ? '自动决策已开启' : '已保存设置', icon: 'success' });
    }).catch(function () {
      wx.hideLoading();
      wx.showToast({ title: '保存失败，请稍后再试', icon: 'none' });
    });
  },

  onShareAppMessage() {
    return {
      title: this.data.fund ? this.data.fund.name : '基金详情',
      path: '/packageA/pages/fund-detail/fund-detail?fundId=' + (this.data.fund ? this.data.fund.id : '')
    };
  }
});
