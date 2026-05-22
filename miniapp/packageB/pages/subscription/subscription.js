// 订阅管理
const api = require('../../../utils/api.js').api;

function toNumber(value) {
  const n = Number(value || 0);
  return Number.isFinite(n) ? n : 0;
}

function formatDate(value) {
  if (!value) return '-';
  const date = new Date(value);
  if (isNaN(date.getTime())) return String(value);
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return date.getFullYear() + '-' + month + '-' + day;
}

function formatLimit(value, unit) {
  const n = toNumber(value);
  return n > 0 ? n + ' ' + unit : '无限';
}

function planFeatureList(plan) {
  return [
    formatLimit(plan.features.funds, '只基金管理'),
    formatLimit(plan.features.agents, '个 Agent'),
    formatLimit(plan.features.workflowRuns, '次/日工作流'),
    plan.features.models,
    'A/B 测试：' + (plan.features.abTest ? '支持' : '不支持'),
    '数据导出：' + (plan.features.export ? '支持' : '不支持')
  ];
}

function normalizePlan(raw) {
  const tier = raw.tier || 'free';
  const priceCents = toNumber(raw.price_cents_month || raw.priceCentsMonth);
  const modelTiers = raw.model_tiers || raw.modelTiers || [];
  const plan = {
    tier: tier,
    name: raw.name || tier,
    price: priceCents / 100,
    priceLabel: tier === 'enterprise' ? '联系我们' : '¥' + (priceCents / 100).toFixed(0) + '/月',
    recommended: !!raw.recommended,
    features: {
      funds: toNumber(raw.max_funds || raw.maxFunds),
      agents: toNumber(raw.max_agents_per_fund || raw.maxAgentsPerFund),
      workflowRuns: toNumber(raw.max_workflow_per_day || raw.maxWorkflowPerDay),
      models: modelTiers.length ? modelTiers.join(' / ') : '基础模型',
      abTest: !!(raw.allow_ab_test || raw.allowABTest),
      export: !!(raw.allow_export || raw.allowExport)
    }
  };
  plan.featureList = planFeatureList(plan);
  return plan;
}

const DEFAULT_PLANS = [
  normalizePlan({ tier: 'free', name: '免费版', price_cents_month: 0, max_funds: 1, max_agents_per_fund: 3, max_workflow_per_day: 1, model_tiers: ['simple'], allow_ab_test: false, allow_export: false }),
  normalizePlan({ tier: 'pro', name: '专业版', price_cents_month: 9900, max_funds: 3, max_agents_per_fund: 10, max_workflow_per_day: 0, model_tiers: ['simple', 'standard', 'critical'], recommended: true, allow_ab_test: true, allow_export: false }),
  normalizePlan({ tier: 'premium', name: '旗舰版', price_cents_month: 24900, max_funds: 10, max_agents_per_fund: 0, max_workflow_per_day: 0, model_tiers: ['simple', 'standard', 'critical'], allow_ab_test: true, allow_export: true }),
  normalizePlan({ tier: 'enterprise', name: '企业版', price_cents_month: 99900, max_funds: 0, max_agents_per_fund: 0, max_workflow_per_day: 0, model_tiers: ['simple', 'standard', 'critical'], allow_ab_test: true, allow_export: true })
];

function normalizeSubscription(payload) {
  const sub = payload && payload.subscription;
  const plan = payload && payload.plan;
  const tier = (sub && (sub.plan_tier || sub.planTier)) || (plan && plan.tier) || 'free';
  return {
    currentPlan: tier,
    subscription: sub ? {
      tier: tier,
      status: sub.status || 'active',
      expireDate: formatDate(sub.end_date || sub.endDate),
      autoRenew: !!(sub.auto_renew || sub.autoRenew)
    } : {
      tier: tier,
      status: 'active',
      expireDate: '-',
      autoRenew: false
    }
  };
}

Page({
  data: {
    currentPlan: 'free',
    plans: DEFAULT_PLANS,
    subscription: null,
    loading: false,
    payMethod: 'wechat',
    // 功能对比表
    compareFeatures: [
      { label: '基金数量', key: 'funds' },
      { label: 'Agent 数量', key: 'agents' },
      { label: '工作流次数/日', key: 'workflowRuns' },
      { label: '模型权限', key: 'models' },
      { label: 'A/B 测试', key: 'abTest' },
      { label: '数据导出', key: 'export' }
    ],
    compareData: []
  },

  onLoad() {
    this.loadSubscription();
    this.loadPlans();
    this.buildCompareData();
  },

  loadSubscription() {
    this.setData({ loading: true });
    api.getSubscription().then((res) => {
      const normalized = normalizeSubscription(res);
      this.setData({
        currentPlan: normalized.currentPlan,
        subscription: normalized.subscription
      });
      this.buildCompareData();
    }).catch(() => {
      this.setData({ subscription: null });
    }).then(() => {
      this.setData({ loading: false });
    });
  },

  loadPlans() {
    api.getSubscriptionPlans().then((res) => {
      const rows = (res && res.plans) || [];
      if (rows.length) {
        this.setData({ plans: rows.map(normalizePlan) });
        this.buildCompareData();
      }
    }).catch(() => {
      this.setData({ plans: DEFAULT_PLANS });
      this.buildCompareData();
    });
  },

  buildCompareData() {
    const { plans, compareFeatures } = this.data;
    const compareData = compareFeatures.map(feat => {
      const row = { label: feat.label, values: [] };
      plans.forEach(plan => {
        const val = plan.features[feat.key];
        if (typeof val === 'boolean') {
          row.values.push(val ? '✓' : '✗');
        } else if (val === 0) {
          row.values.push('无限');
        } else {
          row.values.push(String(val));
        }
      });
      return row;
    });
    this.setData({ compareData });
  },

  subscribe(e) {
    const tier = e.currentTarget.dataset.tier;
    if (tier === this.data.currentPlan) return;
    if (tier === 'enterprise') {
      wx.showModal({
        title: '企业版咨询',
        content: '请联系 sales@fundgpt.ai 获取企业版报价方案。',
        showCancel: false
      });
      return;
    }

    const plan = this.data.plans.find(p => p.tier === tier);
    if (!plan) return;

    wx.showModal({
      title: '确认订阅',
      content: `确定升级到${plan.name}（${plan.priceLabel}）？`,
      success: (res) => {
        if (res.confirm) {
          this.doSubscribe(tier);
        }
      }
    });
  },

  doSubscribe(tier) {
    wx.showLoading({ title: '处理中...' });
    api.subscribe(tier, this.data.payMethod).then(() => {
      wx.showToast({ title: '订阅成功', icon: 'success' });
      this.setData({ currentPlan: tier });
      this.loadSubscription();
    }).catch(() => {
      wx.showToast({ title: '订阅失败，请重试', icon: 'none' });
    }).then(() => {
      wx.hideLoading();
    });
  },

  cancelSubscription() {
    wx.showModal({
      title: '取消订阅',
      content: '取消后将在当前周期结束时降级为免费版，确定取消？',
      success: (res) => {
        if (res.confirm) {
          api.cancelSubscription().then(() => {
            wx.showToast({ title: '已取消订阅', icon: 'success' });
            this.loadSubscription();
          }).catch(() => {
            wx.showToast({ title: '操作失败', icon: 'none' });
          });
        }
      }
    });
  },

  selectPayMethod(e) {
    this.setData({ payMethod: e.currentTarget.dataset.method });
  }
});
