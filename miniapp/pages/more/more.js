// 更多/设置 - 系统配置与功能入口
const api = require('../../utils/api.js').api;

function toNumber(value) {
  const n = Number(value || 0);
  return Number.isFinite(n) ? n : 0;
}

function currentMonth() {
  const now = new Date();
  const month = String(now.getMonth() + 1).padStart(2, '0');
  return now.getFullYear() + '-' + month;
}

Page({
  data: {
    apiBase: '',
    version: '1.0.0',
    serverStatus: 'checking', // checking | connected | disconnected
    fundInfo: {
      name: '',
      mode: ''
    },
    showAboutModal: false,
    showFundPicker: false,
    funds: [
      { name: '稳健成长一号', mode: 'balanced' },
      { name: '科技创新二号', mode: 'aggressive' },
      { name: '固收增强三号', mode: 'conservative' }
    ],
    currentPlanName: '免费版',
    monthCost: '0.00'
  },

  onShow() {
    this.loadSettings();
    this.checkHealth();
    this.loadPlanInfo();
  },

  loadSettings() {
    const apiBase = wx.getStorageSync('apiBase') || 'http://localhost:8000';
    const fundName = wx.getStorageSync('fundName') || '稳健成长一号';
    const fundMode = wx.getStorageSync('fundMode') || 'balanced';
    this.setData({
      apiBase,
      fundInfo: {
        name: fundName,
        mode: fundMode
      }
    });
  },

  loadPlanInfo() {
    api.getSubscription().then((res) => {
      this.setData({
        currentPlanName: (res && res.plan && res.plan.name) || '免费版'
      });
    }).catch(() => {
      this.setData({ currentPlanName: '免费版' });
    });

    api.getMonthlyUsage(currentMonth()).then((res) => {
      const summary = (res && res.summary) || {};
      const cents = summary.price_cents !== undefined ? summary.price_cents : summary.cost_cents;
      this.setData({ monthCost: (toNumber(cents) / 100).toFixed(2) });
    }).catch(() => {
      this.setData({ monthCost: '0.00' });
    });
  },

  onApiBaseInput(e) {
    this.setData({ apiBase: e.detail.value });
  },

  saveApiBase() {
    const { apiBase } = this.data;
    if (!apiBase.trim()) {
      wx.showToast({ title: '请输入API地址', icon: 'none' });
      return;
    }
    wx.setStorageSync('apiBase', apiBase);
    const app = getApp();
    if (app && app.globalData) {
      app.globalData.apiBase = apiBase;
    }
    wx.showToast({ title: '已保存', icon: 'success' });
    this.checkHealth();
  },

  checkHealth() {
    this.setData({ serverStatus: 'checking' });
    api.health().then((res) => {
      this.setData({ serverStatus: 'connected' });
      if (res && res.fund_name) {
        this.setData({
          'fundInfo.name': res.fund_name,
          'fundInfo.mode': res.mode || ''
        });
      }
    }).catch(() => {
      this.setData({ serverStatus: 'disconnected' });
    });
  },

  goToABTest() {
    wx.navigateTo({
      url: '/packageB/pages/abtest/abtest'
    });
  },

  goToTrades() {
    wx.navigateTo({
      url: '/packageA/pages/trades/trades'
    });
  },

  goToRoundtable() {
    wx.switchTab({
      url: '/pages/decision/decision'
    });
  },

  goToSubscription() {
    wx.navigateTo({
      url: '/packageB/pages/subscription/subscription'
    });
  },

  goToModelConfig() {
    wx.navigateTo({
      url: '/packageB/pages/model-config/model-config'
    });
  },

  goToUsage() {
    wx.navigateTo({
      url: '/packageB/pages/usage/usage'
    });
  },

  switchFund() {
    const { funds } = this.data;
    const names = funds.map(f => f.name);
    wx.showActionSheet({
      itemList: names,
      success: (res) => {
        const selected = funds[res.tapIndex];
        wx.setStorageSync('fundName', selected.name);
        wx.setStorageSync('fundMode', selected.mode);
        this.setData({
          fundInfo: {
            name: selected.name,
            mode: selected.mode
          }
        });
        wx.showToast({ title: '已切换至' + selected.name, icon: 'none' });
      }
    });
  },

  clearCache() {
    wx.showModal({
      title: '清除缓存',
      content: '确定要清除所有本地缓存数据吗？API地址配置将被保留。',
      success: (res) => {
        if (res.confirm) {
          const apiBase = wx.getStorageSync('apiBase');
          wx.clearStorageSync();
          if (apiBase) {
            wx.setStorageSync('apiBase', apiBase);
          }
          wx.showToast({ title: '缓存已清除', icon: 'success' });
        }
      }
    });
  },

  showAbout() {
    this.setData({ showAboutModal: true });
  },

  hideAbout() {
    this.setData({ showAboutModal: false });
  },

  copyApiBase() {
    wx.setClipboardData({
      data: this.data.apiBase,
      success() {
        wx.showToast({ title: '已复制', icon: 'success' });
      }
    });
  }
});
