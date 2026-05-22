var api = require('../../../utils/api.js').api;

function formatDateTime(value) {
  if (!value) return '-';
  var date = new Date(value);
  if (isNaN(date.getTime())) return String(value);
  return date.getFullYear() + '-' + String(date.getMonth() + 1).padStart(2, '0') + '-' + String(date.getDate()).padStart(2, '0') + ' ' + String(date.getHours()).padStart(2, '0') + ':' + String(date.getMinutes()).padStart(2, '0');
}

function statusText(status) {
  var map = {
    pending: '待审批',
    pending_user: '待审批',
    approved: '已通过',
    rejected: '已拒绝',
    executed: '已执行',
    failed: '失败'
  };
  return map[status] || status || '--';
}

function statusClass(status) {
  if (status === 'pending_user') return 'pending';
  return status || 'pending';
}

function actionText(action) {
  var map = { buy: '买入', sell: '卖出', hold: '持有', rebalance: '调仓' };
  return map[action] || action || '--';
}

function normalizeRisk(raw) {
  var review = raw.riskReview || raw.risk_review || {};
  if (typeof review === 'string') {
    try { review = JSON.parse(review); } catch (e) { review = {}; }
  }
  var checks = review.checks || [];
  var verdict = review.verdict || review.overall || (raw.riskScore && raw.riskScore > 0.7 ? 'warning' : 'pass');
  var normalized = verdict === 'approved' ? 'pass' : (verdict === 'blocked' ? 'fail' : verdict);
  return {
    overall: normalized || 'pass',
    overallText: normalized === 'fail' ? '未通过' : (normalized === 'warning' ? '警告' : '通过'),
    checks: checks.map(function (item) {
      var status = item.status || (item.severity === 'high' ? 'fail' : 'pass');
      return {
        name: item.ruleName || item.name || item.ruleCode || '风控检查',
        status: status,
        statusText: status === 'fail' ? '失败' : (status === 'warning' ? '警告' : '通过'),
        detail: item.explanation || item.detail || item.message || item.adjustmentHint || '--'
      };
    })
  };
}

function normalizeAction(item, index) {
  var action = item.action || item.type || 'hold';
  var quantity = item.quantity === undefined || item.quantity === null ? '-' : item.quantity;
  var price = item.price === undefined || item.price === null ? '-' : Number(item.price).toFixed(2);
  // priceValue preserves the raw number for diff computations during a
  // refresh-quote pass. We can't reuse `price` because it's already
  // been formatted to 2 decimals for display.
  var priceValue = item.price === undefined || item.price === null ? null : Number(item.price);
  // stableKey gives normalizeAction-produced rows a deterministic
  // identity that survives the refresh-quote round-trip even on plans
  // whose server-side action.id is missing (legacy / unsaved). It
  // mirrors the web client's actionKey() helper so diffing logic stays
  // the same across the two surfaces.
  var stableKey = item.id ? item.id : (item.symbol || '') + '::' + action + '::' + (item.sortOrder != null ? item.sortOrder : index);
  return {
    id: item.id || String(index),
    stableKey: stableKey,
    type: action,
    typeText: actionText(action),
    stockCode: item.symbol || item.instrumentKey || '--',
    targetPosition: item.amount ? String(item.amount) : (item.positionSide || item.openClose || '-'),
    quantity: quantity,
    price: price,
    priceValue: priceValue,
    reason: item.reasoningZh || item.reasoning || item.reason || '--'
  };
}

function normalizePlan(raw) {
  raw = raw || {};
  var actions = Array.isArray(raw.actions) ? raw.actions : [];
  var confidence = raw.expectedReturn !== undefined && raw.expectedReturn !== null ? Math.abs(Number(raw.expectedReturn)) : 0;
  var confidencePercent = Math.max(0, Math.min(100, Math.round(confidence <= 1 ? confidence * 100 : confidence)));
  if (confidencePercent === 0 && raw.riskScore !== undefined && raw.riskScore !== null) {
    confidencePercent = Math.max(0, Math.min(100, Math.round((1 - Number(raw.riskScore)) * 100)));
  }
  var status = statusClass(raw.status);
  return {
    id: raw.id,
    fundId: raw.fundId || raw.fund_id || '',
    title: raw.tradingDate ? raw.tradingDate + ' 投资方案' : '投资方案',
    date: formatDateTime(raw.createdAt || raw.updatedAt || raw.tradingDate),
    tradingDate: raw.tradingDate || '',
    status: status,
    statusText: statusText(raw.status),
    confidencePercent: confidencePercent,
    reasoning: raw.reasoningZh || raw.reasoning || raw.reasoningEn || '暂无推理说明',
    actions: actions.map(normalizeAction),
    riskReview: normalizeRisk(raw),
    relatedRoundtable: raw.discussionSnapshot || raw.roundtableId ? {
      id: raw.roundtableId || raw.id,
      title: '决策圆桌记录',
      date: raw.tradingDate || formatDateTime(raw.createdAt),
      consensus: '查看详情'
    } : null
  };
}

// PRICE_DRIFT_DIALOG_THRESHOLD mirrors the web client. Refreshes whose
// largest absolute drift is at or below this fraction are silently
// applied (status toast only); anything above triggers the confirmation
// modal listing per-action old/new prices. The miniapp echoes the web's
// 0.3% default for consistency — change both at the same time.
var PRICE_DRIFT_DIALOG_THRESHOLD = 0.003;

function formatDriftPct(drift) {
  if (drift === null || drift === undefined || isNaN(drift)) {
    return '--';
  }
  var sign = drift > 0 ? '+' : '';
  return sign + (drift * 100).toFixed(2) + '%';
}

// computePriceRefreshRows compares two normalized action arrays (the
// snapshot taken before refresh and the response payload) and returns
// the rows whose |drift| exceeded the dialog threshold. Sells and
// non-priced actions are filtered out: their reference prices are
// either advisory or undefined, so the diff isn't actionable.
function computePriceRefreshRows(beforeActions, afterActions) {
  var byKey = {};
  beforeActions.forEach(function (item) {
    byKey[item.stableKey] = item;
  });
  var rows = [];
  afterActions.forEach(function (item) {
    var before = byKey[item.stableKey];
    if (!before) return;
    var oldPrice = before.priceValue;
    var newPrice = item.priceValue;
    if (typeof oldPrice !== 'number' || oldPrice <= 0) return;
    if (typeof newPrice !== 'number' || newPrice <= 0) return;
    var drift = (newPrice - oldPrice) / oldPrice;
    if (Math.abs(drift) <= PRICE_DRIFT_DIALOG_THRESHOLD) return;
    rows.push({
      key: item.stableKey,
      symbol: item.stockCode,
      oldPrice: oldPrice.toFixed(2),
      newPrice: newPrice.toFixed(2),
      drift: drift,
      driftText: formatDriftPct(drift),
      driftClass: drift > 0 ? 'up' : (drift < 0 ? 'down' : 'flat')
    });
  });
  return rows;
}

Page({
  data: {
    plan: null,
    loading: false,
    errorText: '',
    expandReasoning: false,
    approvalComment: '',
    expandedActions: {},
    refreshing: false,
    priceRefreshDialogOpen: false,
    priceRefreshRows: []
  },

  onLoad: function (options) {
    var planId = options.planId || options.id || '';
    this.loadPlanDetail(planId);
  },

  loadPlanDetail: function (planId) {
    if (!planId) {
      this.setData({ plan: null, errorText: '缺少方案 ID' });
      return;
    }
    this.setData({ loading: true, errorText: '' });
    api.getPlan(planId).then(function (raw) {
      this.setData({ plan: normalizePlan(raw), loading: false });
    }.bind(this)).catch(function () {
      this.setData({ plan: null, loading: false, errorText: '方案详情加载失败' });
      wx.showToast({ title: '方案详情加载失败', icon: 'none' });
    }.bind(this));
  },

  onToggleReasoning: function () {
    this.setData({ expandReasoning: !this.data.expandReasoning });
  },

  onCommentInput: function (e) {
    this.setData({ approvalComment: e.detail.value });
  },

  onApprove: function () {
    if (!this.data.plan) return;
    wx.showModal({
      title: '确认审批',
      content: '确认通过此投资方案？',
      success: function (res) {
        if (res.confirm) {
          api.approvePlan(this.data.plan.fundId, this.data.plan.id, { comment: this.data.approvalComment }).then(function (updated) {
            this.setData({ plan: normalizePlan(updated) });
            wx.showToast({ title: '已审批通过', icon: 'success' });
          }.bind(this)).catch(function () {
            wx.showToast({ title: '审批失败', icon: 'none' });
          });
        }
      }.bind(this)
    });
  },

  onReject: function () {
    if (!this.data.plan) return;
    if (!this.data.approvalComment) {
      wx.showToast({ title: '请填写拒绝原因', icon: 'none' });
      return;
    }
    wx.showModal({
      title: '确认拒绝',
      content: '确认拒绝此投资方案？',
      success: function (res) {
        if (res.confirm) {
          api.rejectPlan(this.data.plan.id, this.data.approvalComment).then(function (updated) {
            this.setData({ plan: normalizePlan(updated) });
            wx.showToast({ title: '已拒绝', icon: 'none' });
          }.bind(this)).catch(function () {
            wx.showToast({ title: '拒绝失败', icon: 'none' });
          });
        }
      }.bind(this)
    });
  },

  // onRefreshQuote re-prices every still-pending action against the
  // latest market quote and surfaces any |drift| > 0.3% as a modal.
  // Approval is not blocked — the modal is informational so the user
  // can sanity-check what changed before they sign off. The backend
  // SlippageGuard remains the hard safety net at execution time.
  onRefreshQuote: function () {
    if (!this.data.plan || this.data.refreshing) return;
    var beforeActions = (this.data.plan.actions || []).slice();
    this.setData({ refreshing: true });
    api.refreshPlanQuote(this.data.plan.id).then(function (updated) {
      var nextPlan = normalizePlan(updated);
      var diffRows = computePriceRefreshRows(beforeActions, nextPlan.actions);
      var patch = { plan: nextPlan, refreshing: false };
      if (diffRows.length > 0) {
        patch.priceRefreshDialogOpen = true;
        patch.priceRefreshRows = diffRows;
      } else {
        wx.showToast({ title: '价格无明显变动', icon: 'none' });
      }
      this.setData(patch);
    }.bind(this)).catch(function () {
      this.setData({ refreshing: false });
      wx.showToast({ title: '刷新报价失败', icon: 'none' });
    }.bind(this));
  },

  onClosePriceRefreshDialog: function () {
    this.setData({ priceRefreshDialogOpen: false });
  },

  onGoRoundtable: function () {
    if (!this.data.plan) return;
    wx.navigateTo({
      url: '/packageB/pages/roundtable/roundtable?planId=' + this.data.plan.id + '&fundId=' + this.data.plan.fundId
    });
  },

  onShareAppMessage: function () {
    return {
      title: this.data.plan ? this.data.plan.title : '方案详情'
    };
  }
});
