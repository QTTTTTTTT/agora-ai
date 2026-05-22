const api = require('../../../utils/api.js').api;

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

function statusText(status) {
  const labels = {
    pending: '待开始',
    running: '运行中',
    completed: '已完成',
    analyzed: '已分析',
    stopped: '已停止',
    failed: '失败'
  };
  return labels[status] || status || '--';
}

function variableText(variableType) {
  const labels = {
    model_change: '模型变更',
    strategy_compare: '策略对比',
    prompt_change: '提示词变更',
    risk_param: '风控参数'
  };
  return labels[variableType] || variableType || '--';
}

function formatDuration(test) {
  if (test.startDate && test.endDate) return test.startDate + ' - ' + test.endDate;
  if (test.startDate) return test.startDate + ' - 进行中';
  return '未开始';
}

function metricValue(metrics, key) {
  const value = metrics && metrics[key];
  return Number(value || 0);
}

function buildMetricComparison(results) {
  const variantA = (results && results.variantA) || {};
  const variantB = (results && results.variantB) || {};
  const rows = [
    { label: '总收益率', key: 'total_return', suffix: '%', higherBetter: true },
    { label: '夏普比率', key: 'sharpe_ratio', suffix: '', higherBetter: true },
    { label: '最大回撤', key: 'max_drawdown', suffix: '%', higherBetter: false },
    { label: '波动率', key: 'volatility', suffix: '%', higherBetter: false },
    { label: '胜率', key: 'win_rate', suffix: '%', higherBetter: true },
    { label: '交易次数', key: 'trade_count', suffix: '次', higherBetter: false }
  ];
  return rows.map((row) => {
    const a = metricValue(variantA, row.key);
    const b = metricValue(variantB, row.key);
    const controlWins = row.higherBetter ? a > b : a < b;
    const experimentWins = row.higherBetter ? b > a : b < a;
    return {
      label: row.label,
      controlValue: a.toFixed(row.suffix === '次' ? 0 : 2) + row.suffix,
      experimentValue: b.toFixed(row.suffix === '次' ? 0 : 2) + row.suffix,
      controlWins: controlWins,
      experimentWins: experimentWins
    };
  });
}

function normalizeTest(test) {
  const results = test.results || {};
  const navSeries = results.navSeries || [];
  const decisionDiffs = results.decisionDiffs || [];
  const confidence = results.confidence || {};
  const variableConfig = test.variableConfig || {};
  const winner = results.winner || '';
  return {
    id: test.id,
    name: test.name,
    variable: variableText(test.variableType),
    oldValue: variableConfig.old || variableConfig.control || test.controlFundId || '--',
    newValue: variableConfig.new || variableConfig.treatment || test.treatmentFundId || '--',
    status: test.status,
    statusText: statusText(test.status),
    duration: formatDuration(test),
    winner: winner === 'variant_b' || winner === 'B' ? 'experiment' : winner === 'variant_a' || winner === 'A' ? 'control' : winner,
    winnerText: winner ? (winner === 'variant_b' || winner === 'B' ? '实验组胜出' : '对照组胜出') : '暂无结论',
    confidenceLevel: Math.round(Number(confidence.score || 0) * 100),
    metricsComparison: buildMetricComparison(results),
    navControl: navSeries.map(p => Number(p.variantA || 0)).filter(v => v > 0),
    navExperiment: navSeries.map(p => Number(p.variantB || 0)).filter(v => v > 0),
    divergences: decisionDiffs.map((d) => ({
      date: d.date,
      controlAction: d.variantAAction || '--',
      experimentAction: d.variantBAction || '--',
      impact: (Number(d.returnImpact || 0) * 100).toFixed(2) + '% 差异'
    })),
    llmAnalysis: (confidence && confidence.recommendation) || (results.scorecard && results.scorecard.verdict) || '暂无 AI 分析总结'
  };
}

Page({
  data: {
    fundId: '',
    tests: [],
    currentIndex: 0,
    currentTest: null,
    canvasWidth: 0,
    canvasHeight: 0
  },

  onLoad() {
    const sysInfo = wx.getSystemInfoSync();
    const fundId = resolveFundId();
    this.setData({
      fundId: fundId,
      canvasWidth: sysInfo.windowWidth - 48,
      canvasHeight: 200
    });
    this.loadTests();
  },

  onReady() {
    this.drawChart();
  },

  loadTests() {
    if (!this.data.fundId) {
      this.setData({ tests: [], currentTest: null });
      return;
    }
    api.getABTests(this.data.fundId).then((res) => {
      const tests = (Array.isArray(res) ? res : []).map(normalizeTest);
      this.setData({
        tests: tests,
        currentIndex: 0,
        currentTest: tests[0] || null
      });
      setTimeout(() => this.drawChart(), 100);
    }).catch(() => {
      this.setData({ tests: [], currentTest: null });
    });
  },

  onTabChange(e) {
    const idx = e.currentTarget.dataset.index;
    this.setData({
      currentIndex: idx,
      currentTest: this.data.tests[idx]
    });
    setTimeout(() => this.drawChart(), 100);
  },

  drawChart() {
    const test = this.data.currentTest;
    if (!test || !test.navControl.length || test.navControl.length !== test.navExperiment.length) return;

    const query = wx.createSelectorQuery();
    query.select('#navCanvas').fields({ node: true, size: true }).exec((res) => {
      if (!res || !res[0]) return;
      const canvas = res[0].node;
      const ctx = canvas.getContext('2d');
      const dpr = wx.getSystemInfoSync().pixelRatio;
      const width = res[0].width;
      const height = res[0].height;

      canvas.width = width * dpr;
      canvas.height = height * dpr;
      ctx.scale(dpr, dpr);
      ctx.clearRect(0, 0, width, height);

      const allValues = test.navControl.concat(test.navExperiment);
      const minVal = Math.min.apply(null, allValues) - 1;
      const maxVal = Math.max.apply(null, allValues) + 1;
      const dataLen = test.navControl.length;
      if (dataLen < 2 || maxVal <= minVal) return;

      const padding = { top: 20, right: 20, bottom: 30, left: 40 };
      const chartW = width - padding.left - padding.right;
      const chartH = height - padding.top - padding.bottom;

      ctx.strokeStyle = '#eee';
      ctx.lineWidth = 0.5;
      for (let i = 0; i <= 4; i++) {
        const y = padding.top + (chartH / 4) * i;
        ctx.beginPath();
        ctx.moveTo(padding.left, y);
        ctx.lineTo(width - padding.right, y);
        ctx.stroke();
        const val = (maxVal - (maxVal - minVal) * (i / 4)).toFixed(1);
        ctx.fillStyle = '#999';
        ctx.font = '10px sans-serif';
        ctx.textAlign = 'right';
        ctx.fillText(val, padding.left - 5, y + 4);
      }

      const drawLine = (data, color) => {
        ctx.strokeStyle = color;
        ctx.lineWidth = 2;
        ctx.beginPath();
        data.forEach((val, idx) => {
          const x = padding.left + (chartW / (dataLen - 1)) * idx;
          const y = padding.top + chartH - ((val - minVal) / (maxVal - minVal)) * chartH;
          if (idx === 0) ctx.moveTo(x, y);
          else ctx.lineTo(x, y);
        });
        ctx.stroke();
      };

      drawLine(test.navControl, '#4285F4');
      drawLine(test.navExperiment, '#FF9800');

      ctx.fillStyle = '#4285F4';
      ctx.fillRect(padding.left, height - 15, 16, 8);
      ctx.fillStyle = '#666';
      ctx.font = '10px sans-serif';
      ctx.textAlign = 'left';
      ctx.fillText('对照组', padding.left + 22, height - 7);
      ctx.fillStyle = '#FF9800';
      ctx.fillRect(padding.left + 80, height - 15, 16, 8);
      ctx.fillStyle = '#666';
      ctx.fillText('实验组', padding.left + 102, height - 7);
    });
  },

  onStartTest() {
    if (!this.data.currentTest) return;
    api.startABTest(this.data.currentTest.id).then(() => {
      wx.showToast({ title: '测试已开始', icon: 'success' });
      this.loadTests();
    }).catch(() => {
      wx.showToast({ title: '启动失败', icon: 'none' });
    });
  },

  onStopTest() {
    if (!this.data.currentTest) return;
    wx.showModal({
      title: '停止测试',
      content: '确认停止当前A/B测试？',
      success: (res) => {
        if (res.confirm) {
          api.stopABTest(this.data.currentTest.id).then(() => {
            wx.showToast({ title: '测试已停止', icon: 'none' });
            this.loadTests();
          }).catch(() => {
            wx.showToast({ title: '停止失败', icon: 'none' });
          });
        }
      }
    });
  },

  onAnalyzeTest() {
    if (!this.data.currentTest) return;
    wx.showToast({ title: '正在分析...', icon: 'loading', duration: 1000 });
    api.analyzeABTest(this.data.currentTest.id).then(() => {
      this.loadTests();
    }).catch(() => {
      wx.showToast({ title: '分析失败', icon: 'none' });
    });
  },

  onShareAppMessage() {
    return { title: 'A/B测试' };
  }
});
