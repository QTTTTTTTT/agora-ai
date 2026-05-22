/**
 * NAV 折线图组件
 * 使用 Canvas 2D 绘制净值走势折线图
 */
Component({
  properties: {
    data: {
      type: Array,
      value: [],
      observer: function () {
        this._drawChart();
      },
    },
    width: {
      type: Number,
      value: 300,
      observer: function () {
        this._drawChart();
      },
    },
    height: {
      type: Number,
      value: 200,
      observer: function () {
        this._drawChart();
      },
    },
  },

  data: {
    canvasId: '',
    dpr: 1,
  },

  lifetimes: {
    attached: function () {
      this.setData({
        canvasId: 'nav-chart-' + Date.now(),
        dpr: wx.getSystemInfoSync().pixelRatio || 2,
      });
    },
    ready: function () {
      this._drawChart();
    },
  },

  methods: {
    _drawChart: function () {
      var that = this;
      var chartData = this.properties.data;
      if (!chartData || chartData.length === 0) return;

      var query = this.createSelectorQuery();
      query
        .select('#navCanvas')
        .fields({ node: true, size: true })
        .exec(function (res) {
          if (!res || !res[0] || !res[0].node) return;

          var canvas = res[0].node;
          var ctx = canvas.getContext('2d');
          var dpr = that.data.dpr;
          var w = that.properties.width;
          var h = that.properties.height;

          canvas.width = w * dpr;
          canvas.height = h * dpr;
          ctx.scale(dpr, dpr);

          that._render(ctx, chartData, w, h);
        });
    },

    _render: function (ctx, data, w, h) {
      // 边距
      var padding = { top: 20, right: 15, bottom: 30, left: 50 };
      var chartW = w - padding.left - padding.right;
      var chartH = h - padding.top - padding.bottom;

      // 清空画布
      ctx.clearRect(0, 0, w, h);

      // 计算数据范围
      var navValues = data.map(function (d) { return d.nav; });
      var minNav = Math.min.apply(null, navValues);
      var maxNav = Math.max.apply(null, navValues);
      var navRange = maxNav - minNav;
      if (navRange === 0) navRange = 0.01;
      // 留 10% 上下空间
      minNav = minNav - navRange * 0.1;
      maxNav = maxNav + navRange * 0.1;
      navRange = maxNav - minNav;

      var len = data.length;

      // 坐标映射函数
      function xPos(i) {
        return padding.left + (i / (len - 1)) * chartW;
      }
      function yPos(val) {
        return padding.top + chartH - ((val - minNav) / navRange) * chartH;
      }

      // ---- 绘制背景网格 ----
      ctx.strokeStyle = '#f0f0f0';
      ctx.lineWidth = 0.5;
      var gridLines = 5;
      for (var g = 0; g <= gridLines; g++) {
        var gy = padding.top + (g / gridLines) * chartH;
        ctx.beginPath();
        ctx.moveTo(padding.left, gy);
        ctx.lineTo(padding.left + chartW, gy);
        ctx.stroke();
      }

      // ---- 绘制 Y 轴标签 ----
      ctx.fillStyle = '#999';
      ctx.font = '10px sans-serif';
      ctx.textAlign = 'right';
      ctx.textBaseline = 'middle';
      for (var y = 0; y <= gridLines; y++) {
        var val = maxNav - (y / gridLines) * navRange;
        var yy = padding.top + (y / gridLines) * chartH;
        ctx.fillText(val.toFixed(4), padding.left - 5, yy);
      }

      // ---- 绘制 X 轴标签 ----
      ctx.textAlign = 'center';
      ctx.textBaseline = 'top';
      var xLabelCount = Math.min(6, len);
      for (var xi = 0; xi < xLabelCount; xi++) {
        var idx = Math.round((xi / (xLabelCount - 1)) * (len - 1));
        var label = data[idx].date || '';
        // 只显示 MM-DD
        if (label.length >= 10) {
          label = label.substring(5, 10);
        }
        ctx.fillText(label, xPos(idx), padding.top + chartH + 8);
      }

      // ---- 绘制渐变填充区域 ----
      var gradient = ctx.createLinearGradient(0, padding.top, 0, padding.top + chartH);
      gradient.addColorStop(0, 'rgba(24, 144, 255, 0.25)');
      gradient.addColorStop(1, 'rgba(24, 144, 255, 0.02)');

      ctx.beginPath();
      ctx.moveTo(xPos(0), yPos(data[0].nav));
      for (var fi = 1; fi < len; fi++) {
        ctx.lineTo(xPos(fi), yPos(data[fi].nav));
      }
      ctx.lineTo(xPos(len - 1), padding.top + chartH);
      ctx.lineTo(xPos(0), padding.top + chartH);
      ctx.closePath();
      ctx.fillStyle = gradient;
      ctx.fill();

      // ---- 绘制基准线（如果有 benchmark 字段）----
      if (data[0].benchmark !== undefined) {
        ctx.strokeStyle = '#faad14';
        ctx.lineWidth = 1;
        ctx.setLineDash([4, 3]);
        ctx.beginPath();
        for (var bi = 0; bi < len; bi++) {
          var bv = data[bi].benchmark;
          if (bi === 0) {
            ctx.moveTo(xPos(bi), yPos(bv));
          } else {
            ctx.lineTo(xPos(bi), yPos(bv));
          }
        }
        ctx.stroke();
        ctx.setLineDash([]);
      }

      // ---- 绘制折线 ----
      ctx.strokeStyle = '#1890ff';
      ctx.lineWidth = 2;
      ctx.lineJoin = 'round';
      ctx.lineCap = 'round';
      ctx.beginPath();
      ctx.moveTo(xPos(0), yPos(data[0].nav));
      for (var li = 1; li < len; li++) {
        ctx.lineTo(xPos(li), yPos(data[li].nav));
      }
      ctx.stroke();

      // ---- 绘制数据点 ----
      // 只在数据量较少时绘制所有点，否则只画首尾和最高最低
      var pointIndices = [];
      if (len <= 15) {
        for (var pi = 0; pi < len; pi++) pointIndices.push(pi);
      } else {
        pointIndices.push(0);
        pointIndices.push(len - 1);
        // 最高点
        var maxIdx = 0;
        var minIdx = 0;
        for (var mi = 1; mi < len; mi++) {
          if (data[mi].nav > data[maxIdx].nav) maxIdx = mi;
          if (data[mi].nav < data[minIdx].nav) minIdx = mi;
        }
        if (pointIndices.indexOf(maxIdx) === -1) pointIndices.push(maxIdx);
        if (pointIndices.indexOf(minIdx) === -1) pointIndices.push(minIdx);
      }

      for (var di = 0; di < pointIndices.length; di++) {
        var pidx = pointIndices[di];
        var px = xPos(pidx);
        var py = yPos(data[pidx].nav);
        // 外圈白色
        ctx.beginPath();
        ctx.arc(px, py, 4, 0, 2 * Math.PI);
        ctx.fillStyle = '#ffffff';
        ctx.fill();
        ctx.strokeStyle = '#1890ff';
        ctx.lineWidth = 2;
        ctx.stroke();
        // 内圈蓝色
        ctx.beginPath();
        ctx.arc(px, py, 2, 0, 2 * Math.PI);
        ctx.fillStyle = '#1890ff';
        ctx.fill();
      }

      // ---- 绘制坐标轴线 ----
      ctx.strokeStyle = '#d9d9d9';
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(padding.left, padding.top);
      ctx.lineTo(padding.left, padding.top + chartH);
      ctx.lineTo(padding.left + chartW, padding.top + chartH);
      ctx.stroke();
    },
  },
});
