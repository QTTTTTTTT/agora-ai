// 模型配置
const api = require('../../../utils/api.js').api;

function normalizeProvider(provider) {
  const value = String(provider || '').toLowerCase();
  if (value === '其他') return 'custom';
  if (value === 'anthropic') return 'claude';
  if (value === 'alibaba') return 'qwen';
  return value || 'custom';
}

function normalizeModel(raw) {
  const modelName = raw.model_name || raw.modelName || raw.id;
  const displayName = raw.display_name || raw.displayName || raw.name || modelName;
  const provider = normalizeProvider(raw.provider);
  const inputPrice = Number(raw.input_price_per_1k || raw.inputPrice || raw.pricePer1k || 0);
  const outputPrice = Number(raw.output_price_per_1k || raw.outputPrice || raw.pricePer1k || 0);
  const tier = raw.tier || 'standard';
  return {
    id: modelName,
    name: displayName,
    provider: provider,
    providerLogo: '/assets/icons/' + provider + '.png',
    pricePer1k: +(((inputPrice || 0) + (outputPrice || inputPrice || 0)) / 2).toFixed(6),
    priceLabel: '¥' + (((inputPrice || 0) + (outputPrice || inputPrice || 0)) / 2).toFixed(4) + '/1K tokens',
    recommended: !!raw.recommended,
    tiers: raw.tiers || [tier],
    available: raw.available !== false
  };
}

function normalizeConfig(raw) {
  return {
    id: raw.id,
    configType: raw.config_type || raw.configType,
    tier: raw.tier,
    provider: normalizeProvider(raw.provider),
    modelName: raw.model_name || raw.modelName,
    baseURL: raw.base_url || raw.baseURL,
    status: raw.is_active === false ? 'inactive' : 'active'
  };
}

Page({
  data: {
    platformModels: [
      {
        id: 'gpt-4o',
        name: 'GPT-4o',
        provider: 'OpenAI',
        providerLogo: '/assets/icons/openai.png',
        pricePer1k: 0.03,
        priceLabel: '¥0.03/1K tokens',
        recommended: true,
        tiers: ['critical']
      },
      {
        id: 'gpt-4o-mini',
        name: 'GPT-4o Mini',
        provider: 'OpenAI',
        providerLogo: '/assets/icons/openai.png',
        pricePer1k: 0.002,
        priceLabel: '¥0.002/1K tokens',
        recommended: true,
        tiers: ['simple']
      },
      {
        id: 'claude-sonnet',
        name: 'Claude 3.5 Sonnet',
        provider: 'Anthropic',
        providerLogo: '/assets/icons/anthropic.png',
        pricePer1k: 0.036,
        priceLabel: '¥0.036/1K tokens',
        recommended: false,
        tiers: ['critical']
      },
      {
        id: 'claude-haiku',
        name: 'Claude 3.5 Haiku',
        provider: 'Anthropic',
        providerLogo: '/assets/icons/anthropic.png',
        pricePer1k: 0.003,
        priceLabel: '¥0.003/1K tokens',
        recommended: false,
        tiers: ['standard', 'simple']
      },
      {
        id: 'deepseek-chat',
        name: 'DeepSeek Chat',
        provider: 'DeepSeek',
        providerLogo: '/assets/icons/deepseek.png',
        pricePer1k: 0.001,
        priceLabel: '¥0.001/1K tokens',
        recommended: true,
        tiers: ['standard']
      },
      {
        id: 'deepseek-reasoner',
        name: 'DeepSeek Reasoner',
        provider: 'DeepSeek',
        providerLogo: '/assets/icons/deepseek.png',
        pricePer1k: 0.008,
        priceLabel: '¥0.008/1K tokens',
        recommended: false,
        tiers: ['critical']
      },
      {
        id: 'qwen-max',
        name: '通义千问 Max',
        provider: 'Alibaba',
        providerLogo: '/assets/icons/qwen.png',
        pricePer1k: 0.012,
        priceLabel: '¥0.012/1K tokens',
        recommended: false,
        tiers: ['critical', 'standard']
      },
      {
        id: 'qwen-turbo',
        name: '通义千问 Turbo',
        provider: 'Alibaba',
        providerLogo: '/assets/icons/qwen.png',
        pricePer1k: 0.002,
        priceLabel: '¥0.002/1K tokens',
        recommended: false,
        tiers: ['standard', 'simple']
      }
    ],
    userConfigs: [],
    currentTierConfig: {
      critical: 'gpt-4o',
      standard: 'deepseek-chat',
      simple: 'gpt-4o-mini'
    },
    tierDescriptions: {
      critical: {
        title: '关键决策',
        desc: '用于最终投资决策、风险评估等高价值步骤。需要最强推理能力。',
        steps: '投资决策、风险评估、异常检测'
      },
      standard: {
        title: '日常任务',
        desc: '用于市场分析、数据汇总、报告生成等常规工作流。平衡性能与成本。',
        steps: '市场分析、报告生成、数据汇总'
      },
      simple: {
        title: '简单任务',
        desc: '用于格式转换、文本提取、分类标签等简单操作。追求低成本高效率。',
        steps: '格式转换、文本提取、分类打标'
      }
    },
    useCustomKey: {
      critical: false,
      standard: false,
      simple: false
    },
    showCustomModal: false,
    customForm: {
      provider: 'openai',
      modelName: '',
      baseURL: '',
      apiKey: ''
    },
    providerOptions: ['openai', 'claude', 'deepseek', 'qwen', '其他'],
    providerIndex: 0,
    testResult: null,
    estimatedCost: null,
    estimatedCostDetail: {
      critical: 0,
      standard: 0,
      simple: 0,
      total: 0,
      defaultTotal: 0,
      saved: 0
    }
  },

  onLoad() {
    this.loadModels();
    this.loadConfig();
    this.estimateCost();
  },

  loadModels() {
    api.getModels().then((res) => {
      const platformModels = (res && res.platform_models) || (res && res.platformModels) || [];
      const customModels = (res && res.custom_models) || (res && res.customModels) || [];
      const models = platformModels.concat(customModels).map(normalizeModel).filter(m => m.available);
      if (models.length) {
        this.setData({ platformModels: models });
        this.estimateCost();
      }
    }).catch(() => {
      this.estimateCost();
    });
  },

  loadConfig() {
    api.getModelConfigs().then((res) => {
      const configs = ((res && res.configs) || []).map(normalizeConfig);
      const tierConfig = Object.assign({}, this.data.currentTierConfig);
      const useCustomKey = Object.assign({}, this.data.useCustomKey);
      configs.forEach((cfg) => {
        if (cfg.configType === 'tier_override' && cfg.tier && cfg.modelName) {
          tierConfig[cfg.tier] = cfg.modelName;
        }
        if (cfg.configType === 'custom_endpoint') {
          useCustomKey.critical = true;
          useCustomKey.standard = true;
          useCustomKey.simple = true;
        }
      });
      this.setData({
        currentTierConfig: tierConfig,
        userConfigs: configs.filter(cfg => cfg.configType === 'custom_endpoint'),
        useCustomKey: useCustomKey
      });
      this.estimateCost();
    }).catch(() => {
      this.estimateCost();
    });
  },

  selectModelForTier(e) {
    const { tier, model } = e.currentTarget.dataset;
    const key = 'currentTierConfig.' + tier;
    this.setData({ [key]: model });
    this.saveTierConfig(tier);
    this.estimateCost();
  },

  saveConfig() {
    const requests = Object.keys(this.data.currentTierConfig).map((tier) => {
      const modelName = this.data.currentTierConfig[tier];
      const model = this.data.platformModels.find(m => m.id === modelName) || {};
      return api.saveModelConfig({
        config_type: 'tier_override',
        tier: tier,
        provider: normalizeProvider(model.provider),
        model_name: modelName
      });
    });
    Promise.all(requests).then(() => {
      wx.showToast({ title: '配置已保存', icon: 'success', duration: 1000 });
    }).catch(() => {
      wx.showToast({ title: '保存失败', icon: 'none' });
    });
  },

  saveTierConfig(tier) {
    const modelName = this.data.currentTierConfig[tier];
    const model = this.data.platformModels.find(m => m.id === modelName) || {};
    api.saveModelConfig({
      config_type: 'tier_override',
      tier: tier,
      provider: normalizeProvider(model.provider),
      model_name: modelName
    }).then(() => {
      wx.showToast({ title: '配置已保存', icon: 'success', duration: 1000 });
    }).catch(() => {
      wx.showToast({ title: '保存失败', icon: 'none' });
    });
  },

  getModelsForTier(tier) {
    return this.data.platformModels.filter(m => m.tiers.indexOf(tier) > -1);
  },

  showAddCustom() {
    this.setData({
      showCustomModal: true,
      customForm: { provider: 'openai', modelName: '', baseURL: '', apiKey: '' },
      providerIndex: 0,
      testResult: null
    });
  },

  hideCustomModal() {
    this.setData({ showCustomModal: false, testResult: null });
  },

  onProviderChange(e) {
    const idx = Number(e.detail.value);
    this.setData({
      providerIndex: idx,
      'customForm.provider': normalizeProvider(this.data.providerOptions[idx])
    });
  },

  onModelNameInput(e) {
    this.setData({ 'customForm.modelName': e.detail.value });
  },

  onBaseURLInput(e) {
    this.setData({ 'customForm.baseURL': e.detail.value });
  },

  onApiKeyInput(e) {
    this.setData({ 'customForm.apiKey': e.detail.value });
  },

  testConnection() {
    const { baseURL, apiKey, modelName } = this.data.customForm;
    if (!baseURL || !apiKey || !modelName) {
      wx.showToast({ title: '请填写完整信息', icon: 'none' });
      return;
    }

    this.setData({ testResult: { status: 'testing', latency: 0 } });
    api.testModelConnection({
      provider: normalizeProvider(this.data.customForm.provider),
      model_name: modelName,
      base_url: baseURL,
      api_key: apiKey
    }).then((res) => {
      this.setData({
        testResult: {
          status: res && res.success ? 'success' : 'fail',
          latency: (res && res.latency_ms) || 0,
          error: res && res.message
        }
      });
    }).catch((err) => {
      this.setData({
        testResult: {
          status: 'fail',
          latency: 0,
          error: (err && err.message) || '连接失败'
        }
      });
    });
  },

  saveCustomEndpoint() {
    const form = this.data.customForm;
    if (!form.modelName || !form.baseURL || !form.apiKey) {
      wx.showToast({ title: '请填写完整信息', icon: 'none' });
      return;
    }

    wx.showLoading({ title: '保存中...' });
    api.saveModelConfig({
      config_type: 'custom_endpoint',
      provider: normalizeProvider(form.provider),
      model_name: form.modelName,
      base_url: form.baseURL,
      api_key: form.apiKey
    }).then((res) => {
      const saved = normalizeConfig((res && res.config) || {});
      const userConfigs = this.data.userConfigs.concat({
        id: saved.id || 'custom_' + Date.now(),
        configType: 'custom_endpoint',
        provider: saved.provider || form.provider,
        modelName: saved.modelName || form.modelName,
        baseURL: saved.baseURL || form.baseURL,
        status: 'active'
      });
      this.setData({ userConfigs, showCustomModal: false, testResult: null });
      wx.showToast({ title: '已保存', icon: 'success' });
    }).catch(() => {
      wx.showToast({ title: '保存失败', icon: 'none' });
    }).then(() => {
      wx.hideLoading();
    });
  },

  deleteConfig(e) {
    const id = e.currentTarget.dataset.id;
    wx.showModal({
      title: '删除确认',
      content: '确定删除该自定义端点配置？',
      success: (res) => {
        if (res.confirm) {
          api.deleteModelConfig(id).then(() => {
            const userConfigs = this.data.userConfigs.filter(c => c.id !== id);
            this.setData({ userConfigs });
            wx.showToast({ title: '已删除', icon: 'success' });
          }).catch(() => {
            wx.showToast({ title: '删除失败', icon: 'none' });
          });
        }
      }
    });
  },

  estimateCost() {
    const { currentTierConfig, platformModels } = this.data;
    // 预估基于每月平均 token 使用量
    const monthlyTokens = { critical: 200, standard: 500, simple: 800 }; // 以 1K 为单位
    const defaultConfig = { critical: 'gpt-4o', standard: 'deepseek-chat', simple: 'gpt-4o-mini' };

    let detail = { critical: 0, standard: 0, simple: 0, total: 0, defaultTotal: 0, saved: 0 };

    const tiers = ['critical', 'standard', 'simple'];
    tiers.forEach(tier => {
      const currentModel = platformModels.find(m => m.id === currentTierConfig[tier]);
      const defaultModel = platformModels.find(m => m.id === defaultConfig[tier]);
      const tokens = monthlyTokens[tier];

      detail[tier] = currentModel ? +(currentModel.pricePer1k * tokens).toFixed(2) : 0;
      const defaultCost = defaultModel ? +(defaultModel.pricePer1k * tokens).toFixed(2) : 0;
      detail.defaultTotal += defaultCost;
    });

    detail.total = +(detail.critical + detail.standard + detail.simple).toFixed(2);
    detail.defaultTotal = +detail.defaultTotal.toFixed(2);
    detail.saved = +(detail.defaultTotal - detail.total).toFixed(2);

    this.setData({
      estimatedCost: detail.total,
      estimatedCostDetail: detail
    });
  },

  toggleUseCustomKey(e) {
    const tier = e.currentTarget.dataset.tier;
    const key = 'useCustomKey.' + tier;
    this.setData({ [key]: !this.data.useCustomKey[tier] });
  }
});
