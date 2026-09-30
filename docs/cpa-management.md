# CPA 账号授权与凭据维护

Owner 登录 Gateway 后，从“CPA 授权管理”进入 `/admin/cpa/`。该页面复用 Gateway 会话。凭据导入、OAuth、刷新、启停、查询额度及删除要求近期身份验证和同源请求；需要重新验证时，返回 Gateway 完成身份验证后再操作。

页面的源码基线是 Management Center `v1.25.0`，固定提交 `b87b9487f63e08ad97b1fb4e7c17b4adb811b922`。仓库保存其 `Button`、`Card` 组件、许可证和原始依赖锁，并使用专门的 Gateway 页面及 API 层。它是受限的源码衍生版本，未嵌入上游完整 SPA。`deploy/cpa-panel/UPSTREAM.json` 和 `upstream.sha256` 记录具体来源；构建结果嵌入 Gateway。

## 授权与导入

1. 选择 Codex 或 Antigravity，点击“新增 / 重新授权”。
2. 打开供应商授权页面，完成登录。供应商回调使用其已注册的本地地址；网页可能显示本地连接失败，复制地址栏中的完整回调 URL 即可。
3. 在原 Gateway 会话中粘贴回调地址并提交。每次流程有效期为 5 分钟，回调只接受一次，不开放额外公网回调端口。Gateway 或 CPA 重启后应重新发起授权。
4. 刷新账号列表，检查供应商状态，再使用 Gateway API Key 完成真实生成验收。

JSON 文件导入仅接受 `refresh_token` 及可选的 `access_token`、`id_token`。页面只向后端发送这三个字段，文件内容不写入浏览器存储，提交后清空文件输入，成功或失败都会清除持有的凭据对象。CPA 对新导入凭据实际执行刷新；Antigravity 还核对真实 Google 身份、项目和 Gemini 额度。不能仅因 access token 尚未过期就视为迁移成功。批量旧 Keyring 迁移应使用[服务端迁移工具](cpa-credentials-migration.md)，不要通过浏览器搬运迁移令牌。

账号启停使用与 Gateway 原有账号页面相同的业务状态。删除会先持久停用，等账号活动请求结束后移除凭据，保留稳定身份映射、控制状态和历史账单。如果排空超时或并发状态无法确认，删除失败，账号继续停用；稍后重试即可。

使用 `legacy-bridge` 回滚链路期间，CPA 面板的 Antigravity 凭据操作会被拒绝，避免两个进程刷新同一 Google 凭据。此时通过 Gateway 原账号页面管理旧链路的业务状态，恢复 CPA 后再维护凭据。

## 管理边界

页面仅使用 `/admin/cpa/api/*` 的明确接口。Gateway 使用独立的 `CPA_MANAGEMENT_KEY_FILE` 在服务端请求 CPA 的 `/internal/gateway-management/*`，该密钥必须与请求转发密钥不同。浏览器不会收到 CPA 管理密钥或原始配置。

通用 CPA 管理 API 和上游面板自动更新继续关闭。本页面不提供 YAML、代理、模型别名、重试、插件、任意上游请求、凭据下载或批量全删接口；直接访问这些路径也不能绕过后端允许列表。账号权限、共享/专属名单、权重、并发、计费和余额继续在 Gateway 页面管理。

## 构建与检查

安装 Bun `1.3.14` 后运行：

```sh
./scripts/build-cpa-panel.sh
./scripts/validate-cpa-panel.sh
go test -count=1 ./internal/server -run TestCPAManagement
```

构建在临时目录安装固定依赖，仓库不保留 `node_modules`。JS 和 CSS 为同源独立文件，继续使用 `script-src 'self'` 与 `style-src 'self'`，无需放开内联脚本或公网 CDN。已提交的资源摘要位于 `deploy/cpa-panel/assets.sha256`。

可选浏览器回归位于 `internal/server/testdata/cpa_admin_browser.cjs`，通过 `PLAYWRIGHT_MODULE` 指定现有 Playwright 安装。它使用合成账号并拦截全部网络请求，覆盖供应商切换、OAuth 回调、凭据导入成功/失败后的清理、状态修改、删除、桌面/手机布局和 CSP。截图为 [桌面](screenshots/cpa-admin-desktop.png) / [手机](screenshots/cpa-admin-mobile.png)。这些测试不代替部署服务器的真实账号 OAuth、刷新后重启及生成请求验收。
