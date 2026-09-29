# 控制台界面优化验证

2026-09-29 完成导航分组、移动导航抽屉、独立概览快照、上游账号紧凑列表和额度订阅卡优化。保留原生 JavaScript、现有 API、精确金额格式、身份验证、权限及计费语义；新增界面状态只保存在当前页面内存，浏览器整页重载后清空。列表刷新会恢复未保存的行内输入。

## 行为与回归

- 导航分为工作台、用量与账务和管理，账号安全位于用户区。Member 不显示管理分组。
- 上游账号保留两个旧 hash，页面内切换 Codex / Antigravity，刷新及浏览器前进后退均可用。摘要显示最终状态、并发、费用、启停和异常；详情包含权限、分配、完整状态、历史及官方额度。
- 搜索、状态筛选与排序复用账号节点；刷新、展开和服务商切换保留行内草稿及额度结果。成功字段采用服务器值；退出与身份变化清空。验证期间切换服务商会取消待执行操作，迟到响应不能覆盖当前列表或注销新身份。
- 概览独立请求本人账务和最近 7 天用量。管理概况使用本月全员统计、开放告警及两个服务商分别加载的账号快照。单个接口失败只影响对应区域；没有新增概览后台轮询。
- 订阅分别展示剩余额度、周期额度、比例、周期结束及最终到期，区分订阅有效性与扣费开关。缺失数据、未启用和无限期分别显示；不合计不同周期额度、不预测下一笔扣款来源。
- 充值、调整、订阅和批量管理保留独立入口；失败自动展开对应区域并保留输入，成功但刷新失败有独立反馈。本人扣费来源可编辑，他人只读。
- 抽屉覆盖遮罩、Escape、首尾 Tab 约束、背景 inert、滚动锁、普通关闭恢复菜单焦点、导航后聚焦内容，以及桌面切换和退出清理。
- 个人请求、监控和账务流水在手机上展示主要字段，次要字段行内展开；管理批量表格保留带名称、可聚焦的横向滚动区域。

## 已运行检查

环境：Go 1.26.8、Node.js 24.16.0、Chromium 153.0.8010.12。

```sh
go test -count=1 ./...
go vet ./...
go build -o /tmp/gateway-ui-verification/gateway ./cmd/gateway
gofmt -l .
git diff --check
```

Go 测试包含上游账号、扣费来源、批量操作、用户选择、模型倍率和新增概览的 Node 回归。新增概览测试覆盖本人数据隔离、Member 请求范围、服务商故障、身份切换、迟到响应与精确金额；订阅新增有效性、未知数据、无限期及进度边界检查。

以下浏览器脚本均已运行，所有 HTTP 请求使用拦截的合成数据，不需要真实账号、上游服务或数据库：

```sh
node internal/server/testdata/dashboard_layout_browser.cjs
node internal/server/testdata/overview_browser.cjs
node internal/server/testdata/upstream_allocation_browser.cjs
node internal/server/testdata/antigravity_accounts_browser.cjs
node internal/server/testdata/billing_sources_browser.cjs
node internal/server/testdata/groups_access_browser.cjs
```

Playwright 可安装在仓库外，通过 `PLAYWRIGHT_MODULE=/path/to/node_modules/playwright` 指定模块；非默认浏览器路径可设置 `PLAYWRIGHT_BROWSERS_PATH`。截图使用系统中文字体，可通过 `SCREENSHOT_DIR` 指定输出目录。

1440×1080、390×844、320px 及 850/851px 断点通过。验证页无页面级横向溢出或未捕获脚本错误；预期的失败请求显示对应错误状态。390×844 上游账号首屏可见完整首个账号摘要，展开的移动流水详情占整行可读宽度。

## 合成数据截图

| 页面 | 桌面 | 手机 |
| --- | --- | --- |
| 概览 | [Owner 概览](screenshots/ui-refresh/overview-owner-desktop.png) | [Member 概览](screenshots/ui-refresh/overview-member-mobile.png) |
| 额度与订阅 | [账务](screenshots/ui-refresh/billing-desktop.png) | [账务](screenshots/ui-refresh/billing-mobile.png) |
| 上游账号 | [账号摘要](screenshots/ui-refresh/accounts-desktop.png) / [展开详情](screenshots/ui-refresh/account-expanded-desktop.png) | [账号摘要](screenshots/ui-refresh/accounts-mobile.png) |
| 导航 | — | [菜单抽屉](screenshots/ui-refresh/navigation-mobile.png) |
