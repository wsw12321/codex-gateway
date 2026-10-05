# 水源喵界面验证

水源喵中转站沿用 `auth.water555.com` 的雾白、深青、湖水青、圆角卡片与克制的品牌排版；登录、控制台、统一登录回调和 CPA 授权管理共享浅色、深色及跟随系统三种外观选择。

`/static/theme.js` 在样式加载前应用外观。浏览器仅在 `shuiyuan-theme` 中保存 `light`、`dark` 或 `system`，不会保存认证数据。未设置、旧值无效或浏览器拒绝存储时，默认跟随系统；用户仍可在当前页面切换。

## 浏览器验证

```sh
PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chromium \
node internal/server/testdata/shuiyuan_theme_browser.cjs
```

浏览器驱动与 Chromium 可复用仓库外的安装。`PLAYWRIGHT_CHROMIUM_EXECUTABLE` 未设置时使用 Playwright 默认浏览器；`SCREENSHOT_DIR` 可改写截图目录。所有请求与账号均为本地合成数据，无需数据库、真实凭证或外部服务。

脚本覆盖完整应用启动及密码登录、统一品牌、同源 CSP、系统外观实时变化、手动选择优先级与刷新持久化、Enter / Space 键操作、存储不可用、无效旧值，以及 320 / 390 / 1440 像素下的登录和控制台布局。移动端还检查恢复入口、设备与项目、API Keys、使用指导、额度与订阅、账号安全和使用统计。

现有控制台布局、OIDC 和 CPA 浏览器回归继续检查导航抽屉、身份切换、登录回调与凭据管理；合成服务已补充共享主题及图标静态资源。

## 已运行检查

2026-10-05 使用 Go 1.26.8、Node.js 24.21.0、Chrome 154.0.8037.97 验证。

```sh
go test -count=1 ./...
go vet ./...
go build -buildvcs=false -o /tmp/shuiyuan-gateway ./cmd/gateway
./scripts/validate-cpa-panel.sh
gofmt -l .
git diff --check
```

主题浏览器回归与现有控制台布局、OIDC、CPA、概览、上游账号、账号分配、扣费来源、群组权限、个人资料、使用指导、订阅套餐、邀请、模型倍率、模型权限批量操作、数据维护及网页工作台接入浏览器回归通过。控制台布局脚本还以 `BROWSER_COLOR_SCHEME=dark` 验证 Owner 概览、账务、上游账号和移动导航。Go 测试包括主题偏好、系统变化、跨标签页同步、存储失败及静态资源缓存与 CSP 检查。

## 合成数据截图

| 页面 | 浅色 | 深色 |
| --- | --- | --- |
| 桌面登录 | [浅色](screenshots/shuiyuan/login-light-desktop.png) | [深色](screenshots/shuiyuan/login-dark-desktop.png) |
| 手机登录 | [浅色](screenshots/shuiyuan/login-light-mobile.png) | [深色](screenshots/shuiyuan/login-dark-mobile.png) |
| 桌面控制台 | [浅色](screenshots/shuiyuan/dashboard-light-desktop.png) | [深色](screenshots/shuiyuan/dashboard-dark-desktop.png) |
| 手机控制台 | [浅色](screenshots/shuiyuan/dashboard-light-mobile.png) | [深色](screenshots/shuiyuan/dashboard-dark-mobile.png) |
| 桌面授权管理 | [浅色](screenshots/shuiyuan/cpa-light-desktop.png) | [深色](screenshots/shuiyuan/cpa-dark-desktop.png) |
| 手机授权管理 | [浅色](screenshots/shuiyuan/cpa-light-mobile.png) | [深色](screenshots/shuiyuan/cpa-dark-mobile.png) |
| 桌面登录回调 | [浅色](screenshots/shuiyuan/callback-light-desktop.png) | [深色](screenshots/shuiyuan/callback-dark-desktop.png) |
| 手机登录回调 | [浅色](screenshots/shuiyuan/callback-light-mobile.png) | [深色](screenshots/shuiyuan/callback-dark-mobile.png) |

管理视图深色截图：[Owner 概览](screenshots/shuiyuan/owner-overview-dark-desktop.png)、[额度与订阅](screenshots/shuiyuan/billing-dark-desktop.png)、[上游账号](screenshots/shuiyuan/accounts-dark-desktop.png)。
