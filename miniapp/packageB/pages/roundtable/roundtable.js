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
  try { return JSON.parse(value); } catch (e) { return null; }
}

function formatDateTime(value) {
  if (!value) return '-';
  var date = new Date(value);
  if (isNaN(date.getTime())) return String(value);
  return date.getFullYear() + '-' + String(date.getMonth() + 1).padStart(2, '0') + '-' + String(date.getDate()).padStart(2, '0') + ' ' + String(date.getHours()).padStart(2, '0') + ':' + String(date.getMinutes()).padStart(2, '0');
}

function roleEmoji(role) {
  var map = { pm: '📊', researcher: '🔎', analyst: '📈', risk: '🛡️', trader: '⚡' };
  return map[role] || '🤖';
}

function roleText(role) {
  var map = { pm: 'PM', researcher: '研究员', analyst: '分析师', risk: '风控', trader: '交易员' };
  return map[role] || role || '--';
}

function directionFromStance(stance) {
  var text = String(stance || '').toLowerCase();
  if (text.indexOf('bear') >= 0 || text.indexOf('sell') >= 0 || text.indexOf('看空') >= 0) return 'bearish';
  if (text.indexOf('neutral') >= 0 || text.indexOf('hold') >= 0 || text.indexOf('中性') >= 0) return 'neutral';
  return 'bullish';
}

function directionText(direction) {
  return direction === 'bearish' ? '看空' : (direction === 'neutral' ? '中性' : '看多');
}

function normalizeParticipants(trace) {
  var source = (trace.memo && trace.memo.participants) || [];
  if (source.length === 0 && trace.memo && trace.memo.agentViews) source = trace.memo.agentViews;
  var seen = {};
  return source.map(function (item) {
    var id = item.agentId || item.id || item.role || item.name;
    if (seen[id]) return null;
    seen[id] = true;
    return { id: id, emoji: roleEmoji(item.role), name: item.name || roleText(item.role), role: roleText(item.role) };
  }).filter(function (item) { return !!item; });
}

function normalizeTopics(trace) {
  var actions = (trace.plan && trace.plan.actions) || [];
  return actions.map(function (item) {
    return { stock: item.symbol || item.instrumentKey || '--', question: (item.action || 'review') + ' · ' + (item.reasoningZh || item.reasoning || '') };
  });
}

function normalizeSpeech(view, idx) {
  var direction = directionFromStance(view.stance || view.direction);
  var evidence = view.evidence || [];
  return {
    agentId: view.agentId || view.id || view.role || String(idx),
    emoji: roleEmoji(view.role),
    name: view.name || roleText(view.role),
    direction: direction,
    directionText: directionText(direction),
    confidence: view.confidencePct || view.confidence || 0,
    topic: (view.symbols && view.symbols[0]) || view.topic || '--',
    summary: view.viewpoint || view.summary || view.reasoning || '--',
    detail: evidence.length > 0 ? evidence.join('\n') : (view.detail || view.content || view.viewpoint || '--')
  };
}

function voteStats(speeches, participantsCount) {
  var votes = { bullish: 0, bearish: 0, neutral: 0, bullishPct: 0, bearishPct: 0, neutralPct: 0 };
  speeches.forEach(function (speech) { votes[speech.direction] = (votes[speech.direction] || 0) + 1; });
  var total = participantsCount || speeches.length || 1;
  votes.bullishPct = Math.round(votes.bullish / total * 100);
  votes.bearishPct = Math.round(votes.bearish / total * 100);
  votes.neutralPct = Math.round(votes.neutral / total * 100);
  return votes;
}

function normalizeRounds(trace, participants) {
  var snapshot = trace.discussion ? parseJSON(trace.discussion.snapshot) : null;
  var rounds = snapshot && Array.isArray(snapshot.rounds) ? snapshot.rounds : [];
  if (rounds.length > 0) {
    return rounds.map(function (round, idx) {
      var speeches = (round.speeches || round.messages || []).map(normalizeSpeech);
      return {
        title: round.title || '第' + (idx + 1) + '轮',
        subtitle: round.subtitle || round.phase || '讨论',
        speeches: speeches,
        votes: voteStats(speeches, participants.length)
      };
    });
  }
  var views = (trace.memo && trace.memo.agentViews) || [];
  if (views.length === 0 && trace.discussion && trace.discussion.summary) {
    views = [{ role: 'pm', viewpoint: trace.discussion.summary, evidence: trace.discussion.consensus || [] }];
  }
  var speeches = views.map(normalizeSpeech);
  return speeches.length > 0 ? [{ title: '观点汇总', subtitle: 'Agent 意见', speeches: speeches, votes: voteStats(speeches, participants.length) }] : [];
}

function normalizeConsensus(trace) {
  var lines = (trace.discussion && (trace.discussion.consensusZh || trace.discussion.consensus)) || (trace.memo && trace.memo.consensus) || [];
  if (lines.length === 0 && trace.memo && trace.memo.finalDecision && trace.memo.finalDecision.actions) {
    lines = trace.memo.finalDecision.actions;
  }
  return lines.map(function (line, idx) {
    return { stock: '结论' + (idx + 1), direction: '共识', directionType: 'bullish', confidence: '', action: line, reached: true };
  });
}

function normalizeRoundtable(trace, planId) {
  trace = trace || {};
  var participants = normalizeParticipants(trace);
  var topics = normalizeTopics(trace);
  var rounds = normalizeRounds(trace, participants);
  var consensus = normalizeConsensus(trace);
  var reached = consensus.length > 0;
  return {
    id: planId,
    date: formatDateTime((trace.run && (trace.run.completedAt || trace.run.startedAt)) || (trace.plan && trace.plan.createdAt)),
    consensusStatus: reached ? '已达成共识' : '暂无共识',
    consensusReached: reached,
    totalRounds: rounds.length,
    participants: participants,
    topics: topics,
    rounds: rounds,
    consensus: consensus,
    noConsensus: reached ? [] : [{ stock: '圆桌', reason: '该方案暂无可展示的共识记录' }]
  };
}

Page({
  data: {
    roundtable: null,
    expandedSpeech: {},
    currentRound: 0,
    loading: false,
    errorText: ''
  },

  onLoad: function (options) {
    var planId = options.planId || '';
    var fundId = resolveFundId(options);
    this.loadRoundtableDetail(planId, fundId);
  },

  loadRoundtableDetail: function (planId, fundId) {
    if (!planId || !fundId) {
      this.setData({ roundtable: null, errorText: '缺少方案或基金信息' });
      return;
    }
    this.setData({ loading: true, errorText: '' });
    api.getDecisionTrace(fundId, { planId: planId }).then(function (trace) {
      this.setData({ roundtable: normalizeRoundtable(trace, planId), loading: false });
    }.bind(this)).catch(function () {
      this.setData({ roundtable: null, loading: false, errorText: '圆桌记录加载失败' });
      wx.showToast({ title: '圆桌记录加载失败', icon: 'none' });
    }.bind(this));
  },

  onToggleSpeech: function (e) {
    var key = e.currentTarget.dataset.key;
    var expanded = this.data.expandedSpeech;
    expanded[key] = !expanded[key];
    this.setData({ expandedSpeech: expanded });
  },

  onAgentTap: function (e) {
    var agentId = e.currentTarget.dataset.id;
    wx.navigateTo({
      url: '/packageB/pages/agent-detail/agent-detail?agentId=' + agentId
    });
  },

  onShareAppMessage: function () {
    return { title: '圆桌回放' };
  }
});
