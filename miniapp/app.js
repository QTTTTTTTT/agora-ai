const DEFAULT_API_BASE = 'http://localhost:8000';

// Sprint 2B — WeChat miniapp login.
//
// Bootstrap order:
//   1. onLaunch reads cached token from storage. If present, optimistically
//      use it (so first paint is fast) and validate via /api/auth/session.
//   2. If no token OR session validation returns 401, fall back to the
//      wx.login → /api/auth/wechat-login handshake automatically. This
//      keeps the miniapp usable without the user ever seeing an explicit
//      login screen, matching how production WeChat miniprograms behave.
//   3. Wrap every wx.request so a 401 mid-session triggers ONE silent
//      re-login + retry. Anything beyond that surfaces the error.

App({
  globalData: {
    apiBase: DEFAULT_API_BASE,
    userInfo: null,
    currentFund: null,
    wechatLoginInFlight: false
  },

  onLaunch() {
    const apiBase = wx.getStorageSync('apiBase') || DEFAULT_API_BASE;
    this.globalData.apiBase = apiBase;

    this.bootstrapSession();
  },

  /**
   * bootstrapSession validates any cached token first; on 401 OR cache
   * miss it transparently exchanges a fresh wx.login() code for a JWT.
   * Pages that read this.globalData.userInfo can subscribe via
   * getApp().waitForLogin().
   */
  bootstrapSession() {
    const token = wx.getStorageSync('token');
    const userInfo = wx.getStorageSync('userInfo');
    if (userInfo) {
      this.globalData.userInfo = userInfo;
    }

    if (token) {
      this.request({
        url: '/api/auth/session',
        method: 'GET',
        showLoading: false,
        showError: false,
        suppressAutoLogin: true
      })
        .then((res) => {
          if (res && res.authenticated) {
            this.globalData.userInfo = res;
            wx.setStorageSync('userInfo', res);
          } else {
            this.clearLogin();
            this.loginWithWechat({ silent: true }).catch(() => {});
          }
        })
        .catch(() => {
          // Cached token rejected — re-handshake with wx.login.
          this.clearLogin();
          this.loginWithWechat({ silent: true }).catch(() => {});
        });
    } else {
      this.loginWithWechat({ silent: true }).catch(() => {});
    }
  },

  /**
   * waitForLogin lets pages await whichever bootstrap path is running.
   */
  waitForLogin(timeoutMs) {
    const start = Date.now();
    const limit = timeoutMs || 6000;
    return new Promise((resolve) => {
      const tick = () => {
        if (this.globalData.userInfo) {
          resolve(this.globalData.userInfo);
          return;
        }
        if (Date.now() - start >= limit) {
          resolve(null);
          return;
        }
        setTimeout(tick, 120);
      };
      tick();
    });
  },

  /**
   * loginWithWechat performs the wx.login → /api/auth/wechat-login swap.
   * Concurrent callers share a single in-flight handshake (mutex via
   * wechatLoginInFlight) so a burst of 401s doesn't spawn N parallel
   * exchanges.
   */
  loginWithWechat(options) {
    const opts = options || {};
    if (this.globalData.wechatLoginInFlight) {
      return this.globalData.wechatLoginInFlight;
    }
    const promise = new Promise((resolve, reject) => {
      wx.login({
        success: (res) => {
          if (!res || !res.code) {
            reject({ code: -1, message: 'wx.login 未返回 code' });
            return;
          }
          this.request({
            url: '/api/auth/wechat-login',
            method: 'POST',
            data: { code: res.code },
            showLoading: !opts.silent,
            loadingText: opts.silent ? '' : '正在登录...',
            showError: !opts.silent,
            suppressAutoLogin: true
          })
            .then((payload) => {
              if (!payload || !payload.token) {
                reject({ code: -1, message: 'wechat-login 响应缺少 token', detail: payload });
                return;
              }
              wx.setStorageSync('token', payload.token);
              const profile = {
                userId: payload.user_id,
                email: payload.email,
                displayName: payload.display_name,
                role: payload.role
              };
              wx.setStorageSync('userInfo', profile);
              this.globalData.userInfo = profile;
              resolve(profile);
            })
            .catch((err) => {
              if (!opts.silent) {
                wx.showToast({ title: '微信登录失败', icon: 'none', duration: 2500 });
              }
              reject(err);
            });
        },
        fail: (err) => {
          reject({ code: -1, message: 'wx.login 调用失败', detail: err });
        }
      });
    });
    this.globalData.wechatLoginInFlight = promise.finally(() => {
      this.globalData.wechatLoginInFlight = false;
    });
    return this.globalData.wechatLoginInFlight;
  },

  clearLogin() {
    this.globalData.userInfo = null;
    wx.removeStorageSync('userInfo');
    wx.removeStorageSync('token');
  },

  /**
   * Unified request helper. Pass suppressAutoLogin=true on requests that
   * are themselves part of the login dance so we don't recurse.
   */
  request({
    url,
    method = 'GET',
    data = {},
    showLoading = true,
    loadingText = '加载中...',
    showError = true,
    suppressAutoLogin = false,
    _retryAfterLogin = false
  }) {
    return new Promise((resolve, reject) => {
      if (showLoading) {
        wx.showLoading({ title: loadingText, mask: true });
      }
      const token = wx.getStorageSync('token') || '';
      wx.request({
        url: `${this.globalData.apiBase}${url}`,
        method,
        data,
        header: {
          'Content-Type': 'application/json',
          Authorization: token ? `Bearer ${token}` : ''
        },
        timeout: 15000,
        success: (res) => {
          if (showLoading) {
            wx.hideLoading();
          }
          const statusCode = res.statusCode;
          if (statusCode >= 200 && statusCode < 300) {
            resolve(res.data);
            return;
          }
          if (statusCode === 401) {
            this.clearLogin();
            if (!suppressAutoLogin && !_retryAfterLogin) {
              this.loginWithWechat({ silent: true })
                .then(() =>
                  this.request({
                    url,
                    method,
                    data,
                    showLoading,
                    loadingText,
                    showError,
                    suppressAutoLogin,
                    _retryAfterLogin: true
                  }).then(resolve, reject)
                )
                .catch((err) => {
                  if (showError) {
                    wx.showToast({ title: '登录已过期，请重新进入小程序', icon: 'none', duration: 2200 });
                  }
                  reject({ code: 401, message: '未授权', detail: err });
                });
              return;
            }
            if (showError) {
              wx.showToast({ title: '登录已过期', icon: 'none', duration: 2200 });
            }
            reject({ code: 401, message: '未授权' });
            return;
          }
          if (showError) {
            wx.showToast({ title: `服务器错误 (${statusCode})`, icon: 'none', duration: 2000 });
          }
          reject({ code: statusCode, message: '服务器错误', detail: res.data });
        },
        fail: (err) => {
          if (showLoading) {
            wx.hideLoading();
          }
          if (showError) {
            wx.showToast({ title: '网络异常，请稍后重试', icon: 'none', duration: 2000 });
          }
          reject({ code: -1, message: '网络异常', detail: err });
        }
      });
    });
  }
});
