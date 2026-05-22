const DEFAULT_API_BASE = 'http://localhost:8000';

App({
  globalData: {
    apiBase: DEFAULT_API_BASE,
    userInfo: null,
    currentFund: null
  },

  onLaunch() {
    // 读取本地存储的 apiBase
    const apiBase = wx.getStorageSync('apiBase') || DEFAULT_API_BASE;
    this.globalData.apiBase = apiBase;

    // 检查登录态
    this.checkLogin();
  },

  /**
   * 检查登录态，尝试从缓存恢复用户信息
   */
  checkLogin() {
    const userInfo = wx.getStorageSync('userInfo');
    const token = wx.getStorageSync('token');

    if (userInfo && token) {
      this.globalData.userInfo = userInfo;
      // 验证 token 是否过期
      this.request({
        url: '/api/auth/session',
        method: 'GET'
      }).then((res) => {
        if (res && res.authenticated) {
          this.globalData.userInfo = res;
        } else {
          this.clearLogin();
        }
      }).catch(() => {
        // token 校验失败，静默处理，用户操作时再提示
      });
    }
  },

  /**
   * 清除登录态
   */
  clearLogin() {
    this.globalData.userInfo = null;
    wx.removeStorageSync('userInfo');
    wx.removeStorageSync('token');
  },

  /**
   * 统一请求封装
   * @param {Object} options - 请求配置
   * @param {string} options.url - 请求路径（不含 apiBase）
   * @param {string} [options.method='GET'] - 请求方法
   * @param {Object} [options.data] - 请求数据
   * @param {boolean} [options.showLoading=true] - 是否显示 loading
   * @param {string} [options.loadingText='加载中...'] - loading 文字
   * @param {boolean} [options.showError=true] - 是否自动提示错误
   * @returns {Promise<Object>} 响应数据
   */
  request({
    url,
    method = 'GET',
    data = {},
    showLoading = true,
    loadingText = '加载中...',
    showError = true
  }) {
    return new Promise((resolve, reject) => {
      if (showLoading) {
        wx.showLoading({
          title: loadingText,
          mask: true
        });
      }

      const token = wx.getStorageSync('token') || '';

      wx.request({
        url: `${this.globalData.apiBase}${url}`,
        method,
        data,
        header: {
          'Content-Type': 'application/json',
          'Authorization': token ? `Bearer ${token}` : ''
        },
        timeout: 15000,
        success: (res) => {
          if (showLoading) {
            wx.hideLoading();
          }

          const statusCode = res.statusCode;

          if (statusCode >= 200 && statusCode < 300) {
            const responseData = res.data;
            resolve(responseData);
          } else if (statusCode === 401) {
            // 未授权，清除登录态
            this.clearLogin();
            if (showError) {
              wx.showToast({
                title: '登录已过期，请重新登录',
                icon: 'none',
                duration: 2000
              });
            }
            reject({ code: 401, message: '未授权' });
          } else {
            // HTTP 错误
            if (showError) {
              wx.showToast({
                title: `服务器错误 (${statusCode})`,
                icon: 'none',
                duration: 2000
              });
            }
            reject({ code: statusCode, message: '服务器错误' });
          }
        },
        fail: (err) => {
          if (showLoading) {
            wx.hideLoading();
          }
          if (showError) {
            wx.showToast({
              title: '网络异常，请稍后重试',
              icon: 'none',
              duration: 2000
            });
          }
          reject({ code: -1, message: '网络异常', detail: err });
        }
      });
    });
  }
});
