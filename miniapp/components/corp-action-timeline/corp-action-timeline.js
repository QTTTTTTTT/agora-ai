/**
 * corp-action-timeline 小程序组件 — Card I 与 Web /
 * Android 端的 CorpActionTimelineCard 一一对应。
 *
 * 调 GET /api/funds/:fundId/corp-actions?limit=N，按 ex_date
 * desc 渲染。每行展示：
 *   - 除权除息日（YYYY-MM-DD）
 *   - 标的（instrumentKey 后缀）
 *   - 类型 badge（split / cash_dividend / combined / reverse_split）
 *   - 简短摘要（"+87 股拆股"、"派息 ¥47.40"）
 *   - 持仓数量 / 现金贴现变化
 *
 * 服务端 503（CorpActionService 未启用）时降级为「公司行动服务
 * 未启用」，不当作错误闪红。
 *
 * 默认折叠，点击 header 展开后 lazy fetch；首页空间紧张，避
 * 免冷启动时多发一次请求。
 */

var apiUtil = require('../../utils/api.js');
var api = apiUtil.api;

// 与 web/Android 中文文案保持一致；英文版本暂不切换语言（小
// 程序当前没有 i18n 切换）。映射 actionType → badge 文案。
var TYPE_LABEL = {
  split: '拆股',
  cash_dividend: '派息',
  combined: '送股 + 派息',
  reverse_split: '缩股',
};

Component({
  properties: {
    fundId: {
      type: String,
      value: '',
      observer: function (newVal, oldVal) {
        if (newVal && newVal !== oldVal && this.data.open) {
          this._reload();
        }
      },
    },
    /** 上限多少条；默认 20，与 web/Android 一致。 */
    limit: {
      type: Number,
      value: 20,
    },
    /** 默认是否展开。首页传 false。 */
    defaultOpen: {
      type: Boolean,
      value: false,
    },
  },

  data: {
    open: false,
    loading: false,
    errorMessage: '',
    items: [],
    subtitle: '拆股 / 派息明细',
  },

  lifetimes: {
    attached: function () {
      this.setData({ open: !!this.properties.defaultOpen });
      if (this.properties.defaultOpen && this.properties.fundId) {
        this._reload();
      }
    },
  },

  methods: {
    toggleOpen: function () {
      var nextOpen = !this.data.open;
      this.setData({ open: nextOpen });
      if (nextOpen && (this.data.items.length === 0 || this.data.errorMessage)) {
        this._reload();
      }
    },

    reload: function () {
      this._reload();
    },

    _reload: function () {
      var that = this;
      var fundId = this.properties.fundId;
      if (!fundId) {
        this.setData({ items: [], loading: false, errorMessage: '' });
        return;
      }
      this.setData({ loading: true, errorMessage: '' });
      api
        .getCorpActions(fundId, { limit: this.properties.limit || 20 })
        .then(function (resp) {
          // 旧后端可能直接返回数组；新后端返回 { items, count }。两者都吞下。
          var rawItems = [];
          if (resp && resp.items) {
            rawItems = resp.items;
          } else if (Array.isArray(resp)) {
            rawItems = resp;
          }
          var formatted = rawItems.map(formatRow);
          that.setData({
            loading: false,
            items: formatted,
            errorMessage: '',
            subtitle:
              formatted.length > 0
                ? '近 ' + formatted.length + ' 笔'
                : '近 90 天无事件',
          });
        })
        .catch(function (err) {
          var msg = '加载失败';
          // 503 → 后端没接 corp action 服务
          if (err && err.code === 503) {
            msg = '公司行动服务未启用';
          } else if (err && err.message) {
            msg = err.message;
          }
          that.setData({
            loading: false,
            items: [],
            errorMessage: msg,
            subtitle: '拆股 / 派息明细',
          });
        });
    },
  },
});

function formatRow(item) {
  var actionType = item.actionType || 'unknown';
  var preQty = numericOr(item.preQuantity, 0);
  var postQty = numericOr(item.postQuantity, 0);
  var shareDelta = postQty - preQty;
  var cashCredit = numericOr(item.cashCredit, 0);
  var splitRatio = numericOr(item.splitRatio, 1);
  var cashDiv = numericOr(item.cashDividend, 0);
  // rowKey 用 instrumentKey + exDate 唯一组合；多个 fund 共
  // 享一个 corp action 时这里仍唯一。
  var rowKey = (item.instrumentKey || 'unknown') + '|' + (item.exDate || '');

  // 摘要：拆股写"1:N"，派息写每股金额，combined 写两边。
  var summary = '';
  if (actionType === 'split' || actionType === 'reverse_split') {
    summary = '拆股 1 : ' + splitRatio.toFixed(2);
  } else if (actionType === 'cash_dividend') {
    summary = '派息 ¥' + cashDiv.toFixed(4) + '/股';
  } else if (actionType === 'combined') {
    summary = '送转 ' + splitRatio.toFixed(2) + ' + 派息 ¥' + cashDiv.toFixed(4);
  } else {
    summary = actionType;
  }

  var shareDeltaLabel = '';
  if (Math.abs(shareDelta) >= 0.5) {
    var sign = shareDelta >= 0 ? '+' : '';
    shareDeltaLabel = sign + Math.round(shareDelta) + ' 股';
  }

  var cashCreditLabel = '';
  if (cashCredit > 0.001) {
    cashCreditLabel = '现金 +¥' + cashCredit.toFixed(2);
  }

  return {
    rowKey: rowKey,
    actionType: actionType,
    typeLabel: TYPE_LABEL[actionType] || actionType,
    instrumentLabel: shortInstrumentLabel(item.instrumentKey),
    exDateLabel: shortDate(item.exDate),
    summary: summary,
    shareDeltaLabel: shareDeltaLabel,
    shareDeltaUp: shareDelta >= 0,
    cashCreditLabel: cashCreditLabel,
  };
}

function shortInstrumentLabel(key) {
  if (!key) return '';
  // 形如 "INSTR:688195.SS" → "688195.SS"；其他格式按原样。
  var idx = key.indexOf(':');
  if (idx >= 0) return key.substring(idx + 1);
  return key;
}

function shortDate(s) {
  if (!s) return '';
  if (s.length >= 10) return s.substring(0, 10);
  return s;
}

function numericOr(v, fallback) {
  var n = Number(v);
  return isFinite(n) ? n : fallback;
}
