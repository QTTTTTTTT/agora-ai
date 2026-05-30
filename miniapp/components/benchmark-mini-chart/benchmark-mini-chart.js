/**
 * benchmark-mini-chart 小程序组件 — Card I 与 Web/Android 端的
 * BenchmarkChart / BenchmarkMiniChart 一一对应。
 *
 * - 调 GET /api/funds/:fundId/benchmark-history?days=N，服务端
 *   已经 rebase 到 100，前端只负责把 [yMin, yMax] 区间映射到
 *   canvas 坐标。
 * - 默认 90d；用户可切 30 / 90 / 365。
 * - 同时绘 fund 主线（indigo）和第一个推荐 benchmark（sky）。
 *   小程序屏幕窄，多于一条 benchmark 就过密；picker 暂不
 *   实现，由服务端推荐。
 * - holdingOverlap 字段透传给 wxml 的 banner 组件，让用户
 *   理解为什么两条线几乎重合。
 *
 * 调用方只需传 fundId：
 *
 *   <benchmark-mini-chart fund-id="{{fund.id}}" />
 */

var apiUtil = require('../../utils/api.js');
var api = apiUtil.api;

var DEFAULT_RANGE_OPTIONS = [
  { id: '30', days: 30, label: '30天' },
  { id: '90', days: 90, label: '90天' },
  { id: '365', days: 365, label: '1年' },
];

// 与 Web/Android 端 benchmark.holdingOverlap* 文案保持一致
// （shared/api-client/src/i18n.ts 的 zh-CN 串）。在小程序端
// 我们暂不切换语言，统一用中文。
var OVERLAP_COPY = {
  dominant: {
    title: '基金主仓与基准重合',
    body:
      '当前主仓与所选基准是同一标的，比较视图里两条线会几乎重合。请关注 alpha 视图（Web 端）以查看相对超额收益。',
  },
  partial: {
    title: '持仓与基准存在重合',
    body:
      '部分持仓与基准重合，比较视图可读性会下降。Web 端可切换到 alpha 视图查看相对走势。',
  },
};

Component({
  properties: {
    fundId: {
      type: String,
      value: '',
      observer: function (newVal, oldVal) {
        if (newVal && newVal !== oldVal) {
          this._reload();
        }
      },
    },
    /** 卡片宽度（rpx 仅作内容布局用，canvas 仍用 px）。
     *  小程序屏幕宽 750rpx，默认留 16rpx*2 边距 → 718rpx。 */
    cardWidthPx: {
      type: Number,
      value: 0,
    },
  },

  data: {
    rangeOptions: DEFAULT_RANGE_OPTIONS,
    rangeId: '90',
    rangeLabel: '近 90 天',

    canvasWidth: 320,
    canvasHeight: 180,

    fundLabel: '基金',
    benchmarkLabel: '',

    loading: false,
    errorMessage: '',
    hasData: false,

    holdingOverlap: null,
    overlapTitle: '',
    overlapBody: '',
    partialFailureToast: '',

    // 缓存最近一次响应，切 range 后给 canvas 复用
    _response: null,
  },

  lifetimes: {
    attached: function () {
      this._resolveCanvasWidth();
    },
    ready: function () {
      this._reload();
    },
  },

  observers: {
    'cardWidthPx': function (w) {
      if (w && w > 0) {
        this.setData({ canvasWidth: Math.max(w - 48, 200) });
        // 已有数据时尺寸变化要重画，否则线条比例错位
        if (this.data._response) this._render(this.data._response);
      }
    },
  },

  methods: {
    _resolveCanvasWidth: function () {
      // 在 attached 时父容器还未 layout 完成，先用 systemInfo 估
      // 一个安全值：屏幕宽 - 卡片左右各 16rpx 边距 - 卡片内
      // padding 24rpx*2。1rpx ≈ 0.5px 在多数机型上，但小程序的
      // canvas 用 px，因此用 windowWidth 直接取 px。
      var info;
      try {
        info = wx.getSystemInfoSync();
      } catch (e) {
        info = { windowWidth: 360 };
      }
      var screenPx = info && info.windowWidth ? info.windowWidth : 360;
      // 卡片外边距 16rpx*2 + 卡片 padding 24rpx*2 ≈ 80rpx ≈ 40px。
      var canvasWidth = Math.max(screenPx - 40, 200);
      this.setData({ canvasWidth: canvasWidth });
    },

    onSelectRange: function (e) {
      var nextId = e && e.currentTarget && e.currentTarget.dataset && e.currentTarget.dataset.id;
      if (!nextId || nextId === this.data.rangeId) return;
      var match = DEFAULT_RANGE_OPTIONS.filter(function (o) { return o.id === nextId; })[0];
      if (!match) return;
      this.setData({
        rangeId: match.id,
        rangeLabel: '近 ' + match.label.replace('天', ' 天'),
      });
      this._reload();
    },

    reload: function () {
      this._reload();
    },

    _reload: function () {
      var that = this;
      var fundId = this.properties.fundId;
      if (!fundId) {
        this.setData({ loading: false, hasData: false, errorMessage: '' });
        return;
      }
      var match = DEFAULT_RANGE_OPTIONS.filter(function (o) { return o.id === that.data.rangeId; })[0];
      var days = match ? match.days : 90;

      this.setData({ loading: true, errorMessage: '', hasData: false });
      api
        .getBenchmarkHistory(fundId, days)
        .then(function (resp) {
          if (!resp || !resp.fund || !resp.fund.points || resp.fund.points.length < 2) {
            that.setData({
              loading: false,
              hasData: false,
              errorMessage: '',
            });
            return;
          }
          that.setData({ _response: resp });
          that._render(resp);
        })
        .catch(function (err) {
          // 503 → 后端没启 ohlc 服务，降级文案；其他错误展示通用提示。
          var msg = '走势加载失败';
          if (err && err.code === 503) {
            msg = '基准走势服务未启用';
          } else if (err && err.message) {
            msg = err.message;
          }
          that.setData({
            loading: false,
            hasData: false,
            errorMessage: msg,
          });
        });
    },

    _render: function (resp) {
      var fundSeries = resp.fund;
      var benchmarks = resp.benchmarks || [];
      var firstBench = benchmarks.length > 0 ? benchmarks[0] : null;
      var partialFailures = resp.partialFailures || [];
      var holdingOverlap = resp.holdingOverlap || null;

      var overlapTitle = '';
      var overlapBody = '';
      if (holdingOverlap && holdingOverlap.overlapStrength && OVERLAP_COPY[holdingOverlap.overlapStrength]) {
        overlapTitle = OVERLAP_COPY[holdingOverlap.overlapStrength].title;
        overlapBody = OVERLAP_COPY[holdingOverlap.overlapStrength].body;
      }

      var partialFailureToast = '';
      if (partialFailures.length > 0) {
        partialFailureToast =
          '部分基准未能加载，已跳过：' +
          partialFailures
            .map(function (f) { return f.id || ''; })
            .filter(function (s) { return !!s; })
            .join(', ');
      }

      this.setData({
        loading: false,
        hasData: true,
        errorMessage: '',
        fundLabel: fundSeries.label || '基金',
        benchmarkLabel: firstBench ? firstBench.label : '',
        holdingOverlap: holdingOverlap,
        overlapTitle: overlapTitle,
        overlapBody: overlapBody,
        partialFailureToast: partialFailureToast,
      });

      this._drawCanvas(fundSeries, firstBench);
    },

    _drawCanvas: function (fundSeries, benchSeries) {
      var that = this;
      // 切 range / 首次绘制时父级还在算布局，延一帧拿 canvas
      // 节点（与 nav-chart 行为一致）。
      var query = this.createSelectorQuery();
      query
        .select('#benchmarkCanvas')
        .fields({ node: true, size: true })
        .exec(function (res) {
          if (!res || !res[0] || !res[0].node) return;
          var canvas = res[0].node;
          var ctx = canvas.getContext('2d');
          var dpr = 2;
          try {
            dpr = wx.getSystemInfoSync().pixelRatio || 2;
          } catch (e) {}
          var w = that.data.canvasWidth;
          var h = that.data.canvasHeight;
          canvas.width = w * dpr;
          canvas.height = h * dpr;
          ctx.scale(dpr, dpr);
          that._renderLines(ctx, w, h, fundSeries, benchSeries);
        });
    },

    /**
     * 双线归一化 + 平铺到 canvas。fund 实线，benchmark 虚线，
     * 两条共享 y-axis 的 [yMin, yMax]，因为服务端已 rebase 到
     * 100，所以两条线本身可比。x 轴用日期并集索引保证两条线
     * 对齐。
     */
    _renderLines: function (ctx, w, h, fundSeries, benchSeries) {
      var padding = { top: 16, right: 12, bottom: 28, left: 36 };
      var chartW = w - padding.left - padding.right;
      var chartH = h - padding.top - padding.bottom;

      ctx.clearRect(0, 0, w, h);

      // 收集日期并集
      var dateSet = {};
      for (var i = 0; i < fundSeries.points.length; i++) {
        dateSet[fundSeries.points[i].date] = true;
      }
      if (benchSeries) {
        for (var j = 0; j < benchSeries.points.length; j++) {
          dateSet[benchSeries.points[j].date] = true;
        }
      }
      var sortedDates = Object.keys(dateSet).sort();
      if (sortedDates.length < 2) {
        // 没东西可画
        return;
      }
      var dateIndex = {};
      for (var k = 0; k < sortedDates.length; k++) {
        dateIndex[sortedDates[k]] = k;
      }

      // y 范围
      var yMin = Infinity;
      var yMax = -Infinity;
      function trackVals(points) {
        for (var p = 0; p < points.length; p++) {
          var v = points[p].value;
          if (typeof v !== 'number') continue;
          if (v < yMin) yMin = v;
          if (v > yMax) yMax = v;
        }
      }
      trackVals(fundSeries.points);
      if (benchSeries) trackVals(benchSeries.points);
      if (!isFinite(yMin) || !isFinite(yMax) || yMin === yMax) {
        yMin = (yMin || 100) - 1;
        yMax = (yMax || 100) + 1;
      } else {
        var span = yMax - yMin;
        yMin -= span * 0.05;
        yMax += span * 0.05;
      }

      function xPos(idx) {
        return padding.left + (idx / (sortedDates.length - 1)) * chartW;
      }
      function yPos(val) {
        return padding.top + chartH - ((val - yMin) / (yMax - yMin)) * chartH;
      }

      // 网格
      ctx.strokeStyle = '#f3f4f6';
      ctx.lineWidth = 0.5;
      var gridLines = 4;
      for (var g = 0; g <= gridLines; g++) {
        var gy = padding.top + (g / gridLines) * chartH;
        ctx.beginPath();
        ctx.moveTo(padding.left, gy);
        ctx.lineTo(padding.left + chartW, gy);
        ctx.stroke();
      }

      // y 轴标签（最大、最小、100 基线）
      ctx.fillStyle = '#9ca3af';
      ctx.font = '10px sans-serif';
      ctx.textAlign = 'right';
      ctx.textBaseline = 'middle';
      ctx.fillText(yMax.toFixed(1), padding.left - 4, padding.top);
      ctx.fillText('100', padding.left - 4, yPos(100));
      ctx.fillText(yMin.toFixed(1), padding.left - 4, padding.top + chartH);

      // x 轴标签（首尾）
      ctx.textAlign = 'center';
      ctx.textBaseline = 'top';
      var firstDate = sortedDates[0];
      var lastDate = sortedDates[sortedDates.length - 1];
      ctx.fillText(shortDate(firstDate), padding.left, padding.top + chartH + 6);
      ctx.fillText(shortDate(lastDate), padding.left + chartW, padding.top + chartH + 6);

      // 100 基准虚线（对应「rebased to 100」）
      ctx.strokeStyle = '#e5e7eb';
      ctx.lineWidth = 1;
      ctx.setLineDash([4, 4]);
      var y100 = yPos(100);
      ctx.beginPath();
      ctx.moveTo(padding.left, y100);
      ctx.lineTo(padding.left + chartW, y100);
      ctx.stroke();
      ctx.setLineDash([]);

      // benchmark 线（先画，让 fund 线在上面）
      if (benchSeries && benchSeries.points && benchSeries.points.length >= 2) {
        ctx.strokeStyle = '#0ea5e9';
        ctx.lineWidth = 1.5;
        ctx.beginPath();
        var benchStarted = false;
        for (var bi = 0; bi < benchSeries.points.length; bi++) {
          var bp = benchSeries.points[bi];
          var bIdx = dateIndex[bp.date];
          if (bIdx === undefined) continue;
          var bx = xPos(bIdx);
          var by = yPos(bp.value);
          if (!benchStarted) {
            ctx.moveTo(bx, by);
            benchStarted = true;
          } else {
            ctx.lineTo(bx, by);
          }
        }
        ctx.stroke();
      }

      // fund 线
      ctx.strokeStyle = '#4f46e5';
      ctx.lineWidth = 2;
      ctx.lineJoin = 'round';
      ctx.lineCap = 'round';
      ctx.beginPath();
      var fundStarted = false;
      for (var fi = 0; fi < fundSeries.points.length; fi++) {
        var fp = fundSeries.points[fi];
        var fIdx = dateIndex[fp.date];
        if (fIdx === undefined) continue;
        var fx = xPos(fIdx);
        var fy = yPos(fp.value);
        if (!fundStarted) {
          ctx.moveTo(fx, fy);
          fundStarted = true;
        } else {
          ctx.lineTo(fx, fy);
        }
      }
      ctx.stroke();
    },
  },
});

function shortDate(s) {
  if (!s) return '';
  if (s.length >= 10) return s.substring(5, 10);
  return s;
}
