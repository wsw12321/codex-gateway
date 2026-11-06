# 额度数据时间、刷新与可视化验证

本记录覆盖管理台额度展示的一组改动：额度数据时间与刷新、使用统计按模型汇总、群组用量可视化、群组指定账号额度。

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

## 群组用量可视化（Owner）

“群组额度”详情的指标卡下方新增“用量可视化”卡片，可在最近 24 个周期间切换（默认当前
周期，选择最新周期时跟随群组进入下一期），旁边标注数据时间与刷新按钮：

- **群组额度构成**：一条线代表本期群组额度，按成员支付金额分色，未用部分为浅色“剩余
  额度”。已移出群组但本期用过群组额度的成员标注“（已移出）”；若成员计数之和小于群组
  已用（成员计数启用前结算的费用），差额显示为灰色“未归属成员”。悬停或点击色块显示
  “本期群组支付、占群组额度、占已用”。
- **成员上限使用**：设置了单个成员周期上限时，列出每位成员“已用 / 上限”及进度条。
- **账号池用量构成**：对“指定账号”池中的账号，以及本周期由群组额度支付过的其他账号，
  按周期起止时间统计已结算成本（`actual_cost_usd`，含模型倍率，与上游账号页一致），
  每个账号一条线：群组成员各自一色，非群组用户为灰色斜纹（纹理与“其他成员”的纯灰
  区分，不依赖颜色）。账号卡注明是否在账号池，以及总成本、成员与非群组占比、群组支付。
  账号池为“全部账号”时不展开全部账号，只显示群组实际支付过的账号。

颜色按用户固定：同一用户在额度条与各账号条中颜色相同。调色板为 7 个分类色（亮/暗两套，
已用 dataviz 校验器在 `#ffffff` 与 `#14262a` 表面验证相邻色 CVD ΔE ≥ 8.4、正常视觉
ΔE ≥ 19.3）；超过 7 个用户时，用量最大的 6 人保留颜色，其余合并为“其他成员（N 人）”，
金额按十进制精确相加。亮色模式下 3 个色块对比度低于 3:1，因此每条线都附带列出姓名、
金额与占比的图例。

新增 `GET /admin/groups/{id}/usage-breakdown?period_id=…`（Owner；`period_id` 可省略，
其他查询参数返回 400）。存储层 `GroupUsageBreakdown` 先通过 `GetGroup` 滚动周期，
成员用量取自 `group_member_usage`（与成员上限计数同源，不受账务清理影响），账号成本
取自 `billing_ledger_entries`。只读，不新增迁移，不改变计费、权限或选号。响应：

```json
{"group_id":"…","period":{"id":"…","starts_at":"…","ends_at":"…"},"current_period":true,
 "limit_usd":"100.000000000000","member_limit_usd":"40.000000000000",
 "used_usd":"62.400000000000","remaining_usd":"37.600000000000","data_complete":true,
 "members":[{"user_id":"…","username":"alice","display_name":"Alice","current_member":true,"group_used_usd":"24.100000000000"}],
 "accounts":[{"account_id":"…","provider":"codex","alias":"团队主账号","in_pool":true,
   "total_cost_usd":"71.300000000000","member_cost_usd":"52.900000000000",
   "non_member_cost_usd":"18.400000000000","group_charged_usd":"48.200000000000",
   "members":[{"user_id":"…","cost_usd":"24.100000000000"}]}],
 "periods":[{"id":"…","starts_at":"…","ends_at":"…"}]}
```

非群组用户只返回合计，不列出其身份。

### 指定账号额度（5 小时 / 每周）

用量卡片底部列出群组“指定账号”的 Codex、Claude 账号池中每个账号的 5 小时与每周已用
比例（线形进度条）、重置时间、状态（正常、已达限额、冷却至某时、查询失败原因）以及
“查询于/观测于”时间，旁边是本次读取的数据时间与“刷新额度”。“全部账号”池与
Antigravity（没有 5 小时/周窗口）不展示。保存账号池后自动重新读取。

新增 `GET /admin/groups/{id}/account-usage`（Owner，不接受查询参数）。与成员的
`/admin/billing/me/account-usage` 不同，它不受“允许成员查看用量”开关、归档或到期
影响，并返回账号 ID、别名、名称与脱敏邮箱，便于 Owner 对照账号页：

```json
{"group_id":"…","truncated":false,"accounts":[{"account_id":"…","provider":"codex",
  "alias":"团队主账号","email_masked":"a***@example.test","source":"live",
  "updated_at":"…","limited":false,
  "five_hour":{"used_percent":38,"reset_at":"…"},"seven_day":{"used_percent":64,"reset_at":"…"}}]}
```

读取复用成员视图的共享缓存与合并逻辑（`collectAccountUsage`，从 `myAccountUsage`
提取，行为不变）：Codex 每账号 1 分钟内只向上游查询一次、失败缓存 10 秒，Claude 只读
CPA 快照；单次最多等待 20 秒，超时读取在后台继续填充缓存。最多 64 个账号。

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
TEST_DATABASE_URL=… go test -count=1 -tags=integration -run 'TestGroupUsageBreakdown' ./internal/store
go test -count=1 -run 'TestGroupUsageBreakdown' ./internal/server
```

群组用量集成测试覆盖：已移出成员保留本期用量、非成员只计入合计、周期开始前的成本与
不在账号池且群组未支付的账号不计入、群组仅支付过的账号标记为不在账号池、新周期从零
开始且旧周期仍可按 ID 查询、其他群组的周期或未知周期返回 404、非法 ID 返回 400。
接口单元测试确认成员 403、只接受单个非空 `period_id`、错误映射且不外泄内部信息。
`TestGroupPoolAccountUsageIsOwnerOnlyAndNamesAccounts` 确认成员 403、拒绝查询参数、
返回账号身份与窗口、失败账号只给固定错误码、第二次读取命中缓存；集成测试确认只列出
“指定账号”的 Codex 与 Claude 账号（Codex 在前），忽略 Antigravity 与“全部账号”池。

存储集成测试确认按扣款排序、失败与进行中计数、窗口外及其他用户不计入、模型筛选，
以及各模型合计与 `SummarizeUsageRequests` 一致（精确到 12 位小数）。接口单元测试
确认未登录 401、默认 24 小时窗口、成员不能查看他人、Owner 可指定用户、非法区间 400、
存储错误不外泄。

`quota_visualization_ui_test.cjs` 覆盖：时间格式含秒、刷新按钮调用对应读取、账务读取中/
成功/失败三种状态、群组卡片刷新、概览余额单独刷新且身份不一致时报错、群组详情时间
随重置清空；十进制精确求和；线形图按金额定宽、颜色按实体固定、悬停与点击显示金额、
超过 7 项合并为“其他”；使用统计默认只读模型汇总、展开时才读一次请求明细、失败时
标注“加载失败”；群组卡片的成员与已移出成员分段、未归属差额、成员上限进度、账号条中
非群组用户纹理与同一用户同色、超过 7 人合并、周期切换与加载失败；指定账号额度的
进度条、失败原因、未知窗口不画条、空账号池说明与重置。浏览器脚本在 1440×1000 与 390×844 视口检查无横向溢出，并逐页点击刷新
按钮确认只读取对应接口。

截图（`docs/screenshots/quota-visualization/`）：

- `overview-quota-freshness-desktop.png`
- `billing-quota-freshness-desktop.png`、`billing-quota-freshness-mobile.png`
- `groups-quota-freshness-desktop.png`
- `upstream-accounts-freshness-desktop.png`
- `usage-models-desktop.png`、`usage-models-mobile.png`、`usage-details-expanded-desktop.png`
- `group-usage-visual-desktop.png`、`group-usage-visual-mobile.png`、`group-account-quota-desktop.png`

浏览器脚本以生产环境的 CSP（`style-src 'self'`，无内联样式）提供页面，并把 CSP
报错视为失败，确认线形图通过 CSSOM 设置宽度不受策略阻止。
