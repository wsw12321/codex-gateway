# OIDC 浏览器验证记录

2026-10-03，Chromium `153.0.8010.12`。脚本
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
- 首次 API 请求前 URL 中的授权参数已移除；没有写入 localStorage/sessionStorage。
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

本次环境提供的 Playwright 在账号站已有 `node_modules/@playwright/test`，另外
通过 `LD_LIBRARY_PATH` 和 `FONTCONFIG_FILE` 指定本地解包的浏览器依赖与中文字体；
未安装或修改系统依赖。

| 页面 | 桌面 | 手机 |
| --- | --- | --- |
| 登录 | [1440 px](screenshots/oidc/login-1440.png) | [390 px](screenshots/oidc/login-390.png) |
| 未绑定账号安全 | [1440 px](screenshots/oidc/security-1440.png) | [390 px](screenshots/oidc/security-390.png) |
| 绑定确认 | [1440 px](screenshots/oidc/confirmation-1440.png) | [390 px](screenshots/oidc/confirmation-390.png) |
| 已绑定账号安全 | [1440 px](screenshots/oidc/linked-1440.png) | [390 px](screenshots/oidc/linked-390.png) |

此测试验证真实浏览器 Cookie 和页面行为，后端接口是受控 fixture，不能证明
真实 Supabase 换码、真实账号归属或数据库锁正确。协议及 store/server 测试应
另行运行；生产启用前仍须完成 [OIDC 运维文档](oidc.md#上线验收)中的真实测试
项目联调。公开发现文档检查结果见[协议验证记录](oidc-protocol-validation.md)。
