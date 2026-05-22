/**
 * 指标卡片组件
 * 显示指标名称、数值、副值、趋势箭头和颜色
 */
Component({
  properties: {
    label: {
      type: String,
      value: '',
    },
    value: {
      type: String,
      value: '--',
    },
    subValue: {
      type: String,
      value: '',
    },
    trend: {
      type: String,
      value: 'flat', // up / down / flat
    },
    icon: {
      type: String,
      value: '',
    },
  },

  data: {
    trendArrow: '',
    trendColor: '',
    trendClass: '',
  },

  observers: {
    'trend': function (trend) {
      var arrow = '';
      var color = '';
      var cls = '';
      switch (trend) {
        case 'up':
          arrow = '↑';
          color = '#ff4d4f';
          cls = 'trend-up';
          break;
        case 'down':
          arrow = '↓';
          color = '#52c41a';
          cls = 'trend-down';
          break;
        default:
          arrow = '→';
          color = '#999';
          cls = 'trend-flat';
      }
      this.setData({
        trendArrow: arrow,
        trendColor: color,
        trendClass: cls,
      });
    },
  },

  lifetimes: {
    attached: function () {
      // 初始化趋势状态
      var trend = this.properties.trend;
      var arrow = '';
      var color = '';
      var cls = '';
      switch (trend) {
        case 'up':
          arrow = '↑';
          color = '#ff4d4f';
          cls = 'trend-up';
          break;
        case 'down':
          arrow = '↓';
          color = '#52c41a';
          cls = 'trend-down';
          break;
        default:
          arrow = '→';
          color = '#999';
          cls = 'trend-flat';
      }
      this.setData({
        trendArrow: arrow,
        trendColor: color,
        trendClass: cls,
      });
    },
  },

  methods: {},
});
