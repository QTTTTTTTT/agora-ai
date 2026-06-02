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
    partial: '部分成交',
    partial_filled: '部分成交',
    partially_filled: '部分成交',
    pending: '待成交',
    working: '挂单中',
    triggered: '已触发',
    submitted: '已提交',
    cancelled: '已取消',
    rejected: '已拒绝',
    expired: '已过期',
    failed: '失败'
  };
  return labels[status] || status || '--';
}

// isOpenStatus 判断是否仍可取消/改单。与服务端 trade_repo.CancelOrder
// / ReplaceOrderFields 的状态白名单一致。
function isOpenStatus(status) {
  return status === 'pending' || status === 'working' || status === 'triggered' || status === 'partial';
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
  var rawStatus = t.status || '';
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
    status: statusText(rawStatus),
    rawStatus: rawStatus,
    canModify: isOpenStatus(rawStatus),
    orderType: t.orderType || '',
    rawQuantity: toNumber(t.quantity),
    rawPrice: toNumber(t.price),
    stopPrice: toNumber(t.stopPrice),
    trailAmount: toNumber(t.trailAmount),
    trailPercent: toNumber(t.trailPercent),
    displayQty: toNumber(t.displayQty),
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
    // P0-5 — 取消/改单：busyOrderId 用于 disable 当前正在请求的行，
    // actionMessage 是状态条文案，replaceForm 持有改单弹窗的输入值。
    busyOrderId: '',
    actionMessage: null,
    replaceVisible: false,
    replaceForm: {
      id: '',
      stockCode: '',
      orderType: '',
      quantity: '',
      limitPrice: '',
      stopPrice: '',
      trailAmount: '',
      trailPercent: '',
      displayQty: '',
      note: ''
    },
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

  // ---- P0-5: 取消订单 ----
  onCancelOrder(e) {
    const id = e.currentTarget.dataset.id;
    if (!id || !this.data.fundId) return;
    if (this.data.busyOrderId) return;
    const trade = (this.data.trades || []).find(t => t.id === id);
    if (!trade) return;
    const stockTitle = trade.stockCode + ' ' + trade.typeText + ' ' + trade.quantity + '股';
    wx.showModal({
      title: '取消订单',
      content: '确认取消该订单？\n' + stockTitle,
      cancelText: '保留订单',
      confirmText: '确认取消',
      success: (res) => {
        if (!res.confirm) return;
        this.executeCancel(id);
      }
    });
  },

  executeCancel(tradeId) {
    this.setData({ busyOrderId: tradeId, actionMessage: null });
    api.cancelOrder(this.data.fundId, tradeId, 'user_requested', '').then((resp) => {
      var order = resp && resp.order ? resp.order : null;
      this.applyOrderUpdate(order, '订单已取消', 'success');
    }).catch((err) => {
      this.applyOrderUpdate(null, this.formatActionError(err, '取消失败'), 'error');
    }).then(() => {
      this.setData({ busyOrderId: '' });
    });
  },

  // ---- P0-5: 改单 ----
  onOpenReplace(e) {
    const id = e.currentTarget.dataset.id;
    if (!id) return;
    const trade = (this.data.trades || []).find(t => t.id === id);
    if (!trade) return;
    this.setData({
      replaceVisible: true,
      actionMessage: null,
      replaceForm: {
        id: trade.id,
        stockCode: trade.stockCode,
        orderType: trade.orderType,
        quantity: trade.rawQuantity ? String(trade.rawQuantity) : '',
        limitPrice: trade.rawPrice ? String(trade.rawPrice) : '',
        stopPrice: trade.stopPrice ? String(trade.stopPrice) : '',
        trailAmount: trade.trailAmount ? String(trade.trailAmount) : '',
        trailPercent: trade.trailPercent ? String(trade.trailPercent) : '',
        displayQty: trade.displayQty ? String(trade.displayQty) : '',
        note: ''
      }
    });
  },

  onCloseReplace() {
    this.setData({ replaceVisible: false });
  },

  onReplaceFieldInput(e) {
    const field = e.currentTarget.dataset.field;
    if (!field) return;
    var form = Object.assign({}, this.data.replaceForm);
    form[field] = e.detail.value;
    this.setData({ replaceForm: form });
  },

  onSubmitReplace() {
    var form = this.data.replaceForm || {};
    if (!form.id || !this.data.fundId) return;
    if (this.data.busyOrderId) return;

    // payload 仅传输用户填写过的字段；空字符串视为不修改。服务端会
    // 对 quantity < filledQty / 全空 payload / 非数值 等情况返回 4xx。
    var payload = {};
    var fields = ['quantity', 'limitPrice', 'stopPrice', 'trailAmount', 'trailPercent', 'displayQty'];
    var hasNumeric = false;
    for (var i = 0; i < fields.length; i++) {
      var key = fields[i];
      var raw = form[key];
      if (raw === '' || raw === undefined || raw === null) continue;
      var num = Number(raw);
      if (!isFinite(num)) {
        this.applyOrderUpdate(null, '请输入有效的 ' + key, 'error');
        return;
      }
      payload[key] = num;
      hasNumeric = true;
    }
    if (!hasNumeric) {
      this.applyOrderUpdate(null, '至少修改一个数值字段', 'error');
      return;
    }
    if (form.note) payload.note = form.note;

    this.setData({ busyOrderId: form.id });
    api.replaceOrder(this.data.fundId, form.id, payload).then((resp) => {
      var order = resp && resp.order ? resp.order : null;
      this.applyOrderUpdate(order, '订单已更新', 'success');
      this.setData({ replaceVisible: false });
    }).catch((err) => {
      this.applyOrderUpdate(null, this.formatActionError(err, '改单失败'), 'error');
    }).then(() => {
      this.setData({ busyOrderId: '' });
    });
  },

  // applyOrderUpdate 把服务端返回的最新订单状态合并回 trades / filtered，
  // 避免整页 reload。注意 cancel/replace 接口返回的是 trim 后的
  // orderResponse（id/status/quantity/limitPrice/...），并不是完整
  // Trade，所以我们把字段合并进现有行，而不是用 normalizeTrade 整体覆盖。
  applyOrderUpdate(order, message, tone) {
    var trades = this.data.trades.slice();
    if (order && order.id) {
      var idx = trades.findIndex(t => t.id === order.id);
      if (idx >= 0) {
        var existing = trades[idx];
        var merged = Object.assign({}, existing);
        if (order.status !== undefined) {
          merged.rawStatus = order.status;
          merged.status = statusText(order.status);
          merged.canModify = isOpenStatus(order.status);
        }
        if (order.quantity !== undefined && order.quantity !== null) {
          merged.rawQuantity = toNumber(order.quantity);
          // P0-5 — 改单后只有未成交部分会更新，已成交数量保持不变。
          // quantity 列展示原始下单量；若 normalizeTrade 已使用 filled
          // 量在 quantity 字段，这里我们覆盖为新的下单量以保持一致。
          merged.quantity = toNumber(order.quantity);
        }
        if (order.limitPrice !== undefined && order.limitPrice !== null) {
          merged.rawPrice = toNumber(order.limitPrice);
          if (toNumber(order.limitPrice)) merged.price = toNumber(order.limitPrice).toFixed(2);
        }
        if (order.stopPrice !== undefined) merged.stopPrice = toNumber(order.stopPrice);
        if (order.trailAmount !== undefined) merged.trailAmount = toNumber(order.trailAmount);
        if (order.trailPercent !== undefined) merged.trailPercent = toNumber(order.trailPercent);
        if (order.displayQty !== undefined) merged.displayQty = toNumber(order.displayQty);
        trades[idx] = merged;
      }
    }
    this.setData({
      trades: trades,
      actionMessage: message ? { text: message, tone: tone || 'info' } : null
    });
    this.applyFilter();
  },

  formatActionError(err, fallback) {
    if (!err) return fallback;
    if (err.errMsg && typeof err.errMsg === 'string') return err.errMsg;
    if (err.message) return err.message;
    if (typeof err === 'string') return err;
    return fallback;
  },

  onDismissActionMessage() {
    this.setData({ actionMessage: null });
  },

  onShareAppMessage() {
    return { title: '交易记录' };
  }
});
