/**
 * API 请求封装模块
 * 所有方法返回 Promise
 */

const DEFAULT_API_BASE = 'http://localhost:8000';

let BASE_URL = '';

function getBaseUrl() {
  const storedBaseUrl = wx.getStorageSync('apiBase') || '';
  if (BASE_URL && (!storedBaseUrl || storedBaseUrl === BASE_URL)) return BASE_URL;
  try {
    const app = getApp();
    if (app && app.globalData && app.globalData.apiBase) {
      BASE_URL = app.globalData.apiBase;
    }
  } catch (e) {
    console.warn('[api] getApp() 不可用，使用空 BASE_URL');
  }
  if (!BASE_URL) {
    BASE_URL = storedBaseUrl || DEFAULT_API_BASE;
  }
  if (storedBaseUrl && storedBaseUrl !== BASE_URL) {
    BASE_URL = storedBaseUrl;
  }
  return BASE_URL;
}

// Sprint 2B — wrap every request so a 401 mid-session triggers a
// transparent wx.login() handshake before retrying. The retry happens
// at most once per call; if it still fails we surface the 401 so the UI
// can prompt the user to leave and re-enter the miniapp.
function tryAutoRelogin() {
  try {
    var app = getApp();
    if (app && typeof app.loginWithWechat === 'function') {
      return app.loginWithWechat({ silent: true });
    }
  } catch (e) {
    // getApp() is not available — fall through.
  }
  return Promise.reject({ code: -1, message: 'no app context for relogin' });
}

/**
 * 基础请求方法
 * @param {string} url - 请求路径
 * @param {string} method - HTTP 方法
 * @param {object} data - 请求数据
 * @param {object} options - 额外选项
 *   - suppressAutoLogin: skip the 401 re-login dance (used by the login
 *     endpoints themselves to avoid recursion).
 *   - _retryAfterLogin: internal flag set when we've already retried once.
 * @returns {Promise}
 */
function request(url, method, data, options) {
  var opts = options || {};
  return new Promise(function (resolve, reject) {
    var baseUrl = getBaseUrl();
    var header = {
      'Content-Type': 'application/json',
    };
    if (opts.header) {
      Object.keys(opts.header).forEach(function (k) {
        header[k] = opts.header[k];
      });
    }

    var token = wx.getStorageSync('token');
    if (token) {
      header['Authorization'] = 'Bearer ' + token;
    }

    wx.request({
      url: baseUrl + url,
      method: method || 'GET',
      data: data || {},
      header: header,
      timeout: opts.timeout || 30000,
      success: function (res) {
        if (res.statusCode >= 200 && res.statusCode < 300) {
          resolve(res.data);
          return;
        }
        if (res.statusCode === 401) {
          if (!opts.suppressAutoLogin && !opts._retryAfterLogin) {
            // First-chance silent recovery: kick off wx.login() to mint
            // a fresh wechat session token, then retry the original
            // request exactly once. The user sees nothing if it
            // succeeds — which is the usual outcome when the only
            // problem was a stale 12h-expired session.
            tryAutoRelogin()
              .then(function () {
                var nextOpts = Object.assign({}, opts, { _retryAfterLogin: true });
                request(url, method, data, nextOpts).then(resolve, reject);
              })
              .catch(function (err) {
                // wx.login itself failed (network down, miniapp not
                // in the wechat session ring, etc). Tell the user the
                // recovery path: they have to back out of the miniapp
                // and re-enter so the launcher can redo the OAuth
                // handshake from scratch.
                wx.showToast({
                  title: '登录已失效，请退出小程序重新进入',
                  icon: 'none',
                  duration: 3000,
                });
                reject({ code: 401, message: '未授权', data: res.data, detail: err });
              });
            return;
          }
          // The retry-after-login attempt also came back 401 — the
          // server has actively rejected the freshly minted token.
          // This is the "your account was force-logged-out" branch
          // (admin revoke, password change on another device, etc).
          // Same recovery path, but distinct copy so the user knows
          // it's not a transient hiccup.
          wx.showToast({
            title: '会话已被服务器注销，请重新登录',
            icon: 'none',
            duration: 3000,
          });
          reject({ code: 401, message: '未授权', data: res.data });
          return;
        }
        if (res.statusCode === 404) {
          reject({ code: 404, message: '资源不存在', data: res.data });
          return;
        }
        reject({
          code: res.statusCode,
          message: (res.data && res.data.message) || '请求失败',
          data: res.data,
        });
      },
      fail: function (err) {
        console.error('[api] 请求失败:', url, err);
        wx.showToast({ title: '网络错误', icon: 'none' });
        reject({ code: -1, message: '网络错误', error: err });
      },
    });
  });
}

/**
 * GET 请求
 * @param {string} url - 请求路径
 * @param {object} params - 查询参数
 * @returns {Promise}
 */
function get(url, params) {
  // 将 params 拼接到 URL 查询字符串
  if (params && typeof params === 'object') {
    const keys = Object.keys(params);
    if (keys.length > 0) {
      const queryParts = [];
      keys.forEach(function (key) {
        if (params[key] !== undefined && params[key] !== null) {
          queryParts.push(
            encodeURIComponent(key) + '=' + encodeURIComponent(params[key])
          );
        }
      });
      if (queryParts.length > 0) {
        url += (url.indexOf('?') === -1 ? '?' : '&') + queryParts.join('&');
      }
    }
  }
  return request(url, 'GET');
}

/**
 * POST 请求
 * @param {string} url - 请求路径
 * @param {object} data - 请求体数据
 * @returns {Promise}
 */
function post(url, data) {
  return request(url, 'POST', data);
}

/**
 * PUT 请求
 * @param {string} url - 请求路径
 * @param {object} data - 请求体数据
 * @returns {Promise}
 */
function put(url, data) {
  return request(url, 'PUT', data);
}

/**
 * DELETE 请求
 * @param {string} url - 请求路径
 * @returns {Promise}
 */
function del(url) {
  return request(url, 'DELETE');
}

// ============================================================
// 业务 API
// ============================================================
const api = {
  // ---- 基金公司 ----
  getCompanies: function () {
    return get('/api/companies');
  },
  createCompany: function (data) {
    return post('/api/companies', data);
  },

  // ---- 基金 ----
  getFunds: function (companyId) {
    return get('/api/companies/' + companyId + '/funds');
  },
  getFund: function (fundId) {
    return get('/api/funds/' + fundId);
  },
  updateFund: function (fundId, data) {
    return put('/api/funds/' + fundId, data);
  },

  // ---- 团队 ----
  getTeam: function (fundId) {
    return get('/api/funds/' + fundId + '/team');
  },
  addTeamMember: function (fundId, data) {
    return post('/api/funds/' + fundId + '/team', data);
  },
  removeTeamMember: function (fundId, agentId) {
    return del('/api/funds/' + fundId + '/team/' + agentId);
  },
  updateTeamMember: function (fundId, agentId, data) {
    return put('/api/funds/' + fundId + '/team/' + agentId, data);
  },
  getAgentLearning: function (agentId) {
    return get('/api/agents/' + agentId + '/learning');
  },
  getAgentLineage: function (agentId) {
    return get('/api/agents/' + agentId + '/lineage');
  },

  // ---- 投资方案 ----
  getPlans: function (fundId, params) {
    return get('/api/funds/' + fundId + '/plans', params || { limit: 50, offset: 0 });
  },
  getPlan: function (planId) {
    return get('/api/plans/' + planId);
  },
  approvePlan: function (fundId, planId, data) {
    return post('/api/plans/' + planId + '/approve', data);
  },
  rejectPlan: function (planId, reason) {
    return post('/api/plans/' + planId + '/reject', { reason: reason || '用户拒绝' });
  },
  // refreshPlanQuote re-pulls the latest market quote for every still-
  // pending action in the plan, re-applies A-share lot-size rules, and
  // returns the updated plan. The miniapp uses this before approval so
  // the user is signing off on prices that reflect the current market
  // state; if any price moved meaningfully (>0.3%) the plan-detail
  // page surfaces a confirmation modal listing the diffs.
  refreshPlanQuote: function (planId) {
    return post('/api/plans/' + planId + '/refresh-quote');
  },

  // ---- 交易 ----
  getTrades: function (fundId, params) {
    return get('/api/funds/' + fundId + '/trades', params || { limit: 100, offset: 0 });
  },
  getPositions: function (fundId) {
    return get('/api/funds/' + fundId + '/portfolio');
  },
  getNavHistory: function (fundId) {
    return get('/api/funds/' + fundId + '/nav');
  },

  // ---- 基准走势（fund vs market） ----
  // Card I: 拉服务端推荐的基准曲线 + 基金 NAV 曲线（已 rebase 到
  // 100），让小程序首页能直接画"基金 vs 大盘"叠加图。
  // - days 默认 90，与 Web/Android 默认窗口一致。
  // - 不传 seriesIds，让服务端按 fund.market 选默认基准。
  getBenchmarkHistory: function (fundId, days) {
    var params = {};
    if (typeof days === 'number' && isFinite(days)) {
      params.days = Math.trunc(days);
    }
    return get('/api/funds/' + fundId + '/benchmark-history', params);
  },

  // ---- 公司行动（拆股 / 派息） ----
  // Card I: 拉已经应用到该 fund 的 corp action 时间线（最新 N 条）。
  // 服务端如果没启 corp-action 服务会返回 503，调用方需 catch
  // 并优雅降级（隐藏整块）。
  getCorpActions: function (fundId, params) {
    return get('/api/funds/' + fundId + '/corp-actions', params || { limit: 20 });
  },

  // ---- 用量与账单 ----
  getTodayUsage: function () {
    return get('/api/usage/today');
  },
  getMonthlyUsage: function (month) {
    return get('/api/usage/monthly', { month: month });
  },
  getUsageHistory: function (params) {
    return get('/api/usage/history', params);
  },
  getBill: function (month) {
    return get('/api/usage/bill', { month: month });
  },

  // ---- 订阅 ----
  getSubscription: function () {
    return get('/api/subscription');
  },
  getSubscriptionPlans: function () {
    return get('/api/plans');
  },
  subscribe: function (tier, paymentMethod) {
    return post('/api/subscription', { tier: tier, payment_method: paymentMethod || 'manual' });
  },
  cancelSubscription: function () {
    return del('/api/subscription');
  },

  // ---- 模型配置 ----
  getModels: function () {
    return get('/api/models');
  },
  getModelConfigs: function () {
    return get('/api/models/config');
  },
  saveModelConfig: function (data) {
    return post('/api/models/config', data);
  },
  deleteModelConfig: function (configId) {
    return del('/api/models/config/' + configId);
  },
  testModelConnection: function (data) {
    return post('/api/models/test', data);
  },

  // ---- 工作流 ----
  startWorkflow: function (fundId) {
    return post('/api/funds/' + fundId + '/workflow/start');
  },
  triggerStep: function (fundId, step) {
    return post('/api/funds/' + fundId + '/workflow/step', { step: step });
  },
  getWorkflowStatus: function (fundId) {
    return get('/api/funds/' + fundId + '/workflow/status');
  },
  getDecisionTrace: function (fundId, params) {
    return get('/api/funds/' + fundId + '/decision-trace', params);
  },

  // ---- 记忆 ----
  getMemories: function (fundId, params) {
    return get('/api/funds/' + fundId + '/memory', params);
  },
  searchMemories: function (fundId, params) {
    return get('/api/funds/' + fundId + '/memory/search', params);
  },

  // ---- A/B 测试 ----
  getABTests: function (fundId) {
    return get('/api/funds/' + fundId + '/abtests');
  },
  createABTest: function (fundId, data) {
    return post('/api/abtests', Object.assign({}, data, { fundId: fundId }));
  },
  startABTest: function (testId) {
    return post('/api/abtests/' + testId + '/start');
  },
  stopABTest: function (testId) {
    return post('/api/abtests/' + testId + '/stop');
  },
  analyzeABTest: function (testId) {
    return post('/api/abtests/' + testId + '/analyze');
  },
  // ---- Agent 市场 ----
  getMarketplace: function () {
    return get('/api/marketplace/listings');
  },

  // ---- 健康检查 ----
  health: function () {
    return get('/api/health');
  },

  // ---- 微信登录 ----
  // suppressAutoLogin avoids recursion: the wechat-login call itself
  // can return 401 from the upstream WeChat session exchange and we
  // don't want to spawn a retry loop inside the helper.
  wechatLogin: function (code) {
    return request('/api/auth/wechat-login', 'POST', { code: code }, { suppressAutoLogin: true });
  },
};

module.exports = {
  api: api,
  request: request,
  get: get,
  post: post,
  put: put,
  del: del,
};
