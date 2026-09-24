# 上游归因、实时并发与信息管理

本次交付包含 gateway、codex-compat 补丁和数据库迁移 `0013_information.sql`、`0014_information_users.sql`。迁移只建立任务、删除防护和幂等记录；部署或打开页面不会创建历史清理任务。

## 管理行为

- Owner 的使用统计与 CSV 增加 `upstream_account_id`、`upstream_masked_email`。成功请求使用最终账号，失败请求使用最后尝试账号；历史缺失显示“未归因”，不会根据邮箱或当前账号推测补填。普通用户响应使用独立 DTO，不包含这两个字段。
- 上游账号的并发来自兼容服务的活跃 root 对话计数，包含等待首响应和流式输出。同一 root 在同一账号上的重叠请求、subagent、fork 和 side 对话共享一个名额；切换到其他账号时在新账号独立计数。结束、失败和取消只释放对应 root 的最后一个执行，禁用账号不清零仍在执行的请求。Gateway 自身的请求级 lease 仍按每个请求计数。
- 并发页面进入时立即查询，随后每 5 秒刷新。页面隐藏、离开和退出登录会取消轮询。刷新只改并发数值，保留未提交的账号设置。接口不可用、旧兼容服务不支持、快照失效或缺少某账号时显示“暂不可用”；只有明确采样为零才显示 `0`。
- 信息管理默认保留 90 天。截止时间为 UTC 当日零点减去保留天数，采用严格“小于截止时间”的边界。确认框显示预览返回的准确 UTC 时间，跨午夜确认仍沿用已确认的截止时间。
- 清理任务状态和每批计数持久化；同一数据库只运行一个任务。维护进程每秒推进一批，重启后继续未完成任务。批次失败会回滚并保留安全错误提示，后续自动重试；不会把原始 SQL 错误写入页面。
- 预览和执行结果提供删除数量、保留数量与原因。预览不是锁定承诺，执行批次会重新检查关联。
- 候选用户必须是普通用户且没有余额、请求、汇总、账务或订阅历史及预留。注册时产生的空账务账户不阻止删除。每批最多 100 人；事务重新核验全部目标，任一不再符合条件则整批回滚并返回原因。
- 用户删除会移除凭证和权限关联，保留安全审计中的原用户、会话及 Key 标识快照；群组已用额度不回退。重复操作 ID 只重放已成功的结果，参数变化会拒绝。

## 接口

所有以下接口限 Owner；写操作要求同源和近期身份验证。预览没有持久化副作用，只要求同源及 Owner 会话。

| 方法与路径 | 输入或响应 |
| --- | --- |
| `GET /admin/upstream-accounts/concurrency` | `{sampled_at, accounts:[{id, active_requests}]}` |
| `GET /admin/information` | `{latest_job, active_job, cleaned_before}` |
| `POST /admin/information/preview` | 输入 `{retention_days}`；响应 `{retention_days, cutoff, delete_counts, retained_counts, retained_reasons}` |
| `POST /admin/information/jobs` | 输入 `{operation_id, retention_days, cutoff}`；返回 202 及任务 |
| `GET /admin/information/jobs/{id}` | 任务状态、已完成批次计数、错误提示 |
| `GET /admin/information/deletable-users` | `search`（兼容 `q`）、`limit`、`offset`；返回候选列表 |
| `POST /admin/information/users/delete` | 输入 `{operation_id, user_ids}`；返回 `{deleted_count, user_ids}`；资格变化返回 409 和 `blockers` |

`operation_id` 使用规范 UUID。任务创建或删除请求结果不明确时，客户端保持原操作 ID 重试，避免重复提交。普通使用统计和全员统计同步返回 `cleaned_before`，页面提示历史清理范围。

## 数据保留与并发安全

请求相关记录以请求发生时间为准，即使结算时间较晚或请求明细已经自动过期，也会使用预留时间或账本的 `usage_requested_at`。请求必须结束，资金及额度预留必须已经结算或释放。当前可用资金批次、有效订阅及仍被保留记录引用的历史不会删除，当前余额、订阅余额和群组已用额度不重算。

清理删除符合条件的消费分摊、预留、账本、操作快照、失效资金批次和已结束订阅历史。删除账务操作后保留只有操作 ID 和清理时间的 tombstone，不含金额或用户外键；重用 ID 返回 `billing_operation_cleaned`。

账本、订阅操作快照和 Key 历史仍拒绝普通 UPDATE/DELETE。清理必须在同一事务中授权确切记录，用户删除必须在同一事务中完成目标核验。清理、汇总、自动保留期清理和用户删除共享数据库 advisory lock；涉及请求和资金时依次取得既有额度锁和账务账户锁。

旧日/月汇总分批删除，跨截止日期的月份按剩余日汇总与完整明细重建可累加指标，无法精确计算的 p95 保留 NULL。迟完成请求和新增维度先持久化到日汇总，避免既有快照遮蔽新增用量，或明细再次过期后丢失增量。后续自动汇总会遵循已确认的清理边界；旧月记录由清理任务统一删除和计数，避免自动汇总少报删除数或重新生成已清理历史。

当前实现每批锁定全部账务账户以获得一致的依赖视图，批次提交后释放。大规模数据库清理可能增加请求入场、充值及结算等待时间；预览也会扫描历史数据。默认工作批量为 500，最终跨月重建以一个事务提交。

## 验证记录

2026-09-22 完成以下验证。宿主机没有 Go/C 编译器，使用已校验官方 Go 1.26.8 和项目固定的 Go Docker 镜像；PostgreSQL 17 使用独立 localhost 端口及临时数据目录。信息清理测试使用各自临时 schema，避免影响其他历史统计测试。

```sh
go build ./cmd/gateway
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
TEST_DATABASE_URL='<disposable PostgreSQL>' go test -count=1 -tags=integration ./internal/store
TEST_DATABASE_URL='<fresh disposable PostgreSQL>' go test -race -count=1 -tags=integration ./internal/store
gofmt -l .
git diff --check
```

覆盖：归因成功/流式/失败/取消/切换、JSON/CSV 权限隔离、并发等待/长流/取消/超时/禁用/重复释放；UTC 边界、跨月、超过默认明细保留期、迟结算请求、任务恢复、防重复充值、余额及群组额度不变、清理与结算/汇总并行；空用户删除、清理后删除、Owner 拒绝、整批回滚及新请求/充值竞争。

Node 页面交互测试和三套 Chromium 浏览器回归通过，包括任务创建重试保留操作 ID、近期验证、删除冲突展示、并发刷新保留编辑内容、离页停止以及管理员/普通用户隔离。截图：

- [信息管理桌面](screenshots/information-desktop.png)、[信息管理移动端](screenshots/information-mobile.png)
- [实时并发桌面](screenshots/upstream-concurrency-desktop.png)、[实时并发移动端](screenshots/upstream-concurrency-mobile.png)

兼容服务最终补丁在固定上游提交 `c77b1369` 上通过应用检查、九个相关包测试、并发与分配竞态测试及执行器安全回归。最终镜像构建及 `scripts/test-sidecar-image.sh` 隔离冒烟通过，包括内部并发接口认证、匿名拒绝和采样格式。

- 补丁 SHA256：`cad1f15d9d14a85416cb6599ca85b42d97453d1903898f90979878d39f20f714`
- 兼容服务镜像：`codex-gateway-compat:v7.2.150-c77b1369-cad1f15d9d14a854-codex-only`
- gateway 最终验证镜像：`codex-gateway-gateway:v0.0.0-information-validation`，仅供本次本地验证；两项最终镜像通过隔离副本的 `scripts/compose.sh build gateway codex-compat` 构建。

当前工作目录缺少 Cloudflare Tunnel token，因此未改动现有配置；`scripts/validate-compose.sh` 在隔离副本、示例配置及一次性凭证下通过全部 Compose、镜像锁、网络、安全和代理校验。未启动或更新部署服务，也未对现有数据库执行历史清理。上线时应按既有发布流程为 gateway 生成不可变版本标签，并配套更新上述兼容服务镜像和两项迁移。
