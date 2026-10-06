# 模型定价验证

模型基础单价覆盖保存在 PostgreSQL。请求准入事务读取有效价格并保存完整快照，
与管理员保存使用同一模型的共享／独占事务锁；查询失败或配置结构冲突拒绝准入。
结算、恢复和历史报表继续使用原请求快照及倍率，不读取当前覆盖价格。

## 接口与操作

- `GET /admin/billing/model-prices` 仅允许 Owner，返回 `models` 列表。每行包含
  `configured_price`、`effective_price`、`schema_version`、`multiplier`、`version`、
  `structure_id`、`source`、`conflict`、`editable` 和 `updated_at`。
- `PUT /admin/billing/model-prices/{model}` 要求 Owner、同源请求及近期身份验证。
  保存提交 `action: "save"`、完整 `price` 矩阵、`operation_id`、`reason`、`version`
  和 `structure_id`。恢复提交 `action: "restore"`，省略 `price`。
- 金额为精确十进制字符串，可为零，最多 18 位整数、12 位小数。服务层、上下文档位、
  阈值、最大输入和缓存计费结构不可通过该接口改变。内部模型始终显示固定零价。
- 恢复清除覆盖但保留递增版本。相同操作和参数重放返回首次响应；过时的新操作返回
  409。页面保留失败草稿和重试 ID，冲突后要求显式重新加载该模型。
- 价格、操作记录、账本和审计同事务提交。当前设置不受历史清理影响，已清理的操作
  ID 由 tombstone 阻止再次执行；保留的价格设置仍保护其操作者引用。

## 回归范围

- `internal/config/model_prices_test.go`：完整服务层／上下文矩阵、两种缓存计费模式、
  v1 兼容、精确金额与零价、只读结构和结构标识。
- `internal/server/model_prices_test.go`：Owner、来源与近期验证，严格请求校验，
  恢复、内部模型限制、存储故障及冲突响应。
- `internal/store/model_prices_integration_test.go`：CRUD、递增版本、幂等、审计失败
  整体回滚、账本可见性、部署结构冲突、重新保存和恢复后重新准入。
- `internal/store/model_prices_acceptance_integration_test.go`：独立连接池间立即生效，
  v1/v2 旧请求按旧价、新请求按新价乘倍率，恢复结算不依赖当前价格表，数据库读取
  失败时准入完全回滚，保存与准入的双向锁等待，以及历史清理和操作者保护。
- `internal/store/model_prices_matrix_integration_test.go`：覆盖价格在各服务层、长短
  上下文与缓存模式下的实际计费选择。
- `internal/server/model_prices_integration_test.go`：真实 HTTP 路由、PostgreSQL
  事务、完整矩阵、版本冲突、幂等及部署结构变化后的恢复。
- `internal/server/testdata/model_prices_ui_test.cjs`：矩阵、十进制校验、验证重试、
  失败草稿、重复提交、乱序响应、409 后显式刷新、搜索和退出清理。

## 验证命令

使用 Go 1.26.8、Node.js、C 编译器及一次性 PostgreSQL：

```sh
go build ./cmd/gateway
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
gofmt -l .
git diff --check
TEST_DATABASE_URL='<disposable-postgresql-url>' go test -count=1 -tags=integration ./internal/store ./internal/server
node --test internal/server/testdata/model_prices_ui_test.cjs
node internal/server/testdata/model_prices_browser.cjs
```

store 与 server 的完整集成测试可分别使用两个全新测试库，避免不同包的历史迁移
夹具共享数据库状态。浏览器脚本完全拦截请求并使用合成数据；通过 `PLAYWRIGHT_MODULE`
指定已安装的 Playwright，通过 `PLAYWRIGHT_CHROMIUM_EXECUTABLE` 指定 Chromium。

页面截图：

- [桌面，1440px](screenshots/model-pricing-desktop.png)
- [手机，390px](screenshots/model-pricing-mobile.png)

## 本次验证结果（2026-10-06）

- Go 1.26.8 Gateway 构建、全量单元测试、全量竞态检查、`go vet ./...`、
  `gofmt -l .` 和 `git diff --check` 全部通过。
- 竞态检查使用已有的本地 Go 1.26.8／GCC 容器，源码只读挂载且容器禁用外部网络。
- PostgreSQL 16.15 一次性容器使用临时数据目录和仅本机可访问端口；在两个全新
  测试库中分别执行全部 store 和 server 集成测试，全部通过，未连接生产数据库。
- 新增 15 项页面状态回归通过；Chromium 真实浏览器检查验证近期身份认证、
  完整矩阵、保存／恢复、失败重试、重复提交、乱序响应、冲突重新加载、Owner
  路由和桌面／手机布局，截图使用合成数据。
- 价格覆盖实际结算覆盖 12 个服务层／上下文／缓存模式组合及 4 个兜底组合，
  并验证 v1/v2 跨实例生效、旧请求快照、双向事务锁等待、失败回滚和清理保护。

升级须先迁移并替换全部旧实例，再开放价格编辑；步骤见
[运维文档](operations.md#模型定价迁移与升级)。运行中的旧实例无法读取新覆盖价，
仅依赖旧程序的未知迁移启动保护不能保证安全混跑。
