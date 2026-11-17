# 模型全系范围验证

2026-11-17 起，订阅、套餐、批量订阅、现金余额和群组的“适用模型”可以选“GPT 全系”
“Gemini 全系”“Claude 全系”。之前这些范围只能保存具体模型 ID，价格目录新增模型后，
所有已有范围都不包含它，Owner 必须逐个编辑。

## 行为

- **保存形式**：全系以 `family:gpt`、`family:gemini`、`family:claude` 与具体模型 ID 存在同一个
  `model_ids` 数组中，不改表结构。只接受这三个；其他 `family:*` 写入时返回 `400`（store 返回
  `ErrInvalid`）。全系不按当前目录校验，具体模型仍须在当前可管理目录中。
- **归类**：与 Gateway 路由一致（`internal/config/model_family.go`）：`claude-*` → Claude，
  `gemini-*`（含已审的 Antigravity 模型）→ Gemini，其余 → GPT（即走 Codex Provider 的模型）。
  内部 `codex-auto-review` 不属于任何全系。价格目录拒绝以 `family:` 开头的模型名，标记不会
  与真实模型冲突；请求的模型名本身是 `family:*` 时不匹配任何范围。
- **扣费判断**：准入时范围包含模型本身或其所属全系即可。改动的四处：
  - 订阅与现金来源（`snapshotBillingFundingTx`，`model_ids ?| [模型, 全系]`）
  - 资金不足时的续期提示（`billingFundingRetryTx`）
  - 群组额度（`groupModelAllowed`）
  - 群组账号池分层（`ListUserUpstreamAllocationGroups`）
- **默认值**：新建群组未指定范围时、旧版本遗留的未初始化订阅／现金／群组范围（启动时由
  `InitializeBillingSubscriptionModels`、`InitializeBillingModelScopes` 初始化），改为目录中出现的
  全部全系，而不是当时的全部具体模型。控制台新建群组默认勾选三个全系。
- **控制台**：订阅、套餐、批量订阅、现金、群组的模型选择器在每个品牌分组多一个
  “X 全系（自动包含以后新增的 X 模型）”。勾选后该组模型显示为已选并锁定，提交时只发送
  全系标记；取消全系时该组当前模型改为逐个选中，范围不会突然缩小。API Key、用量筛选、
  模型权限、模型鉴定的选择器不提供全系。各处显示范围时全系排在前面，被全系覆盖的具体
  模型不再重复列出。
- **回退**：旧二进制把 `family:*` 当作不存在的模型，不会用这些范围扣费，失败关闭。

## 一次性转换（`0049_model_family_scopes.sql`）

用户选择了在升级时自动转换已有范围。迁移只运行一次，使用的是上一个版本同步的模型目录
（即 Owner 保存这些范围时的目录），按与代码相同但冻结在迁移里的规则分系：

- 逐行检查 `billing_subscriptions`、未被替换的 `billing_subscription_entitlements`、未删除的
  `billing_plans`、`user_groups`、`billing_settings`（现金）。某行已包含某系的全部现行模型时，
  加入该系标记，保留原有具体模型，数组按字节序排序（与 Go `sort.Strings` 一致）。
- 变更的套餐 `version` 加一，与 Owner 编辑相同：旧报价页购买会提示刷新，续费资格不变。
  变更的订阅换新 `config_version`：旧续费页会提示刷新。
- 权益段中的套餐快照（`plan_snapshot`）和账务流水不变。
- 有变更时写一条 `billing.model_families_converted` 审计事件，`metadata.scopes` 按范围
  （subscriptions、entitlements、plans、groups、cash）和全系列出行数，`metadata.rows` 为总数。

## 已运行检查

环境：Go 1.26.11、Node.js 22.23.4、PostgreSQL 16、预装 Chromium。

```sh
gofmt -l .                     # 无输出
go vet ./...
go test -count=1 ./...         # 见下方说明
TEST_DATABASE_URL=postgres://gateway:…@127.0.0.1:5432/gateway_test?sslmode=disable \
  go test -count=1 -tags=integration ./internal/store ./internal/server
node --test internal/server/testdata/model_picker.cjs
PLAYWRIGHT_MODULE=/opt/node-tools/node_modules/playwright node internal/server/testdata/billing_plans_browser.cjs
PLAYWRIGHT_MODULE=/opt/node-tools/node_modules/playwright node internal/server/testdata/groups_access_browser.cjs
```

新增或更新的测试：

| 层 | 测试 | 覆盖 |
|---|---|---|
| config | `TestModelFamilyOfFollowsProviderRouting`、`TestModelScopeAllowsExactModelsAndFamilies`、`TestModelFamilyTokensAreClosed`、`TestModelFamilyScopeCoversCatalogFamilies`、`TestPricingRejectsFamilyTokenModelNames` | 归类与路由一致、Antigravity 模型属于 Gemini、未知标记、`family:` 模型名被价格目录拒绝 |
| server | `TestBillingPeriodAndModelValidation`、`TestModelCatalogGroupsMatchBillingFamilies`、`TestGroupModelIDsInputPreservesOmissionAndValidatesConfiguredModels`、`TestDashboardModelFamiliesMatchConfig` | 写入校验、目录分组与全系一致、控制台的全系表与 Go 一致 |
| store | `TestGroupModelAllowedCoversFamilies` | 群组额度按全系匹配 |
| store 集成 | `TestModelFamilyScopesFundLaterModelsPostgresIntegration` | `family:gpt` 订阅为之后才定价的 GPT 模型扣费、不为 Claude 扣费；耗尽后的续期提示；群组账号池按全系分层；新建群组默认全系；现金接受全系、拒绝未知全系 |
| store 集成 | `TestModelFamilyMigrationConvertsCoveredScopesPostgresIntegration` | 在 0048 结构上造数后应用 0049：全覆盖的订阅、权益段、套餐、群组、现金加标记；部分覆盖不变；套餐版本与订阅配置版本；审计计数 |
| store 集成 | `TestBillingModelScopesPostgresIntegration`、`TestBillingPlanMigrationPreservesLegacyPostgresIntegration` | 初始化改为全系后的预期 |
| Node | `model_picker.cjs` 新增 4 项 | 全系勾选／取消／锁定、只提交标记、仅计费选择器提供、新建群组默认全系、范围文案 |
| 浏览器 | `billing_plans_browser.cjs`、`groups_access_browser.cjs` | 套餐载入全系、清空、勾选 Claude 全系后提交 `["family:claude","gpt-example","gemini-example"]`；新增订阅提交 `["family:gpt","claude-example"]`；截图前等待对话框动画结束 |

截图（已更新）：

- [套餐编辑中的全系选项](screenshots/multi-subscriptions/plan-models-desktop.png)
- [移动端选择器](screenshots/multi-subscriptions/model-picker-mobile.png)
- [订阅卡片](screenshots/multi-subscriptions/subscriptions-desktop.png)
- [套餐目录](screenshots/multi-subscriptions/plans-desktop.png)
- [群组表单](screenshots/groups-form-desktop.png)（[移动端](screenshots/groups-form-mobile.png)）

`internal/server` 的 `TestClientConfigurator`（`configure_client_test.cjs`）在本次的云端会话里
失败，原因是会话自身的 `ANTHROPIC_BASE_URL`、`CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` 环境变量
被客户端配置脚本识别为冲突设置；在改动前的提交上同样失败，去掉这两个变量后通过，与本次
改动无关。集成测试在同一个数据库上重复运行时，`TestPostgresIntegration` 等使用固定用户名的
测试会因用户已存在而冲突，需使用新库。

## 未验证

- 生产数据上的转换结果需在升级后用上面的审计查询核对。
