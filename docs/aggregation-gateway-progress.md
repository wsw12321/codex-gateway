# 聚合网关开发进度与待办

聚合网关改造（用户自带供应商、卖家套餐、套餐商城）的完整设计见[改造计划](aggregation-gateway-plan.md)。本页记录开发分支、已完成的更改和剩余待完成项，每完成一项就更新。

## 开发分支

- 分支名 `aggregation-gateway`，是这项改造的长期开发分支，阶段 2–5 都在这里开发。五个阶段均已完成，剩下的是可选小项（见下方“剩余待完成项”）。
- 从 `main` 的 `8386b12`（Import the full gateway model catalog in OpenCode setup）分出，第一个提交 `2a800c5` 是阶段 1。
- 每个阶段完成后都可以单独合并。合并前先 rebase 到最新 `main`，检查迁移编号是否和 `main` 上的新迁移冲突；冲突时顺延编号，并同步修改 `TestEmbeddedMigrationsCoverRequiredSchema` 和 `migrations/README.md`。
- 功能由 `marketplace_settings.enabled` 控制，默认关闭。关闭时，各阶段合并后的线上行为应与 `main` 一致（例外见下方阶段 1 的“行为变化”）。

## 当前更改

### 阶段 1：上游账号归属（已完成，提交 `2a800c5`）

验证记录见[阶段 1 验证](aggregation-gateway-stage1-validation.md)。

- **迁移 `0051_tenant_upstream_ownership.sql`**：
  - `upstream_accounts` 新增：
    - `owner_user_id`：NULL 表示平台账号，`ON DELETE RESTRICT`；
    - `admin_suspended`：Owner 强制停用；
    - 按主人和协议的部分索引。
  - `egress_registrations` 新增 `tenant_user_id`（`ON DELETE CASCADE`）。阶段 2 才会使用。
  - 新建 `marketplace_settings` 单行表，字段：`enabled`（默认 false）、`commission_rate`（0–1，最多 6 位小数）、`max_accounts_per_user`（默认 20）、`max_plans_per_user`（默认 20）、`allow_member_suppliers`（默认 false）、`revision`。
  - 触发器 `reject_tenant_upstream_account`：群组账号池、专属授权、用户账号权限名单只接受平台账号。
  - 触发器 `guard_upstream_account_owner`：
    - 归属只能设置一次（阶段 2 的迁移 0052 改为：只接受认领和 Owner 转移，删除后释放）；
    - 已进入上述平台结构的账号不能被认领；
    - 未被任何平台结构使用的平台行可以被认领一次。
- **平台路径只用平台账号**：新增 `platformAccountSQL`（`internal/store/tenant_ownership.go`），作用于以下函数：
  - `EligibleUpstreamAccounts`、`EligibleUpstreamAccountLimits`、`SelectUpstreamAccount`、`userPermittedAccountsTx`；
  - 群组“全部账号”池：准入快照 `snapshotGroupUpstreamAllocationGroupTx` 和查询 `ListGroupUpstreamAllocationGroup`。
- **Owner 管理只作用于平台账号**：对租户账号一律返回“不存在”。涉及：
  - `ListUpstreamAccounts`、分配份额列表；
  - 权重、并发、别名、专属设置；
  - 用户账号权限的单个和批量保存；
  - 模型鉴别；
  - 平台 API Key 供应商的列表、换 Key、启停、删除。
- **商城设置**：
  - `MarketplaceSettings` 和 `SetMarketplaceSettings`（`internal/store/marketplace_settings.go`）：带版本校验，每次修改写审计事件 `gateway.marketplace_settings_changed`。
  - `GET /admin/market/settings`：需要 Owner 会话。
  - `PUT /admin/market/settings`：需要同源请求、Owner 和 5 分钟内的身份验证（`internal/server/marketplace.go`）。
- **行为变化（仅在开关打开后）**：Gateway 没有记录的账号不再可用。开关关闭时与升级前一致。
- **测试**：
  - `TestTenantOwnershipPostgresIntegration`
  - `TestMarketplaceSettingsHTTPPostgresIntegration`
  - `TestNormalizeCommissionRate`
  - `TestPlatformAccountPredicate`
  - 回放旧迁移的升级测试改用 `billingIntegrationTenantOwnershipSchema`，补齐归属列。
- **文档**：[改造计划](aggregation-gateway-plan.md)、[阶段 1 验证](aggregation-gateway-stage1-validation.md)、`migrations/README.md` 补上了 `0050` 和 `0051` 两条说明、README 中的一段说明。

### 阶段 2：我的供应商（已完成，提交 `daec9c6`、`61566a7`、`0f40a62`、`00e666b`）

说明见[我的供应商](tenant-providers.md)，验证记录见[阶段 2 验证](aggregation-gateway-stage2-validation.md)。

- **迁移 `0052_tenant_provider_lifecycle.sql`**：`upstream_accounts.credential_removed_at`（已释放）、`egress_servers.owner_user_id`、`marketplace_settings.max_egress_servers_per_user`，替换归属触发器（只接受带 `claim`/`transfer` 标记的变更），新增同主人出口绑定、登记节点和节点主人不可变三个触发器。
- **Store**（`daec9c6`）：
  - 出口登记 prepare 时认领账号（`claimRegistrationAccountTx`）；
  - 释放 `ReleaseUpstreamAccount`、转移 `TransferUpstreamAccountOwner`、停用 `SetUpstreamAccountSuspended`；
  - `WithUpstreamOwner` 按主人划分管理函数；
  - 开关打开时同步不创建账号行；
  - 成员节点只接受公网 IPv4，数量有上限。
- **插件**（`61566a7`）：解码三个拒绝码；新文件删除、原位改写的文件保留并恢复原主人的出口；不再停用他人账号；`ownership` 协议路由；e2e `TestCredentialOwnershipConflict`。
- **服务端**（`0f40a62`）：`/admin/me/providers`、`/admin/me/egress-servers`、Owner 的 `/admin/tenant-providers`、停用与转移路由，归属检查、OAuth 绑定、限流、审计，商城设置新字段，`/admin/state` 商城状态。
- **前端**（`00e666b`）：成员“我的供应商”“我的出口”，Owner“租户供应商”和“转给成员…”，Node 测试与截图。

### 阶段 3：套餐供应商范围、规则 5、自用（已完成，提交 `612c9b2`、`c09e61d`、`14934bc`、`5db6866`）

说明见[我的供应商](tenant-providers.md)的“自用”与“套餐供应商范围”，验证记录见[阶段 3 验证](aggregation-gateway-stage3-validation.md)。

- **迁移 `0053_plan_provider_scopes.sql`**：`billing_plans.seller_user_id`（不可修改）、`billing_plan_provider_scopes`/`billing_plan_provider_accounts`（触发器只接受卖家仍有凭据的账号）、`billing_subscriptions.source_plan_id`（回填、不可修改）、`billing_accounts.self_use_enabled`、规则 5 的预留约束（自用结算 = 规则 5 且扣费与未覆盖均为 0，未新增 `self_use` 列）、`billing_reservation_layers.kind` 与订阅层列。
- **Store**（`612c9b2`）：规则 5 准入（自用 → 群组 → 受限订阅 → 个人；无新层时仍记规则 4）、`resolvePlanPoolTx`、`LayerEligibleAccounts`/`SelectLayerUpstreamAccount`（按层 ID 判断）、规则 5 结算（成员账号不扣个人资金，缺少归因不按自用）、`SetPlanProviderScope`/`PlanProviderScopes`/`PlanScopeCandidates`/`SubscriptionProviderScopes`、`SetSelfUse`，全站用量核对自用成本。
- **插件**（`c09e61d`）：分层的 `/eligible`、`/select`，绑定复用要求在某层仍有资格，`layer-select` 路由声明 `layer_select_v1`；e2e `TestLayerSelect`。
- **服务端**（`14934bc`）：逐层 `/eligible`、`/select`、`/attempt`，转发前探测 `layer-select`（旧插件收不到成员层），`forwardMixed` 与 `/v1/models` 接入成员供应商，范围与自用路由，账务页的自用和订阅范围。
- **前端**（`5db6866`）：套餐卡片的范围与编辑器、自用开关、订阅范围、自用流水，Node 测试与截图。
- **阶段 2 留下的衔接**：释放或转出的账号在解析时按“仍归卖家所有”过滤，编辑器标出并在保存时移除；`/v1/models` 只算未释放、未停用的成员供应商；额度保护覆盖开始服务的成员 OAuth 账号（集成测试验证）；成员出口节点只被账号绑定引用，套餐范围引用账号而非节点，节点删除沿用已有的绑定检查。

### 阶段 4：卖家套餐、审核、商城、分账（已完成，提交 `86ef02d`、`0424e8d`、`1cab10b`）

说明见[套餐商城](marketplace.md)，验证记录见[阶段 4 验证](aggregation-gateway-stage4-validation.md)。

- **迁移 `0054_marketplace.sql`**：`billing_plans` 新增 `review_status`（现有套餐为 approved，约束保证平台套餐始终已通过且不能被下架）、`review_note`、`reviewed_by_user_id`、`reviewed_at`、`submitted_at`、`description`、`admin_suspended`；新建 `marketplace_sales`（成交额、成交时的抽成比例、抽成、净额、每个卖家各自的买家编号 `buyer_number`、两条流水引用，流水被清理时引用清空）；ledger `entry_type` 新增 `plan_sale_income`。
- **Store**（`86ef02d`）：
  - 卖家套餐增删改复用 `writeBillingPlan`（`sellerPlanWrite`）：新套餐为草稿，数量上限读 `max_plans_per_user`（在卖家账户锁内计数）；实质修改（名称、说明、价格、额度、周期、模型、起购期数）让已通过、已驳回、待审核的套餐回到待审核；上下架不算实质修改；被 Owner 下架的套餐不能重新上架。
  - Owner 的平台套餐函数（`UpdateBillingPlan`、`DeleteBillingPlan`、`ListBillingPlans`）只作用于平台套餐。
  - `SubmitSellerPlan`、`ReviewSellerPlan`（按审核时的版本，驳回必须写原因）、`SetSellerPlanSuspended`、`SetSellerPlanListing`。
  - 分账在 `transactBillingPlan` 中：预读卖家后按 UUID 顺序锁定买卖双方账户；禁止自购；套餐须已通过、未下架、卖家正常、商城开启（续费同样）；含成员 API Key 供应商的套餐要求 `AcknowledgeThirdParty`；抽成 12 位小数向下取整（`commissionAmounts`）；卖家净额新建现金 lot 并写 `plan_sale_income`（不含买家，不记操作人）；写 `marketplace_sales`；幂等沿用 `operation_id`。
  - 读取：`MarketPlans`、`SellerPlans`、`PendingSellerPlans`、`AllSellerPlans`、`SellerSales`（去掉买家身份）、`AllMarketplaceSales`、`SellerPlanUsage`（按账号聚合订阅扣费）。
- **服务端**（`0424e8d`）：`/admin/me/plans`（增删改、provider-scope、submit、listing、usage）、`/admin/me/sales`，`GET /admin/market/plans`，Owner 的 `/admin/market/reviews`、`/admin/market/plans/{id}/suspend`、`/admin/market/sales`；购买与续费接受 `acknowledge_third_party`；`/admin/state` 加入每人套餐上限和 Owner 的待审数量。卖家路由只对成员开放，Owner 路由对成员 403，开关关闭时全部 404。
- **前端**（`1cab10b`）：套餐页在开关打开时即商城（成员套餐卡片显示卖家、说明、供应商构成、第三方 API 提示，购买时额外确认）；成员“我的套餐”（发布、编辑、提交、上下架、供应商范围、用量、销售收入）；Owner“商城管理”（商城设置、审核、成员套餐处置、全部成交，导航显示待审数量）；Node 测试 `marketplace_ui_test.cjs` 与截图脚本 `marketplace_browser.cjs`。

### 阶段 5：加固与文档（已完成，提交 `ea0242d`、`b159874`、`d09f482`）

验证记录见[阶段 5 验证](aggregation-gateway-stage5-validation.md)。

- **迁移 `0055_tenant_user_deletion.sql`**：`upstream_accounts.owner_deleted_at`（约束只允许出现在无主、已释放的账号上），归属触发器增加只用于已释放账号的 `owner_deleted` 理由，`marketplace_sales(buyer_user_id)` 索引。
- **用户删除与停用**（`ea0242d`）：
  - 新的阻止原因 `providers`、`egress_servers`、`seller_plans`、`plan_subscriptions`、`marketplace_sales`，候选列表与事务内复查共用；成员修改自己的套餐不再算管理操作关联。
  - 删除时已释放账号清空主人并标记，不进入任何管理范围（`accountScopeSQL`、`InScope`），结算按成员账号处理，认领时清除标记；已删除且无人引用的卖家套餐随之删除；审计记录这两项。
  - 停用沿用阶段 3、4 的规则（卖家套餐停售，账号不进入订阅层），现在另有告警。
- **成交记录清理**（`ea0242d`）：早于截止时间、两条流水都已清理的成交记录随信息清理删除，报告计入 `marketplace_sales`。
- **告警**（`b159874`）：准入报告解析为空的受限订阅层，Gateway 产生 `subscription_layer_empty`；`billing_attribution_missing` 按层种类说明规则 5 的结算方式。
- **界面**（`d09f482`）：信息维护页的阻止原因、候选列表说明和“商城成交”，截图。
- **文档**：`docs/threat-model.md` 完整的多租户一节；`docs/operations.md` 的完整升级顺序、成员停用与永久删除、聚合网关告警；README 完整介绍；`docs/tenant-providers.md`、`docs/marketplace.md`、`migrations/README.md`。
- **2026-11-25 确认的取舍**：已释放账号在主人被永久删除后清空主人并标记（不进入平台账号列表）；成交记录跟随信息清理。
- **后续补充（2026-11-25，按用户要求；提交 `279037d`、`3e9f08b`、`aeefd50`）**：
  - 迁移 `0056_third_party_consent.sql`：买家对卖家 API Key 供应商的确认记在订阅上，购买或续费时写入，可在订阅卡片上随时确认或撤回；没有确认时卖家的 API Key 供应商不为该订阅服务（补上售出后加入供应商的缺口）。
  - Owner 在“租户供应商”删除成员的出口服务器（`DELETE /admin/tenant-providers/egress-servers/{id}`），成员仍须先删除节点才能被永久删除。
  - 删除候选搜索列出不能删除的匹配用户、原因和处理方法。

## 剩余待完成项

五个阶段的计划项均已完成。以下是可选小项，开工前需要向用户确认是否要做：

- [ ] Owner 的平台套餐编辑器还没有“说明”字段（数据库和商城已支持，平台套餐的说明目前为空）。
- [ ] 卖家看不到被驳回前后的完整审核历史，只看到最近一次的备注；审计事件里有完整记录。
- [ ] 没有退款流程；买家与卖家的纠纷只能由 Owner 手工调整余额。
- [ ] 成员已停用时，Owner 不能删除其未删除的套餐（只能强制下架）；这样的成员要先恢复启用、由成员删除套餐，才能被永久删除。

合并到 `main` 前：rebase 到最新 `main`，检查迁移编号（`0051`–`0056`）是否与 `main` 上的新迁移冲突。

## 待确认的默认取舍

计划里的默认做法已全部确认。若后续工作发现计划没有覆盖的取舍，再向用户确认。

已经确认的决策：
- 购买款项按抽成比例扣除后，净额记入卖家余额，不做提现；
- 纯余额按量付费只走平台供应商；
- 自用免费；
- 所有用户都可以发布套餐，上架前须经审核；
- 整个功能由 `marketplace_settings.enabled` 控制，默认关闭；设置读取失败时，租户功能 fail closed（阶段 1–3 已按此实现，阶段 4、5 沿用）；
- 2026-11-25 确认（阶段 5 已实现）：
  - **已释放账号的历史归属**：永久删除成员时，其已释放账号保留记录和用量历史，清空主人并标记 `owner_deleted_at`；不进入任何人（包括平台）的管理范围，下次登录同一上游账号时按认领处理。
  - **成交记录保留期**：跟随信息清理，买卖双方两条流水都已清理后随同一次清理删除。
- 2026-11-24 确认（阶段 4 实施）：
  - **买家匿名**：卖家看不到买家身份。销售记录中买家只显示匿名编号，用量只按套餐和账号聚合。
  - **审核范围**：卖家修改供应商范围无需审核，不提升 `version`，不影响续费绑定（阶段 3 的 `SetPlanProviderScope` 已如此）；修改价格、额度、周期、模型、名称、描述要重新进入审核，审核期间下架，并按现有规则提升 `version`。
- 2026-11-24 确认（阶段 3 已实现）：
  - 自用层永远排第一，成员可以关闭；
  - 卖家套餐额度用完时不从买家现金补扣，差额记 `uncovered_usd`，由卖家承担；
  - 个人层（平台不限制订阅 + 现金）保持在最后一层，受限套餐层总排在现金前面；
  - Owner 的模型权限对所有请求生效，包括自用。
- 2026-11-24 确认（阶段 2 已实现）：
  - **出口**：成员可以新增自己的出口服务器并选择，逻辑与 Owner 的出口一致；不新增或不选择时走全局出口。（取代原“卖家不能选择出口节点”）
  - **归属**：上游账号归属可以更改，但只能由 Owner 更改；账号删除后释放归属。（取代阶段 1 的“归属只能设置一次”）

## 已知问题与开发环境

- **与本改动无关的已有失败**：
  - `TestClientConfigurator` 的两个 Node 用例：执行环境设置了 `ANTHROPIC_BASE_URL` 和 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST`，配置器把它们识别为冲突的 Claude 设置。去掉这两个变量后通过。
  - `scripts/tests/test_lock_images.py` 的 28 个用例：需要镜像仓库工具。
- **本地集成测试**：
  - 准备一次性 PostgreSQL 16 数据库：

    ```bash
    pg_ctlcluster 16 main start
    su postgres -c "psql -c \"CREATE ROLE gateway LOGIN PASSWORD 'password' SUPERUSER;\""
    su postgres -c "createdb -O gateway gateway_test"
    ```

  - 运行测试：

    ```bash
    TEST_DATABASE_URL='postgres://gateway:password@127.0.0.1:5432/gateway_test?sslmode=disable' \
      env -u ANTHROPIC_BASE_URL -u CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST \
      go test -count=1 -tags=integration ./internal/store ./internal/server
    ```

  - 完整集成测试要在新建的空数据库上运行。部分旧用例会创建固定用户名和唯一的 Owner。
- **共享单行表**：`marketplace_settings` 是单行表，集成测试修改后要在 `t.Cleanup` 里恢复，否则会影响同一数据库里的其他用例。
- **回放旧迁移的测试**：当前选路会读取 `owner_user_id`、`credential_removed_at` 和 `marketplace_settings`。回放旧迁移前缀的升级测试，需要先调用 `billingIntegrationTenantOwnershipSchema`。
- **插件 e2e**：没有 Docker 时，可按 `plugin.Dockerfile` 的构建阶段手工构建 CPA 与插件 `.so` 后运行 `go test -tags e2e ./e2e`，步骤见[阶段 2 验证](aggregation-gateway-stage2-validation.md#已运行检查)。
- **截图脚本**：`internal/server/testdata/tenant_providers_browser.cjs` 只拦截成员与租户供应商接口，其他面板的请求返回 404，不影响截图。`plan_scopes_browser.cjs` 同理；它的成员页需要显式调用 `loadBillingDashboard()`（夹具不运行 `loadDashboard`）。
- **升级测试与新列**：回放旧迁移前缀后再调用 `Migrate` 的测试会比较整行快照（如 `cpa_v848_migration_integration_test.go`），新迁移给这些表加列时，要在快照里排除新列。准入、购买、结算对 0053 之前的结构用 `planScopeSchemaTx` 判断。
- **阶段 4 的升级测试**：购买与续费对 0054 之前的结构用 `marketplaceSchemaTx`（`to_regclass('marketplace_sales')`）判断，没有该表时按平台套餐处理；`ListBillingPlans` 和 Owner 的套餐修改对 0053 之前的结构不做卖家过滤。
- **阶段 4 的截图脚本**：`internal/server/testdata/marketplace_browser.cjs` 覆盖成员商城与第三方确认、“我的套餐”、Owner“商城管理”。阶段 3 的 `plan_scopes_browser.cjs` 夹具不提供 `/admin/market/plans`，Owner 页面会多一条“商城套餐加载失败”的提示，不影响它的断言。
- **阶段 5 的测试**：`TestTenantUserDeletionPostgresIntegration` 使用独立 schema（`informationIntegrationStore`），会运行信息清理，不影响其他用例；它自己创建 Owner。`information_browser.cjs` 也会重新生成 `upstream-concurrency-*` 截图，阶段 5 只提交了 `information-*` 三张。
- **插件 e2e 构建脚本**：阶段 3 的手工构建步骤与阶段 2 相同（见[阶段 2 验证](aggregation-gateway-stage2-validation.md#已运行检查)），构建约需 5 分钟。
