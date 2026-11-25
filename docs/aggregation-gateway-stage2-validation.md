# 聚合网关阶段 2 验证：我的供应商

2026-11-24 完成[聚合网关改造计划](aggregation-gateway-plan.md)的阶段 2。成员可以添加和管理自己的上游账号、API Key 供应商和出口服务器；Owner 可以停用成员账号、转移账号归属。使用说明见[我的供应商](tenant-providers.md)。

功能由 `marketplace_settings.enabled` 控制，默认关闭。关闭时新路由全部返回 404，Owner 的页面只多出“租户供应商”和“转给成员”两个入口，行为与升级前一致。成员账号在本阶段还不服务请求，自用与套餐层在阶段 3 实现。

## 本阶段采用的两条决定

用户在开发前确认，替换了计划中的原有取舍：

1. **出口**：成员可以新增自己的出口服务器并选择，逻辑与 Owner 的出口一致；不新增或不选择时走全局出口。（原计划：成员不能选择出口。）
2. **归属**：上游账号归属可以更改，但只能由 Owner 更改；账号删除后释放归属。（原阶段 1：归属只能设置一次。）

## 行为

- **归属生命周期**（迁移 `0052_tenant_provider_lifecycle.sql`）：
  - **认领**：出口登记在 prepare 阶段确保账号行时决定归属。登记记录发起人（`egress_registrations.tenant_user_id`，Owner 为空）。新账号归发起人；同一主人重新授权允许；已释放的账号被认领；登记开始后由元数据同步创建、且未进入任何平台结构的平台行可以被成员认领；其他情况返回 `ErrAccountOwnedElsewhere`。成员超出账号上限返回 `ErrProviderLimitReached`。
  - **释放**：删除凭据（OAuth 删除成功后、供应商删除时）设置 `credential_removed_at`，出口绑定改回全局。已释放的账号不服务任何资金路径（`platformAccountSQL` 排除），可被下一次登录认领，保留原主人用于历史用量。
  - **转移**：只有 Owner，`TransferUpstreamAccountOwner`，带期望的当前主人；指向旧主人节点的绑定改回全局并推送插件。
  - 所有归属变更都通过 `changeUpstreamOwnerTx`，事务内设置 `gateway.upstream_owner_change = claim | transfer`。替换后的触发器只接受带这个标记的变更，继续拒绝租户账号进入平台结构，并拒绝与出口节点主人不一致的结果。
- **成员出口服务器**：`egress_servers.owner_user_id`（不可修改）。账号只能绑定同一主人的节点，登记只能选择发起人自己的节点（两个触发器 + store 校验）。成员节点只接受公网 IPv4 字面量（`ValidateTenantEgressServer`，复用 `egress.PublicSupplierIP`），数量受 `max_egress_servers_per_user` 限制。
- **同步不再创建账号行**：商城开关打开期间，`SyncUpstreamAccounts` 只更新已有行。账号只能经出口登记进入数据库，成员中途放弃的登录不会变成平台账号。
- **按主人划分的管理**：`Store.WithUpstreamOwner` 把别名、权重、并发、账号列表、用量汇总、API Key 供应商、出口节点和绑定限定到一个成员；不带主人的 store 只管理平台账号（同时修复了阶段 1 遗留的问题：Owner 账号汇总会列出租户账号）。
- **插件**：`register-egress` 对三种拒绝返回 409 与固定 code。插件只解码这三个 code：登录开始时不存在的凭据文件（导入总是新文件）被删除；被 CPA 原位改写的已有文件保留，并按原主人的绑定重写出口，只有重写失败才停用账号。其他情况不再停用账号。登录状态返回 `{"status":"error","error":<code>}`，导入返回 409。新增 `GET .../gateway-management/{provider}/ownership`（`credential_ownership_v1`），Gateway 只通过支持它的插件为成员登录。
- **服务端路由**：
  - 成员：`/admin/me/providers`（列表、OAuth、导入、供应商、状态、别名、并发、权重、出口、刷新、额度、删除、并发快照）和 `/admin/me/egress-servers`（增删改、连通性测试）。
  - Owner：`GET /admin/tenant-providers`、`PUT /admin/tenant-providers/{id}/suspension`、`PUT /admin/{upstream|antigravity|anthropic}-accounts/{id}/owner`。
  - 访问控制：
    - 成员路由只对成员开放，Owner 访问返回 403；开关关闭返回 404；设置读取失败返回 503。
    - 写操作需要同源请求和 5 分钟内的身份验证，并写 `tenant_provider.operation` 审计。
    - 访问他人或平台的账号、节点返回 404。直接调用 CPA 的操作（状态、刷新、额度、删除、额度保护）先检查归属。
  - OAuth 流程绑定发起用户、会话和路由类型（成员或 Owner）；每个成员最多两个未完成的授权；登录和导入前检查插件协议和账号上限。
  - 限流：列表 30 次/分钟，OAuth、导入、供应商、出口测试各 10 次/分钟，刷新、额度各 20 次/分钟，均按用户计。
  - 商城设置新增 `max_egress_servers_per_user`；打开开关要求已配置账号出口。`/admin/state` 返回成员可见的商城状态。
- **界面**：成员侧栏“我的供应商”分组（我的供应商、我的出口），Owner 的“租户供应商”页和上游账号卡片上的“转给成员…”。

## 数据库

`0052_tenant_provider_lifecycle.sql`（只能前进）：

- `upstream_accounts.credential_removed_at`；回填迁移前已删除的 API Key 供应商。
- `egress_servers.owner_user_id` 及部分索引。
- `marketplace_settings.max_egress_servers_per_user`（默认 5，0–32）。
- 替换 `guard_upstream_account_owner()`；新增 `guard_upstream_account_egress_owner`、`guard_egress_registration_owner`、`guard_egress_server_owner` 三个触发器函数。

升级无需人工回填，开关保持原状态。旧二进制遇到 0052 会拒绝启动；回退需要恢复备份。原计划中阶段 3、4 的迁移顺延为 `0053`、`0054`。

## 已运行检查

环境：Go 1.26.11、Node.js 22、Playwright（Chromium 158）、PostgreSQL 16（一次性本地数据库）。

```sh
gofmt -l .                       # 无输出
go vet ./...
go vet -tags=integration ./internal/store ./internal/server
go build ./cmd/gateway
env -u ANTHROPIC_BASE_URL -u CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST go test -count=1 ./...
env -u ANTHROPIC_BASE_URL -u CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST CGO_ENABLED=1 go test -race -count=1 ./internal/store ./internal/server
TEST_DATABASE_URL='postgres://…/gateway_test?sslmode=disable' \
  env -u ANTHROPIC_BASE_URL -u CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST \
  go test -count=1 -tags=integration ./internal/store ./internal/server   # 新建的空数据库
(cd deploy/codex-compat/plugin && go vet ./... && go vet -tags e2e ./... && CGO_ENABLED=1 go test -race -count=1 ./...)
python3 -m unittest scripts.tests.test_cpa_plugin_deploy
env -u ANTHROPIC_BASE_URL -u CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST \
  node --test internal/server/testdata/*_test.cjs                # 292 项：291 通过，1 项按设计跳过
node internal/server/testdata/tenant_providers_browser.cjs
node internal/server/testdata/egress_browser.cjs
node internal/server/testdata/dashboard_layout_browser.cjs
```

以上全部通过。

**插件 e2e**：本地没有 Docker 守护进程，按 `plugin.Dockerfile` 的构建阶段手工复现：拉取 CLIProxyAPI `v8.1.1`（校验 commit `6eb8554f…`），应用残留补丁，按安全依赖下限升级模块，构建 `cli-proxy-api`，再以 `-buildmode=c-shared` 构建插件，然后运行：

```sh
CPA_E2E_BINARY=… CPA_E2E_PLUGIN_DIR=… go test -tags e2e -count=1 -v ./e2e
```

18 个用例全部通过，包括新增并已加入 Dockerfile 必过列表的 `TestCredentialOwnershipConflict`。完整镜像构建（`./scripts/compose.sh build codex-compat`、`./scripts/test-cpa-plugin-image.sh`）需要 Docker，未运行。

## 新增和修改的测试

- `TestTenantProviderLifecyclePostgresIntegration`（store）：
  - 认领、重新授权、他人或平台登录被拒、账号上限；
  - 按主人划分的别名、并发、权重和汇总；
  - 释放后被他人认领（审计 reason=claim）；已释放的平台账号不可用，重新登录后恢复；
  - 开关打开时同步不创建行，开关切换期间同步创建的行可被登录认领；
  - 转移：平台结构中的账号、过期的期望主人、不存在的用户被拒；转移带走他人节点时绑定改回全局；转回平台；
  - 成员节点：域名、私有、回环、CGNAT 地址被拒，数量上限，他人不能读、改、删、绑定或用于登记，触发器拒绝跨主人绑定和修改节点主人；
  - 停用只作用于租户账号；租户账号列表；成员供应商计入上限并在删除时释放。
- `TestTenantOwnershipPostgresIntegration`：改为验证“只有带 claim/transfer 标记的变更才被接受”。
- `TestTenantProvidersHTTPPostgresIntegration`（server，带假 CPA）：
  - 开关关闭时 404、Owner 403、`/admin/state` 状态；无账号出口不能开启；
  - 成员只看到和管理自己的账号，对他人和平台账号的别名、并发、状态、刷新、删除均 404；Owner 路由对成员账号 404；
  - 停用后成员不能启用；成员不能停用或转移；转移成功与期望主人冲突；
  - 成员节点的地址校验、数量上限、隔离，Owner 节点列表不含成员节点，跨主人绑定 404；
  - 插件协议探测失败 503、账号上限 409；`register-egress` 返回 `account_owned_elsewhere`；
  - 删除走 CPA 停用、并发确认、删除凭据，再释放归属；关闭开关后路由回到 404。
- `TestMarketplaceSettingsHTTPPostgresIntegration`：新字段和账号出口前置条件。
- 插件单元测试：新凭据文件被删且原账号保持启用；原位改写的文件保留并恢复原主人的出口；恢复失败时停用；导入被拒；`RegisterEgress` 只解码三个固定 code；`ownership` 路由。
- `tenant_providers_ui_test.cjs`（`TestTenantProvidersDashboardBehavior`）：页面门控、出口接口前缀、列表与上限、停用账号、OAuth 流程与拒绝提示、回调地址与导入解析、停用与转移请求。
- 既有单元测试的假数据库补上新的同步、归属查询和 `admin_suspended` 列；`TestEmbeddedMigrationsCoverRequiredSchema` 迁移数为 52；升级测试辅助函数补上 `credential_removed_at`。

## 截图

由 `internal/server/testdata/tenant_providers_browser.cjs` 生成：

- `docs/screenshots/tenant-providers-member-desktop.png`、`tenant-providers-member-mobile.png`：成员的供应商页，正在授权登录。
- `docs/screenshots/tenant-providers-member-egress-desktop.png`：成员的出口页。
- `docs/screenshots/tenant-providers-owner-desktop.png`、`tenant-providers-owner-mobile.png`：Owner 的租户供应商页。
- `docs/screenshots/tenant-providers-owner-transfer.png`：转移归属对话框。

## 已知限制与后续

- 成员账号在阶段 3 之前不服务请求；`/v1/models` 也还不包含成员供应商的模型。
- 删除账号时“从套餐范围中移除”由阶段 3 按 `credential_removed_at` 处理。
- 额度保护的自动检查不区分归属，账号开始服务请求（阶段 3）后对成员 OAuth 账号同样生效；成员不能手动设置额度保护状态。
- Owner 的并发快照接口仍返回 CPA 中全部账号的计数（只有 ID 和数字）；成员的并发快照只含自己的账号。
- 与本改动无关、在未修改的代码上同样失败：`scripts/tests/test_lock_images.py`（需要镜像仓库工具）；`TestClientConfigurator` 需要去掉执行环境中的 `ANTHROPIC_BASE_URL` 和 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST`。
