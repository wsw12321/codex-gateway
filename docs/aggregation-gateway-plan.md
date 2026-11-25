# 聚合网关改造计划：用户供应商 + 卖家套餐 + 商城

> 2026-11-24 确定的分阶段计划。下文“默认取舍”已全部确认：阶段 4 的两条（买家匿名、只有实质修改才重新审核）于阶段 3 完成后确认，实现见[阶段 4 验证](aggregation-gateway-stage4-validation.md)。阶段 3 开发前确认了四条默认取舍（自用层第一且可关闭、卖家套餐额度不足由卖家承担、个人层保持最后、模型权限对自用生效），实现见[阶段 3 验证](aggregation-gateway-stage3-validation.md)。阶段 2 的两条用户决定（成员自建出口、归属可由 Owner 变更且删除后释放）已写入下文，实现见[阶段 2 验证](aggregation-gateway-stage2-validation.md)。开发分支为 `aggregation-gateway`；已完成的更改、剩余待完成项和开发环境说明见[开发进度与待办](aggregation-gateway-progress.md)。各阶段完成情况见文末“进度”，阶段 1 的验证记录见[聚合网关阶段 1 验证](aggregation-gateway-stage1-validation.md)。

## Context

现在的网关是单租户模型。只有 Owner 能登录上游 OAuth 账号（CPA 托管）或添加 API Key 供应商（`model_suppliers`，Gateway 自己转发）。套餐（`billing_plans`）也只有 Owner 能发布，用户用余额购买后钱不归任何人。`docs/threat-model.md` 明确假设“所有上游账号都由同一订阅者控制”。

目标是改成聚合网关：
- 每个用户都能登录自己的上游账号，或配置 URL + Key 网关（统称“供应商”）。
- 用户能发布套餐（价格、周期、额度、模型与现有一致），并为套餐选择自己的供应商或“不限制”。
- 商城里任何人都能用余额购买任何人发布、且已通过审核的套餐。
- 套餐的供应商变更从下一次请求起生效。

### 已确认的产品决策

| 问题 | 决策 |
|---|---|
| 购买款项 | 扣买家余额，按 Owner 设置的抽成比例（可为 0%）扣除后，净额记入卖家余额。不做提现。 |
| 纯余额按量付费 | 只走平台（Owner）供应商。 |
| 自用 | 用户使用自己的供应商免费（记录用量，扣费 0）。 |
| 卖家资格 | 所有用户都可以添加供应商、发布套餐。套餐上架商城前须经 Owner 审核。 |

### 计划中采用的默认取舍（2026-11-24 已全部确认）

- 自用层永远排第一，用户可在“额度与订阅”页关闭。
- 卖家套餐额度用完时，不从买家现金补扣。超出部分记为 `uncovered_usd`，由卖家承担。原因：买家现金只对应平台供应商；也能防止恶意第三方供应商虚报用量来消耗买家现金。
- 个人层（平台不限制订阅 + 现金）保持在最后一层，这是插件现有契约（`decodeScope` 要求个人层在末位）。所以受限套餐层总排在现金前面。
- ~~卖家不能选择出口节点。~~ 2026-11-24 改为：成员可以新增自己的出口服务器并选择，逻辑与 Owner 的出口一致；不新增或不选择时走全局出口。账号只能绑定同一主人的节点（阶段 2 已实现）。
- 卖家看不到买家身份：销售记录中买家只显示匿名编号，用量只按套餐和账号聚合。
- Owner 的模型权限（`model_access`）对所有请求生效，包括自用。
- 修改套餐的供应商范围：无需审核，不提升 `version`，不影响续费绑定。修改价格、额度、周期、模型、名称、描述：重新进入审核，期间下架，并按现有规则提升 `version`。
- 整个功能由 `marketplace_settings.enabled`（默认关闭）控制。关闭时行为与现在完全一致；设置读取失败时，租户相关功能 fail closed，平台路径不受影响。

---

## 核心设计

### 1. 供应商归属
- 新增 `upstream_accounts.owner_user_id UUID NULL`。`NULL` 表示平台账号，现有账号全部是平台账号。
- 新增 `admin_suspended bool`：Owner 强制停用，卖家不能解除。
- OAuth 和导入的账号：登录在发起时就记下租户身份（`egress_registrations.tenant_user_id`）。回调 `/internal/upstream-accounts/register-egress` 的 prepare 阶段在确保账号行的同一事务里决定归属（阶段 2 实现，`claimRegistrationAccountTx`）。
  - 账号已被其他人或平台拥有时，返回冲突码 `account_owned_elsewhere`（还有 `provider_limit_reached`、`account_in_platform_use`）。
  - 插件收到这些码时，只删除这次登录新保存的凭据文件；被 CPA 原位改写的已有文件保留并恢复原主人的出口，**不再调用 `SetEnabled(id,false)`**。
- **归属可变更，但只能通过两条路径**（2026-11-24 确认）：登录或导入认领已释放的账号；Owner 转移。删除凭据释放归属（`credential_removed_at`），已释放的账号不服务任何请求。数据库触发器只接受带 `claim`/`transfer` 标记的变更。
- API Key 供应商：`CreateModelSupplier` 直接写入 `owner_user_id`。
- `SyncUpstreamAccounts` 永远不修改 `owner_user_id`；商城开关打开期间也不再创建新账号行。
- 平台专属的结构只接受平台账号，由 DB trigger 兜底：群组账号池、专属授权 `upstream_account_users`、用户账号权限名单、模型鉴别。

### 2. 扣费层规则 5（funding rule 5）
在现有规则 4（`internal/store/billing_layers.go` `fundingLayersTx`）的基础上扩展。层的顺序如下：

1. **自用层**：本人在该协议下、未停用的供应商。扣费 0。
2. **群组层**：与现在相同，只含平台账号。
3. **受限订阅层**：每个满足以下任一条件的有效订阅单独成层：
   - 来源套餐是卖家套餐；
   - 来源套餐是“指定供应商”的平台套餐。

   层内顺序沿用 `sortBillingFundingSources` 的优先级规则。层 ID 用订阅 UUID。
4. **个人层**：平台“不限制”订阅（包括 Owner 手工发放的订阅）+ 现金，内部排序不变。个人层只能选平台账号。

**层内账号池在每次准入时按套餐*当前*范围实时解析**：
- 不限制：卖家当前全部账号。
- 指定：名单 ∩ 仍归卖家所有 ∩ 未被强制停用 ∩ 协议匹配。

解析结果为空的层直接跳过。现有上限不变：65 层，每层 256 个账号，且受 `maxGroupsHeader` 约束。

**结算**：
- 按层顺序，找出包含实际服务账号的层。
  - 自用层：扣 0。
  - 订阅层：扣该订阅当前周期的剩余额度，不足部分只能流向后续同样包含该账号的层。
  - 都扣不完的部分记为 `uncovered_usd`。
- 卖家账号的用量永远不会扣到买家现金。
- 规则 1–4 的在途请求按原规则结算。

### 3. “即刻生效”的三道保证
1. **准入**：每个请求都按当前范围重新解析账号池，所以下一次请求一定使用新范围。
2. **请求级资格检查（插件改动）**：
   - 现在插件的 `/eligible` 和 `/select` 只传 `user_id` 加账号列表（`scheduler.go` `allocate` / `selectLayered`），网关分不清当前是哪一层。
   - 改为在两个调用里都带上本请求的层 `groups:[{group_id, account_ids}]`。网关的 `upstreamAccountSelection` 已经能解析 `groups` 字段。
   - 网关逐层实时判断：
     - 个人层：只允许平台账号。
     - 群组层：平台账号，且在该群组内。
     - 订阅层：该订阅属于本人、仍有效，且其来源套餐的当前范围包含该账号。
     - 自用层：账号归本人，且自用已开启。
   - 插件 `allocate` 复用对话绑定时，被绑定的账号也必须在逐层资格的并集内。否则解除绑定，现有逻辑已会这样处理。
   - 插件在能力响应中声明 `layer-select`。网关检测到旧插件时，不生成含租户账号的层（fail closed）。
3. **`/attempt` 与请求账本**：
   - `internal/proxy/ledger.go` 分开保存“非个人层并集”和“含个人层”两个标志。
   - `recordUpstreamAttempt` 拒绝不在非个人层并集里的租户账号。

### 4. 收入与抽成
- 改动 `transactBillingPlan`（`internal/store/billing_plans.go`）。
- 卖家套餐禁止自购。
- 按 UUID 顺序锁定买家和卖家的 `billing_accounts`，防止互买时死锁。
- 用现有 `debitBillingPlanCashTx` 扣买家现金。
- 抽成用 `internal/billing` 精确十进制计算，12 位小数向下取整，抽成比例在成交时固化。
- 卖家净额：新建现金 lot，并写入 ledger `plan_sale_income`。
- 写入 `marketplace_sales`。
- 幂等沿用 `billing_operations` 的 `operation_id`。
- 续费同样分账。

### 5. 审核
- 套餐状态：`draft → pending → approved / rejected`。
- 商城只展示 `approved AND active AND deleted_at IS NULL` 且卖家正常的套餐。
- Owner 自己的平台套餐自动通过审核。
- Owner 可以强制下架套餐，也可以强制停用租户供应商。

---

## 分阶段实施

每个阶段可以独立合并；功能开关关闭时，线上行为不变。

### 阶段 1：归属底座（用户无感知）
- **迁移** `0051_tenant_upstream_ownership.sql`：
  - `upstream_accounts.owner_user_id` 及 `(owner_user_id, provider)` 索引、`admin_suspended`。
  - `egress_registrations.tenant_user_id`。
  - `marketplace_settings` 单行表：`enabled`、`commission_rate NUMERIC(7,6)`（取值 0–1）、`max_accounts_per_user`、`max_plans_per_user`、`allow_member_suppliers`、`updated_by_user_id`。
  - 三类平台专属表的 trigger。
- **只限平台账号的现有路径**：
  - `EligibleUpstreamAccountLimits`（`internal/store/upstream_concurrency.go:87`）
  - `SelectUpstreamAccount`
  - `userPermittedAccountsTx`
  - `snapshotGroupUpstreamAllocationGroupTx` 的 `all` 策略
  - Owner 账号列表
  - 模型鉴别
  - `/v1/models` 的供应商追加
- **未知账号（无 DB 行）**：开关开启时视为不可用，避免登录过程中的竞态。
- **测试**：store 和 server 回归，验证无租户账号时行为完全不变；更新 `TestEmbeddedMigrationsCoverRequiredSchema` 中的迁移数量和 `migrations/README.md`。

### 阶段 2：“我的供应商”（需求 1，已完成）

> 实际实现与下列原计划的差异：新增迁移 `0052_tenant_provider_lifecycle.sql`（释放标记、成员出口服务器、可变更归属的触发器）；成员可以自建出口服务器（只接受公网 IPv4）；Owner 不能把成员账号绑定到平台节点，改为可以停用和转移归属；“从套餐范围中移除”留给阶段 3 按释放标记处理；共享函数的方式改为把成员和协议放进请求上下文，复用现有 handler。详见[阶段 2 验证](aggregation-gateway-stage2-validation.md)。

- **Store**：在账号操作上加 owner scope 谓词。访问他人的账号一律返回 `ErrNotFound`（HTTP 404，不暴露是否存在）。
- **Server**：新建 `internal/server/tenant_providers.go`。
  - 路由 `/admin/me/providers/{codex|anthropic|antigravity}/...`：
    - 列表；
    - OAuth begin、status、callback：OAuth 会话绑定发起人，status 和 callback 校验必须是同一人；
    - 凭据导入（codex、anthropic）；
    - API Key 供应商的增删改：需要 `SUPPLIERS_ENABLED` 且 `allow_member_suppliers`；
    - 状态、别名、并发上限、权重、刷新、额度查看、删除：先排空在途请求，再从套餐范围中移除。
  - 实现方式：把 `cpa_admin.go` 的 `cpaOAuthBegin/Status/Callback/cpaImport` 和 `model_suppliers.go` 的 `saveModelSupplier` 抽成带 tenant 参数的共享函数，Owner 路由和租户路由共用。
  - 中间件：
    - 读：`requireSession`。
    - 写：`browserOrigin(requireRecentVerification(...))`。
    - 新增 `marketplaceEnabled` 门控和每用户限流（OAuth 发起最多 2 个并发、模型发现限频）。
    - 数量上限读 `marketplace_settings`。
    - 审计事件 `tenant_provider.*`。
- **Owner**：
  - 新增 `/admin/tenant-providers`：列出所有租户供应商（含主人），可强制停用或恢复。
  - 现有出口绑定路由允许作用于租户账号。
  - 额度保护（quota guard）同样作用于租户 OAuth 账号。
- **插件**：实现上面“供应商归属”中的冲突清理，补单元测试和 e2e 测试，按 `docs/compatibility-upgrades.md` 重建插件镜像。
- **前端**（`internal/server/assets/app.js` 与 `index.html`）：
  - 新增 `my-providers` 区块，所有人可见，开关关闭时隐藏；OAuth 交互参考 `assets/cpa/panel.js`。
  - Owner 新增 `tenant-providers` 区块。
  - 新增 Node 测试 `testdata/tenant_providers_ui_test.cjs`。

### 阶段 3：套餐供应商范围 + 规则 5 + 自用（需求 2）

> 已完成。与原计划的差异：未新增 `self_use` 列（规则 5 中扣费与未覆盖都为 0 即自用，由约束保证）；`layer_select_v1` 在单独的 `layer-select` 路由上声明；群组层、个人层的逐层判断沿用平台资格；开关关闭时平台套餐的指定范围不生效、卖家套餐订阅暂停扣费。详见[阶段 3 验证](aggregation-gateway-stage3-validation.md)。
- **迁移** `0053_plan_provider_scopes.sql`（阶段 2 已占用 0052）：
  - `billing_plans.seller_user_id`（NULL 表示平台）。
  - `billing_plan_provider_scopes(plan_id, mode all|selected, revision)` 和 `billing_plan_provider_accounts(plan_id, account_id ON DELETE CASCADE)`。现有套餐初始化为 `all`。
  - `billing_subscriptions.source_plan_id`：不可变；从 `plan_id` 回填；逻辑删除或解绑后仍保留，用来找到范围。
  - `billing_accounts.self_use_enabled`，默认 true。
  - `funding_rule_version` 放宽为 1–5。
  - `billing_reservation_layers` 新增 `kind`（group/subscription/self/personal）和 `subscription_id`，并放宽 `all_accounts = (group_id IS NULL)` 约束。
  - 扣费记录新增 `self_use` 标记。
- **Store**：
  - 用规则 5 改写 `fundingLayersTx` 和 `snapshotBillingFundingTx`：受限订阅从个人来源中移出，单独成层。
  - 新增 `resolvePlanPoolTx`。
  - 新增按层判断资格的 `LayerEligibleAccounts`。
  - 改写 `settlementLayerCandidates` / `bindSettlementLayerTx` 和 `settleBillingTx` 的规则 5 分支。
  - 只有自用层时也允许准入。
  - 写范围时校验：卖家套餐的账号必须归卖家，平台套餐的账号必须是平台账号。
- **Server**：
  - `upstream_allocation.go` 的 `/eligible` 和 `/select` 按层判断。
  - `upstream_attempt.go` 和 `proxy/ledger.go` 拒绝层外的租户账号。
  - `supplier_routing.go` 的 `forwardMixed` 改用同一套按层函数。
  - `proxy/account_access.go` 增加能力协商。
  - 新增 `PUT /admin/billing/plans/{id}/provider-scope`（Owner）。
  - 新增 `PUT /admin/billing/me/self-use`。
- **插件**：
  - `gatewayclient.go` 的 `Eligible` / `Select` 带上层信息。
  - `scheduler.go` 按层判断资格，复用绑定前校验层并集。
  - 声明 `layer-select` 能力。
  - 补单元测试和 e2e 测试，重建镜像。
  - 部署顺序：先部署插件，再部署网关。
- **前端**：
  - Owner 套餐编辑器增加“供应商：不限制 / 指定”选择器，按协议分组，显示账号状态。
  - “额度与订阅”页增加自用开关，订阅卡片显示供应商范围。

### 阶段 4：卖家套餐、审核、商城、分账（需求 3，已完成）

> 实际实现与下列原计划的差异：卖家看到的买家标识是每个卖家各自的买家顺序编号（`marketplace_sales.buyer_number`），无需密钥或额外的表；套餐新增 `admin_suspended` 表示 Owner 强制下架；成交记录对账务流水的引用在信息清理删除流水时清空（不阻止清理）；卖家套餐的增删改沿用 `plan_create`/`plan_update`/`plan_delete` 操作类型，提交、审核、下架只写审计事件；第三方 API 确认只针对含成员 API Key 供应商的卖家套餐（平台供应商由 Owner 运营）；续费同样要求套餐已通过审核、未被下架、卖家正常；界面把“销售收入”并入“我的套餐”，把审核、成员套餐处置、全部成交和商城设置放在 Owner 的“商城管理”一页。详见[套餐商城](marketplace.md)和[阶段 4 验证](aggregation-gateway-stage4-validation.md)。
- **迁移** `0054_marketplace.sql`：
  - `billing_plans` 新增 `review_status`（现有套餐为 approved）、`review_note`、`reviewed_by_user_id`、`reviewed_at`、`submitted_at`、`description`。
  - 新建 `marketplace_sales`，字段：`operation_id`、`plan_id/version`、`seller`、`buyer`、`subscription_id`、`kind`、`period_count`、`gross`、`rate`、`commission`、`net`、两条 ledger 引用。
  - 扩展 ledger `entry_type`（`plan_sale_income`）和 operation 类型（卖家套餐增删改、审核、商城设置）。
- **Store**：
  - 卖家套餐 CRUD 复用 `normalizeBillingPlan` / `writeBillingPlan` / `validateBillingModelsTx`。
  - 材料变更进入审核流。
  - 在 `transactBillingPlan` 中分账（见核心设计 4）。
  - 卖家被停用时，其套餐下架、其账号不可选；已购订阅的层被跳过（fail closed）。
- **路由**：
  - 卖家：
    - `GET/POST /admin/me/plans`
    - `PUT/DELETE /admin/me/plans/{id}`
    - `PUT .../{id}/provider-scope`
    - `POST .../{id}/submit`
    - `PUT .../{id}/listing`
    - `GET /admin/me/sales`：明细（买家匿名）和汇总
    - `GET /admin/me/plans/{id}/usage`：订阅人数、消耗、按账号聚合的请求数和 token 数
  - 商城：
    - `GET /admin/market/plans`：所有已上架套餐，包括平台套餐。字段有卖家显示名、模型、价格、周期、额度、最低期数、供应商摘要（官方 OAuth / 第三方 API、各协议当前可用账号数）、`third_party_api` 标记。
    - 购买沿用 `POST /admin/billing/me/purchases`。含第三方 API 的套餐需要买家显式确认 `acknowledge_third_party=true`。
  - Owner：
    - `GET /admin/market/reviews`
    - `POST /admin/market/reviews/{plan_id}`：通过或驳回，附备注
    - `PUT /admin/market/plans/{id}/suspend`
    - `GET/PUT /admin/market/settings`
    - `GET /admin/market/sales`：全部成交和抽成汇总
- **前端**：
  - `商城`：合并现有 `plans` 区块。
  - `我的套餐`：编辑器、供应商范围、审核状态。
  - `销售收入`。
  - Owner 的 `套餐审核`（显示待审数量）和 `商城设置`。
  - 每个区块配 Node 测试和 Playwright 截图脚本。

### 阶段 5：加固与文档
- **重写 `docs/threat-model.md` 的多租户部分**：
  - 第三方 API 供应商能看到买家的提示词和回复，商城必须明示。
  - 恶意供应商可以返回注入内容，影响 Agent 客户端。
  - OAuth 归属冲突。
  - 卖家与买家之间的数据隔离。
  - CPA 凭据数量增长。
- **用户删除与停用**：
  - `information` 的用户删除，在其仍拥有供应商或仍有有效订阅的套餐时拒绝执行。
  - 清理任务覆盖 `marketplace_sales`。
- **监控告警**：
  - `subscription_layer_empty`：买家已付费，但卖家没有可用账号。
  - `billing_attribution_missing`：扩展到新的层类型。
- **文档**：
  - 新增 `docs/tenant-providers.md`、`docs/marketplace.md`。
  - 更新 README、`docs/operations.md`（升级顺序：备份 → 插件镜像 → 网关迁移 → 打开开关）。
  - 按阶段补 `docs/*-validation.md` 和截图。

---

## 关键文件

- **Store**：
  - `internal/store/billing_layers.go`
  - `billing.go`（`reserveBillingTx`、`settleBillingTx`）
  - `billing_funding.go`
  - `billing_plans.go`
  - `upstream_concurrency.go`
  - `upstream_allocation.go`
  - `upstream_account_access.go`
  - `upstream_accounts.go`
  - `egress.go`
  - `group_upstream_accounts.go`
  - `model_suppliers.go`
  - `user_upstream_access.go`
- **Server**：
  - `internal/server/server.go`（routes）
  - `upstream_allocation.go`
  - `upstream_attempt.go`
  - `supplier_routing.go`
  - `cpa_admin.go`
  - `model_suppliers.go`
  - `billing_plans.go`
  - `middleware.go`
  - 新增 `tenant_providers.go`、`marketplace.go`
- **Proxy**：`internal/proxy/ledger.go`、`account_access.go`、`client.go`
- **插件**：`deploy/codex-compat/plugin/internal/gatewayplugin/{scheduler,gatewayclient,credentials,headers}.go`、e2e harness、`plugin.lock.json` 及相关锁文件
- **前端**：`internal/server/assets/{app.js,index.html,style.css}`、`internal/server/testdata/*.cjs`
- **迁移**：`internal/store/migrations/0051–0054`、`internal/store/store_test.go`、`migrations/README.md`

## 可复用的现有实现

- **扣费层快照与按账号结算**：`fundingLayersTx`、`settlementLayerCandidates`、`bindSettlementLayerTx`。
- **套餐校验、购买、续费、幂等**：`normalizeBillingPlan`、`transactBillingPlan`、`debitBillingPlanCashTx`、`claimBillingOperationTx`。
- **模型范围**：`validateBillingModelsTx`、`config.ModelScopeKeys`。
- **供应商 SSRF 防护和发现**：`egress.SupplierBaseURL`、`discoverSupplierModels`、`security.EncryptSupplierKey`。
- **OAuth 与导入外观**：`cpaCall`，插件的 `gateway-management/{provider}`。
- **按用户隔离的写接口模式**：`model_routes.go`（`myModelRoutes`、`replaceMyModelRoutes`）。
- **审计**：`appendBillingAuditTx`、`Server.audit`。
- **前端**：模型选择器、套餐编辑器（`billing_plans` 相关函数）、`ownerOnlySections` 和 `routeFromHash` 的门控模式。

## 验证

每个阶段都要通过：

```sh
gofmt -l . && go vet ./... && go build ./cmd/gateway
go test -count=1 ./...
go test -race -count=1 ./...
TEST_DATABASE_URL='postgres://…/gateway_test?sslmode=disable' go test -count=1 -tags=integration ./internal/store ./internal/server
(cd deploy/codex-compat/plugin && go vet ./... && go test -race -count=1 ./...)
python3 -m unittest discover -s scripts/tests -p 'test_*.py' -v
node --test internal/server/testdata/<新增>_test.cjs
```

重点用例：

- **隔离**：
  - 成员 A 读写 B 的供应商或套餐时得到 404。
  - 成员访问审核、设置、全部成交接口时得到 403。
  - 开关关闭时，新路由全部不可用。
- **即刻生效（PostgreSQL 集成测试）**：
  - 第 1 个请求准入后，卖家把范围从“账号 X”改为“账号 Y”，第 2 个请求的层只含 Y。
  - 卖家撤销账号后，同一请求在下一次 `/select` 重试时被拒绝，对话绑定被解除。
  - “不限制”范围下，卖家新增的账号在下一次请求自动加入。
- **规则 5 结算**：
  - 自用扣 0。
  - 订阅额度不足时不碰现金，差额记 `uncovered`。
  - 个人层永远不会选中租户账号。
  - 规则 4 的在途请求兼容。
- **分账**：
  - 金额精确，抽成比例在成交时固化。
  - 并发重复购买只扣一次。
  - 买家与卖家互相购买时不死锁。
  - 禁止自购。
  - 余额不足时整体回滚。
- **归属冲突**：
  - B 登录 A 已拥有的账号：新凭据被删除，A 的账号保持可用（插件 e2e）。
- **插件兼容**：
  - 旧插件 + 新网关：不生成含租户账号的层。
  - 新插件 + 旧网关：行为不变。
- **界面**：
  - Playwright 截图脚本（桌面 1440×1080、手机 390×844），覆盖我的供应商、我的套餐、商城、审核、销售收入。

## 风险提示

- **服务条款**：把 ChatGPT、Claude 订阅账号的使用权转售给他人，很可能违反供应商条款，账号可能被封。系统没有自动退款，只能由 Owner 手工调整余额。可以考虑在后续阶段加退款流程。
- **隐私**：第三方 API 供应商可以完整看到请求内容；即便是 OAuth 账号，卖家也能看到自己账号的用量。商城文案和买家确认必须写清楚。
- **单实例限制**：API Key 供应商的运行时（并发、粘滞、冷却）保存在内存中，仍只支持单 Gateway 实例。

## 进度

| 阶段 | 状态 | 分支 / 提交 |
|---|---|---|
| 1 归属底座 | 已完成：迁移 `0051`、平台账号过滤、`/admin/market/settings` | `aggregation-gateway` / `2a800c5` |
| 2 我的供应商 | 已完成：迁移 `0052`、认领/释放/转移、成员出口、`/admin/me/providers`、`/admin/tenant-providers`、插件冲突处理、界面 | `aggregation-gateway` / `daec9c6`–`00e666b` |
| 3 套餐供应商范围、规则 5、自用 | 已完成：迁移 `0053`、规则 5 准入与结算、逐层资格（插件 `layer_select_v1`）、平台套餐范围、自用、界面 | `aggregation-gateway` / `612c9b2`–`5db6866` |
| 4 卖家套餐、审核、商城、分账 | 已完成：迁移 `0054`、卖家套餐与审核、分账与成交记录、商城与卖家/Owner 接口、界面 | `aggregation-gateway` / `86ef02d`–`1cab10b` |
| 5 加固与文档 | 未开始 | `aggregation-gateway` |

逐项待办见[开发进度与待办](aggregation-gateway-progress.md#剩余待完成项)。
