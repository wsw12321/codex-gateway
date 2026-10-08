# Anthropic 接入验证记录

验证日期：2026-10-08。使用 Go 1.26.8、Node.js 24、PostgreSQL 16.15 和锁定的 CPA v8.0.4。所有账号、令牌和浏览器数据均为合成测试数据，未连接真实 Claude 订阅账号。

## 实现范围

- `anthropic` 单一 Gateway provider，三类账号的管理、授权、分配和账单归因隔离。
- CPA 上游验证的账号 UUID + 组织 UUID 身份；OAuth、CPA/Claude Code 格式令牌导入、刷新、重新授权、重复身份隔离、停用排空后删除。
- 原生 Messages、`count_tokens`、`beta=true`、Bearer/`x-api-key` 严格鉴权、原生错误及 SSE 事件转发。
- 累计 usage、流中断/取消结算、两种缓存 TTL、缺失分项保守计费、不可变账本、日/月聚合和历史明细清理兼容。
- Claude 控制台、CPA 页签、模型价格/倍率、账号范围、CLI 配置器、桌面与移动端布局。

## 已执行验证

| 检查 | 结果 |
| --- | --- |
| `go build ./cmd/gateway` | 通过；测试二进制输出到临时目录 |
| `go test -count=1 ./...` | 全量通过 |
| `go test -race -count=1 ./...` | 全量通过；在具备 C 编译器的缓存工具链容器中执行，仓库只读挂载 |
| `go vet ./...`、`gofmt -l .`、`git diff --check` | 通过 |
| `go test -count=1 -tags=integration ./internal/store` | 全量通过，使用新建的一次性数据库；新增归因隔离用例随后单独通过 |
| `go test -count=1 -tags=integration ./internal/server` | 全量通过；新增流中断计费及取消用例随后单独通过 |
| Node 前端、配置器回归 | 通过；作为 Go 服务端单元测试的一部分执行 |
| CPA 面板 Bun 测试、构建、资源哈希 | 通过 |
| Playwright 合成浏览器回归 | Claude、CPA、Antigravity、账号范围、概览通过；桌面/手机截图已保存 |
| `scripts/validate-compose.sh` | 使用临时部署目录、合成 `.env`/secrets 和真实锁定 Squid/Caddy 镜像通过；未创建站点配置 |
| `python3 scripts/tests/test_relay_validation.py` | 23 项通过 |
| CPA 固定源码构建、包测试、race、vet、镜像冒烟 | 通过；含 Claude 原生端点、身份/传输边界、最多两个账号重试、三个 provider 的 capability 和管理密钥隔离 |

Messages 的 PostgreSQL 回归验证了输入归一、混合 TTL 归因、流式累计用量、工具格式透传、Token 计数无费用预留、空账号范围拒绝、重复结算、倍率与快照不变性。中断 SSE 保留最后有效用量并结算一次，释放并发租约；provider 不匹配的归因被丢弃，费用和释放流程继续完成。

测试入口包括：

- `internal/proxy/anthropic_test.go`
- `internal/server/anthropic_test.go`、`anthropic_accounts_sync_test.go`、`anthropic_integration_test.go`
- `internal/config/anthropic_pricing_test.go`
- `internal/store/anthropic_billing_test.go`、`anthropic_billing_integration_test.go`
- `internal/server/testdata/anthropic_accounts_browser.cjs`
- `deploy/cpa-panel/test/api.test.ts`

## CPA 构建证据

- 上游 commit：`d33f63f8e3d98428440ebca5a5b6a981a61ff71e`。
- Gateway 补丁 SHA256：`7d5eceed9f551bf11d82d179a92c61542ad55cb8766fea81e52000b7a7801a50`。
- 镜像：`codex-gateway-compat:v8.0.4-d33f63f8-7d5eceed9f551bf1-cpa`。
- 本地 OCI manifest-list digest：`sha256:a1a994a7808bca67ad5ad20f4a5b8cfaf51ede37cce64ea6d6cafeb007d8685f`。

这是本次本地构建证据，不替换旧版本 `deploy/cpa-v8-validation.lock.json` 的历史发布记录，也不表示镜像已推送或生产已升级。

## 上线前仍需实际账号验收

真实 OAuth 授权、刷新后重启、订阅限流恢复、流式工具续轮、thinking 签名、图片以及实际账单归因，须在部署环境用授权账号验收。先迁移数据库并升级 Gateway/CPA，再启用 Claude 账号。配置和排空回退步骤见 [Claude Code 接入说明](claude-code.md)；截图也列在该说明中。
