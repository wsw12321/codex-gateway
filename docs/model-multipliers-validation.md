# 模型计费倍率验证

倍率按请求准入事务中的数据库快照生效。未设置的模型使用 `1`；读取失败拒绝
准入。管理保存与准入按模型使用事务锁，设置首次创建也遵循相同边界，进程内没有
倍率缓存。结算和恢复只读取 `billing_reservations.pricing_multiplier`，保持基础
价格快照不变，将最终使用的倍率写入账本。

## 接口与页面

- `GET /admin/billing/model-multipliers`：Owner 会话，返回
  `{"models":[{"model":"gpt-6-astra","multiplier":"1","updated_at":null,"editable":true}]}`。
  列表按当前定价目录提供，内部模型固定 `1.0`、只读，首次设置前更新时间为空。
- `PUT /admin/billing/model-multipliers/{model}`：Owner、同源且近期身份验证。
  请求为 `{"multiplier":"0.5","operation_id":"<UUID>","reason":"活动折扣"}`，
  响应为该模型的倍率、更新时间及 `editable`。仅接受字符串十进制，无指数、符号或
  空白，严格大于零，最多 18 位整数、12 位小数。
- 同一操作 ID 和相同规范化参数重放返回原结果；换模型、原因、操作者或倍率复用
  ID 返回冲突。历史清理后的 ID 保留 tombstone，禁止重新执行。
- 保存失败保留输入及可重试的操作 ID，防止重复提交与旧响应覆盖。成功后刷新当前
  数据；提示“仅影响新请求”。账单的已有金额 API 字段不变，增加 `pricing_multiplier`。

## 回归范围

数值回归位于 `internal/billing/multiplier_test.go`，包括默认、折扣、加价、非法
输入、上界溢出、缓存类别及单次舍入边界。例如基础费用 `0.0000000000004` 乘以
`2` 后应得到 `0.000000000001`，不能先把基础费用舍入成零。

`internal/store/model_multipliers_integration_test.go` 使用一次性 PostgreSQL 的隔离
schema，覆盖旧/新快照乱序完成、重复结算、恢复、v1/v2 定价、资金分摊、群组、
报表及上游账号统计、幂等和事务审计、数据库失败、旧数据迁移和历史清理。
`internal/server/gemini_native_integration_test.go` 验证真实 Gemini 原生请求的公开
模型和 customtools 别名使用相同折扣。权限与输入验证位于
`internal/server/model_multipliers_test.go`。

页面截图使用合成数据：

- [桌面](screenshots/model-multipliers-desktop.png)
- [窄屏](screenshots/model-multipliers-mobile.png)

上线前执行 `go build ./cmd/gateway`、`go test -count=1 ./...`、
`go test -race -count=1 ./...`、`go vet ./...`；配置一次性 `TEST_DATABASE_URL` 后执行
`go test -count=1 -tags=integration ./internal/store ./internal/server`。

## 本次验证结果（2026-09-28）

- Gateway 构建、完整单元测试、`go vet`、`gofmt -l .` 和 `git diff --check` 通过。
- 完整竞态检测通过。本机缺少 C 编译器，改在本地固定的
  `golang:1.26.8-bookworm` 镜像中设置 `CGO_ENABLED=1`，只读挂载源码和已下载模块、
  禁用容器网络，执行 `go test -race -count=1 ./...`。
- PostgreSQL `17.6-alpine3.22` 一次性容器使用临时数据目录，仅绑定本机测试端口。
  全部 store/server 集成测试在全新测试库通过；后补的倍率表不可读时恢复结算、
  溢出回滚等专项测试也通过。测试没有连接生产数据库。
- Node 页面状态回归通过：`node --test internal/server/testdata/model_multipliers_ui_test.cjs`。
  完整单元测试通过 Go 包内 wrapper 执行此测试；竞态测试容器未装 Node，页面回归
  单独在宿主机执行。
- Chromium 真实浏览器交互和 1440px / 390px 截图验证通过：
  `node internal/server/testdata/model_multipliers_browser.cjs`。该脚本拦截全部请求、
  使用合成数据，可通过 `PLAYWRIGHT_MODULE` 指定已安装的 Playwright 包，
  `PLAYWRIGHT_BROWSERS_PATH` 指定 Chromium 缓存。测试包含失败保留输入、重试复用
  operation ID、近期身份验证、防重复提交、过时响应、Owner 路由及窄屏无横向溢出。
