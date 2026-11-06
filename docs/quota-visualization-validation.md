# 额度数据时间、刷新与可视化验证

本记录覆盖管理台额度展示的一组改动：额度数据时间与刷新、使用统计按模型汇总。

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

## 使用统计：最近 24 小时各模型用量

“使用统计 · 个人明细”默认不再列出每条请求，而是显示最近 24 小时各模型用了多少额度：

- 汇总卡：实际扣款（已结算 `usage_charge`，含模型倍率）、请求数（含模型数）、Token；
- 一条按扣款占比分色的线形图，悬停或点击色块显示“模型：金额，占比”，图例列出
  每个模型的金额与占比；超过 7 个有扣款的模型时，较小的合并为灰色“其他模型（N 项）”，
  其金额按十进制精确相加；
- 明细表按实际扣款降序，列出扣款、占比进度条、请求（附失败、进行中数量）与 Token。
  没有已结算扣款的模型只在表中出现。

“展开请求明细”展开原有的筛选、汇总卡、请求表与 CSV 导出；首次展开时才请求
`/admin/usage`，收起后再展开复用已加载结果。从概览“查看使用记录”或全员汇总
点击用户进入时自动展开。Owner 查看某个用户时，模型用量同样只统计该用户。

新增 `GET /admin/usage/models`（需要登录），接受与 `/admin/usage` 相同的筛选参数；
未提供 `from` 时统计 `until`（默认当前时间）之前 24 小时。普通成员传入 `user_id`
会被忽略，只能查看本人；Owner 可传 `user_id`。响应：

```json
{"from":"…","until":"…","generated_at":"…","truncated":false,
 "total":{"requests":57,"tokens":1873400,"charged_usd":"6.418200000000"},
 "models":[{"model":"gpt-5.6","requests":31,"errors":1,"in_progress":0,
   "input_tokens":…,"cached_input_tokens":…,"output_tokens":…,"tokens":1204000,
   "charged_usd":"3.912000000000"}]}
```

存储层 `SummarizeUsageByModel` 与 `SummarizeUsageRequests` 使用同一筛选条件与
账务关联，因此各模型之和等于同区间汇总；最多返回 200 个模型，合计仍覆盖全部模型。
只读查询，不新增迁移。

## 验证

```sh
gofmt -l .
go vet ./...
go test -count=1 ./...
node --test internal/server/testdata/quota_visualization_ui_test.cjs
node internal/server/testdata/quota_visualization_browser.cjs   # 需要 Playwright，生成截图
```

```sh
TEST_DATABASE_URL='postgres://gateway:password@127.0.0.1:5432/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration -run 'TestSummarizeUsageByModel' ./internal/store
go test -count=1 -run 'TestUsageModels' ./internal/server
```

存储集成测试确认按扣款排序、失败与进行中计数、窗口外及其他用户不计入、模型筛选，
以及各模型合计与 `SummarizeUsageRequests` 一致（精确到 12 位小数）。接口单元测试
确认未登录 401、默认 24 小时窗口、成员不能查看他人、Owner 可指定用户、非法区间 400、
存储错误不外泄。

`quota_visualization_ui_test.cjs` 覆盖：时间格式含秒、刷新按钮调用对应读取、账务读取中/
成功/失败三种状态、群组卡片刷新、概览余额单独刷新且身份不一致时报错、群组详情时间
随重置清空；十进制精确求和；线形图按金额定宽、颜色按实体固定、悬停与点击显示金额、
超过 7 项合并为“其他”；使用统计默认只读模型汇总、展开时才读一次请求明细、失败时
标注“加载失败”。浏览器脚本在 1440×1000 与 390×844 视口检查无横向溢出，并逐页点击刷新
按钮确认只读取对应接口。

截图（`docs/screenshots/quota-visualization/`）：

- `overview-quota-freshness-desktop.png`
- `billing-quota-freshness-desktop.png`、`billing-quota-freshness-mobile.png`
- `groups-quota-freshness-desktop.png`
- `upstream-accounts-freshness-desktop.png`
- `usage-models-desktop.png`、`usage-models-mobile.png`、`usage-details-expanded-desktop.png`

浏览器脚本以生产环境的 CSP（`style-src 'self'`，无内联样式）提供页面，并把 CSP
报错视为失败，确认线形图通过 CSSOM 设置宽度不受策略阻止。
