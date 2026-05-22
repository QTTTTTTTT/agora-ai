// 记忆中心 - 多层记忆管理
const api = require('../../utils/api.js').api;

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

function formatDate(value) {
  if (!value) return '';
  const date = new Date(value);
  if (isNaN(date.getTime())) return String(value).slice(0, 10);
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return date.getFullYear() + '-' + month + '-' + day;
}

function normalizeEntry(entry) {
  return {
    id: entry.id,
    layer: entry.layer,
    title: entry.title || '未命名记忆',
    source: entry.agentId ? 'agent/' + entry.agentId : 'fund',
    date: formatDate(entry.tradingDate || entry.createdAt),
    tags: entry.tags || [],
    content: entry.content || '',
    expanded: false,
    agent: entry.agentId || null
  };
}

Page({
  data: {
    fundId: '',
    activeLayer: 'long_term',
    layers: [
      { key: 'long_term', label: '长期记忆', icon: '🧠' },
      { key: 'daily', label: '每日日志', icon: '📅' },
      { key: 'dreams', label: '梦境归纳', icon: '💭' },
      { key: 'agent', label: 'Agent记忆', icon: '🤖' }
    ],
    entries: [],
    filteredEntries: [],
    searchQuery: '',
    searchResults: [],
    showSearch: false,
    stats: {
      total: 0,
      byLayer: {}
    }
  },

  onShow() {
    this.loadEntries();
  },

  loadEntries() {
    const fundId = this.data.fundId || resolveFundId();
    if (!fundId) {
      this.setData({ entries: [], filteredEntries: [], stats: { total: 0, byLayer: {} } });
      return;
    }
    this.setData({ fundId: fundId });
    const layer = this.data.activeLayer;
    api.getMemories(fundId, { layer: layer }).then((res) => {
      const entries = ((res && res.entries) || []).map(normalizeEntry);
      const byLayer = Object.assign({}, this.data.stats.byLayer, { [layer]: entries.length });
      this.setData({
        entries: entries,
        filteredEntries: entries,
        stats: { total: entries.length, byLayer: byLayer }
      });
    }).catch(() => {
      this.setData({ entries: [], filteredEntries: [], stats: { total: 0, byLayer: {} } });
    });
  },

  switchLayer(e) {
    const key = e.currentTarget.dataset.key;
    this.setData({ activeLayer: key, showSearch: false, searchQuery: '', entries: [], filteredEntries: [] });
    this.loadEntries();
  },

  filterEntries() {
    const { entries } = this.data;
    const filteredEntries = entries.map(e => Object.assign({}, e, { expanded: false }));
    this.setData({ filteredEntries });
  },

  toggleSearch() {
    const showSearch = !this.data.showSearch;
    this.setData({ showSearch, searchQuery: '', searchResults: [] });
    if (!showSearch) this.filterEntries();
  },

  onSearchInput(e) {
    this.setData({ searchQuery: e.detail.value });
  },

  doSearch() {
    const { fundId, activeLayer, searchQuery } = this.data;
    if (!searchQuery.trim()) {
      this.filterEntries();
      return;
    }
    api.searchMemories(fundId, { layer: activeLayer, q: searchQuery }).then((res) => {
      this.setData({ filteredEntries: (res || []).map(normalizeEntry) });
    }).catch(() => {
      this.setData({ filteredEntries: [] });
    });
  },

  viewEntry(e) {
    const id = e.currentTarget.dataset.id;
    const filteredEntries = this.data.filteredEntries.map(entry => {
      if (entry.id === id) return Object.assign({}, entry, { expanded: !entry.expanded });
      return entry;
    });
    this.setData({ filteredEntries });
  },

  copyContent(e) {
    const id = e.currentTarget.dataset.id;
    const entry = this.data.filteredEntries.find(m => m.id === id) || this.data.entries.find(m => m.id === id);
    if (entry) {
      wx.setClipboardData({
        data: entry.content,
        success() {
          wx.showToast({ title: '已复制', icon: 'success' });
        }
      });
    }
  }
});
