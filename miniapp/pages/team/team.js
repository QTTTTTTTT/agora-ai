/**
 * 团队管理页面
 */
var util = require('../../utils/util.js');
var api = require('../../utils/api.js').api;

function resolveFundId() {
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

function normalizeAgent(row) {
  var agent = row.agent || row;
  return {
    id: agent.id || row.agentId || row.id,
    name: agent.name || row.role || '--',
    role: row.role || agent.role || '--',
    model: agent.modelName || agent.llmModel || row.modelName || '--',
    status: row.status || agent.status || 'active',
    style: agent.focus || row.focus || '',
    description: agent.focus || row.focus || '',
    joined_at: row.joinedAt || agent.joinedAt || ''
  };
}

Page({
  data: {
    fundId: '',
    team: [],
    teamCount: 0,
    // 角色分布统计
    roleStats: [],
    // 招聘弹窗
    showHireModal: false,
    selectedRole: 'researcher',
    roles: [
      { value: 'researcher', label: '研究员' },
      { value: 'pm', label: '基金经理' },
      { value: 'risk', label: '风控经理' },
      { value: 'trader', label: '交易员' },
    ],
    availableModels: ['gpt-4o', 'claude-sonnet', 'deepseek-v3', 'qwen-max'],
    selectedModel: 'gpt-4o',
    modelIndex: 0,
    // 可选：投资风格
    styles: [
      { value: 'balanced', label: '均衡型' },
      { value: 'growth', label: '成长型' },
      { value: 'value', label: '价值型' },
      { value: 'conservative', label: '保守型' },
      { value: 'aggressive', label: '激进型' },
      { value: 'momentum', label: '动量型' },
    ],
    selectedStyleIndex: 0,
  },

  onLoad: function () {
    this.setData({ fundId: resolveFundId() });
    this._loadTeam();
  },

  onShow: function () {
    this._loadTeam();
  },

  /**
   * 加载团队数据
   */
  _loadTeam: function () {
    var fundId = this.data.fundId || resolveFundId();
    if (!fundId) {
      this._applyTeam([]);
      return;
    }
    this.setData({ fundId: fundId });
    api.getTeam(fundId).then(function (rows) {
      this._applyTeam((rows || []).map(normalizeAgent));
    }.bind(this)).catch(function () {
      this._applyTeam([]);
    }.bind(this));
  },

  _applyTeam: function (team) {
    var roleCount = {};
    var roleColorMap = {
      pm: '#6366f1',
      researcher: '#22c55e',
      analyst: '#3b82f6',
      risk: '#f59e0b',
      trader: '#ef4444',
    };

    team.forEach(function (agent) {
      var role = agent.role;
      if (!roleCount[role]) {
        roleCount[role] = { role: role, name: util.getRoleName(role), count: 0, color: roleColorMap[role] || '#999' };
      }
      roleCount[role].count++;
    });

    var roleStats = Object.keys(roleCount).map(function (key) {
      var item = roleCount[key];
      item.percent = team.length > 0 ? ((item.count / team.length) * 100).toFixed(0) : 0;
      return item;
    });

    this.setData({
      team: team,
      teamCount: team.length,
      roleStats: roleStats,
    });
  },

  /**
   * 点击 agent 卡片 → 跳转详情
   */
  onAgentTap: function (e) {
    var agent = e.detail.agent;
    if (agent && agent.id) {
      wx.navigateTo({
        url: '/packageB/pages/agent-detail/agent-detail?id=' + agent.id,
      });
    }
  },

  /**
   * 显示招聘弹窗
   */
  showHire: function () {
    this.setData({ showHireModal: true });
  },

  /**
   * 隐藏招聘弹窗
   */
  hideHire: function () {
    this.setData({ showHireModal: false });
  },

  /**
   * 切换角色
   */
  onRoleChange: function (e) {
    this.setData({ selectedRole: e.detail.value });
  },

  /**
   * 切换模型
   */
  onModelChange: function (e) {
    var idx = e.detail.value;
    this.setData({
      modelIndex: idx,
      selectedModel: this.data.availableModels[idx],
    });
  },

  /**
   * 切换风格
   */
  onStyleChange: function (e) {
    this.setData({ selectedStyleIndex: e.detail.value });
  },

  /**
   * 确认招聘（添加 agent）
   */
  confirmHire: function () {
    var that = this;
    var fundId = this.data.fundId || resolveFundId();
    if (!fundId) {
      wx.showToast({ title: '请先选择基金', icon: 'none' });
      return;
    }
    var role = this.data.selectedRole;
    var style = this.data.styles[this.data.selectedStyleIndex].value;
    api.addTeamMember(fundId, { role: role, focus: style }).then(function () {
      that.setData({ showHireModal: false });
      that._loadTeam();
      wx.showToast({ title: '招聘成功', icon: 'success' });
    }).catch(function () {
      wx.showToast({ title: '招聘失败', icon: 'none' });
    });
  },

  /**
   * 解雇 agent（弹确认框后移除）
   */
  fireAgent: function (e) {
    var that = this;
    var agentId = e.currentTarget.dataset.id;
    var agent = this.data.team.filter(function (a) { return a.id === agentId; })[0];
    if (!agent) return;

    wx.showModal({
      title: '确认解雇',
      content: '确定要解雇 ' + agent.name + '（' + util.getRoleName(agent.role) + '）吗？此操作不可撤销。',
      confirmColor: '#ff4d4f',
      success: function (res) {
        if (res.confirm) {
          api.removeTeamMember(that.data.fundId || resolveFundId(), agentId).then(function () {
            that._loadTeam();
            wx.showToast({ title: '已解雇', icon: 'none' });
          }).catch(function () {
            wx.showToast({ title: '解雇失败', icon: 'none' });
          });
        }
      },
    });
  },

  /**
   * 阻止弹窗穿透滚动
   */
  preventScroll: function () {
    // do nothing — 阻止滚动穿透
  },
});
