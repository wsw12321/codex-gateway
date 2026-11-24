# 聚合网关开发进度与待办

聚合网关改造（用户自带供应商、卖家套餐、套餐商城）的完整设计见[改造计划](aggregation-gateway-plan.md)。本页记录开发分支、已完成的更改和剩余待完成项，每完成一项就更新。

## 开发分支

- 分支名 `aggregation-gateway`，是这项改造的长期开发分支，阶段 2–5 都在这里开发。阶段 2 已完成，下一步是阶段 3。
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

## 剩余待完成项

### 阶段 3：套餐供应商范围、扣费层规则 5、自用（需求 2）

- [ ] **迁移 `0053_plan_provider_scopes.sql`**（阶段 2 已占用 0052）：
  - `billing_plans.seller_user_id`（NULL 表示平台）。
  - `billing_plan_provider_scopes(plan_id, mode all|selected, revision)` 和 `billing_plan_provider_accounts(plan_id, account_id ON DELETE CASCADE)`。现有套餐初始化为 `all`。
  - `billing_subscriptions.source_plan_id`：不可变，从 `plan_id` 回填。
  - `billing_accounts.self_use_enabled`，默认 true。
  - `funding_rule_version` 放宽为 1–5。
  - `billing_reservation_layers` 新增 `kind`（group/subscription/self/personal）和 `subscription_id`，并放宽 `all_accounts = (group_id IS NULL)` 约束。
  - 扣费记录新增 `self_use` 标记。
- [ ] **准入规则 5**：改写 `fundingLayersTx` 和 `snapshotBillingFundingTx`。
  - 层的顺序：自用 → 群组 → 受限订阅（按订阅优先级）→ 个人层。个人层必须保持最后，这是插件 `decodeScope` 的契约。
  - 受限订阅：来源是卖家套餐，或“指定供应商”的平台套餐。它们从个人来源中移出，单独成层，层 ID 用订阅 UUID。
  - 只有自用层时也允许准入。
  - 层数上限 65，每层账号上限 256，并受 `maxGroupsHeader` 约束。
- [ ] **`resolvePlanPoolTx`**：每次准入按套餐当前范围实时解析账号池，保证“下一次请求即生效”：
  - 不限制：卖家当前全部账号；
  - 指定：名单 ∩ 仍归卖家所有 ∩ 未被强制停用 ∩ 未释放（`credential_removed_at IS NULL`）∩ 协议匹配。
- [ ] **阶段 2 留下的衔接**：
  - 账号释放或被 Owner 转移后，从原卖家套餐的指定范围中移除（或在解析时按“仍归卖家所有”过滤，并在套餐编辑器中提示失效账号）。
  - 成员的出口节点被删除前，确认没有套餐依赖它（节点绑定已有引用检查）。
  - `/v1/models` 追加成员供应商的模型时，只算未释放、未停用的账号。
  - 额度保护对成员 OAuth 账号的自动检查在账号开始服务请求后生效，验证一次。
- [ ] **按层判断资格** `LayerEligibleAccounts`：
  - 个人层：只允许平台账号。
  - 群组层：平台账号，且在该群组内。
  - 订阅层：订阅属于本人、仍有效，且来源套餐的当前范围包含该账号。
  - 自用层：账号归本人，且自用已开启。
- [ ] **插件**（选路契约改动，两边要一起落地）：
  - [ ] `gatewayclient.go` 的 `Eligible` / `Select` 带上本请求的层 `groups:[{group_id, account_ids}]`。网关的 `upstreamAccountSelection` 已能解析 `groups` 字段。
  - [ ] `scheduler.go` 的 `allocate`：复用对话绑定前，确认被绑定的账号在逐层资格的并集内。
  - [ ] 能力响应中声明 `layer-select`。网关检测到旧插件时，不生成含租户账号的层（fail closed）。
  - [ ] 部署顺序：先部署插件，再部署网关。
- [ ] **网关**：
  - `upstream_allocation.go` 的 `/eligible` 和 `/select` 按层判断。
  - `upstream_attempt.go` 和 `internal/proxy/ledger.go`：分开保存“非个人层并集”和“含个人层”，拒绝层外的租户账号。
  - `supplier_routing.go` 的 `forwardMixed` 改用同一套按层函数。
  - `/v1/models` 追加该用户能用到的租户供应商模型。
- [ ] **结算规则 5**：改写 `settlementLayerCandidates` / `bindSettlementLayerTx` 和 `settleBillingTx`。
  - 自用层扣 0。
  - 订阅层扣该订阅当前周期的剩余额度，不足部分只流向后续同样包含该账号的层，再不足记为 `uncovered_usd`。
  - 卖家账号的用量永远不扣买家现金。
  - 规则 1–4 的在途请求按原规则结算。
- [ ] **接口与前端**：
  - `PUT /admin/billing/plans/{id}/provider-scope`（Owner）；
  - `PUT /admin/billing/me/self-use`；
  - 套餐编辑器增加“供应商：不限制 / 指定”选择器；
  - 订阅卡片显示供应商范围；
  - 自用开关。
- [ ] **验证（PostgreSQL 集成测试）**：
  - 第 1 个请求准入后修改范围，第 2 个请求只用新范围；
  - 撤销账号后，下一次 `/select` 重试被拒绝，对话绑定被解除；
  - “不限制”范围下，卖家新增的账号在下一次请求自动加入；
  - 个人层永远不会选中租户账号；
  - 规则 4 的在途请求兼容；
  - 插件新旧版本组合都 fail closed。

### 阶段 4：卖家套餐、审核、商城、分账（需求 3）

- [ ] **迁移 `0054_marketplace.sql`**：
  - `billing_plans` 新增 `review_status`（现有套餐为 approved）、`review_note`、`reviewed_by_user_id`、`reviewed_at`、`submitted_at`、`description`。
  - 新建 `marketplace_sales`。
  - ledger `entry_type` 新增 `plan_sale_income`；扩展 operation 类型。
- [ ] **卖家套餐增删改**：复用 `normalizeBillingPlan` / `writeBillingPlan` / `validateBillingModelsTx`。
  - 数量上限读 `max_plans_per_user`。
  - 修改价格、额度、周期、模型、名称、描述：重新进入审核，期间下架，并提升 `version`。
  - 修改供应商范围：无需审核，不提升 `version`。
- [ ] **分账**：在 `transactBillingPlan` 中实现。
  - 禁止自购。
  - 按 UUID 顺序锁定买家和卖家的 `billing_accounts`。
  - 用 `debitBillingPlanCashTx` 扣买家现金。
  - 抽成：12 位小数向下取整，抽成比例在成交时固化。
  - 卖家净额：新建现金 lot，并写 `plan_sale_income`。
  - 写 `marketplace_sales`，用 `operation_id` 保证幂等。
  - 续费同样分账。
- [ ] **卖家被停用时**：其套餐下架，其账号不可选；已购订阅的层被跳过。
- [ ] **路由**：
  - 卖家：`/admin/me/plans`（含 provider-scope、submit、listing）、`/admin/me/sales`（买家匿名）、`/admin/me/plans/{id}/usage`。
  - 商城：`GET /admin/market/plans`。购买沿用 `POST /admin/billing/me/purchases`；含第三方 API 的套餐需要 `acknowledge_third_party=true`。
  - Owner：`/admin/market/reviews`、`/admin/market/plans/{id}/suspend`、`/admin/market/sales`。
- [ ] **前端**：
  - 商城：合并现有 `plans` 区块。
  - 我的套餐。
  - 销售收入。
  - 套餐审核（显示待审数量）。
  - 商城设置：阶段 1 的设置接口（阶段 2 加了 `max_egress_servers_per_user`）目前还没有界面。
- [ ] **验证**：
  - 金额精确；
  - 并发重复购买只扣一次；
  - 买家与卖家互相购买时不死锁；
  - 余额不足时整体回滚；
  - 成员访问审核、设置、全部成交接口得到 403；
  - 写 `docs/marketplace.md` 和阶段 4 验证记录。

### 阶段 5：加固与文档

- [ ] **重写 `docs/threat-model.md` 的多租户部分**：
  - 单一订阅者的假设已不再成立；
  - 第三方 API 供应商能看到买家的请求内容；
  - 恶意供应商可以返回注入内容，影响 Agent 客户端；
  - OAuth 归属冲突；
  - 卖家与买家之间的数据隔离；
  - 成员自建出口服务器（阶段 2 只接受公网 IPv4，`docs/threat-model.md` 已补一段，阶段 5 合并进完整的多租户部分）；
  - CPA 凭据数量增长。
- [ ] **用户删除与停用**：`upstream_accounts.owner_user_id` 和 `egress_servers.owner_user_id` 都是 `ON DELETE RESTRICT`；已释放的账号仍保留原主人。
  - 信息管理的永久删除，要先处理该用户的供应商、出口服务器、已释放账号的历史归属和仍有有效订阅的套餐，否则给出明确错误，不能只返回数据库错误。
  - 清理任务覆盖 `marketplace_sales`。
- [ ] **告警**：
  - `subscription_layer_empty`：买家已付费，但卖家没有可用账号；
  - `billing_attribution_missing`：扩展到新的层类型。
- [ ] **文档**：
  - `docs/operations.md` 写明升级顺序：备份 → 插件镜像 → 网关迁移 → 打开开关。
  - README 完整介绍。
  - 各阶段的截图。

## 待确认的默认取舍

以下是计划里的默认做法，用户尚未逐条最终确认，实施对应阶段前需要再确认：

1. 自用层永远排第一，用户可以关闭。（阶段 3）
2. 卖家套餐额度用完时不从买家现金补扣，差额由卖家承担（`uncovered_usd`）。（阶段 3）
3. 个人层（平台不限制订阅 + 现金）保持在最后一层，所以受限套餐层总排在现金前面。（阶段 3）
4. 卖家看不到买家身份：销售记录匿名，用量只按套餐和账号聚合。（阶段 4）
5. Owner 的模型权限对所有请求生效，包括自用。（阶段 3）
6. 修改供应商范围无需审核，也不影响续费绑定；修改其他实质内容需要重新审核。（阶段 4）
7. 整个功能由 `marketplace_settings.enabled` 控制，默认关闭；设置读取失败时，租户功能 fail closed。（阶段 1–2 已按此实现）

已经确认的决策：
- 购买款项按抽成比例扣除后，净额记入卖家余额，不做提现；
- 纯余额按量付费只走平台供应商；
- 自用免费；
- 所有用户都可以发布套餐，上架前须经审核；
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
- **截图脚本**：`internal/server/testdata/tenant_providers_browser.cjs` 只拦截成员与租户供应商接口，其他面板的请求返回 404，不影响截图。
