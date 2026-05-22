var util = require('../../../utils/util.js');
var api = require('../../../utils/api.js').api;

function pad2(n) {
  return String(n).padStart(2, '0');
}

function todayString() {
  var now = new Date();
  return now.getFullYear() + '-' + pad2(now.getMonth() + 1) + '-' + pad2(now.getDate());
}

function firstDayOfMonthString() {
  var now = new Date();
  return now.getFullYear() + '-' + pad2(now.getMonth() + 1) + '-01';
}

function parseDate(value) {
  if (!value) return null;
  var date = new Date(value);
  return isNaN(date.getTime()) ? null : date;
}

function toNumber(value) {
  var n = Number(value || 0);
  return isFinite(n) ? n : 0;
}

function sumFees(trade) {
  return toNumber(trade.feeCommission) + toNumber(trade.feeStampTax) + toNumber(trade.feeTransfer);
}

function sideText(side) {
  if (side === 'buy') return '买入';
  if (side === 'sell') return '卖出';
  return side || '--';
}

function statusText(status) {
  var labels = {
    filled: '已成交',
    partial_filled: '部分成交',
    partially_filled: '部分成交',
    pending: '待成交',
    submitted: '已提交',
    cancelled: '已取消',
    rejected: '已拒绝',
    failed: '失败'
  };
  return labels[status] || status || '--';
}

function normalizeTrade(t) {
  var executedAt = parseDate(t.executedAt || t.createdAt);
  var date = executedAt ? executedAt.getFullYear() + '-' + pad2(executedAt.getMonth() + 1) + '-' + pad2(executedAt.getDate()) : '';
  var time = executedAt ? pad2(executedAt.getHours()) + ':' + pad2(executedAt.getMinutes()) + ':' + pad2(executedAt.getSeconds()) : '';
  var side = t.side || '';
  var quantity = toNumber(t.filledQty || t.quantity);
  var price = toNumber(t.filledPrice || t.price);
  var amount = toNumber(t.amount) || quantity * price;
  var fee = sumFees(t);
  return {
    id: t.id,
    date: date,
    time: time,
    type: side,
    typeText: sideText(side),
    stockCode: t.symbol || t.instrumentKey || '--',
    stockName: t.instrumentKey || t.symbol || '--',
    quantity: quantity,
    price: price.toFixed(2),
    amount: util.formatMoney(amount),
    amountValue: amount,
    fee: util.formatMoney(fee),
    feeValue: fee,
    status: statusText(t.status),
    strategy: t.orderType || t.tradingMode || '--',
    slippage: '--',
    subOrders: 1
  };
}

function resolveFundId(options) {
  if (options && (options.fundId || options.id)) return options.fundId || options.id;
  try {
    var app = getApp();
    if (app && app.globalData && app.globalData.currentFund && app.globalData.currentFund.id) {
      return app.globalData.currentFund.id;
    }
  } catch (e) {}
  var storedFund = wx.getStorageSync('currentFund');
  if (storedFund && storedFund.id) return storedFund.id;
  return wx.getStorageSync('currentFundId') || '';
}

Page({
  data: {
    fundId: '',
    loading: false,
    // 筛选
    filterTab: 'all', // all / buy / sell
    searchKey: '',
    dateStart: firstDayOfMonthString(),
    dateEnd: todayString(),
    // 统计
    stats: {
      total: 0,
      buyCount: 0,
      sellCount: 0,
      totalFee: '0.00'
    },
    // 交易列表
    trades: [],
    filteredTrades: [],
    // 展开状态
    expandedId: '',
    // 底部统计
    totalAmount: '0.00',
    totalFees: '0.00'
  },

  onLoad(options) {
    var fundId = resolveFundId(options || {});
    this.setData({ fundId: fundId });
    this.loadTrades();
  },

  onPullDownRefresh() {
    this.loadTrades().then(function () {
      wx.stopPullDownRefresh();
    }).catch(function () {
      wx.stopPullDownRefresh();
    });
  },

  loadTrades() {
    if (!this.data.fundId) {
      this.setData({ trades: [], filteredTrades: [] });
      this.applyFilter();
      return Promise.resolve();
    }

    this.setData({ loading: true });
    return api.getTrades(this.data.fundId, { limit: 200, offset: 0 }).then((res) => {
      var rows = Array.isArray(res) ? res : [];
      var trades = rows.map(normalizeTrade);
      this.setData({ trades: trades, loading: false });
      this.applyFilter();
    }).catch(() => {
      this.setData({ trades: [], filteredTrades: [], loading: false });
      this.applyFilter();
    });
  },

  onFilterTab(e) {
    const tab = e.currentTarget.dataset.tab;
    this.setData({ filterTab: tab });
    this.applyFilter();
  },

  onSearchInput(e) {
    this.setData({ searchKey: e.detail.value });
    this.applyFilter();
  },

  onDateStartChange(e) {
    this.setData({ dateStart: e.detail.value });
    this.applyFilter();
  },

  onDateEndChange(e) {
    this.setData({ dateEnd: e.detail.value });
    this.applyFilter();
  },

  applyFilter() {
    const { trades, filterTab, searchKey, dateStart, dateEnd } = this.data;
    let filtered = trades.slice();

    if (filterTab === 'buy') {
      filtered = filtered.filter(t => t.type === 'buy');
    } else if (filterTab === 'sell') {
      filtered = filtered.filter(t => t.type === 'sell');
    }

    if (searchKey) {
      const key = searchKey.toUpperCase();
      filtered = filtered.filter(t =>
        String(t.stockCode || '').toUpperCase().includes(key) || String(t.stockName || '').toUpperCase().includes(key)
      );
    }

    filtered = filtered.filter(t => !t.date || (t.date >= dateStart && t.date <= dateEnd));

    const buyCount = filtered.filter(t => t.type === 'buy').length;
    const sellCount = filtered.filter(t => t.type === 'sell').length;
    const totalFeeValue = filtered.reduce((sum, t) => sum + toNumber(t.feeValue), 0);
    const totalAmountValue = filtered.reduce((sum, t) => sum + toNumber(t.amountValue), 0);

    this.setData({
      filteredTrades: filtered,
      stats: {
        total: filtered.length,
        buyCount: buyCount,
        sellCount: sellCount,
        totalFee: util.formatMoney(totalFeeValue)
      },
      totalAmount: util.formatMoney(totalAmountValue),
      totalFees: util.formatMoney(totalFeeValue)
    });
  },

  onToggleExpand(e) {
    const id = e.currentTarget.dataset.id;
    this.setData({
      expandedId: this.data.expandedId === id ? '' : id
    });
  },

  onShareAppMessage() {
    return { title: '交易记录' };
  }
});
