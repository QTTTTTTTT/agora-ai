// 决策中心 - 方案审批管理
const api = require('../../utils/api.js').api;

function resolveFundId() {
  try {
    const app = getApp();
    if (app && app.globalData && app.globalData.currentFund && app.globalData.currentFund.id) {
      return app.globalData.currentFund.id;
    }
  } catch (e) {}
  const storedFund = wx.getStorageSync('currentFund');
  if (storedFund && storedFund.id) return storedFund.id;
  return wx.getStorageSync('currentFundId') || '';
}

function formatDate(value) {
  if (!value) return '';
  const date = new Date(value);
  if (isNaN(date.getTime())) return String(value).slice(0, 10);
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return date.getFullYear() + '-' + month + '-' + day;
}

function statusKey(status) {
  if (status === 'approved' || status === 'executed' || status === 'completed') return 'approved';
  if (status === 'rejected') return 'rejected';
  // Sprint 3 / L2: partial-fill plans surfaced as a distinct mixed
  // status; WXSS uses status-mixed class to give it an amber dot
  // instead of the green "all good" / red "all bad" dichotomy.
  if (status === 'mixed') return 'mixed';
  return 'pending';
}

function riskLabel(plan) {
  if (plan.status === 'rejected') return { status: 'rejected', label: '拒绝' };
  if (plan.riskScore !== undefined && plan.riskScore !== null) {
    return Number(plan.riskScore) >= 0.8 ? { status: 'approved_with_warnings', label: '有警告' } : { status: 'approved', label: '通过' };
  }
  return { status: 'approved', label: '通过' };
}

function normalizePlan(plan) {
  const actions = plan.actions || [];
  const buyCount = actions.filter(a => a.action === 'buy').length;
  const sellCount = actions.filter(a => a.action === 'sell').length;
  const risk = riskLabel(plan);
  return {
    id: plan.id,
    title: plan.tradingDate ? plan.tradingDate + ' 投资方案' : '投资方案',
    date: formatDate(plan.tradingDate || plan.createdAt),
    status: statusKey(plan.status),
    buyCount: buyCount,
    sellCount: sellCount,
    adjustCount: actions.length,
    riskStatus: risk.status,
    riskLabel: risk.label,
    confidence: plan.expectedReturn !== undefined && plan.expectedReturn !== null ? Math.min(1, Math.abs(Number(plan.expectedReturn))) : 0,
    confidencePct: Math.round((plan.expectedReturn !== undefined && plan.expectedReturn !== null ? Math.min(1, Math.abs(Number(plan.expectedReturn))) : 0) * 100),
    summary: plan.reasoningZh || plan.reasoning || '暂无方案说明',
    agent: plan.pmAgentId || '--'
  };
}

Page({
  data: {
    fundId: '',
    plans: [],
    filteredPlans: [],
    currentTab: 'pending',
    currentTabIndex: 0,
    tabs: [
      { key: 'pending', label: '待审批' },
      { key: 'approved', label: '已通过' },
      { key: 'rejected', label: '已拒绝' }
    ],
    stats: {
      pending: 0,
      approved: 0,
      rejected: 0
    }
  },

  onShow() {
    this.loadPlans();
  },

  loadPlans() {
    const fundId = this.data.fundId || resolveFundId();
    if (!fundId) {
      this.setData({ plans: [], filteredPlans: [] });
      return;
    }
    this.setData({ fundId: fundId });
    api.getPlans(fundId, { limit: 50, offset: 0 }).then((res) => {
      const plans = (Array.isArray(res) ? res : []).map(normalizePlan);
      const stats = {
        pending: plans.filter(p => p.status === 'pending').length,
        approved: plans.filter(p => p.status === 'approved').length,
        rejected: plans.filter(p => p.status === 'rejected').length
      };
      this.setData({ plans, stats });
      this.filterPlans();
    }).catch(() => {
      this.setData({ plans: [], filteredPlans: [] });
    });
  },

  switchTab(e) {
    const key = e.currentTarget.dataset.key;
    const index = this.data.tabs.findIndex(t => t.key === key);
    this.setData({
      currentTab: key,
      currentTabIndex: index
    });
    this.filterPlans();
  },

  filterPlans() {
    const { plans, currentTab } = this.data;
    const filteredPlans = plans.filter(p => p.status === currentTab);
    this.setData({ filteredPlans });
  },

  approvePlan(e) {
    const id = e.currentTarget.dataset.id;
    api.approvePlan(this.data.fundId, id, {}).then(() => {
      wx.showToast({ title: '已批准', icon: 'success' });
      this.loadPlans();
    }).catch(() => {
      wx.showToast({ title: '批准失败', icon: 'none' });
    });
  },

  rejectPlan(e) {
    const id = e.currentTarget.dataset.id;
    wx.showModal({
      title: '拒绝方案',
      content: '确认拒绝该投资方案？',
      success: (res) => {
        if (res.confirm) {
          api.rejectPlan(id, '小程序端拒绝').then(() => {
            wx.showToast({ title: '已拒绝', icon: 'none' });
            this.loadPlans();
          }).catch(() => {
            wx.showToast({ title: '拒绝失败', icon: 'none' });
          });
        }
      }
    });
  },

  viewPlanDetail(e) {
    const id = e.currentTarget.dataset.id;
    wx.navigateTo({
      url: '/packageA/pages/plan-detail/plan-detail?id=' + id
    });
  },

  viewRoundtable(e) {
    const id = e.currentTarget.dataset.id;
    wx.navigateTo({
      url: '/packageB/pages/roundtable/roundtable?planId=' + id + '&fundId=' + (this.data.fundId || resolveFundId())
    });
  }
});
