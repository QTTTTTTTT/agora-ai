var api = require('../../../utils/api.js').api;

function resolveFundId(options) {
  if (options && options.fundId) return options.fundId;
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

function parseJSON(value) {
  if (!value) return null;
  if (typeof value === 'object') return value;
  try {
    return JSON.parse(value);
  } catch (e) {
    return null;
  }
}

function formatDateTime(value) {
  if (!value) return '-';
  var date = new Date(value);
  if (isNaN(date.getTime())) return String(value);
  var y = date.getFullYear();
  var m = String(date.getMonth() + 1).padStart(2, '0');
  var d = String(date.getDate()).padStart(2, '0');
  var hh = String(date.getHours()).padStart(2, '0');
  var mm = String(date.getMinutes()).padStart(2, '0');
  return y + '-' + m + '-' + d + ' ' + hh + ':' + mm;
}

function roleText(role) {
  var map = {
    pm: '基金经理',
    researcher: '研究员',
    analyst: '分析师',
    risk: '风控经理',
    trader: '交易员'
  };
  return map[role] || role || '--';
}

function roleEmoji(role) {
  var map = {
    pm: '📊',
    researcher: '🔎',
    analyst: '📈',
    risk: '🛡️',
    trader: '⚡'
  };
  return map[role] || '🤖';
}

function statusText(status) {
  var normalized = status === 'active' ? 'online' : (status || 'offline');
  var map = { online: '在线', active: '在线', busy: '忙碌', offline: '离线', inactive: '离线' };
  return map[normalized] || normalized;
}

function progressValue(value) {
  var n = Math.abs(Number(value || 0));
  if (!isFinite(n)) return 0;
  if (n <= 1) return Math.min(100, Math.round(n * 100));
  return Math.min(100, Math.round(n));
}

function normalizeAgent(row) {
  row = row || {};
  var skillConfig = parseJSON(row.skillConfig);
  var domainConfig = parseJSON(row.domainConfig);
  var evolutionConfig = parseJSON(row.evolutionConfig);
  var model = row.modelName || row.llmModel || row.modelProvider || '--';
  if (row.modelProvider && row.modelName) model = row.modelProvider + ' / ' + row.modelName;
  var focus = row.focus || (domainConfig && domainConfig.focus) || '';

  return {
    id: row.id || row.agentId,
    emoji: roleEmoji(row.role),
    name: row.name || roleText(row.role),
    role: roleText(row.role),
    roleKey: row.role || '',
    status: row.status === 'active' ? 'online' : (row.status || 'offline'),
    statusText: statusText(row.status),
    model: model,
    modelProvider: row.modelProvider || '',
    modelName: row.modelName || row.llmModel || '',
    investStyle: focus || '未配置',
    createdTime: formatDateTime(row.joinedAt),
    fundId: row.fundId || '',
    hasCustomModelConfig: !!row.hasCustomModelConfig,
    latestLearningSummary: row.latestLearningSummary || '',
    latestLearningAt: row.latestLearningAt || '',
    latestLearningReturn: row.latestLearningReturn,
    latestLearningTags: row.latestLearningTags || [],
    skillConfig: skillConfig,
    domainConfig: domainConfig,
    evolutionConfig: evolutionConfig
  };
}

function buildCapabilities(agent, learning) {
  var result = [];
  if (agent.hasCustomModelConfig || agent.modelName || agent.modelProvider) {
    result.push({ name: '模型配置', value: agent.hasCustomModelConfig ? 100 : 60, valueText: agent.hasCustomModelConfig ? '独立配置' : '默认配置', color: '#667eea' });
  }
  var dailyReturn = learning && learning.lastDailyReturn !== undefined ? learning.lastDailyReturn : agent.latestLearningReturn;
  if (dailyReturn !== undefined && dailyReturn !== null) {
    var pct = Number(dailyReturn) * 100;
    result.push({ name: '最近学习收益', value: progressValue(pct), valueText: pct.toFixed(2) + '%', color: pct >= 0 ? '#43a047' : '#e53935' });
  }
  var lessons = (learning && learning.recentLessons) || [];
  if (lessons.length > 0) {
    result.push({ name: '近期经验数', value: Math.min(100, lessons.length * 20), valueText: String(lessons.length), color: '#ff9800' });
  }
  var tags = (learning && learning.lastLearningTags) || agent.latestLearningTags || [];
  if (tags.length > 0) {
    result.push({ name: '学习标签', value: Math.min(100, tags.length * 20), valueText: String(tags.length), color: '#8b5cf6' });
  }
  return result;
}

function buildSkills(agent) {
  var cfg = agent.skillConfig || {};
  var skills = Array.isArray(cfg.skills) ? cfg.skills : [];
  return skills.map(function (item) {
    return {
      icon: item.icon || '🧩',
      name: item.name || item.key || '技能',
      desc: item.desc || item.description || item.content || '已启用'
    };
  });
}

function buildActivities(agent, learning) {
  var rows = [];
  if (learning && learning.lastLearningSummary) {
    rows.push({ action: '学习复盘', time: formatDateTime(learning.lastLearningDate || learning.learningUpdatedAt), detail: learning.lastLearningSummary });
  } else if (agent.latestLearningSummary) {
    rows.push({ action: '学习复盘', time: formatDateTime(agent.latestLearningAt), detail: agent.latestLearningSummary });
  }
  var records = (learning && learning.records) || [];
  records.slice(0, 5).forEach(function (record) {
    rows.push({
      action: record.title || '学习记录',
      time: formatDateTime(record.createdAt || record.tradingDate),
      detail: record.summary || (record.lessons || []).join('；') || (record.adjustments || []).join('；') || '已记录'
    });
  });
  return rows;
}

function normalizeMemory(entry) {
  return {
    key: entry.title || entry.layer || entry.id || '记忆',
    summary: entry.content || (entry.tags || []).join('、') || '--'
  };
}

function normalizeModel(row) {
  row = row || {};
  return {
    label: row.display_name || row.displayName || row.model_name || row.modelName || row.name || row.provider || 'Model',
    provider: row.provider || '',
    modelName: row.model_name || row.modelName || row.name || ''
  };
}

Page({
  data: {
    fundId: '',
    agentId: '',
    agent: null,
    capabilities: [],
    skills: [],
    activities: [],
    memories: [],
    loading: false,
    errorText: ''
  },

  onLoad: function (options) {
    var agentId = options.agentId || options.id || '';
    var fundId = resolveFundId(options);
    this.setData({ agentId: agentId, fundId: fundId });
    this.loadAgentDetail(agentId, fundId);
  },

  loadAgentDetail: function (agentId, fundId) {
    if (!agentId || !fundId) {
      this.setData({ agent: null, capabilities: [], skills: [], activities: [], memories: [], errorText: '缺少基金或 Agent 信息' });
      return;
    }
    this.setData({ loading: true, errorText: '' });

    Promise.all([
      api.getTeam(fundId),
      api.getAgentLearning(agentId).catch(function () { return null; }),
      api.getMemories(fundId, { layer: 'agent', agentId: agentId }).catch(function () { return { entries: [] }; })
    ]).then(function (results) {
      var team = Array.isArray(results[0]) ? results[0] : [];
      var row = team.filter(function (item) { return (item.id || item.agentId) === agentId; })[0];
      if (!row) {
        this.setData({ agent: null, capabilities: [], skills: [], activities: [], memories: [], loading: false, errorText: '未找到该 Agent 或已不在当前基金团队中' });
        return;
      }
      var agent = normalizeAgent(row);
      var learning = results[1];
      var memoryContext = results[2] || {};
      this.setData({
        agent: agent,
        capabilities: buildCapabilities(agent, learning),
        skills: buildSkills(agent),
        activities: buildActivities(agent, learning),
        memories: (memoryContext.entries || []).slice(0, 8).map(normalizeMemory),
        loading: false,
        errorText: ''
      });
    }.bind(this)).catch(function () {
      this.setData({ agent: null, capabilities: [], skills: [], activities: [], memories: [], loading: false, errorText: 'Agent 详情加载失败' });
    }.bind(this));
  },

  onChangeModel: function () {
    if (!this.data.agent) return;
    api.getModels().then(function (res) {
      var rows = [].concat(res.platform_models || res.platformModels || [], res.custom_models || res.customModels || []);
      var models = rows.map(normalizeModel).filter(function (item) { return item.modelName; });
      if (models.length === 0) {
        wx.showToast({ title: '暂无可用模型', icon: 'none' });
        return;
      }
      wx.showActionSheet({
        itemList: models.slice(0, 6).map(function (item) { return item.label; }),
        success: function (action) {
          var selected = models[action.tapIndex];
          api.updateTeamMember(this.data.fundId, this.data.agent.id, {
            modelConfig: { provider: selected.provider, modelName: selected.modelName }
          }).then(function (updated) {
            var agent = normalizeAgent(updated || this.data.agent);
            this.setData({ agent: agent, capabilities: buildCapabilities(agent, null), skills: buildSkills(agent) });
            wx.showToast({ title: '模型已更新', icon: 'success' });
          }.bind(this)).catch(function () {
            wx.showToast({ title: '模型更新失败', icon: 'none' });
          });
        }.bind(this)
      });
    }.bind(this)).catch(function () {
      wx.showToast({ title: '模型列表加载失败', icon: 'none' });
    });
  },

  onChangeStyle: function () {
    if (!this.data.agent) return;
    var styles = [
      { label: '宏观研究', value: 'macro' },
      { label: '成长投资', value: 'growth' },
      { label: '价值投资', value: 'value' },
      { label: '动量策略', value: 'momentum' },
      { label: '风险控制', value: 'risk_control' }
    ];
    wx.showActionSheet({
      itemList: styles.map(function (item) { return item.label; }),
      success: function (res) {
        var selected = styles[res.tapIndex];
        api.updateTeamMember(this.data.fundId, this.data.agent.id, { focus: selected.value }).then(function (updated) {
          var agent = normalizeAgent(updated || this.data.agent);
          this.setData({ agent: agent });
          wx.showToast({ title: '风格已更新', icon: 'success' });
        }.bind(this)).catch(function () {
          wx.showToast({ title: '风格更新失败', icon: 'none' });
        });
      }.bind(this)
    });
  },

  onDismiss: function () {
    if (!this.data.agent) return;
    wx.showModal({
      title: '解雇Agent',
      content: '确认解雇 ' + this.data.agent.name + '？此操作不可撤销。',
      confirmColor: '#e53935',
      success: function (res) {
        if (res.confirm) {
          api.removeTeamMember(this.data.fundId, this.data.agent.id).then(function () {
            wx.showToast({ title: '已解雇', icon: 'none' });
            setTimeout(function () { wx.navigateBack(); }, 1000);
          }).catch(function () {
            wx.showToast({ title: '解雇失败', icon: 'none' });
          });
        }
      }.bind(this)
    });
  },

  onShareAppMessage: function () {
    return {
      title: this.data.agent ? this.data.agent.name + ' - ' + this.data.agent.role : 'Agent详情'
    };
  }
});
