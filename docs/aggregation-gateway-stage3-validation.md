# 聚合网关阶段 3 验证：套餐供应商范围、扣费层规则 5 与自用

2026-11-24 完成[聚合网关改造计划](aggregation-gateway-plan.md)的阶段 3。成员自己的账号开始为成员服务（自用，不扣费）；Owner 可以把平台套餐的供应商范围设为“指定账号”；为阶段 4 的卖家套餐准备好了只为卖家账号付费的订阅层。插件改为按扣费层询问账号资格，Gateway 逐层实时判断。

功能由 `marketplace_settings.enabled` 控制，默认关闭。关闭时：新路由全部返回 404；新请求不生成自用层和受限订阅层，按规则 4 准入和结算，与升级前一致。

## 本阶段确认的取舍

2026-11-24 开发前与用户逐条确认，均按计划默认执行：

1. 自用层永远排第一，成员可以关闭。
2. 卖家套餐额度不足时不从买家现金补扣，差额记为 `uncovered_usd`，由卖家承担。
3. 个人层（平台“不限制”订阅 + 现金）保持在最后一层，受限订阅层总排在现金前面。
4. Owner 的模型权限对所有请求生效，包括自用。

## 行为

### 准入（规则 5）

`reserveBillingTx` 按以下顺序生成扣费层，每层的账号池在准入时实时解析：

1. **自用层**（`SelfFundingLayerID = 00000000-0000-4000-8000-000000000002`）：商城开关打开、本人自用开关打开时，本人在该协议下未被停用、未释放的账号。
2. **群组层**：与规则 4 相同，只含平台账号。
3. **受限订阅层**（层 ID 为订阅 UUID）：来源套餐是卖家套餐，或开关打开时范围为“指定”的平台套餐。这些订阅从个人来源中移出，按订阅优先级排序（与 `sortBillingFundingSources` 相同），各自绑定准入时的周期。账号池由 `resolvePlanPoolTx` 按套餐当前范围解析：“不限制”为卖家当前全部账号；“指定”为名单中仍归卖家所有、未停用、未释放、协议匹配的账号；卖家不是正常状态时为空。平台套餐的账号池再经过用户的平台账号权限过滤。
4. **个人层**：其余订阅与现金，只能选平台账号。

- 解析结果为空的层直接跳过；只有自用层时也允许准入。
- 没有自用层和受限订阅层时，记录为规则 4，走原有代码路径。
- 规则 5 的层数上限为 64（CPA 接受的上限），每层最多 256 个账号。
- 开关关闭时：卖家套餐的订阅不参与扣费（它只为卖家账号付费，而成员账号此时不服务任何请求）；平台套餐的指定范围不生效，订阅回到个人来源。

### 逐层资格与选路

- **插件**（`layer_select_v1`）：请求带扣费层时，`/eligible` 和 `/select` 改为只带 `groups` 的请求体，每层列出本层候选（个人层列出全部候选）。可用账号是各层资格的并集；对话绑定只在被绑定账号仍在某层有资格时复用；选路按层顺序逐层选择。插件在单独的 `layer-select` 路由上声明该能力，已有能力应答不变（Gateway 严格解码这些应答）。没有扣费层的请求仍用原来的列表协议。
- **Gateway** `LayerEligibleAccounts` 按层 ID 判断，不信任调用方声明的层类型：
  - 自用层 ID：本人账号，商城开关和自用开关打开，本人为正常状态；
  - 本人的订阅 ID：订阅启用、允许扣费、未到期；卖家套餐要求开关打开且卖家正常，账号在来源套餐当前范围内；平台“指定”套餐要求账号在名单内且通过平台资格；订阅不能付费时该层没有账号；
  - 其他 ID（群组、个人层、未知）：只有平台账号，与之前相同。
  - 所有层都排除额度保护暂停的账号。
- 成员层内的选择是对刚授权账号的加权选择（`selectAuthorizedUpstreamAccount`）；平台层仍用 `SelectUpstreamAccount`。
- `/eligible`、`/select` 的请求体上限从 8 KiB 提高到 64 KiB（分层请求会在多个层中重复同一账号）。
- **转发前协商**：请求含自用层或卖家套餐层时，Gateway 先探测插件的 `layer-select`。旧插件（404）收不到这些层；去掉后没有剩余层的请求返回 503 `upstream_access_protocol_unavailable`，不会改用其他账号。
- **`/attempt` 归因**：只接受在本次请求某个层的名单里、且此刻在该层仍有资格的账号，或个人层下的平台账号；请求账本保存每个请求的层。
- **API Key 供应商混合路由**（`forwardMixed`）：使用同一套逐层资格和选择，加载出现在成员层中的成员供应商；官方账号按选中它的层转发。
- **`/v1/models`**：成员供应商的模型只有在本人的成员层（自用或卖家套餐）此刻能用到该供应商时才出现，已停用、已释放的不算。

### 结算（规则 5）

`settleBillingTx` 按实际服务账号找出包含它的层：

- **自用层**：扣 0，`actual_cost_usd` 照记，`charged_usd` 与 `uncovered_usd` 都为 0。这是唯一不满足 `actual = charged + uncovered` 的结算，数据库约束只在规则 5 下允许，接口据此给出 `self_use: true`。
- **群组层**：与规则 4 相同，绑定第一个仍有额度的群组。
- **受限订阅层**：按层顺序从准入时绑定的周期扣费（分配记录 `source_type = 'subscription'`）。
- **个人层**：只有服务账号是平台账号时才扣个人来源。成员账号判断同时看准入层和当前归属，账号行缺失按成员账号处理。
- 扣不完的部分记为 `uncovered_usd`。卖家账号的用量永远不扣买家现金。
- 缺少归因时不按自用处理：由第一个非自用层支付（与规则 4 相同）；只有自用层时整笔记为未覆盖。不在任何层里的成员账号不扣任何资金。
- 规则 1–4 的在途请求按原规则结算。

### 套餐范围与自用开关

- `SetPlanProviderScope`：带版本校验（`revision`），校验名单中的账号都属于套餐的卖家（平台套餐为平台账号）且仍有凭据，每个协议最多 256 个；不提升套餐版本，不影响续费绑定；写审计 `billing.plan_provider_scope_changed`。数据库触发器同样拒绝不属于卖家的账号。
- `PlanProviderScopes` 列出范围及每个账号当前是否可用；账号被转出、删除或停用后保留在名单里但标记为不可用，不再服务该套餐（阶段 2 留下的“从范围中移除”按此实现）。
- `SetSelfUse` 与准入共用账务账户锁，写审计 `billing.self_use_changed`。
- 额度保护：成员 OAuth 账号开始服务请求后，自动检查与平台账号相同（`ClaimUpstreamQuotaCheck` 不区分归属），暂停期间不出现在任何层。集成测试已验证一次。

### 路由

全部在商城开关关闭时返回 404 `marketplace_disabled`：

| 路由 | 权限 | 说明 |
|---|---|---|
| `GET /admin/billing/plan-provider-scopes` | Owner | 平台套餐的范围和可选平台账号 |
| `PUT /admin/billing/plans/{plan_id}/provider-scope` | Owner、同源、5 分钟内验证 | `{"mode","account_ids","revision"}`；套餐不存在 404，版本冲突 409 `plan_scope_changed`，账号无效 400 `invalid_plan_scope` |
| `PUT /admin/billing/me/self-use` | 成员、同源、5 分钟内验证 | `{"enabled": bool}`；Owner 403 |

`GET /admin/billing/me`（及 Owner 查看用户）在开关打开时增加 `self_use_enabled`，以及每个由套餐购买的订阅的 `provider_scope`（模式、是否卖家套餐、是否单独成层、各协议当前可用账号数）。账务流水的自用记录带 `self_use: true`；全站用量汇总新增 `self_use_usd`，按 `charged + uncovered + self_use = actual` 核对。

### 界面

- Owner 套餐卡片显示“供应商：不限制”或“指定 N 个账号，M 个已失效”，并有“供应商范围”编辑器：按协议分组勾选平台账号，停用的账号有标注，已转出或删除的账号列在提示中并在保存时移除。
- 成员“额度与订阅”页有“自用”开关；订阅卡片显示供应商范围；流水显示“自用：使用自己的供应商，不扣费”。

截图（`internal/server/testdata/plan_scopes_browser.cjs` 生成）：

- `docs/screenshots/plan-scope-owner-desktop.png`、`plan-scope-owner-mobile.png`
- `docs/screenshots/plan-scope-editor-desktop.png`、`plan-scope-editor-mobile.png`
- `docs/screenshots/plan-scope-member-billing-desktop.png`、`plan-scope-member-billing-mobile.png`

## 与计划的差异

- **没有新增 `self_use` 列**：计划在扣费记录上加 `self_use` 标记。实现改为用约束表达：规则 5 结算中“有成本、扣费 0、未覆盖 0”恰好就是自用，接口据此给出 `self_use`。这样升级测试回放旧迁移时不会因多出的列而改变受保护的行快照。
- **能力声明放在单独路由**：`layer_select_v1` 由新的 `layer-select` 路由声明，而不是在现有能力应答中加字段，因为现有 Gateway 严格解码这些应答，先部署的新插件会让旧 Gateway 失败。
- **群组层的逐层判断沿用规则 4**：群组层（及个人层、未知层）的资格仍是平台资格，不在选路时重新核对群组成员关系；群组账号池仍以准入快照为准（规则 4 的既有设计）。逐层判断的重点是成员账号只能出现在为它付费的层。
- **开关关闭时的受限订阅**：计划未写明。实现为：平台套餐的指定范围不生效（与升级前一致），卖家套餐的订阅暂停扣费（失败关闭）。
- **旧插件的处理**：按计划不发送含成员账号的层；由于旧插件的列表资格本来只接受平台账号，即使发送也不会选中成员账号，这里做成显式去掉并在无层可用时返回 503。
- **升级测试的旧库兼容**：准入、购买和结算对 0053 之前的库结构（只在回放旧迁移的升级测试中出现）用 `to_regclass` 判断，与准入路径已有的结构判断一致。

## 数据库

`0053_plan_provider_scopes.sql`（只能前进）：

- `billing_plans.seller_user_id`（NULL 为平台套餐，不可修改）。
- `billing_plan_provider_scopes(plan_id, mode, revision, updated_at, updated_by_user_id)`，现有套餐初始化为 `all`；`billing_plan_provider_accounts(plan_id, account_id)`，触发器只接受套餐卖家仍有凭据的账号。
- `billing_subscriptions.source_plan_id`：从 `plan_id` 回填，不可修改。
- `billing_accounts.self_use_enabled`（默认 true）。
- `billing_reservations`：`funding_rule_version` 放宽为 1–5；规则 5 可以没有个人来源；结算金额约束允许规则 5 的自用结算。
- `billing_reservation_layers`：新增 `kind`（`group`/`subscription`/`self`/`personal`，回填现有行）、`subscription_id`、`subscription_period_id`，替换个人层唯一索引，新增自用层和订阅层唯一索引。

升级无需人工回填。旧二进制遇到 0053 会拒绝启动；回退需要恢复备份。部署顺序：备份 → 插件镜像 → Gateway（执行迁移）→ 打开开关（见 `docs/operations.md`）。

## 测试

- **Store 集成测试** `TestFundingRule5PostgresIntegration`：开关关闭时保持规则 4、不使用本人账号；只有自用时准入并按 0 结算、流水标记自用；缺少归因不按自用；关闭自用后拒绝准入；自用层排在个人层前，平台账号扣现金、本人账号免费；个人层不接受本人账号；停用后立即失去资格；本人 OAuth 账号被额度保护认领，暂停后失去资格；平台套餐范围从下一次请求起生效，第一个请求按自己的准入快照结算（订阅 5 + 现金 2），范围外账号不扣订阅；过期版本 409，成员账号不能进入平台范围；关闭开关后范围不生效；卖家套餐只为卖家账号付费，卖家新增账号在下一次请求加入，额度不足记未覆盖且不扣买家现金，释放的账号立即移出，关闭开关后订阅层关闭；规则 4 在途请求在开关打开后按原规则结算。
- **Store 单元测试** `TestRule5SettlementCandidates`、`TestFundingRuleVersionAndLayerIDs`。
- **Server 集成测试** `TestPlanScopesAndLayersHTTPPostgresIntegration`：开关关闭时路由 404、账务页无自用字段；范围的权限、校验、版本冲突和候选账号；账务页的自用和订阅范围；逐层 `/eligible`（自用层只有本人账号，订阅层只有名单账号，个人层只有平台账号）；列表协议仍只返回平台账号；自用层选择；自用开关的权限与效果；`/attempt` 接受自用层的本人账号和个人层的平台账号，拒绝他人的成员账号和放错层的本人账号。
- **Proxy 单元测试** `TestTenantLayersNeedLayerSelect`、`TestLedgerDispatchLayers`。
- **插件单元测试** `TestLayerEligibilityKeepsMemberAccountsInTheirLayer`、`TestBindingRequiresEligibilityInSomeLayer`、`TestUnlayeredRequestsKeepTheListProtocol`、`TestGatewayClientLayerContract`。
- **插件 e2e** `TestLayerSelect`（已加入 Dockerfile 必过列表）：经过真实 CPA，本人账号只通过自用层服务，关闭后个人层只选平台账号。
- **界面** `plan_scopes_ui_test.cjs`（`TestPlanScopesDashboardBehavior`）与 `plan_scopes_browser.cjs`。

## 已运行检查

环境：Go 1.26.11、Node.js 22、Playwright（Chromium）、PostgreSQL 16（一次性本地数据库）。

```sh
gofmt -l .                       # 无输出
go vet ./...
go vet -tags=integration ./internal/store ./internal/server
go build ./cmd/gateway
env -u ANTHROPIC_BASE_URL -u CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST go test -count=1 ./...
env -u ANTHROPIC_BASE_URL -u CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST CGO_ENABLED=1 go test -race -count=1 ./internal/store ./internal/server ./internal/proxy
TEST_DATABASE_URL='postgres://…/gateway_test?sslmode=disable' \
  env -u ANTHROPIC_BASE_URL -u CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST \
  go test -count=1 -tags=integration ./internal/store ./internal/server   # 新建的空数据库
(cd deploy/codex-compat/plugin && go vet ./... && go vet -tags e2e ./... && CGO_ENABLED=1 go test -race -count=1 ./...)
python3 -m unittest scripts.tests.test_cpa_plugin_deploy
env -u ANTHROPIC_BASE_URL -u CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST \
  node --test internal/server/testdata/*_test.cjs                # 297 项：296 通过，1 项按设计跳过
node internal/server/testdata/plan_scopes_browser.cjs
node internal/server/testdata/tenant_providers_browser.cjs
node internal/server/testdata/dashboard_layout_browser.cjs
```

以上全部通过。

**插件 e2e**：本地没有 Docker 守护进程，按 `plugin.Dockerfile` 的构建阶段手工复现：拉取 CLIProxyAPI `v8.1.1`（校验 commit `6eb8554f…`），应用残留补丁，按安全依赖下限升级模块，构建 `cli-proxy-api`，再以 `-buildmode=c-shared` 构建插件，然后运行 `CPA_E2E_BINARY=… CPA_E2E_PLUGIN_DIR=… go test -tags e2e -count=1 -v ./e2e`：19 个用例全部通过，无跳过，包括新增的 `TestLayerSelect`。

**未运行**：完整镜像构建（`./scripts/compose.sh build codex-compat`、`./scripts/test-cpa-plugin-image.sh`）需要 Docker；`scripts/tests/test_lock_images.py` 需要镜像仓库工具（与本改动无关的已知情况）。

**已知与本改动无关的失败**：在设置了 `ANTHROPIC_BASE_URL` 和 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` 的执行环境中，`TestClientConfigurator` 的两个 Node 用例失败（配置器把它们识别为冲突的 Claude 设置）；去掉这两个变量后通过。
