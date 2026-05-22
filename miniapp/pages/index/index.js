/**
 * 首页 / Dashboard
 */
var util = require('../../utils/util.js');
var api = require('../../utils/api.js').api;

function toNumber(value) {
  var n = Number(value || 0);
  return isFinite(n) ? n : 0;
}

function formatTradeTime(value) {
  if (!value) return '';
  var d = new Date(value);
  if (isNaN(d.getTime())) return String(value);
  var month = d.getMonth() + 1;
  var day = d.getDate();
  var hour = d.getHours();
  var min = d.getMinutes();
  return (month < 10 ? '0' + month : month) + '-' + (day < 10 ? '0' + day : day) + ' ' + (hour < 10 ? '0' + hour : hour) + ':' + (min < 10 ? '0' + min : min);
}

function resolveCurrentFundId(funds) {
  var storedFundId = wx.getStorageSync('currentFundId');
  if (storedFundId) return storedFundId;
  if (funds && funds.length > 0) return funds[0].id;
  return '';
}

function normalizeNavPoint(p) {
  return {
    date: p.date || p.tradingDate || p.createdAt || '',
    nav: toNumber(p.nav),
    benchmark: p.benchmark
  };
}

Page({
  data: {
    fund: null,
    positions: [],
    recentTrades: [],
    navHistory: [],
    workflowStatus: 'idle',
    workflowStep: '',
    // 格式化后的展示数据
    navDisplay: '--',
    navChangeDisplay: '--',
    navChangePctDisplay: '--',
    isUp: true,
    totalAumDisplay: '--',
    dailyPnlDisplay: '--',
    sharpeDisplay: '--',
    maxDrawdownDisplay: '--',
    // 工作流步骤列表
    workflowSteps: ['数据采集', '研究分析', '风险评估', '决策生成', '交易执行'],
    currentStepIndex: -1,
  },

  onLoad: function () {
    this._loadData();
  },

  onPullDownRefresh: function () {
    this._loadData().then(function () {
      wx.showToast({ title: '已刷新', icon: 'success', duration: 1000 });
    }).catch(function () {}).then(function () {
      wx.stopPullDownRefresh();
    });
  },

  /**
   * 加载基金数据
   */
  _loadData: function () {
    var that = this;
    return api.getCompanies().then(function (companies) {
      var company = companies && companies[0];
      if (!company) throw new Error('no company');
      return api.getFunds(company.id).then(function (funds) {
        var fundId = resolveCurrentFundId(funds);
        var fund = (funds || []).filter(function (item) { return item.id === fundId; })[0] || funds[0];
        if (!fund) throw new Error('no fund');
        wx.setStorageSync('currentFundId', fund.id);
        wx.setStorageSync('currentFund', { id: fund.id, name: fund.name });
        return Promise.all([
          Promise.resolve(Object.assign({}, fund, { company_name: company.name || '' })),
          api.getPositions(fund.id),
          api.getTrades(fund.id, { limit: 5, offset: 0 }),
          api.getNavHistory(fund.id)
        ]);
      });
    }).then(function (result) {
      that._applyDashboardData(result[0], result[1] || [], result[2] || [], result[3] || []);
    }).catch(function () {
      that.setData({
        fund: null,
        positions: [],
        recentTrades: [],
        navHistory: [],
        navDisplay: '--',
        navChangeDisplay: '--',
        navChangePctDisplay: '--',
        totalAumDisplay: '--',
        dailyPnlDisplay: '--',
        sharpeDisplay: '--',
        maxDrawdownDisplay: '--'
      });
    });
  },

  _applyDashboardData: function (fund, positions, recentTrades, navHistory) {
    var nav = toNumber(fund.nav);
    var totalAssets = toNumber(fund.totalAssets || fund.currentCapital);
    var formattedNavHistory = (navHistory || []).map(normalizeNavPoint).filter(function (p) { return p.nav > 0; });
    var latestNav = formattedNavHistory.length > 0 ? formattedNavHistory[formattedNavHistory.length - 1] : null;
    var previousNav = formattedNavHistory.length > 1 ? formattedNavHistory[formattedNavHistory.length - 2] : null;
    var navChange = latestNav && previousNav ? toNumber(latestNav.nav) - toNumber(previousNav.nav) : 0;
    var navChangePct = previousNav && toNumber(previousNav.nav) !== 0 ? navChange / toNumber(previousNav.nav) : 0;
    var isUp = navChange >= 0;

    // 格式化持仓数据
    var formattedPositions = positions.map(function (p) {
      return {
        id: p.id,
        symbol: p.symbol,
        name: p.name,
        quantity: p.quantity,
        marketValue: util.formatMoney(p.marketValue),
        pnlPct: util.formatPercent(toNumber(p.unrealizedPnl) / Math.max(toNumber(p.marketValue), 1)),
        isUp: toNumber(p.unrealizedPnl) >= 0,
      };
    });

    // 格式化交易数据
    var formattedTrades = recentTrades.map(function (t) {
      var side = t.side || '';
      return {
        id: t.id,
        time: formatTradeTime(t.executedAt || t.createdAt),
        action: side,
        actionText: side === 'buy' ? '买入' : side === 'sell' ? '卖出' : side,
        symbol: t.symbol,
        name: t.instrumentKey || t.symbol,
        quantity: t.filledQty || t.quantity,
        price: toNumber(t.filledPrice || t.price).toFixed(2),
        isBuy: side === 'buy',
      };
    });

    this.setData({
      fund: fund,
      positions: formattedPositions,
      recentTrades: formattedTrades,
      navHistory: formattedNavHistory,
      workflowStatus: fund.workflowStatus || fund.workflow_status || 'idle',
      navDisplay: util.formatNav(nav),
      navChangeDisplay: (navChange >= 0 ? '+' : '') + navChange.toFixed(4),
      navChangePctDisplay: util.formatPercent(navChangePct),
      isUp: isUp,
      totalAumDisplay: util.formatMoney(totalAssets),
      dailyPnlDisplay: util.formatMoney(nav > 0 ? navChange * totalAssets / nav : 0),
      sharpeDisplay: '--',
      maxDrawdownDisplay: '--',
    });
  },

  /**
   * 启动 / 暂停工作流
   */
  startWorkflow: function () {
    var that = this;
    if (!this.data.fund || !this.data.fund.id) {
      wx.showToast({ title: '请先选择基金', icon: 'none' });
      return;
    }
    if (this.data.workflowStatus === 'running') {
      // 暂停
      this.setData({ workflowStatus: 'idle', workflowStep: '', currentStepIndex: -1 });
      wx.showToast({ title: '工作流已暂停', icon: 'none' });
      if (this._wfTimer) {
        clearInterval(this._wfTimer);
        this._wfTimer = null;
      }
      return;
    }

    api.startWorkflow(this.data.fund.id).catch(function () {
      wx.showToast({ title: '后端启动失败，仅更新本地状态', icon: 'none' });
    });

    var steps = this.data.workflowSteps;
    var idx = 0;
    this.setData({
      workflowStatus: 'running',
      workflowStep: steps[idx],
      currentStepIndex: idx,
    });

    this._wfTimer = setInterval(function () {
      idx++;
      if (idx >= steps.length) {
        clearInterval(that._wfTimer);
        that._wfTimer = null;
        that.setData({
          workflowStatus: 'completed',
          workflowStep: '全部完成',
          currentStepIndex: steps.length,
        });
        wx.showToast({ title: '工作流已完成', icon: 'success' });
        // 3秒后恢复 idle
        setTimeout(function () {
          that.setData({ workflowStatus: 'idle', workflowStep: '', currentStepIndex: -1 });
        }, 3000);
        return;
      }
      that.setData({
        workflowStep: steps[idx],
        currentStepIndex: idx,
      });
    }, 2000);
  },

  /**
   * 跳转基金详情
   */
  goToFundDetail: function () {
    if (!this.data.fund) return;
    wx.navigateTo({ url: '/packageA/pages/fund-detail/fund-detail?fundId=' + this.data.fund.id });
  },

  /**
   * 跳转交易记录
   */
  goToTrades: function () {
    wx.navigateTo({ url: '/packageA/pages/trades/trades?fundId=' + (this.data.fund ? this.data.fund.id : '') });
  },

  /**
   * 跳转团队管理
   */
  goToTeam: function () {
    wx.switchTab({ url: '/pages/team/team' });
  },

  /**
   * 跳转决策中心
   */
  goToDecision: function () {
    wx.switchTab({ url: '/pages/decision/decision' });
  },
});
