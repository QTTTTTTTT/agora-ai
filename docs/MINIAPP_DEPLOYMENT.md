# WeChat Miniapp Deployment

This guide covers the end-to-end steps to take the `miniapp/` package from
the placeholder `wx0000000000000000` AppID to a published WeChat miniapp.

## 1. Prerequisites

- **WeChat Mini Program account** (小程序). Register at
  <https://mp.weixin.qq.com/wxopen/waregister>. For an experimental build
  you can pick the **Personal (个人)** subject; production must use an
  **Enterprise (企业)** subject so the user-data scope unlocks. Financial
  category requires an additional license — see §5.
- **WeChat Developer Tools** (开发者工具) installed locally
  <https://developers.weixin.qq.com/miniprogram/dev/devtools/download.html>.
- Backend deployed somewhere reachable from `https://*.weixin.qq.com`
  (TLS required, no self-signed certs in production).

## 2. Replace the placeholder AppID

1. Open <https://mp.weixin.qq.com/> → 设置 → 开发设置 → 复制 **AppID**
   and (re-)generate **AppSecret**.
2. Edit `miniapp/project.config.json` and replace:

   ```diff
   - "appid": "wx0000000000000000",
   + "appid": "<your real AppID>"
   ```

3. Export the AppID + secret to the server environment so
   `/api/auth/wechat-login` can call `jscode2session`:

   ```bash
   export WECHAT_MINIAPP_APPID=wx1234567890abcdef
   export WECHAT_MINIAPP_SECRET=<the secret you just generated>
   # Optional override (for staging environments that proxy WeChat through a
   # corporate gateway). Leave unset to use the default api.weixin.qq.com.
   # export WECHAT_JSCODE_SESSION_URL=https://api.weixin.qq.com/sns/jscode2session
   ```

   Restart the backend so it picks up the new env. The boot log will say
   `wechat login not configured` on every request until both vars are
   present.

## 3. Whitelist your backend domain

WeChat blocks any `wx.request` that targets a host not pre-declared in
**设置 → 开发设置 → 服务器域名**.

1. Visit the mini program admin console.
2. Under **request 合法域名** add the production API host (e.g.
   `https://api.example.com`). HTTP is rejected — only HTTPS works.
3. Repeat for `socket 合法域名` if you later add WebSockets. The default
   `connectSocket` calls hit the same `apiBase`.
4. Save. Changes propagate to clients within 24h; for testing use the
   developer-tools "不校验合法域名" toggle.

## 4. Login flow check-list

The miniapp now performs a silent `wx.login() → /api/auth/wechat-login`
exchange on every cold start (see `miniapp/app.js#loginWithWechat`). To
validate end-to-end:

1. Start the backend with `WECHAT_MINIAPP_APPID`/`SECRET` exported.
2. Open the miniapp in 微信开发者工具.
3. Watch the **网络 (Network)** panel — the first call should be `POST
   /api/auth/wechat-login` returning a JWT.
4. Re-open the miniapp; you should NOT see a second login call (the JWT
   is cached and `/api/auth/session` validates it instead). Manually
   clearing `wx.removeStorageSync('token')` should trigger a fresh
   handshake on next request.
5. Any 401 returned by a protected endpoint mid-session should trigger
   one silent re-login + retry (see `miniapp/utils/api.js`). Verify by
   forcing a 401 (e.g. stop the backend, restart it, make a request).

## 5. Category-specific compliance

Financial / investment categories on WeChat require:

- An ICP filing for the public backend domain.
- A **金融** or **金融工具** category, which requires a financial
  license (营业执照 + 证券/基金/保险等资质). For the experimental phase
  the platform should submit under **教育 → 在线教育** or **工具 → 商务
  服务** (AI 量化教学 / 仿真) and add a clear disclaimer in the splash
  copy that the product is for simulation only.
- A privacy policy (隐私协议) hosted under a URL on the whitelisted
  domain. Reference it inside `app.json → permission`.

## 6. Submitting for review

1. In 微信开发者工具 click **上传 → 提交版本**.
2. Visit **mp.weixin.qq.com → 版本管理 → 开发版本** and submit the just-
   uploaded build for review (审核).
3. Fill out:
   - **功能页面 / Function pages**: at minimum the home page (`/pages/index/index`).
   - **隐私协议链接**: link to the public privacy policy.
   - **业务介绍**: explain that the app demonstrates AI-driven fund
     simulation, no real trading.
   - **测试账号**: provide a username/password for the email-login flow
     (use a sandbox user with a synthetic fund).
4. Review SLAs: 2–7 working days. WeChat may bounce the submission for:
   - Real-trading language in screenshots → soften to "模拟".
   - Missing privacy policy → add one and resubmit.
   - Logo or icon trademark issues → re-export at 144×144 with a clean
     mark, no third-party logos.

## 7. Operational checklist (post-launch)

- Set up **Real-time logs** (实时日志) inside 开发者工具 to capture
  `console.error` from production sessions.
- Configure **业务域名 → wx.web-view 白名单** if any page embeds an
  external H5 page (e.g. KYC).
- Renew the AppSecret yearly and roll the backend env (rolling restart
  picks the new value up without a redeploy).
- Maintain parity between miniapp endpoints and `server/cmd/server/main.go`
  routes; any new server endpoint not used by the web SPA still needs an
  entry in `miniapp/utils/api.js` (camelCase wrapper) to stay reachable.

## 8. Local development tips

- Backend defaults to `http://localhost:8000`. Update the cached value
  via the **More 页面 → 设置 API Base** input (the miniapp persists the
  override in `wx.setStorageSync('apiBase', ...)`).
- When SMTP isn't configured (Sprint 2A), the email-verification page
  surfaces the code in the JSON response (`dev_code` field). The miniapp
  doesn't render that path — use the web app at `http://localhost:5173`
  for the verification flow during dev.
- The wechat-login handshake requires real WeChat credentials. For unit
  testing the backend, set `WECHAT_JSCODE_SESSION_URL` to a local mock
  (`httptest.NewServer`) — see `server/cmd/server/auth_wechat_test.go`.
