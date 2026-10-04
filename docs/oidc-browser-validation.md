# OIDC 浏览器验证记录

本记录包含首次绑定以及本次首次开通和 SSO 再验证的浏览器回归。
接口采用合成响应；下述通过结果不代表已经完成真实账号中心联调。

2026-10-04，Chromium `154.0.8037.97`。脚本
`internal/server/testdata/oidc_browser.cjs` 使用真实浏览器、临时本地 HTTPS 服务，
以及 `gateway.localhost` / `accounts.localhost` 两个不同站点，加载仓库中的实际
回调 HTML/JS、管理台 HTML/JS 和样式。认证响应、账号、密码与令牌均为合成数据；
临时证书和私钥在退出时删除，未使用真实 Supabase 客户端。

浏览器使用与 Gateway `internal/httpx.SecurityHeaders` 相同的 CSP。临时证书
仅在测试浏览器中忽略信任错误；生产 TLS/Cookie 设置不变。

通过的检查：

- 从账号站链接导航回网关，回调 GET 不发送两个 Strict Cookie；落地页后续
  同源 POST 同时携带事务和本地会话 Cookie，Origin 为网关站点。两个 Cookie
  保持 `Secure`、`HttpOnly`、`SameSite=Strict`，JavaScript 不能读取。
- 首次 API 请求前 URL 中的授权参数已移除；授权码、流程 ID、密码及待执行操作
  不写入浏览器存储。再验证只临时保存白名单中的功能页名称，回调后立即清除。
- 1440 px 桌面与 390 px 手机的登录入口、账号安全、绑定预览和绑定后状态正常，
  页面无横向溢出；截图经人工检查，中文字体及按钮、账号说明均可见。
- 本地二次验证取消不会发起绑定；绑定预览必须确认，取消不写绑定；双击确认
  只发送一次。解绑取消不发请求，确认后仍需密码验证；当前统一登录会话被撤销
  的响应使页面返回登录页。
- 拒绝授权、重复回调参数、缺少事务 Cookie、原会话失效显示错误且隐藏确认区；
  重复参数不调用换码接口。
- `pagehide` 后迟到的完成响应不能恢复预览；BFCache 恢复提示重新开始；退出后
  迟到的绑定发起响应不能跳到账号中心。已绑定登录完成后返回概览。
- 关闭开关隐藏统一登录按钮。浏览器 JavaScript 和本地测试服务均无未处理错误。
- 首次登录必须明确选择：取消不建号；绑定先消费开通流程，再通过固定
  `/?link=water5` 引导本地登录及重新授权；创建双击只发送一次流程 ID。
- 纯 SSO 账号的密码和 Passkey 可选，唯一登录方式的解绑按钮禁用并给出说明。
  再验证可以使用吾水阁账号，同标签页返回原计费页面后重新读取服务端状态，
  提示重新操作，不自动提交购买或密码变更。
- 取消后迟到的再验证发起响应只取消其确切流程，不跳转；回调离开时发送
  `keepalive` 取消请求，迟到响应不恢复开通预览或继续导航。

复现需要 Node.js、OpenSSL、Playwright 及其 Chromium/系统依赖：

```sh
PLAYWRIGHT_MODULE=/path/to/playwright \
  node internal/server/testdata/oidc_browser.cjs
```

默认截图写入 `docs/screenshots/oidc/`，可用 `SCREENSHOT_DIR` 改写输出目录。

账号中心应用入口 `/?login=water5` 的浏览器回归也已通过：桌面与手机从账号中心
跨站点击后，网关移除登录提示并自动以本站 Origin 发起一次 POST；已有网关会话
通过站内请求恢复 Strict Cookie 并直接进入控制台。OIDC 关闭、会话服务失败、
重复登录提示均不发起自动登录；等待配置期间完成本地登录也会取消自动跳转。
这些检查使用模拟用户与服务，真实 Supabase 换码及线上部署仍需另行验收。

本次运行使用仓库外 `/tmp/gateway-oidc-browser/node_modules/playwright` 和
`PLAYWRIGHT_CHROMIUM_EXECUTABLE=/usr/bin/google-chrome`，未修改系统依赖。
Node 单元回归 `testdata/oidc_ui_test.cjs` 同时由 Go 测试执行，覆盖单次消费、
精确流程取消、安全返回路径、取消后不重放操作及原页面恢复后的取消。

| 页面 | 桌面 | 手机 |
| --- | --- | --- |
| 登录 | [1440 px](screenshots/oidc/login-1440.png) | [390 px](screenshots/oidc/login-390.png) |
| 未绑定账号安全 | [1440 px](screenshots/oidc/security-1440.png) | [390 px](screenshots/oidc/security-390.png) |
| 绑定确认 | [1440 px](screenshots/oidc/confirmation-1440.png) | [390 px](screenshots/oidc/confirmation-390.png) |
| 已绑定账号安全 | [1440 px](screenshots/oidc/linked-1440.png) | [390 px](screenshots/oidc/linked-390.png) |
| 首次开通选择 | [1440 px](screenshots/oidc/registration-1440.png) | [390 px](screenshots/oidc/registration-390.png) |
| 纯 SSO 账号安全 | [1440 px](screenshots/oidc/sso-only-1440.png) | [390 px](screenshots/oidc/sso-only-390.png) |
| 吾水阁账号再验证 | [1440 px](screenshots/oidc/reauth-1440.png) | [390 px](screenshots/oidc/reauth-390.png) |

此测试验证真实浏览器 Cookie 和页面行为，后端接口是受控 fixture，不能证明
真实 Supabase 换码、真实账号归属或数据库锁正确。协议及 store/server 测试应
另行运行；生产启用前仍须完成 [OIDC 运维文档](oidc.md#上线验收)中的真实测试
项目联调。公开发现文档检查结果见[协议验证记录](oidc-protocol-validation.md)。

真实账号中心验收还应在桌面和手机各复核以下流程：

- 未绑定账号授权后显示两个明确选项；选择前不建号，取消不建号，创建成功后
  进入控制台，后续登录直接进入原账号。重复点击只提交一次流程 ID。
- “绑定已有账号”取消本次开通后进入固定 `/?link=water5`，密码或 Passkey
  登录后引导重新验证、重新授权和绑定确认；不跨登录保存首次授权凭据。
- 纯 SSO 用户不被要求添加密码或 Passkey，可创建和查看 API Key、使用浏览器
  客户端并完成正常计费操作；窗口从交换完成时起算，创建账号按钮不延长期限。
- 五分钟后可用“使用吾水阁账号验证”在同标签页完成认证，回到原功能页面，
  刷新服务端状态并提示重试，不自动重新发起购买、删除或密码保存。
- SSO 为唯一登录方式时无法解绑，添加本地凭据后才可解绑；本地会话保持登录，
  验证窗口清空，绑定产生的 SSO 会话退出。
- 拒绝授权、错误身份、切号、退出、取消、重复/过期流程、迟到回调和 BFCache
  恢复均不能切换用户、恢复旧验证或覆盖较新的验证。后端并发正确性仍由数据库
  集成测试验证，真实授权链路仍须使用独立客户端上线验收。
