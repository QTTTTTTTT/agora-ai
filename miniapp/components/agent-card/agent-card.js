/**
 * Agent 卡片组件
 * 显示头像emoji、名称、角色badge、模型、状态指示灯
 * 点击触发 tap 事件
 */
var util = require('../../utils/util.js');

Component({
  properties: {
    agent: {
      type: Object,
      value: {},
      observer: function (newVal) {
        if (newVal) {
          this._updateDisplay(newVal);
        }
      },
    },
  },

  data: {
    roleIcon: '',
    roleName: '',
    statusColor: '',
    statusText: '',
    styleName: '',
  },

  lifetimes: {
    attached: function () {
      var agent = this.properties.agent;
      if (agent && agent.role) {
        this._updateDisplay(agent);
      }
    },
  },

  methods: {
    _updateDisplay: function (agent) {
      this.setData({
        roleIcon: util.getRoleIcon(agent.role),
        roleName: util.getRoleName(agent.role),
        statusColor: util.getStatusColor(agent.status),
        statusText: this._getStatusText(agent.status),
        styleName: util.getStyleName(agent.style),
      });
    },

    _getStatusText: function (status) {
      var map = {
        active: '运行中',
        idle: '空闲',
        offline: '离线',
        error: '异常',
        busy: '忙碌',
      };
      return map[status] || status || '未知';
    },

    onTap: function () {
      this.triggerEvent('tap', { agent: this.properties.agent });
    },
  },
});
