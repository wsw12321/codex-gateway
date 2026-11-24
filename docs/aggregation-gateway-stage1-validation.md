# 聚合网关阶段 1 验证：上游账号归属

2026-11-24 完成[聚合网关改造计划](aggregation-gateway-plan.md)的阶段 1。这一阶段只打基础，用户和 Owner 的现有体验不变：没有页面改动，也还没有任何途径创建租户账号。

## 行为

- **账号归属**：
  - `upstream_accounts.owner_user_id` 为 NULL 表示平台账号。现有账号全部是平台账号。
  - 非 NULL 表示租户账号，由阶段 2 的“我的供应商”创建。
  - 归属只能设置一次。从平台改为租户、从租户改回平台、在租户之间转移，都会被数据库触发器拒绝。
  - 已经进入群组账号池、专属授权或用户账号权限名单的平台账号，不能再变成租户账号。
- **平台专属结构只接受平台账号**：群组账号池（`group_upstream_accounts`）、专属授权（`upstream_account_users`）、用户账号权限名单（`user_upstream_access_accounts`）。应用层先校验，租户账号按“账号不存在”处理；数据库触发器兜底。
- **平台选路只用平台账号**：
  - 以下路径都排除租户账号和被 Owner 强制停用（`admin_suspended`）的账号：
    - 资格查询：`EligibleUpstreamAccounts`、`EligibleUpstreamAccountLimits`，即插件的 `/eligible`；
    - 加权选路：`SelectUpstreamAccount`，即插件的 `/select` 和 Gateway 自己的供应商选路；
    - 扣费层账号池：`userPermittedAccountsTx`；
    - 群组“全部账号”池：准入快照和查询都适用。
  - 因此群组、个人订阅和现金只会由平台账号服务。
- **Gateway 没有记录的账号**：
  - 商城开关关闭时，仍和以前一样可用（并发上限 1）。
  - 开关打开后不可用。开关打开后，未登记的账号可能是正在登录、归属还没提交的租户账号。正常的 Owner 登录和导入在提交出口登记时就会写入账号行，不受影响。
- **Owner 管理与列表只看平台账号**：
  - 管理页的账号列表、分配份额、权重、并发、别名、专属设置、模型鉴别、API Key 供应商的列表与修改，都只作用于平台账号。
  - 对租户账号的请求返回“不存在”。
  - 额度保护（quota guard）和出口绑定不区分归属，以后也会保护租户 OAuth 账号。
- **商城设置**：
  - `marketplace_settings` 单行表，默认关闭。字段有抽成比例（0–1，最多 6 位小数）、每人供应商上限（0–256，默认 20）、每人套餐上限（0–100，默认 20）、是否允许成员添加 API Key 供应商（默认否）。
  - `GET /admin/market/settings` 需要 Owner 会话。
  - `PUT /admin/market/settings` 需要同源请求、Owner 和 5 分钟内的身份验证。请求必须带全部字段和当前 `revision`：
    - 版本不符返回 `409 marketplace_revision_changed`；
    - 未知字段、类型错误、超出范围返回 400。
  - 每次修改写入一条 `gateway.marketplace_settings_changed` 审计事件，记录修改前后的开关和抽成比例。

## 数据库

`0051_tenant_upstream_ownership.sql`（只能前进）：

- `upstream_accounts` 新增 `owner_user_id`（`ON DELETE RESTRICT`）和 `admin_suspended`，并新增按主人和协议的部分索引。
- `egress_registrations` 新增 `tenant_user_id`（`ON DELETE CASCADE`），供阶段 2 在登录提交时写入归属。
- 新建 `marketplace_settings` 并插入默认行。
- 新增三个平台专属表的触发器 `reject_tenant_upstream_account`，以及 `upstream_accounts` 的归属守卫 `guard_upstream_account_owner`。

升级时无需回填：现有账号全部成为平台账号，开关关闭，行为与升级前一致。旧二进制遇到这个迁移会拒绝启动；回退需要先恢复备份。

## 已运行检查

环境：Go 1.26.11、Node.js 22、PostgreSQL 16（一次性本地数据库）。

```sh
gofmt -l .                       # 无输出
git diff --check
go vet ./...
go vet -tags=integration ./internal/store ./internal/server
go build ./cmd/gateway
go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 ./internal/store ./internal/server
TEST_DATABASE_URL='postgres://…/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/store ./internal/server
```

以上全部通过。完整集成测试在新建的空数据库上运行。

另外两项与本次改动无关、在未修改的代码上同样失败：
- `TestClientConfigurator`：执行环境本身设置了 `ANTHROPIC_BASE_URL` 和 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST`，配置器把它们识别为冲突的 Claude 设置。去掉这两个变量后通过。
- `scripts/tests/test_lock_images.py`：需要镜像仓库工具。

新增测试：

- `TestTenantOwnershipPostgresIntegration`：
  - 平台和租户账号同时作为候选时：
    - 资格查询和选路只返回平台账号；
    - 只有租户账号时，返回 `ErrNoUpstreamAccount`；
    - 租户本人在平台路径上也不能使用自己的账号。
  - 未登记账号在开关关闭时可用，打开后不可用。
  - Owner 的列表和修改（权重、并发、专属）对租户账号返回“不存在”。
  - 群组：“指定账号”池拒绝租户账号；“全部账号”池的查询和准入快照都不含租户账号。
  - 用户账号权限名单拒绝租户账号。
  - 两个触发器拒绝直接写入的 SQL。
  - 归属不能修改或清除；已进入群组池的平台账号不能被认领；未配置的平台行可以被认领一次。
  - 商城设置：抽成比例校验、版本冲突和审计条数。
- `TestMarketplaceSettingsHTTPPostgresIntegration`：成员读写返回 403；默认值；版本冲突 409；缺字段、类型错误、超范围、未知字段返回 400；Owner 成功保存。
- `TestNormalizeCommissionRate`、`TestPlatformAccountPredicate`：单元测试。
- 回放旧迁移的升级测试 `TestGroupPriorityMigrationPreservesLegacyPostgresIntegration` 改用测试辅助函数 `billingIntegrationTenantOwnershipSchema`，补齐当前选路需要的归属列和开关表。
