# 模型批量权限、账号专属与群组额度

## 使用规则

模型权限页面可以同时选择多个模型和多个用户，一次启用或禁用全部组合。
“全部已有用户”和“新用户默认值”同样支持多个模型；默认值仍只在注册时复制。
批量写入在一个事务内完成，任何无效目标或缺失权限记录都会使整次操作回滚。

上游账号默认为共享。Owner 可以将账号改为专属，并选择至少一位用户。
用户可使用所有共享账号以及授权给自己的专属账号；分配算法只对这些账号计算
原有权重和近 24 小时费用差额。已有会话、重试和故障切换都重新校验授权；
失去权限的绑定会被撤销，正在执行的请求继续完成。权重为零仍仅阻止新分配。

群组页面支持创建、编辑、成员批量加入／移除和空群组归档。每个用户最多属于
一个群组，转组必须先移除。请求按“群组 → 个人日额度 → 周额度 → 月额度 →
现金余额”结算，个人来源遵守本人的禁用设置。群组支付金额为实际费用、群组
剩余额度、成员本期剩余上限三者的最小值，仅余下费用扣个人额度，同一请求可以
分摊。群组有可用额度时，个人额度为零也可接受请求。群组耗尽、额度为零、成员
达到上限、周期未开始或群组已到期时回退个人；所有来源均不可用时才拒绝新请求。

“单个成员周期用量上限”与群组共用周期，留空表示不限，0 表示不使用群组额度。
例如群组周额度 1000、成员上限 100，成员已用 95，本次实际费用 20，则群组
支付 5、个人支付 15。成员已用只统计新规则下实际由群组支付的金额。

固定周期分别是 24 小时、7 天和 31 天；自定义周期使用正整数天。周期起点留空时
创建即生效，指定未来起点时需等待开始。总周期数支持整数 `0–99`，新建默认
`1` 期，`0` 表示无限期；有限期在最后一期结束时到期。空闲期间跨过的周期
照常计数，直接推进到对应期号，不补发或结转额度；到期后停止换期，保留群组
及成员关系。列表、详情和个人账务显示当前期号、总期数、最终到期时间及状态。

仅修改周期数时保留当前周期和群组、成员用量，沿原时间轴调整最终到期时间；
有限总期数不得小于当前期号。延长已到期群组也沿原时间轴计算，仅当新期限覆盖
当前时间时恢复，并推进到应处周期；延长并不从操作时刻重新计时。仅修改群组
额度或成员上限时保留累计，下调至已用以下后回退个人。修改周期类型或起点时
关闭旧期、从第 1 期重开；过去起点选择包含当前时刻的周期作为第 1 期。
自然换期和重开周期都会从 0 统计新周期的成员用量。
日期支持公元 1–9999 年；自定义长周期的最终日期越界时拒绝操作。

群组和成员计数在结算事务中原子更新，不因并发超额或重复结算而透支。已接收
请求最终资金不足时记录未覆盖费用，不产生负余额。请求接收时固定资金来源、
群组和周期，跨期、移除成员、调组、重开周期和归档均不改变其结算归属；后来
新增的资金来源不会追加给旧请求。模型目录查询不消耗群组额度。

成员计数独立于历史流水和成员关系，以“群组周期＋用户”为键。清理历史和退群
再加入不会恢复额度。正式删除用户时清理对应成员计数，群组总用量保持不变。

## 接口与持久化

所有管理写入沿用 Owner、同源请求和近期身份验证检查，并记录审计。
群组写入使用 `operation_id` 与 `reason`，同一操作的重试返回原结果，参数冲突
返回 409。创建、编辑和查询群组使用可空十进制字符串 `member_limit_usd`；创建时
省略或传 `null` 表示不限，编辑时省略保留现值、传 `null` 清除上限。
群组详情的成员 `used_usd` 和可空 `remaining_usd` 分别表示本期群组支付和上限
剩余，`remaining_usd: null` 表示不限。个人账务接口提供 `group` 摘要及
`group_member_used_usd`、`group_member_remaining_usd`，仅包含所查看用户的数据，
不包含其他成员明细。

创建、编辑接受可选整数 `period_count`：创建省略默认 `1`，编辑省略保留原值；
`null`、小数、字符串和超出 `0–99` 的值均被拒绝。列表、详情和个人账务的
`group` 返回 `period_count`、`current_period_number`、`expires_at`，无限期的
`expires_at` 为 `null`。显式传入周期数时参与操作幂等校验，省略时兼容旧指纹；
旧操作响应缺少期限字段时按自身周期快照补为 `1/1`，最终到期为该快照的周期
结束时间，重试不会读取新配置来改写旧操作结果。

用量流水增加 `group_charged_usd` 和 `personal_charged_usd`，`charged_usd` 继续
表示总已覆盖金额；非用量记录不提供支付分摊字段。每笔请求满足
`actual_cost_usd = group_charged_usd + personal_charged_usd + uncovered_usd`。

| 接口 | 用途 |
| --- | --- |
| `GET /admin/model-access/users?models=a&models=b` | 查询多个模型的用户权限 |
| `PUT /admin/model-access/users` | `models × user_ids` 或全部用户批量写入 |
| `PUT /admin/model-access/defaults` | 多模型注册默认值 |
| `PUT /admin/upstream-accounts/{id}/access` | `mode`、`user_ids`、`reason` |
| `GET/POST /admin/groups` | 列表／创建群组 |
| `GET/PUT/DELETE /admin/groups/{id}` | 详情／编辑／归档空群组 |
| `PUT /admin/groups/{id}/members` | `action: add/remove`、`user_ids` |

原单模型权限接口保持兼容。迁移 `0011_upstream_account_access.sql` 和
`0012_user_groups.sql` 位于 `internal/store/migrations`；既有账号默认共享，
既有用户无群组，既有账单不补扣。迁移后保留不可变的原始请求账单及群组周期引用。

新规则下群组不可用时继续检查个人额度，所有资金来源均不可用时返回 HTTP 429、
错误码 `insufficient_quota` 和 `Retry-After`。授权查询失败、身份无效或缺少可用
上游账号时拒绝转发。`Retry-After` 仅参考实际存在的后续周期，已到期群组不会
提供虚假的续期时间；到期前已受理的请求继续使用原绑定周期结算。

## 验证

使用以下命令完成验证；数据库必须使用独立的一次性 PostgreSQL，具体检查结果
以本次执行记录为准：

- `go test -count=1 ./...`
- `go test -race -count=1 ./...`（固定 Go 1.26.8 构建镜像，启用 CGO）
- `go vet ./...`，网关二进制构建及 `gofmt` 检查
- `TEST_DATABASE_URL=... go test -count=1 -tags=integration ./internal/store`
- `node internal/server/testdata/model_access_batch_browser.cjs`
- `node internal/server/testdata/groups_access_browser.cjs`
- `node internal/server/testdata/groups_billing_ui_test.cjs`（由 Go 资产测试自动调用）
- gateway 与 codex-compat 镜像构建；兼容服务内置回归及选路竞态测试
- `scripts/test-sidecar-image.sh` 的隔离运行校验
- `scripts/validate-compose.sh`（使用临时副本、`.invalid` 域名和非生产密钥）

本次周期数变更于 2026-10-07 完成以下验证，均通过：

- Go 1.26.8：`go test -count=1 ./...`、`go vet ./...`，以及网关构建（输出到仓库外）。
- 受影响包竞态检查：`go test -race -count=1 ./internal/store ./internal/server`。
- 一次性 PostgreSQL 16.15：`go test -count=1 -tags=integration ./internal/store ./internal/server`。
- 群组数据库竞态回归：`go test -race -count=1 -tags=integration ./internal/store ./internal/server -run 'TestGroup(ExpiryFunding|PeriodRetryAfter|PeriodLimits|sPostgres|PriorityBilling)'`。
- 9 项群组 Node 回归及 Chrome 155 桌面、移动端浏览器回归；浏览器无页面错误，更新 6 张群组截图。
- `gofmt -l .` 无输出，`git diff --check` 无错误。

迁移回归覆盖生效中、未来开始、已结束和已归档群组的快照保留。资金回归覆盖
最终到期后的个人回退、全来源不足拒绝、延迟及重复结算、期限延长、空闲跳期、
新旧幂等重试，以及超长自定义周期和年份上限；日期范围内不存在完整下期时，
停止换期且不承诺续期，个人资金仍可正常使用。一次性测试数据库已清理。

浏览器脚本使用 `PLAYWRIGHT_MODULE` 指定可选测试驱动，所有 HTTP 请求均被本地
合成数据拦截，不访问真实 OAuth 账号。截图覆盖桌面和移动端：

- [模型权限桌面](screenshots/model-access-desktop.png)、[移动端](screenshots/model-access-mobile.png)
- [群组桌面](screenshots/groups-desktop.png)、[移动端](screenshots/groups-mobile.png)
- [周期数与成员上限表单桌面](screenshots/groups-form-desktop.png)、[移动端](screenshots/groups-form-mobile.png)
- [支付分摊流水桌面](screenshots/groups-billing-desktop.png)、[移动端](screenshots/groups-billing-mobile.png)
- [专属账号桌面](screenshots/upstream-exclusive-desktop.png)、[移动端](screenshots/upstream-exclusive-mobile.png)

回归覆盖原子批量回滚、授权变更撤销旧绑定、身份伪造和内部头泄漏、多人并发结算、
群组全额付款、群组／成员边界分摊、个人回退、资金不足的未覆盖费用、自然换期和
手动重开、换组／归档后的延迟结算、幂等重试、取消、零额度、上限修改、退群再
加入和清理历史后的计数、刷新失败，以及身份改变后的迟到响应和旧会话 401。
浏览器另外覆盖周期数默认值、有限期、无限期、到期状态、小于当前期号的拒绝、
修改总数后保留群组和成员用量，以及成员上限的空值、零值及非法输入；桌面和
移动端截图包括周期数表单、期限摘要及包含群组／个人／未覆盖金额的流水。

## 发布

本次周期数升级使用 `0031_group_period_limits.sql`。所有既有群组（含未来开始、
周期已结束和已归档群组）都设为第 `1/1` 期，以迁移前保存的当前周期结束时间
作为最终到期时间，保留原周期 ID、起止时间、额度、群组用量及成员用量。
迁移时周期已经结束的群组视为到期；需要继续使用时由 Owner 明确增加总期数
或重开周期。升级前记录这些值，迁移后逐项核对。生产迁移和部署另行执行，
详细步骤见 [群组周期数迁移与升级](operations.md#群组周期数迁移与升级)。

以下是较早的群组优先扣费升级（`0027`）规则，与本次 `0031` 保留成员用量的
行为不同：

群组优先扣费升级前备份数据库，并停止全部旧网关写入，再运行新镜像迁移和启动。
不得同时运行会写入账务的新旧网关。升级保留群组当前周期和总已用金额，所有成员
用量统一从 0 开始，不回填历史成员用量，也不退还历史个人扣款。
迁移前已受理请求标记为旧扣费规则，继续使用原个人来源及原群组周期结算，不增加
新成员计数；迁移后新受理请求采用群组优先规则。完整步骤见
[群组优先扣费迁移与升级](operations.md#群组优先扣费迁移与升级)。

gateway 与 codex-compat 必须配套更新。网关每次 Codex 生成请求检查兼容服务的
`upstream_account_access_v1` 能力；旧镜像缺少此能力会被拒绝，不能绕过专属限制。
此版本兼容镜像为 `codex-gateway-compat:v7.2.150-c77b1369-cad1f15d9d14a854-codex-only`。

本次仅构建与验证，没有部署、启动生产服务、修改现有 `.env` 或操作真实 OAuth。
直接侧车冒烟只检查元数据、模型目录和权限协议；真实 JSON／SSE 生成验证应使用
有效用户 API Key 经 Gateway 完成。
