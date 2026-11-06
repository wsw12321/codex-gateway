# 额度数据时间、刷新与可视化验证

本记录覆盖管理台额度展示的一组改动：所有额度旁标注数据时间并提供刷新按钮。

## 额度数据时间与刷新

页面上的额度都是某一次读取的快照。为避免把停留已久的页面误读为当前额度，
每处额度旁都显示“数据时间：YYYY/MM/DD HH:mm:ss”（浏览器本地时区，精确到秒），
并在旁边提供只重新读取该数据的“刷新”按钮。数据时间是本次响应到达浏览器的时间；
上游账号额度另外标注上游查询或 CPA 观测时间。

| 位置 | 数据来源 | 刷新范围 |
| --- | --- | --- |
| 概览 · 现金余额、各订阅剩余 | `GET /admin/billing/me?limit=1` | 只重读本人账务，不重载请求、全员统计、账号与告警 |
| 额度与订阅 · 现金余额、订阅额度、群组额度卡片 | 同一次 `GET /admin/billing/me` 或 `/admin/billing/users/{id}` | 重读当前查看用户的账务，保留流水页码 |
| 额度与订阅 · 群组账号用量 | `GET /admin/billing/me/account-usage` | 原有“刷新用量”，时间改为精确到秒；每个账号仍显示“查询于/观测于” |
| 群组额度 · 群组列表 | `GET /admin/groups` | 与页首“刷新”相同 |
| 群组额度 · 群组详情指标 | `GET /admin/groups/{id}` | 只重读当前群组 |
| 上游账号 · 账号列表 | `GET /admin/upstream-accounts` 等 | 按当前历史统计区间重读列表、状态与近 24 小时费用 |

读取中显示“正在刷新…”，失败显示“数据时间：加载失败”或沿用原有错误提示，不保留
旧时间，避免把失败前的数据当成刚读取的数据。退出登录或切换身份时清空时间。

行为与安全影响：只改前端展示与读取时机，不新增接口、不改变权限、计费或选号。
刷新按钮复用既有只读接口，受原有会话与 Owner 校验约束。

## 验证

```sh
gofmt -l .
go vet ./...
go test -count=1 ./...
node --test internal/server/testdata/quota_visualization_ui_test.cjs
node internal/server/testdata/quota_visualization_browser.cjs   # 需要 Playwright，生成截图
```

`quota_visualization_ui_test.cjs` 覆盖：时间格式含秒、刷新按钮调用对应读取、账务读取中/
成功/失败三种状态、群组卡片刷新、概览余额单独刷新且身份不一致时报错、群组详情时间
随重置清空。浏览器脚本在 1440×1000 与 390×844 视口检查无横向溢出，并逐页点击刷新
按钮确认只读取对应接口。

截图（`docs/screenshots/quota-visualization/`）：

- `overview-quota-freshness-desktop.png`
- `billing-quota-freshness-desktop.png`、`billing-quota-freshness-mobile.png`
- `groups-quota-freshness-desktop.png`
- `upstream-accounts-freshness-desktop.png`
