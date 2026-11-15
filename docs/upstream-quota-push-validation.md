# 上游账号额度推送验证

## 背景

额度守卫原本每 10–60 分钟才读一次 CPA 的被动观测（见
[剩余用量保险额度验证](upstream-quota-guard-validation.md)）。繁忙账号可能在两次读取之间
把保险额度用穿，直到撞上上游的硬上限。

CPA 其实已经把每次上游尝试的完整响应头放进用量记录（`UsageRecord.ResponseHeaders`），
交给插件的 `usage.handle`。来源是 `RecordAPIResponseMetadata` → `logging.SetResponseHeaders`；
WebSocket 的 `codex.rate_limits` 事件经 `MergeResponseHeaders` 并入同一份头。插件原先只用
这份记录判断失败。现在插件在内存里读出限额头，额度变化时立即通知 Gateway。

## 行为

- **插件**（`deploy/codex-compat/plugin/internal/gatewayplugin/quotapush.go`）
  - 账号取自插件自己的分配记录（按 `SelectionID` 查找），不采信用量记录里的 provider
    和凭证字段。成功和失败的尝试都会读取。
  - 取头规则与 CPA 记录被动观测时相同：Codex 只取 `X-Codex-*`，Claude 只取
    `Anthropic-Ratelimit-Unified-*`；每个头取最后一个值并去掉首尾空白，值不超过 512 字节、
    不含控制字符，最多 64 个。超过 64 个时优先保留默认限额的窗口。
  - 归一复用现有的 `normalizeCodexSignals` 与 `anthropicSnapshot`，请求体与额度查询路由
    的回答形状相同。不构成合法快照的头不推送，由定时检查读取 CPA 记录并给出固定错误码。
  - 只有变化才推送：任一窗口的整数用量、窗口长度、状态或达到上限标记改变，或重置时间
    相差超过 60 秒。
  - 每个账号只保留最新一份待发报告；与正在发送或等待发送的报告相同的，不再排队。由一个
    后台 goroutine 依次发送，单次受 `CallbackTimeout` 限制。处理用量记录本身不做 I/O，因为 CPA 的用量分发是单个队列加反压。
  - 只有 204 算送达。失败后该账号 1 分钟内不再发送，并忘掉“已送达”的记录，之后有新
    记录时重发。
- **Gateway**（`internal/server/upstream_quota_push.go`）
  - 路由为 `POST /internal/upstream-accounts/quota-observed`（Codex）和
    `POST /internal/anthropic-accounts/quota-observed`（Claude）。Antigravity 没有这条路由。
  - 鉴权与其他插件回调相同（`SIDECAR_API_KEY`），并要求 JSON、无查询参数、不超过 16 KiB、
    无重复键或多余字段。
  - Codex 请求体经 `proxy.DecodeObservedUpstreamQuota` 校验，该函数与 Owner 额度查询共用
    同一校验，且必须带 `observed_at`。Claude 窗口经 `anthropicAccountUsage` 校验，同名窗口
    不能重复。观测时间不能早于 2000 年，也不能晚于当前时间 5 分钟以上。
  - 没有 5 小时或周窗口用量的报告返回 204，不写库。
  - 写入方式与 Owner 查询相同（非自动读数），另加 `RequireNewer`：观测时间不比已存的新就
    原样返回，不写库。自动状态当即按规则判定，状态转变照常审计（`via: "observation"`）；
    手动状态保留到下一次定时检查，期间只更新快照。
  - 写入后按新的下次检查时间重排进程内计划。每次推送本身不写审计。

## 安全影响

- 不新增供应商请求，也不改变 CPA 构造请求的方式。插件只读 CPA 已经收到的响应头。
- 推送内容只有账号 ID、限额数值、重置时间和观测时间。套餐类型、额度余额、限额显示名和
  `Retry-After` 都不会发出；golden 测试检查了这一点。
- 新接口只接受插件的内部密钥；账号必须是该 provider 已同步的账号，否则返回 404。
- 写入次数与请求量无关：每个窗口周期最多约 100 次整数变化，加上状态和重置时间的变化。
- 失败时不放行：推送失败只会让守卫退回定时检查，不会把账号判为可用。

## 兼容性

- 新插件配旧 Gateway：推送收到 404，按账号退避 1 分钟，定时检查照常。
- 旧插件配新 Gateway：没有推送，行为与之前相同。
- 不需要迁移，不改插件版本号，也不改 `plugin.lock.json`。

## 验证

2026-11-15 在本机使用 Go 1.26.11 和一次性 PostgreSQL 16 完成。插件 e2e 所用的 CPA
二进制在本机按 `plugin.Dockerfile` 的步骤构建：取 `v8.0.69` 的固定提交 `f7ae8fee`，
打上 residual 补丁，安全依赖升到下限版本；插件以 `-buildmode=c-shared` 针对同一份源码构建。

```sh
gofmt -l .                                                   # 无输出
go vet ./... && go vet -tags integration ./internal/store ./internal/server
go test -count=1 ./...
go test -race -count=1 ./internal/server ./internal/store ./internal/proxy
TEST_DATABASE_URL='postgres://…' go test -count=1 -tags=integration ./internal/store ./internal/server
(cd deploy/codex-compat/plugin && go vet ./... && go vet -tags e2e ./... && go test -race -count=1 ./...)
CPA_E2E_BINARY=… CPA_E2E_PLUGIN_DIR=… go test -tags e2e -count=1 -v ./e2e   # 在插件目录，17 个全部 PASS，无 SKIP
python3 -m unittest discover -s scripts/tests -p 'test_*.py'                 # 100 个，1 个跳过
scripts/ci-go-tool.sh github.com/securego/gosec/v2/cmd/gosec@v2.22.8 -- -exclude-dir=deploy/codex-compat/plugin ./...   # 0 issues
(cd deploy/codex-compat/plugin && ../../../scripts/ci-go-tool.sh github.com/securego/gosec/v2/cmd/gosec@v2.22.8 -- -exclude-generated ./...)   # 0 issues
```

### 插件

| 测试 | 覆盖 |
|---|---|
| `TestQuotaSignalsFromHeaders` | 大小写、取最后一个值、控制字符和超长值被丢弃、另一 provider 的头被忽略、Antigravity 无信号；截断到 64 个时保留默认窗口 |
| `TestQuotaPushCodexOnlyOnChange` | 经真实分配后推送一次，信封与额度路由一致；30 秒后同样的用量（重置时间随时钟漂移）不再推送；用量 +1（包括失败的尝试）推送；进入新周期推送 |
| `TestQuotaPushClaude` | Claude 窗口与授权页面一致；更细的利用率舍入后相同则不推送 |
| `TestQuotaPushIgnoresRecordsWithoutQuota` | Antigravity、未知或缺失 selection、没有头、没有限额头、不合法快照都不推送；用量记录自称的 provider 不被采信 |
| `TestQuotaPushBacksOffAfterAFailedDelivery` | 失败后 1 分钟内不发送；退避结束后重发同样的额度 |
| `TestQuotaPushKeepsOnlyTheNewestReportWhileDelivering` | 发送被阻塞时用量处理立即返回；与正在发送的报告相同的不排队；期间多次变化只发送最新一份 |
| `TestGatewayClientReportQuota` | 路径、鉴权和请求体；只有 204 算送达；Antigravity 和形状不符的报告不发请求 |
| `TestQuotaPushBodiesMatchGolden` | 固定两份请求体（`testdata/quota_push_{codex,claude}.json`），并确认不含套餐、限额显示名和 `Retry-After` |
| `TestQuotaPush`（e2e） | 真实 CPA 下，一次 Codex 请求后收到一次推送（30%/55%，重置时间正确）；同样的第二次请求不再推送；Claude 请求推送 5 小时和 7 天窗口；不含令牌和套餐；供应商只收到三次客户端请求 |

另做了变异检查：把重置时间容差改为 0，`TestQuotaPushCodexOnlyOnChange` 失败；不记录正在
发送的报告，`TestQuotaPushKeepsOnlyTheNewestReportWhileDelivering` 失败。

### Gateway

| 测试 | 覆盖 |
|---|---|
| `TestObservedUpstreamQuotaRecordsCodexReport` | 非自动、`RequireNewer` 写入；观测时间与窗口正确；重排进程内计划；没有 5 小时或周窗口时不写库 |
| `TestObservedUpstreamQuotaRecordsClaudeReport` | Claude 窗口四舍五入写入；没有用量的窗口不写库 |
| `TestObservedUpstreamQuotaRejectsInvalidReports` | 错误鉴权返回 401；空体、多余字段、重复键、非法账号、缺少或超出范围的观测时间、错误信封、百分比越界、provider 与路径不符、重复或未知的 Claude 窗口、查询参数、非 JSON、超长都返回 400；Antigravity 路径不处理；以上都不写库 |
| `TestObservedUpstreamQuotaStoreErrors` | 账号不存在返回 404，非法返回 400，其他错误返回 500 且不泄露原因；失败时不重排计划；没有守卫仓库时返回 503 |
| `TestObservedUpstreamQuotaAcceptsPluginGolden` | 读取插件的两份 golden 请求体并成功写入 |
| `TestDecodeObservedUpstreamQuota` | 解码器接受插件格式（含微秒），拒绝缺少或未来的观测时间、多余字段、重复键、多个值、主限额不一致和 provider 不符 |
| `TestUpstreamQuotaGuardPostgresIntegration`（新增部分） | 相同或更早的观测不改行、不审计；更新的观测越过保险额度时立即受限并审计；手动状态下推送只更新快照 |
| `TestObservedUpstreamQuotaPostgresIntegration` | 经真实路由和数据表：首个报告写入；97% 立即受限、恢复时间为重置后 5 分钟、审计 `via: observation`；迟到的 10% 报告不改变状态；Claude 报告写到 Claude 账号；Codex 路径不能写 Claude 账号；未知账号返回 404 |

另做了变异检查：去掉 `RequireNewer` 的判断，Postgres 集成测试失败。

### 未运行

- `plugin.Dockerfile` 的镜像构建：本机没有 Docker 守护进程。构建内容已按上面的步骤
  逐项在本机复现；`TestQuotaPush` 已加入镜像构建必须通过的 e2e 列表。
- `govulncheck`：本机的 `scripts/ci-go-tool.sh` 用 Go 1.24 构建出的工具无法加载 Go 1.26
  模块。本改动没有新增依赖。
- 本会话的环境设置了 `ANTHROPIC_BASE_URL` 等变量，`TestClientConfigurator` 需在去掉这些
  变量后运行。基线提交也是如此，与本改动无关。
