# 扣费层回退与对话并发占用验证

## 背景

2026-11-04 线上事故：一名用户属于两个优先级都是 100 的群组。一个群组的 GPT 账号池
是“指定账号”但名单为空，另一个只有一个需要重新登录的账号。旧准入只为请求绑定
一个扣费群组，并把请求限定在这个群组的账号池里。CPA 在池里找不到账号，1–2 ms
就返回 429，网关把它显示成“上游当前已达到使用限制”。该用户唯一可用的专属账号，
以及个人余额，始终没有被使用。

## 规则

- **准入（funding rule 4）**：先按成员优先级列出有额度的群组，再列个人来源（订阅
  与现金，沿用原有排序），排成有序“扣费层”。
  - 每个群组层保存准入时的账号池，只保留该用户有权使用的账号：共享账号或授权给
    该用户的专属账号，并满足用户自己的账号权限。
  - 账号池为空的群组整层跳过。
  - 有个人资金时追加一个个人层，个人层不限定账号，仍由 CPA 的每用户资格回调筛选。
  - 有资金但没有任何层时返回 503 `upstream_account_unavailable`，且不写入任何
    quota、usage 或 billing 记录。
- **转发**：网关在 `X-Codex-Gateway-Groups` 中按顺序发送各层，个人层固定为
  `{"group_id":"00000000-0000-0000-0000-000000000000","all_accounts":true}`，且只能
  是最后一层。
  - 只有一个个人层时不发送该头，与升级前一致。
  - 多层时先访问 CPA 的 `/funding-layers` 路由。返回 404 说明是旧 CPA，只发送
    第一层；返回其他错误时 fail closed。
- **CPA 选号**：
  1. 先去掉并发已满的账号。当前对话已经占用名额的账号保留。
  2. 按层顺序调用网关 `/select`，空层不发起调用，返回 429 就换下一层。
  3. 已绑定的账号如果仍在候选中，就优先使用；如果已满或无权使用，就解除绑定，
     按上面的规则重新分配。
  4. 上游 401/402/403/408/429/5xx 或传输错误后，下一轮先换同层的其他账号，再换
     下一层。
  5. 每个请求最多尝试 4 个上游账号。在执行前抢不到名额的账号不计入尝试次数，
     也不会进入冷却、不会解除绑定。
- **结算**：
  - 由第一个“包含实际服务账号、且周期额度与成员上限仍有余量”的群组层支付。
  - 都没有余量时，由第一个包含该账号的群组按原有拆分逻辑支付，剩余部分转给
    个人来源。
  - 账号只在个人层中时，由个人支付。
  - 账号归因缺失时，按第一层结算。如果请求有多层且有用量，产生告警
    `billing_attribution_missing`。
  - 迁移前受理的 rule 1–3 请求按原规则结算。
- **对话并发占用**：账号并发上限按根对话计，子代理与父对话共用一个名额。
  - 每次成功请求结束后，该对话继续占用名额 5 分钟，以最后一次结束时间为准。
  - 对话换到其他账号时，旧账号上的空闲占用立即释放；账号停用时，该账号上的空闲
    占用也立即释放。
  - 以下请求不会建立占用：没有对话标识的请求、count-tokens，以及失败的尝试。
    账号报错还会清掉该对话原有的占用。
- **错误提示**：CPA 自身返回的 429 `upstream_concurrency_exceeded` 显示为“暂无可用
  的上游账号，请稍后重试”，其余 429 仍显示“上游当前已达到使用限制”。CPA 的 503
  `gateway_allocation_unavailable` 映射为 503 `upstream_allocation_unavailable`。这两种
  情况会产生告警 `upstream_account_unavailable`。

## 兼容与安全影响

- 迁移 `0043_billing_reservation_layers.sql` 只能前进。它放宽规则版本约束，并新增
  `billing_reservation_layers`（随 reservation 级联删除，不进入新的清理范围）。
  rule 4 下 `billing_reservations.group_id` 在结算前是临时值（第一个群组层）。
- CPA 原有的能力响应字节不变，分层能力在独立路由上声明，所以新旧网关与新旧 CPA
  可以任意组合，部署顺序不限。
- 付费来源取决于 CPA 返回的账号归因，但只能在准入时预授权的层中选择，CPA 无法
  让未授权的群组付费。
- 补丁 SHA-256 为
  `cb038168680fe81257d940469afd7c782b0cb937d8c913a825cbdfebfce36097`，
  镜像标签为 `codex-gateway-compat:v8.0.48-dbb70621-cb038168680fe812-cpa`。
  `max-retry-credentials` 改为 4，网关选号器使用常量 `gatewayMaxCredentialAttempts`。
- 账号并发上限的含义变为“最近 5 分钟内活跃的对话数”。控制台并发数包含保持中的
  空闲对话，必要时应调高账号的并发上限。

## 验证

CPA（在 `v8.0.48` 上应用补丁后的工作区，使用锁定的 Go 1.26.10 镜像）：

```sh
go build ./... && go vet ./sdk/cliproxy/auth ./sdk/api/handlers ./internal/api
go test -count=1 ./sdk/cliproxy/auth ./sdk/api/handlers ./internal/api ./internal/config \
  ./sdk/cliproxy ./sdk/api/handlers/openai ./sdk/api/handlers/claude
go test -race -count=1 ./sdk/cliproxy/auth ./internal/watcher ./sdk/cliproxy ./sdk/api/handlers
```

新增的 CPA 测试：

- `TestGatewayFundingLayersSelectInBillingOrder`
- `TestGatewayFundingLayersSkipFullAccountsBeforeSelection`
- `TestGatewayFundingLayersHeldConversationKeepsItsAccount`
- `TestGatewayFundingLayersSlotRaceSpendsNoAttemptAndFourAccountsMax`
- `TestGatewayDisableDropsIdleHolds`
- `TestGatewayHoldRequiresConversationAndMovesWithIt`
- `TestGatewayGroupScopeLayersKeepBillingOrder`
- `TestGatewayGroupScopeRejectsMalformedLayers`
- `TestGatewayAllocationErrorBodyKeepsStableCode`
- `TestGatewayFundingLayersCapabilityIsSeparateAndAuthenticated`

已有的根对话并发测试改为验证占用保持：注入时钟，4:59 时仍然占用，5:00 时释放。
镜像构建会核对 Dockerfile 中列出的回归测试名。

网关：

```sh
gofmt -l . && go vet ./... && go test -count=1 ./... && go test -race -count=1 ./...
TEST_DATABASE_URL=… go test -count=1 -tags=integration -p 1 ./internal/store ./internal/server
```

新增的网关测试：

- 扣费层协商：`TestFundingLayersNegotiateWithSidecar`、`TestFundingLayersRejectMalformedPersonalLayer`
- 错误映射：`TestSidecarAllocationFailuresKeepTheirMeaning`
- 序列化格式：`TestAllocationGroupWireFormat`
- 结算选层：`TestSettlementLayerCandidatesFollowServingAccount`

Postgres 集成测试：

- `TestFundingLayersPostgresIntegration` 覆盖以下场景：
  - 事故复现：两个不可用的群组账号池加一个专属账号，应由个人现金支付，群组周期
    不变。
  - 第一个群组在结算前耗尽时，由第二个群组支付。
  - 准入后修改账号池，不影响已保存的快照。
  - 账号归因缺失时，按第一层结算。
  - 零用量请求不扣费。
  - 没有可用账号时直接拒绝，不写入任何记录。
  - rule 3 记录按原规则结算。
  - 重叠群组的并行结算不发生死锁。
- `TestFundingLayersGatewayPostgresIntegration` 走真实 HTTP 准入、转发和结算，只替换 CPA：
  - 新 CPA 收到 `[群组, 个人]` 两层，账本 `group_id` 为空，费用扣在个人订阅上。
  - 旧 CPA 只收到第一层，客户端看到“暂无可用的上游账号”。

已有的集成测试中，断言规则版本 3 的改为 4。依赖“全部账号”群组池的测试通过
`billingIntegrationFundingAccount` 注册一个共享账号。按迁移子集回放的升级测试
通过 `billingIntegrationFundingLayerSchema` 补装 0043。
