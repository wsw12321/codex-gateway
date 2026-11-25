# 品牌升级验证（百川阁）

2026-11-25 将站点品牌从“水源喵中转站”升级为“百川阁”。本次只改文案与品牌展示，不改接口、数据、权限或计费。

## 品牌规范

| 项目 | 内容 | 位置 |
| --- | --- | --- |
| 主品牌 | 百川阁 | 浏览器标题、登录页与回调页页头、控制台侧栏、控制台页头、CPA 授权管理、Passkey 显示名 |
| 拼音字标 | BAICHUAN GE | 页头品牌下方的小字；登录页主视觉的眉标为 `BAICHUAN GE · AI GATEWAY` |
| 主标语 | 众源成流，聚流成渊 | 登录页主视觉、首页 `<title>` 与描述、登录页和回调页页脚、控制台侧栏品牌下方 |
| 卖家页副标语 | 你出水源，我来汇流 | 我的供应商、我的套餐、成员的“我的出口” |
| 买家页副标语 | 一处接入，百家模型 | 套餐（商城） |

- 副标语是各页说明文字上方的一行 `.page-slogan`（衬线、主题强调色），带 `data-page-role="seller"` 或 `"buyer"`。分区标题区里的 `.eyebrow` 在控制台中是隐藏的，所以副标语不复用它。
- “出口服务器”页由 Owner 和成员共用：副标语带 `member-market`，只在成员开启商城后显示；Owner 看到的平台出口页没有副标语。
- 页面标题统一为“页面名 · 百川阁”，例如“套餐 · 百川阁”；成员的出口页标题与页头一致，为“我的出口 · 百川阁”（以前标题栏显示“出口服务器”）。
- 泛指本站的“中转站”改为“本站”：使用指导里的“本站模型目录”，CPA 授权管理里的“本站权限”和账号状态“本站：…”。

## 保持不变

- **标志图片**：`favicon.svg` 内嵌原始 `Logo-WaterMiao.png`，登录页大图仍显示 “WaterMiao” 字样。替换需要新的标志素材，本次未改。
- **Passkey**：WebAuthn RP ID 不变，已注册的 Passkey 继续可用。新的显示名“百川阁”只在新注册时写入；部分密码管理器会继续显示注册时保存的旧名称。
- **外观偏好**：浏览器存储键仍为 `shuiyuan-theme`，用户已保存的浅色、深色或自动设置不会丢失。
- **吾水阁**：统一登录（`auth.water555.com`）是独立品牌，相关文案不变。
- **历史文档**：`docs/` 中的旧验证记录和截图保留“水源喵”原样，作为当时的记录。

## 改动文件

- `internal/server/assets/index.html`、`style.css`、`app.js`：登录页、控制台、页面标题、副标语与样式；`app.js` 用 `brandName` 常量生成页面标题和邀请注册标题。
- `internal/server/assets/oidc-callback.html`、`oidc-callback.js`：统一登录回调页与提示文案。
- `deploy/cpa-panel/index.html`、`deploy/cpa-panel/src/main.tsx`：CPA 授权管理源码；`internal/server/assets/cpa/*` 与 `deploy/cpa-panel/assets.sha256` 由 `scripts/build-cpa-panel.sh`（Bun 1.3.14）重新生成。重新构建前已确认未改源码时构建结果与仓库一致；本次只有 `index.html` 与 `panel.js` 的文字变化，`panel.css` 不变。
- `internal/identity/service.go`：Passkey 显示名。
- `internal/server/assets_brand_test.go`：新增测试，检查品牌名、主标语、副标语所在页面与次数，并确认面向用户的页面中不再出现“水源喵”“SHUIYUAN”“中转站”“AI RELAY”。
- 浏览器脚本：`shuiyuan_theme_browser.cjs` 检查登录页的主标语和侧栏品牌，并补上缺失的 `/admin/model-routes` 合成响应（升级前脚本因此中断）；`marketplace_browser.cjs` 检查套餐页的买家副标语、我的套餐的卖家副标语和页面标题；`tenant_providers_browser.cjs` 检查我的供应商与成员出口页的卖家副标语，并确认 Owner 看不到它。`cpa_admin_browser.cjs` 与 `oidc_browser.cjs` 的标题断言改为匹配“百川阁”。

## 已运行检查

2026-11-25 使用 Go 1.26.11、Node.js 22.23.5、Chromium 158.0.8127.36、Bun 1.3.14 验证。

```sh
gofmt -l .
go vet ./...
go build ./cmd/gateway
go test -count=1 ./...
./scripts/validate-cpa-panel.sh
node internal/server/testdata/shuiyuan_theme_browser.cjs
node internal/server/testdata/marketplace_browser.cjs
node internal/server/testdata/tenant_providers_browser.cjs
git diff --check
```

- 以上全部通过。主题脚本覆盖 320 / 390 / 1440 像素下的登录、控制台与恢复页，没有横向溢出。
- `go test` 里 `TestClientConfigurator` 的两项 Claude 配置用例会读取运行环境的 `ANTHROPIC_BASE_URL` 等变量；在已配置 Claude Code 的终端中运行时会把它们当作冲突而失败，升级前同样如此。去掉这两个变量后（`env -u CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST -u ANTHROPIC_BASE_URL go test -count=1 ./...`）全部通过。
- CPA 授权管理与统一登录回调页的页头另用合成页面在 320 / 390 / 1440 像素下检查：品牌、标题、眉标与页脚为新文案，没有横向溢出，390 像素下主题切换与品牌保持同一行（与升级前一致）。
- `cpa_admin_browser.cjs` 与 `oidc_browser.cjs` 在升级前的主干上已经失败（CPA 出口选择框已改名为“新增凭据使用的出口”，脚本仍按旧标签查找；OIDC 脚本第 286 行的一次性登录标记断言不成立），与本次改动无关，未一并修复。`cpa_admin_browser.cjs` 的新标题断言位于失败点之前，已通过。

## 合成数据截图

| 页面 | 桌面 | 手机 |
| --- | --- | --- |
| 登录（主标语） | [浅色](screenshots/brand/login-light-desktop.png) | [深色](screenshots/brand/login-dark-mobile.png) |
| 控制台概览（侧栏品牌） | [浅色](screenshots/brand/dashboard-light-desktop.png) | [深色](screenshots/brand/dashboard-dark-mobile.png) |
| 买家页：套餐 | [桌面](screenshots/brand/buyer-plans-desktop.png) | [手机](screenshots/brand/buyer-plans-mobile.png) |
| 卖家页：我的供应商 | [桌面](screenshots/brand/seller-providers-desktop.png) | [手机](screenshots/brand/seller-providers-mobile.png) |
| 卖家页：我的套餐 | [桌面](screenshots/brand/seller-my-plans-desktop.png) | — |
| 卖家页：我的出口 | [桌面](screenshots/brand/seller-egress-desktop.png) | — |
