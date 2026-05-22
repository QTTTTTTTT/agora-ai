// 用量与账单
const api = require('../../../utils/api.js').api;

const HISTORY_LIMIT = 20;

function getCurrentMonth() {
  const now = new Date();
  const month = String(now.getMonth() + 1).padStart(2, '0');
  return now.getFullYear() + '-' + month;
}

function toNumber(value) {
  const n = Number(value || 0);
  return Number.isFinite(n) ? n : 0;
}

function centsToYuan(cents) {
  return toNumber(cents) / 100;
}

function formatMoney(value) {
  return centsToYuan(value).toFixed(2);
}

function formatYuan(value) {
  return toNumber(value).toFixed(2);
}

function parseBreakdown(value) {
  if (!value) return {};
  if (typeof value === 'string') {
    try {
      return JSON.parse(value) || {};
    } catch (e) {
      return {};
    }
  }
  return value;
}

function getBreakdownCostCents(item) {
  return toNumber(
    item.price_cents !== undefined ? item.price_cents :
      item.cost_cents !== undefined ? item.cost_cents :
        item.price !== undefined ? item.price : item.cost
  );
}

function getBreakdownCalls(item) {
  return toNumber(
    item.total_calls !== undefined ? item.total_calls :
      item.calls !== undefined ? item.calls : item.count
  );
}

function breakdownToList(raw, totalCalls) {
  const obj = parseBreakdown(raw);
  const keys = Object.keys(obj);
  const baseCost = keys.reduce((sum, key) => sum + getBreakdownCostCents(obj[key] || {}), 0);
  return keys.map((key) => {
    const item = obj[key] || {};
    const calls = getBreakdownCalls(item);
    const inputTokens = toNumber(item.input_tokens || item.inputTokens);
    const outputTokens = toNumber(item.output_tokens || item.outputTokens);
    const costCents = getBreakdownCostCents(item);
    let percent = 0;
    if (totalCalls > 0 && calls > 0) {
      percent = Math.round(calls * 100 / totalCalls);
    } else if (baseCost > 0 && costCents > 0) {
      percent = Math.round(costCents * 100 / baseCost);
    }
    return {
      name: key,
      calls: calls,
      tokens: inputTokens + outputTokens,
      cost: formatMoney(costCents),
      totalCost: formatMoney(costCents),
      percent: Math.max(1, Math.min(100, percent))
    };
  }).sort((a, b) => b.percent - a.percent);
}

function daysInMonth(yearMonth) {
  const parts = String(yearMonth || '').split('-');
  const year = Number(parts[0]);
  const month = Number(parts[1]);
  if (!year || !month) return 30;
  return new Date(year, month, 0).getDate();
}

function buildEmptyCalendar(yearMonth) {
  const days = daysInMonth(yearMonth);
  const calendar = [];
  for (let d = 1; d <= days; d++) {
    calendar.push({ day: d, calls: 0, level: 0 });
  }
  return calendar;
}

function formatTime(value) {
  if (!value) return '';
  const date = new Date(value);
  if (isNaN(date.getTime())) return String(value);
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  const hour = String(date.getHours()).padStart(2, '0');
  const minute = String(date.getMinutes()).padStart(2, '0');
  return month + '-' + day + ' ' + hour + ':' + minute;
}

function planLabel(tier) {
  const labels = {
    free: '免费版',
    pro: '专业版',
    premium: '旗舰版',
    enterprise: '企业版'
  };
  return labels[tier] || tier || '未知套餐';
}

function billStatusLabel(status) {
  const labels = {
    paid: '已支付',
    pending: '待支付',
    overdue: '逾期',
    cancelled: '已取消'
  };
  return labels[status] || status || '待支付';
}

function normalizeToday(payload) {
  const summary = (payload && payload.summary) || payload || {};
  const totalCalls = toNumber(summary.total_calls || summary.totalCalls);
  const inputTokens = toNumber(summary.input_tokens || summary.inputTokens);
  const outputTokens = toNumber(summary.output_tokens || summary.outputTokens);
  return {
    totalCalls: totalCalls,
    inputTokens: inputTokens,
    outputTokens: outputTokens,
    totalTokens: inputTokens + outputTokens,
    costCents: formatMoney(summary.price_cents !== undefined ? summary.price_cents : summary.cost_cents),
    priceCents: formatMoney(summary.price_cents),
    dailyLimit: toNumber(payload && payload.daily_limit),
    remainingCalls: toNumber(payload && payload.remaining_calls),
    byModel: breakdownToList(summary.model_breakdown || summary.modelBreakdown, totalCalls),
    byStep: breakdownToList(summary.step_breakdown || summary.stepBreakdown, totalCalls)
  };
}

function normalizeMonthly(payload, selectedMonth) {
  const summary = (payload && payload.summary) || payload || {};
  const totalCalls = toNumber(summary.total_calls || summary.totalCalls);
  const inputTokens = toNumber(summary.input_tokens || summary.inputTokens);
  const outputTokens = toNumber(summary.output_tokens || summary.outputTokens);
  const totalCostYuan = centsToYuan(summary.price_cents !== undefined ? summary.price_cents : summary.cost_cents);
  const now = new Date();
  const currentMonth = getCurrentMonth();
  const daysUsed = selectedMonth === currentMonth ? now.getDate() : daysInMonth(selectedMonth);
  return {
    summary: {
      totalCost: formatYuan(totalCostYuan),
      totalTokens: inputTokens + outputTokens,
      totalCalls: totalCalls,
      avgDailyCost: formatYuan(daysUsed > 0 ? totalCostYuan / daysUsed : 0),
      daysUsed: daysUsed
    },
    calendar: buildEmptyCalendar(selectedMonth),
    modelRank: breakdownToList(summary.model_breakdown || summary.modelBreakdown, totalCalls),
    trend: []
  };
}

function normalizeHistoryEntry(entry) {
  const inputTokens = toNumber(entry.input_tokens || entry.inputTokens);
  const outputTokens = toNumber(entry.output_tokens || entry.outputTokens);
  return {
    id: entry.id,
    time: formatTime(entry.created_at || entry.createdAt),
    step: entry.step_name || entry.stepName || 'unknown',
    model: entry.model_name || entry.modelName || 'unknown',
    tokens: inputTokens + outputTokens,
    cost: formatMoney(entry.price_cents !== undefined ? entry.price_cents : entry.cost_cents)
  };
}

function normalizeBill(payload, selectedMonth) {
  const bill = (payload && payload.bill) || payload || {};
  const detailsSource = parseBreakdown(bill.details_json || bill.detailsJSON);
  const details = [];
  const modelBreakdown = parseBreakdown(detailsSource.model_breakdown || detailsSource.modelBreakdown);
  Object.keys(modelBreakdown).forEach((key) => {
    details.push({ label: key + ' 使用费', amount: formatMoney(getBreakdownCostCents(modelBreakdown[key] || {})) });
  });
  const customKeyCredit = toNumber(bill.custom_key_credit || bill.customKeyCredit);
  if (customKeyCredit > 0) {
    details.push({ label: '自带 Key 抵扣', amount: -Number(formatMoney(customKeyCredit)) });
  }
  return {
    month: selectedMonth,
    subscriptionFee: formatMoney(bill.subscription_fee || bill.subscriptionFee),
    subscriptionPlan: planLabel(bill.plan_tier || bill.planTier),
    modelUsageFee: formatMoney(bill.model_usage_fee || bill.modelUsageFee),
    customKeyDiscount: -Number(formatMoney(customKeyCredit)),
    total: formatMoney(bill.final_amount !== undefined ? bill.final_amount : bill.finalAmount),
    status: billStatusLabel(bill.status),
    details: details
  };
}

Page({
  data: {
    activeTab: 'today',
    tabs: [
      { key: 'today', label: '今日' },
      { key: 'monthly', label: '本月' },
      { key: 'history', label: '历史' },
      { key: 'bill', label: '账单' }
    ],
    selectedMonth: getCurrentMonth(),
    // 今日概览
    todaySummary: null,
    todayHourly: [],
    // 本月概览
    monthlySummary: null,
    monthlyCalendar: [],
    monthlyModelRank: [],
    monthlyTrend: [],
    // 历史明细
    usageHistory: [],
    historyPage: 0,
    hasMore: true,
    // 账单
    bill: null,
    billExpanded: false
  },

  onLoad() {
    this.loadToday();
    this.loadMonthly();
  },

  switchTab(e) {
    const tab = e.currentTarget.dataset.tab;
    this.setData({ activeTab: tab });
    if (tab === 'today' && !this.data.todaySummary) this.loadToday();
    if (tab === 'monthly' && !this.data.monthlySummary) this.loadMonthly();
    if (tab === 'history' && this.data.usageHistory.length === 0) this.loadHistory();
    if (tab === 'bill' && !this.data.bill) this.loadBill();
  },

  loadToday() {
    api.getTodayUsage().then((res) => {
      this.setData({
        todaySummary: normalizeToday(res),
        todayHourly: []
      });
    }).catch(() => {
      this.setData({ todaySummary: null, todayHourly: [] });
    });
  },

  loadMonthly() {
    api.getMonthlyUsage(this.data.selectedMonth).then((res) => {
      const normalized = normalizeMonthly(res, this.data.selectedMonth);
      this.setData({
        monthlySummary: normalized.summary,
        monthlyCalendar: normalized.calendar,
        monthlyModelRank: normalized.modelRank,
        monthlyTrend: normalized.trend
      });
    }).catch(() => {
      this.setData({
        monthlySummary: null,
        monthlyCalendar: [],
        monthlyModelRank: [],
        monthlyTrend: []
      });
    });
  },

  loadHistory() {
    if (!this.data.hasMore) return;
    const offset = this.data.historyPage;
    api.getUsageHistory({ offset: offset, limit: HISTORY_LIMIT }).then((res) => {
      const entries = (res && res.entries) || [];
      const total = toNumber(res && res.total);
      const mapped = entries.map(normalizeHistoryEntry);
      this.setData({
        usageHistory: this.data.usageHistory.concat(mapped),
        historyPage: offset + entries.length,
        hasMore: offset + entries.length < total
      });
    }).catch(() => {
      if (offset === 0) {
        this.setData({ usageHistory: [], hasMore: false });
      }
    });
  },

  loadBill() {
    api.getBill(this.data.selectedMonth).then((res) => {
      this.setData({ bill: normalizeBill(res, this.data.selectedMonth) });
    }).catch(() => {
      this.setData({ bill: null });
    });
  },

  changeMonth(e) {
    this.setData({
      selectedMonth: e.detail.value,
      monthlySummary: null,
      monthlyCalendar: [],
      monthlyModelRank: [],
      monthlyTrend: [],
      bill: null
    });
    this.loadMonthly();
    if (this.data.activeTab === 'bill') this.loadBill();
  },

  onReachBottom() {
    if (this.data.activeTab === 'history' && this.data.hasMore) {
      this.loadHistory();
    }
  },

  toggleBillDetail() {
    this.setData({ billExpanded: !this.data.billExpanded });
  },

  exportBill() {
    wx.showModal({
      title: '导出账单',
      content: '将以 CSV 格式导出本月账单到剪贴板。',
      success: (res) => {
        if (res.confirm && this.data.bill) {
          const b = this.data.bill;
          let csv = '项目,金额\n';
          csv += '订阅费(' + b.subscriptionPlan + '),' + b.subscriptionFee + '\n';
          if (b.details) {
            b.details.forEach(d => {
              csv += d.label + ',' + d.amount + '\n';
            });
          }
          csv += '合计,' + b.total + '\n';
          wx.setClipboardData({
            data: csv,
            success: () => {
              wx.showToast({ title: '已复制到剪贴板', icon: 'success' });
            }
          });
        }
      }
    });
  }
});
