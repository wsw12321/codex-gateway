# CPA 插件迁移验证记录

本文记录把 `codex-compat` 从整份源码补丁迁移到 CPA 官方插件的验证过程。迁移目标、
架构与取舍见 [兼容层升级规程](compatibility-upgrades.md) 的“插件模式”一节。
插件模式自路线图第 8 步起是默认部署，源码补丁镜像保留为回退（`CPA_INTEGRATION=patch`）。

## 阶段 0：可行性验证

日期 2026-11-11。对象是未修改的 CLIProxyAPI `v8.0.59`
（`c5251b57b3fa5dcbc689f514f4aa166daacb4481`），以及只加了拒绝选择器的同一版本。
方法：一次性探针插件（Go，`-buildmode=c-shared`）记录每个钩子收到的内容；假上游
返回固定的 Responses SSE 或指定错误；两个明文正向代理用于观察出口。探针和假上游
不进入仓库。全部在本机回环地址完成，未使用真实账号。

| 项 | 结论 | 观察 |
|---|---|---|
| 调度请求可见入站头 | 通过 | `scheduler.pick` 的 `Options.Headers` 含全部 `X-Codex-Gateway-*` 头 |
| 调度请求可见会话元数据 | 通过 | `caller_scope`、`canonical_session_id`、`selection_id`、`request_path`；有父会话时含 `parent_session_id` |
| 候选集已排除试过的账号 | 通过 | 首个账号 429 后，第二次调度的候选不含它；处于冷却的账号也不在候选中 |
| FrontendAuth 的 Principal 进入 `caller_scope` | 通过 | 不同 Principal 得到不同 `caller_scope`；管理密钥不能访问数据面，数据面密钥不能访问管理面 |
| 钩子之间可关联 | 通过 | before-auth 与 `request.complete` 的 `RequestID` 相同；`trace_id` 贯穿拦截、终态与 usage；usage 带 `selection_id`；after-auth 元数据带 `selected_auth_id` |
| usage 插件收到每次失败 | 通过 | `usage-statistics-enabled: false` 时仍收到；失败记录含状态码与上游正文（`usage_limit_reached` 可识别） |
| 终态回调 | 通过 | 成功、失败、客户端断开（`canceled`）各一次 |
| 所有生成端点都经过调度钩子 | 通过 | `/v1/responses`（流式与非流式）、`/v1/responses/compact`、`/v1/messages`、`/v1beta/models/*`、Claude 模型走 `/v1/responses` |
| 内部头不到达上游 | 通过 | 上游只收到执行器自己构造的头；任意自定义头与 Cookie 均未转发 |
| 凭证级代理 | 通过 | 配置项的 `proxy-url` 生效；原生 `PATCH /v0/management/auth-files/fields` 能把 `proxy_url` 写入 OAuth 凭证文件 |
| 管理路由 | 通过 | 插件路由位于 `/v0/management/<path>`；无密钥 401；插件内可经回环调用原生管理接口 |
| 宿主回调 | 通过 | `host.auth.list`、`host.auth.get` 不需要回调上下文，在 `scheduler.pick` 内也可调用；`host.auth.get` 返回完整凭证 JSON |
| 插件加载 | 通过 | 文件名 `<id>-v<版本>.so`，放在插件目录或其 `<os>/<arch>` 子目录 |
| 双 Go 运行时稳定性 | 通过 | 1500 个流式请求（150 并发，每个约 400 个分片）全部 200，各钩子各调用 1500 次，进程常驻内存约 108 MB |
| 原版在插件未处理时回退轮询 | 确认存在 | 插件返回未知账号或“未处理”时，原版改用内置轮询并成功调用上游 |
| 拒绝选择器 | 通过 | `routing.strategy: "plugin-only"` 下，未知账号、未处理、插件在配置中禁用、插件文件缺失均返回 503，上游零调用 |

与计划不一致的发现：

- **插件内 panic 不会被宿主熔断，而是整个 CPA 进程退出。** 宿主的 `recover` 接不住
  另一个 Go 运行时里的 panic。因此插件必须在自己的 ABI 入口和每个后台协程里
  `recover`，并把它转成错误信封。进程退出本身是拒绝服务而非放行，但会中断所有
  进行中的请求。
- **`Reject` 的渲染不能当契约用。** 调度插件用 `Reject` 拒绝时，CPA 返回 HTTP 500，
  错误码被拼进 message（`upstream_concurrency_exceeded: ...`）。用错误信封的
  `http_status` 可以控制状态码，但响应体的 `type`/`code` 仍由 CPA 决定。这印证了
  用 Gateway 请求账本传递拒绝原因的设计。
- **原生换号规则与补丁不完全相同。** 实测 429、500、403 会换下一个账号，400 直接
  返回。403 在补丁规则里属于不重放的“其他 4xx”。可用原生的
  `oauth-request-scoped-errors` 调整，见取舍清单第 1 项。
- **CPA 启动时会联网查询客户端版本号**（与是否启用插件无关）。生产环境由出口
  白名单拦截。

本阶段没有验证的内容：真实 OAuth 凭证经凭证级代理的推理、刷新与额度查询；v8
OAuth 会话级代理参数；Provider 缓存键是否全部按 `caller_scope` 隔离（需要逐个
阅读源码）。这些留到对应阶段，并在真实账号验收中覆盖。

## 实现与验证

阶段 0 之后按“插件优先、残余补丁兜底”实现，分为以下提交：

| 内容 | 主要文件 |
|---|---|
| 插件核心：选号、可信头、会话绑定、并发名额、账号控制状态、稳定账号 ID、终态与用量处理 | `deploy/codex-compat/plugin/` |
| 残余补丁：`routing.strategy: "plugin-only"` | `deploy/codex-compat/residual/` |
| Gateway 请求账本与 `POST /internal/*-accounts/attempt` | `internal/proxy/ledger.go`、`internal/server/upstream_attempt.go` |
| 插件账号路由；Gateway 在插件模式下改走 `/v0/management/gateway/*` | `management.go`、`internal/proxy/plugin_management.go` |
| 构建、入口、健康检查、Compose 覆盖文件、CI | `plugin.Dockerfile`、`plugin-entrypoint.sh`、`deploy/cpa-plugin.override.yml` |
| 账号独立出口 | `egress.go`、`internal/server/plugin_egress.go` |

### 自动化测试

| 层次 | 内容 | 结果 |
|---|---|---|
| 插件单测（含 `-race`） | 可信头与账号池解析、Gateway 回调契约、资金层顺序、授权变化后解绑、按调用方隔离、子代理跟随父会话、名额竞争、持有与释放、失败切换、额度锁与旧结果、控制文件格式与写入失败、身份算法、管理路由、出口解析与改写、panic 转拒绝 | 通过 |
| 端到端（9 组） | 在真实 CPA 进程中加载插件；OAuth 凭证文件、假 Gateway、经 TLS 拦截代理的假上游 | 通过 |
| Gateway 单测与 `-race` | 账本归因、拒绝原因映射、伪造与越界上报、管理路由映射与密钥、出口推送、配置校验；原有全部测试 | 通过 |
| 部署静态校验 | 锁文件、残余补丁、Dockerfile、入口脚本、覆盖文件之间的一致性 | 通过 |
| 镜像探针（本地模拟容器挂载） | 插件只读、两把密钥互不通用、刷新锁独占、三个命名空间路由、账号控制持久化、无 Gateway 时拒绝、临时目录 `noexec` 时不健康且拒绝 | 通过 |
| gosec、govulncheck | Gateway 与插件 module | 见下 |

端到端的 9 组：

| 测试 | 覆盖 |
|---|---|
| `TestAllocation` | 采用 Gateway 的选择并上报；内部头不到上游；绑定会话复用；两个调用方同一 session id 互不影响；无权限用户 503；权限缩小账号池；群组账号池；不可信请求被拒 |
| `TestServerErrorMovesToNextAuthorizedAccount` | 500 后换账号，最后一次上报为实际账号，会话改绑到新账号 |
| `TestRequestErrorIsNotReplayed` | 400 不换账号 |
| `TestUsageLimitLocksAccountDurably` | `usage_limit_reached` 写入额度锁，`0600`，重启后仍生效 |
| `TestConcurrencyLimit` | 名额已满时 429 且上报原因；客户端断开后释放 |
| `TestFailsClosed` | Gateway 不可达、插件被禁用、控制文件损坏、凭证无身份，全部 503 且上游零调用 |
| `TestPerAccountEgress` | 上传凭证自带的代理被清除后才使用；推送绑定后经账号出口并携带路由凭据；未推送的改绑在下一次选号时改写；重启后保持；路由无法确认时零调用；解绑回到全局出口；数据面密钥不能改出口 |
| `TestAccountManagement` | 管理路由鉴权；账号列表字段与脱敏；禁用立即生效且跨重启；重新启用；错误请求；并发快照 |
| `TestManagementBoundary` | 无密钥或数据面密钥不能访问 CPA 管理 API |

用未打残余补丁的原版 CPA 运行 `TestFailsClosed` 时，“插件被禁用”一项失败：原版改用
内置轮询并成功调用了上游。这说明该测试能发现放行，也说明残余补丁是必要的。

### 对后续上游版本的实测

| 上游版本 | 残余补丁 | 插件 | 端到端 |
|---|---|---|---|
| v8.0.59（固定版本） | 无 fuzz 应用 | 编译通过 | 9/9 |
| `d1ea84f`（2026-11-10，v8.0.64 之后） | 第一版无 fuzz 应用 | 未修改，编译通过 | 9/9 |
| v8.0.67（2026-11-11） | 第一版失败，调整插入点后的现行版本无 fuzz 应用 | 未修改，编译通过 | 9/9 |

v8.0.59 到 v8.0.67 之间上游改了 289 个文件。插件接口在此区间新增了错误分类字段（向后
兼容），插件无需改动。

### 过程中的发现

- **CPA 从临时目录下的私有副本加载插件。** sidecar 的 `/tmp` 是 16 MB 的 `noexec`
  tmpfs，插件在那里无法加载。覆盖文件为此另挂一个可执行 tmpfs 并设置 `TMPDIR`，威胁
  模型已记录。镜像探针分别验证了有、无该目录的两种行为。
- **构建中的依赖升级可能降级。** 用 `go get 模块@版本` 固定安全版本时，若上游已经要求
  更高版本就会被降级。构建步骤改为只在低于下限时升级，并在之后核对下限。
- **gosec 的两类误报。** 插件 module 内 gosec 会分析 cgo 生成的包装代码，因此该 module
  的扫描使用 `-exclude-generated`；C 边界的两处 `unsafe` 调用加了说明性的 `#nosec`。
  Gateway 中一个名字含 “Token” 的头常量被误判为硬编码凭据，已改名。
- **govulncheck 报告 GO-2026-6859**（`net/http`，Go 1.26.10 中存在，1.26.11 修复）。
  Gateway module 在本次改动之前同样命中，属于既有问题，需要单独升级 Go 工具链处理，
  本次未改。
- **本地沙箱的一个既有测试失败。** `TestClientConfigurator` 在设置了
  `ANTHROPIC_BASE_URL` 等变量的环境中失败，改动前的提交上同样失败，与本次无关。

### 未验证的内容

- **镜像没有在本地构建过。** 本地环境没有 Docker 守护进程。`plugin.Dockerfile` 的每一步
  都在同一 CPA 提交上逐条复现过（含发布用的构建参数），入口脚本、健康检查和镜像探针
  在模拟容器挂载的命名空间中以服务用户运行过；实际镜像构建、Trivy 扫描和
  `scripts/test-cpa-plugin-image.sh` 只在 CI 中运行。
- **没有使用真实账号。** 真实 OAuth 凭证经账号出口的推理、刷新和模型列表，Claude 与
  Antigravity 的真实流量，以及 Gemini 原生请求，都需要按运维手册在维护窗口验收。
- **Provider 缓存键尚未逐个核对。** 会话绑定和会话哈希已验证按调用方隔离；CPA 内部
  各 Provider 的缓存是否都使用调用方作用域，需要逐个阅读源码确认。
- **尚未迁移：** 上游额度查询、模型鉴定、“CPA 授权管理”页面的凭证操作（含先选出口
  再授权的登记流程）。

后续步骤与验收标准见 [CPA 插件模式路线图](cpa-plugin-roadmap.md)。

## 路线图第 1 步：Go 1.26.11

日期 2026-11-12。按 `CLAUDE.md` 的 “Go or base-image version bump” 清单，把 `go.mod`
的 `toolchain`、CI 的 `GO_VERSION`、`deploy/images.sources`、六个 Dockerfile 的
`ARG GOLANG_IMAGE=`、两份 CPA 锁文件与 Mihomo 源码锁中的 Go 版本统一改为 1.26.11，
`deploy/images.lock.env` 由 `scripts/lock-images.sh` 重新生成。golang 镜像锁定为
`golang:1.26.11-bookworm@sha256:dc40a13b…98fc0`（2026-11-10 发布）。生成前
`lock-images.sh --check` 确认其余九个镜像的 digest 与仓库一致，所以锁文件只变了
Go 两行。`scripts/validate-compose.sh` 中对 CPA 源码锁 `go_version` 的断言同步更新。
插件 `go.mod` 的 `go` 指令表示最低版本，仍为 1.26.10，构建使用的工具链由 CI 和
镜像决定。

### 结果

| 检查 | Go 1.26.10（改动前） | Go 1.26.11 |
|---|---|---|
| govulncheck v1.1.4，Gateway module | GO-2026-6859（`net/http`） | 无发现 |
| govulncheck v1.1.4，插件 module | GO-2026-6859（`net/http`） | 无发现 |

在 Go 1.26.11 下还通过了：

- Gateway：`go build`、`gofmt -l .` 无输出、`go vet ./...`、`go test -count=1 ./...`、
  `go test -race -count=1 ./...`。
- 插件 module：`go vet ./...`、`go test -race -count=1 ./...`。
- gosec v2.22.8：两个 module 均为 0 个问题。go-licenses v1.6.0 许可证检查通过。
- `scripts/lock-images.sh --check`；在本地按原样执行 CI 的 “Verify Go toolchain sources
  agree” 步骤。
- 176 项部署与脚本测试（含 `test_cpa_plugin_deploy`），175 项通过，1 项按环境条件跳过。
- 在一份隔离副本中用合成配置运行 `validate-compose.sh`，三种出口模式的静态策略检查都
  通过；`compose.sh config --quiet` 通过。脚本最后用 `docker run` 做的容器探针需要
  Docker 守护进程，本地无法运行。

`TestClientConfigurator` 仍只在设置了 `ANTHROPIC_BASE_URL` 与
`CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` 的沙箱中失败，去掉这两个变量后通过，与上文
记录的既有情况相同。

### 发现：仓库级 gosec 扫描会失败

插件 module 加入后，CI 中从仓库根目录运行的 gosec 会失败：gosec 按目录而不是按
module 遍历，在根 module 中无法解析插件 module 的依赖。它报告类型错误，并在问题数为
0 的情况下以退出码 1 结束。改动前的提交上同样如此。这条分支没有开 PR，CI 也不对它的
push 运行，所以此前没有暴露。

根目录的扫描改为 `-exclude-dir=deploy/codex-compat/plugin`，插件仍由它自己 module 内的
步骤扫描。CI 注释原来说 C ABI 薄层（插件的 `main.go`）由根目录的扫描覆盖，这并不
成立：根目录扫描在类型错误下无法通过；在 module 内，cgo 把 `main.go` 改写成生成的
文件，`-exclude-generated` 会跳过它，不加这个参数时 gosec 只对 cgo 自己的包装代码
报告误报（G115）。注释已据实改写。薄层中两处长度转换都受 `maxMessageBytes`
（64 MiB）约束，薄层应保持最小。

### 未验证

镜像构建、Trivy 和镜像探针只在 CI 中运行（本地没有 Docker 守护进程），属于路线图
第 2 步。

## 路线图第 2 步：插件镜像首次通过 CI

日期 2026-11-12。PR #4 在提交 `0a3df3e` 上的
[CI 运行 43137551706](https://github.com/wsw12321/codex-gateway/actions/runs/43137551706)
第一次就全部通过：`go-test`、`go-security`、`supply-chain`、`containers`、
`postgres-integration`（`publish-images` 只在 `main` 上运行，PR 中跳过）。路线图列出的
几处只有 CI 能暴露的问题，日志中的结果如下：

| 项 | 结果 |
|---|---|
| 构建阶段在 `golang:1.26.11-bookworm` 内的测试 | CPA 的 `internal/pluginhost`、`sdk/pluginapi`、`sdk/pluginabi` 测试通过；插件 `go vet`（含 `-tags e2e`）与 `go test -race` 通过 |
| 端到端 | 9 个测试按名字逐一断言为 PASS，无 SKIP；只用回环网络 |
| 插件文件的属主与权限 | 运行阶段以 `--chown=0:0 --chmod=0444` 复制 `codex-gateway-v1.0.0.so` |
| `scripts/test-cpa-plugin-image.sh` | 两行通过信息都已输出：完整性、密钥隔离、刷新锁、三个命名空间路由、账号控制持久化与拒绝检查通过；无可执行临时目录时插件不加载、容器不健康、生成请求被拒绝 |
| Trivy（`--ignore-unfixed --severity HIGH,CRITICAL`） | 插件镜像的 Debian 12.15 系统包与 `cli-proxy-api` 均为 0；Gateway、补丁镜像、Bridge、账号出口镜像同为 0 |
| 根目录 gosec（排除插件目录）与插件 module 的 gosec、govulncheck | 通过 |

Trivy 的结果中没有插件 `.so`，Go 二进制的检测只列出了 `cli-proxy-api`。插件的依赖由
`go-security` 中插件 module 的 govulncheck 覆盖。

## 路线图第 4 步：额度查询

日期 2026-11-12。插件新增四个管理路由，按补丁镜像的格式提供 Gateway 用到的全部额度
查询，Gateway 不需要改动（路径映射在插件模式引入时已就位）。设计与取舍见
[兼容层升级规程](compatibility-upgrades.md#额度查询)，行为差异见同文第 14–17 项。

### 评估：为什么不用 `QuotaProvider`

- CPA 的 `POST /v0/management/quota/fetch` 输出 `QuotaFetchResponse`（剩余比例、窗口名），
  装不下 Gateway 严格解析的 `account/rateLimits/read` 信封（整数百分比、窗口分钟数、
  `rateLimitReachedType`、限额 ID）。
- 该接口出错时把插件返回的错误文本拼进响应，并以错误级别写日志。
- 它交给插件的宿主 HTTP 客户端会跟随重定向（跨源时去掉凭证头），而补丁镜像的规则是
  不跟随。宿主的 `host.http.do` 回调又只能使用全局代理，无法指定账号出口。
- 注册为 `codex`、`claude` 的 `QuotaProvider` 还会改变 CPA 自己管理页面的行为。

所以查询由插件的管理路由自己发出：先向 Gateway 确认路由，用独立的传输层只连该代理，
拒绝重定向。

### 测试

插件单测（`internal/gatewayplugin/quota_test.go`，含 `-race`）：

| 项 | 覆盖 |
|---|---|
| 请求解码 | 与补丁镜像一致：只接受 `{"method":"account/rateLimits/read","id":6}`；空、`null`、数组、方法或 id 错误、`6.0`、`6e0`、字符串 id、`params`、未知字段、尾随 JSON、重复键、大小写不同、缺字段一律 400；超过 256 字节 413 |
| 归一化 | 黄金文件 `testdata/codex_quota_rpc.json` 固定输出；24 种合法与非法形状逐一对应补丁镜像的错误码；只报告已知的 `rateLimitReachedType` |
| 出口 | 未绑定走全局出口，绑定走账号路由；全局出口未配置或路由无法确认时零上游调用 |
| 请求内容 | URL、方法固定；请求头恰好三个，令牌只在 `Authorization` |
| 错误映射 | 23 种情况：账号不存在、参数多余、注册表不可用、凭证被禁用、缺少或含控制字符的令牌、凭证文件被换成其他账号、传输失败与超时、401/403/429/408/504/500/302、超大响应、结构变化；上游正文和令牌从不出现在响应中 |
| 传输层 | 真实的 `httptest` 代理：只经给定代理（忽略 `HTTP_PROXY`/`HTTPS_PROXY`），携带路由凭据，302 不跟随；非法代理地址被拒绝 |
| 授权页面 | Codex 门面换算剩余比例并排序；Antigravity 只保留合法的 `gemini-*` 模型，丢弃不合法的重置时间，回滚期间不触碰 Google 凭证；Claude 取最新的凭证或模型观测，过期冷却不输出 |
| 回环读取 | 只按 `auth_index` 查询，只认 `id` 与 `auth_index` 都匹配的条目；错误状态、格式错误、缺管理密钥都视为不可用 |

Gateway（`internal/proxy/plugin_management_test.go`）：`TestPluginQuotaContract` 用插件的
黄金文件喂给 Gateway 的严格解析器，检查请求走 `/v0/management/gateway/upstream-accounts/quota?id=`
并使用管理密钥；插件的固定错误码映射到与补丁镜像相同的 Gateway 错误码。

端到端（真实 CPA 进程，新增 2 组，共 11 组）：

| 测试 | 覆盖 |
|---|---|
| `TestQuotaQuery` | 未绑定账号经全局出口查询，绑定账号经账号出口并携带路由凭据；返回值正确，计划类型和邮箱不外泄；内部请求头不到上游；401、302（不跟随）、结构变化、小数百分比的固定错误码；路由无法确认时零上游调用；无密钥或数据面密钥被拒；授权页面的 Codex 额度 |
| `TestClaudePassiveQuota` | 请求前返回空窗口；一次经 CPA 的 Claude 请求后，CPA 记录的 `anthropic-ratelimit-unified-*` 响应头经管理 API 回读为 5 小时与 7 天窗口；不含令牌和邮箱；Codex 账号不能当作 Claude 查询 |

本地按 `plugin.Dockerfile` 的步骤在固定 CPA 提交上应用残余补丁、编译 CPA 和插件，11 组
端到端测试全部通过。镜像中的同一步骤由 CI 运行，构建会逐一断言这两个新测试名。

### 未验证

- chatgpt.com 是否接受插件的 Go 标准 TLS 客户端（补丁镜像用 CPA 的浏览器指纹客户端），
  以及 Antigravity 的固定客户端版本号是否被接受。两者都要在真实账号验收中确认，已加入
  第 3 步的验收清单。

## 路线图第 7 步（出口部分）：账号出口不回退

日期 2026-11-12。按[路线图的原则 3](cpa-plugin-roadmap.md#原则)，不改 CPA，靠配置和网络
保证账号出口失败时请求失败。

### 依据：CPA 的代理退路

在固定提交 `c5251b57` 的 CPA 源码中确认：

- `internal/runtime/executor/helps/proxy_helpers.go` 的 `NewProxyAwareHTTPClient` 按
  “请求级代理 → 凭证 `proxy_url` → 全局 `proxy-url`”取代理；构建失败时退到上下文传输层，
  再退到带环境代理的默认传输层。
- 管理接口 `api-call` 的 `apiCallTransport` 依次尝试凭证代理、全局代理，最后直连。
- Codex、Claude、Antigravity 的令牌刷新与 Antigravity 模型探测都使用凭证的 `proxy_url`，
  凭证没有代理时使用全局 `proxy-url`。

插件模式原来同时配置了全局 `proxy-url` 和 `HTTPS_PROXY`（都是 Squid 3128），所以上述
退路可以落到全局出口。

### 改动

- 插件把 Gateway 的“继承全局出口”也写成凭证上的显式 `proxy_url`
  （`CPA_PLUGIN_GLOBAL_PROXY_URL`，现为必填，只接受 `http://主机:端口`，且不能是账号出口
  监听）。没有全局出口配置时拒绝把账号留在空代理上。
- 插件后台对账：注册后每 2 秒一轮直到确认全部凭证，之后每分钟一轮；停机时停止。
- 入口脚本生成的配置去掉 `proxy-url`，并在启动 CPA 前清除 `HTTP_PROXY`、`HTTPS_PROXY`、
  `ALL_PROXY` 及小写形式；覆盖文件同样把它们置空。
- Gateway 侧不需要改动：出口变更时的即时推送保留，推送失败由插件的每次选号检查和后台
  对账兜底。

### 测试

插件单测（含 `-race`）：

| 测试 | 覆盖 |
|---|---|
| `TestLoadConfig` | 全局出口必填；拒绝缺失、等于账号出口监听、带凭据、SOCKS；对账间隔 10–3600 秒 |
| `TestPickReplacesAProxyTheGatewayDidNotBind` | 预置代理与空代理都被替换为显式全局出口，替换前本次跳过该账号 |
| `TestPickRefusesTheGlobalRouteWithoutAGlobalEgress` | 没有全局出口配置时不写空代理、不使用账号 |
| `TestReconcileWritesEveryCredentialsRoute` | 一轮对账写入绑定路由与全局出口；第二轮无写入；路由无法确认的凭证保持原样并计为失败 |
| `TestBackgroundReconcileFollowsTheLifecycle` | 注册启动后台对账（重复注册不重复启动），停机后停止 |

端到端（真实 CPA 进程，新增 1 组，共 12 组）。harness 生成的 CPA 配置与入口脚本一致，
不含 `proxy-url`，进程环境中没有代理变量；启动后等待插件的首轮对账写好全部凭证：

| 测试 | 覆盖 |
|---|---|
| `TestPerAccountEgress` | 启动后无代理和预置代理的凭证都带上显式全局出口；其余绑定、重新绑定、重启、路由无法确认、解绑场景不变，解绑后凭证回到显式全局出口 |
| `TestDeadEgressNeverFallsBack` | 账号绑定到不可用的出口节点（账号出口监听拒绝 CONNECT）：只授权该账号时请求失败（5xx），该账号的请求从未经全局出口或其他途径到达上游；另一个账号有权限时由它经自己的出口完成请求 |

部署静态校验：生成的配置中没有 `proxy-url`；入口脚本在 `exec` 前清除代理变量，全局出口只
接受 `http://`；覆盖文件把三个代理变量置空。

本地按 `plugin.Dockerfile` 的步骤（固定提交、残余补丁、`CGO_ENABLED=1` 编译 CPA 与插件）
运行端到端测试，12 组全部通过。

### 未验证

- 生产网络中 `compat_internal` 拒绝直连这一前提由 Compose 的 `internal: true` 保证，已在
  第 3 步的验收清单中加入“出口失败不回退”一项，用真实节点确认。
- CPA 自身的后台请求（Antigravity 版本清单、Grok CLI 版本）原来经 Squid 3128，被白名单
  拒绝；现在直连，同样失败，CPA 使用内置的回退版本。

## 路线图第 4 步（改写）：额度改为 CPA 被动观测

日期 2026-11-12。取代上文第 4 步的第一次实现：插件不再向 `chatgpt.com` 和 `cloudcode-pa`
发请求，删除了自带的 HTTP 客户端、写死的 URL 和 User-Agent。依据是
[路线图的原则 1](cpa-plugin-roadmap.md#原则)。

### 依据：CPA 的被动观测

在固定提交 `c5251b57` 的 CPA 源码中确认：`sdk/cliproxy/auth/quota_signals.go` 对
`codex`、`claude`（和 `devin`）把响应中的限额头写入凭证的 `QuotaState.Signals` 与
`ObservedAt`；Codex 认可的头为 `x-codex-*` 中带 `-used-percent`、`-window-minutes`、
`-reset-at`、`-reset-after-seconds`、`-limit-reached`、`-allowed` 等后缀的名字，以及
`x-codex-plan-type`、`x-codex-active-limit`、`x-codex-credits-*`。WebSocket 的
`codex.rate_limits` 事件由 `internal/runtime/executor/helps/codex_quota.go` 归一成同样的头，
额外限额用 `X-Codex-Additional-<名字>-` 前缀。Antigravity 不在被动观测之列。

### 测试

插件单测（含 `-race`）：

| 测试 | 覆盖 |
|---|---|
| `TestNormalizeCodexSignalsMatchesGolden` | 一组 Codex 响应头映射出的信封与第一次实现的黄金文件逐字段一致，只多出 `observed_at`；套餐和限额显示名不输出 |
| `TestNormalizeCodexSignalsShapes` | 20 种合法与非法形状：只有百分比、只有达到限额标志、只有额外限额、WebSocket 命名、小写头名、无观测、只有套餐与余额、缺百分比、越界、非数字、窗口与重置时间越界、负的相对重置；小数向上取整；相对重置以观测时间为起点；两种命名映射到同一 ID；非法名字与 `codex` 同名被略过；额外限额封顶 |
| `TestCodexQuotaFromObservation` | 路由输出与黄金文件一致；取凭证与各模型观测中最新且含 Codex 信号的一次；授权页面换算剩余比例并带观测时间；无观测时 Owner 得到 `quota_not_observed`、授权页面得到空列表 |
| `TestCodexQuotaErrors` | 13 种错误映射，包括观测读取失败（503）、无观测或只有非 Codex 信号（409）、观测值越界（502）、凭证被换成其他账号；响应中不含令牌 |
| `TestAntigravityQuotaIsUnavailable` | 返回 `{"status":"unavailable","quota":[]}`，未知账号 404，多余请求体 400 |

Gateway：`TestPluginQuotaContract` 用新的黄金文件，确认 `observed_at` 被解析；拒绝非时间、
无时区、一小时后的未来、2000 年以前和数字形式的观测时间，接受缺省（补丁镜像）；
`quota_not_observed`、`quota_observation_unavailable` 映射到新的 Gateway 错误码。控制台的
node 测试确认观测快照显示“观测于 …”和“最近观测”，过期计时从观测时间算起；实时查询的
文案不变。

端到端（真实 CPA 进程）：

| 测试 | 覆盖 |
|---|---|
| `TestCodexPassiveQuota` | 请求前 Owner 得到 `quota_not_observed`、授权页面为空；一次经 CPA 的 Codex 请求后，CPA 记录的 `x-codex-*` 头经管理 API 回读为默认限额与 `code_review` 限额，观测时间在一分钟内；授权页面换算剩余比例；读取额度没有产生任何上游请求，上游只收到 CPA 自己的推理请求；越界的观测值得到固定错误码；只有管理密钥能访问 |
| `TestClaudePassiveQuota` | 不变 |

本地按 `plugin.Dockerfile` 的步骤运行端到端测试，12 组全部通过。

### 未验证

- 真实 Codex 响应头的确切集合（例如额外限额的命名、百分比是否带小数）要在真实账号验收中
  确认，已在第 3 步的清单中改为“显示最近一次观测及观测时间”。
- 运行本仓库测试时，`TestClientConfigurator` 在本会话中失败，原因是会话环境自带
  `ANTHROPIC_BASE_URL` 等变量，被配置脚本识别为冲突设置；去掉这两个变量后
  `internal/server` 全部通过。与本次改动无关。

## 路线图第 5 步：凭证操作改用 CPA 原生管理接口

日期 2026-11-12。“CPA 授权管理”页面的新增/重新授权、导入、刷新、删除在插件模式下可用，
每一步都由 CPA 自己的管理接口完成。设计见
[兼容层升级规程](compatibility-upgrades.md#凭证操作)，行为差异见同文第 19–21 项。

### 依据：CPA 的原生接口

在固定提交 `c5251b57` 的 CPA 源码中确认：

- `POST /v8/management/oauth/auth-url` 接受请求体中的 `proxy_url`，经
  `validateOAuthSessionProxyURL` 校验后作为本次登录的出口；换取令牌使用该代理，保存凭证
  时由 `applySessionProxyToAuthRecord` 写入 `proxy_url`。v0 的 `/{provider}-auth-url`
  不接受代理，所以插件用 v8 接口。
- `POST /v0/management/oauth-callback` 接受 `{provider, code, state}`；
  `GET /v0/management/get-auth-status` 完成后返回 `auth_id`。
- `POST /v0/management/oauth/import` 在该版本只支持 Vertex，所以导入用
  `POST /v0/management/auth-files?name=`（原始 JSON）加 `POST /v0/management/auth-files/refresh`。
  刷新接口的响应包含完整凭证，插件读取 `ok` 后丢弃。
- CPA 保存的 Claude 凭证带 `account_uuid` 与 `organization_uuid`（授权与刷新都会写入）；
  Antigravity 授权只记录邮箱与项目，没有 Google subject。

### 测试

插件单测（`credentials_test.go`，含 `-race`）：

| 测试 | 覆盖 |
|---|---|
| `TestLoginStartsInCPAWithTheChosenEgress` | 登记路由与显式全局出口都交给 CPA；外来主机、无路由凭据、SOCKS、未知字段、非法登记号都不到达 CPA；授权 URL 主机不对时拒绝 |
| `TestLoginCompletesWithIdentityAndEgressBinding` | 回调只接受本插件发起、同一 Provider 的 state；CPA 尚未列出凭证时继续等待；完成后身份识别、出口登记、写入提交后的路由；重复轮询不再访问 CPA 与 Gateway |
| `TestLoginFailuresFailClosed` | 无身份的新凭证被删除且不登记出口；出口登记失败时账号被停用；CPA 报错后会话作废；与已有 Claude 凭证同一身份的新凭证被删除 |
| `TestCPAStoredIdentities` | Claude 由两个 UUID 组成身份，已记录的 subject 须一致；Antigravity 以 subject 优先、否则邮箱，邮箱登记在单独文件中，补丁镜像读取的 subject 注册表不受影响；畸形邮箱与 subject 被拒 |
| `TestImportStoresTokensWithTheirEgressAndLetsCPARefresh` | 上传的文件只含 `type`、令牌和出口；CPA 刷新后识别身份并登记出口；响应不含令牌；Antigravity、缺刷新令牌、令牌含空白、未知字段、外来出口被拒；上传失败、刷新失败、刷新后无身份时清理文件 |
| `TestRefreshAndDeleteGoThroughCPA` | 刷新前写入当前路由，路由无法确认时不刷新；删除只针对已停用且无进行中请求的账号，控制状态保留 |
| `TestCredentialRoutesRespectTheAntigravityRollback` | Bridge 回滚期间 Antigravity 的凭证操作不到达 CPA |

Gateway：`TestPluginManagementPath` 覆盖授权、回调、状态、导入、刷新、删除六类路径的映射。

端到端（真实 CPA 进程，新增 1 组，共 13 组）。harness 的 TLS 拦截代理新增假的
`auth.openai.com/oauth/token`：

| 测试 | 覆盖 |
|---|---|
| `TestCredentialOperations` | 导入：CPA 经所选出口（登记路由的凭据）用导入的刷新令牌刷新，凭证最终带提交后的路由，Gateway 收到出口登记，账号随后经该路由服务请求；刷新：成功，CPA 发出的令牌请求都经账号路由；删除：未停用时 409，停用后文件删除、列表中消失；授权：授权 URL 主机与 state 正确，回调前不访问供应商，CPA 经所选出口换取令牌（及随后的刷新），状态返回稳定账号 ID，凭证带提交后的路由；整个过程没有任何请求经全局出口；未知 state 404；数据面密钥无法访问 |

本地按 `plugin.Dockerfile` 的步骤运行端到端测试，13 组全部通过。

### 未验证

- 真实 OpenAI、Anthropic、Google 的授权与令牌端点，以及 Claude 授权码的格式（端到端只
  模拟了 Codex 的令牌端点），在第 3 步的真实账号验收中确认。
- CPA 可能在导入或授权后自行再刷新一次凭证（端到端中观察到），也可能跳过刚刷新过的凭证；
  两者都是 CPA 原生行为，请求都经所选出口。

## 路线图第 6 步：模型鉴定走 CPA 的普通请求路径

日期 2026-11-12。插件模式下的模型鉴定可用。探针是普通的 `/v1/responses` 请求，由 CPA
生成上游请求；Gateway 用一次性身份把它固定到被鉴定的账号。设计见
[兼容层升级规程](compatibility-upgrades.md#模型鉴定)，行为差异见同文第 22 项。

### 测试

Gateway：

| 测试 | 覆盖 |
|---|---|
| `TestModelIdentificationProbeThroughCPA` | 探针是带一次性身份、独立调用方作用域和请求令牌的普通请求，请求体只有模型、输入、输出上限和 `stream: false`，不带补丁协议的诊断头；请求期间该身份固定到被鉴定的账号，结束后作废；16 种结果：正常回答（忽略 reasoning）、上报为其他账号、插件报告并发耗尽或分配不可用、401、429、502、400、工具调用、未完成、空回答、输出令牌超限、回答过长、非 JSON、重复键、未知状态，各自对应固定错误码 |
| `TestModelIdentificationCapabilityFollowsTheIntegration` | 插件模式只接受 `model_identification_cpa_v1` |
| `TestProbeAllocationAnswersOnlyThePinnedAccount` | 探针身份的 `eligible` 只返回固定账号（带其并发上限），候选中没有时为空；`select` 只选固定账号，否则 429；`attempt` 上报其他账号被拒；探针结束后身份失效 |

插件单测：`TestModelIdentificationRoutes` 覆盖协议路由、经 CPA 列出模型（去重、丢弃非法
ID）、未知账号的诊断错误、畸形列表；探针本身没有插件路由。

端到端（真实 CPA 进程，新增 1 组，共 14 组）：

| 测试 | 覆盖 |
|---|---|
| `TestModelIdentification` | 协议路由；CPA 为凭证注册的模型中包含测试模型；未知账号 404；数据面密钥无法访问；只授权给账号 a 的探针身份发出的非流式请求由 a 完成，CPA 把上游流式回答聚合成 Responses JSON，归因上报为 a；a 返回 500 时探针失败，b 从未被尝试 |

本地按 `plugin.Dockerfile` 的步骤运行端到端测试，14 组全部通过。

### 未验证

- 用真实账号跑一次完整鉴定（三道题与评分），在第 3 步的真实账号验收中进行。

## 路线图第 8 步第 1 项：插件模式成为默认

日期 2026-11-12。`docker-compose.yml` 改为插件镜像与 `CPA_INTEGRATION=plugin`；源码补丁
镜像改为回退，由 `.env` 的 `CPA_INTEGRATION=patch` 选择。

### 改动

- `docker-compose.yml`：`codex-compat` 使用 `plugin.Dockerfile`、锁定的插件镜像标签和残余
  补丁 SHA-256；去掉 `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY`，增加 `TMPDIR` 与可执行的
  `/run/cliproxy-plugin` tmpfs；Gateway 设 `CPA_INTEGRATION=plugin`。
- `deploy/patch-cpa.override.yml`（取代 `deploy/cpa-plugin.override.yml`）：恢复补丁镜像、
  代理变量和 `TMPDIR=/tmp`，Gateway 设 `CPA_INTEGRATION=patch`。插件 tmpfs 仍挂载但不使用。
- 选择机制沿用 `EGRESS_MODE` 的做法：`scripts/egress-settings.compose.yml` 读取
  `CPA_INTEGRATION`（缺省 `plugin`），`scripts/validate-egress.py` 只接受 `plugin`/`patch`，
  并在部署前核对渲染结果中 Gateway 的设置与 CPA 镜像一致；`scripts/egress-common.sh` 在
  `patch` 时为每次调用叠加回退覆盖文件。
- `scripts/validate-compose.sh` 按当前模式检查：插件模式核对锁定镜像、残余补丁校验和、无
  代理变量与两个 tmpfs；回退模式核对补丁镜像与补丁校验和。
- 插件镜像的入口脚本拒绝命令行参数：命令行登录在插件模式下没有出口，也绕过出口登记，
  凭证只在“CPA 授权管理”页面维护。
- CI：校验两种模式的 Compose；默认镜像随应用镜像构建，构建内运行插件的端到端测试；补丁
  镜像单独构建并运行原有的 `test-sidecar-image.sh`；Trivy 扫描两者。
- Gateway 代码中 `CPA_INTEGRATION` 未设置时仍按 `patch` 处理（不需要管理密钥），部署由
  Compose 显式设置；不一致时 Gateway 的能力探测拒绝派发。

### 测试

- `scripts/tests/test_cpa_plugin_deploy.py`：默认 Compose 为插件模式（镜像、Dockerfile、
  构建参数、无代理变量、tmpfs）；回退覆盖文件恢复补丁镜像（标签由 `source.lock.json`
  推出）；`.env` 选择回退的链路（设置文件、包装脚本、校验器、两个 env 示例）；入口脚本
  拒绝命令行登录。脚本测试共 178 个通过（1 个需 Docker 守护进程的跳过）。
- 本地用 Docker Compose v5.6.1 渲染两种模式：插件模式的 Gateway 为 `plugin`、
  `codex-compat` 为插件镜像且无 `HTTPS_PROXY`；回退模式为 `patch` 与补丁镜像；无效值被
  `CPA_INTEGRATION must be plugin or patch` 拒绝。`validate-compose.sh` 新增的模式检查对
  两种渲染结果都通过；`validate-egress.py --deployment` 对两种渲染结果通过，对 Gateway 与
  镜像不一致的模型报错。
- 完整的 `validate-compose.sh` 在本地 Compose v5.6.1 下停在一项既有的 cloudflared 检查
  （上一个提交同样如此，与本次无关）；CI 中运行完整校验。

### 未验证

- 镜像构建、`test-cpa-plugin-image.sh`、`test-sidecar-image.sh` 与 Trivy 只能在 CI 中运行。
- 真实账号验收（第 3 步）。

## 路线图第 7 步（其余部分）：缓存键核对与 Antigravity 模型限制

日期 2026-11-13。完成第 7 步剩下的两项。设计见
[兼容层升级规程](compatibility-upgrades.md#antigravity-模型限制)，缓存键的逐项结果见
[CPA Provider 缓存键核对](cpa-provider-cache-audit.md)，行为差异见升级规程第 4、5 项。

### 依据：CPA 的模型注册

在固定提交 `c5251b57` 的 CPA 源码中确认：

- Antigravity 账号注册的模型是内置目录（`registry.GetAntigravityModels()`）与
  `fetchAvailableModels` 为该账号返回的 ID 的交集，再经 `applyExcludedModels` 过滤。两条
  注册路径（`service_models.go` 与 `antigravity_models.go`）都读取
  `oauth-excluded-models` 中键为 `antigravity` 的规则。
- 规则不区分大小写，通配符只有 `*`，没有“除此之外”的写法，所以只能列出要排除的部分。
- 内置目录在该提交有 11 个 ID：五个已审模型，以及 `claude-opus-4-6-thinking`、
  `claude-sonnet-4-6`、`claude-opus-5-5-high`、`claude-sonnet-5-5-high`、
  `gemini-3.1-flash-image`、`gpt-oss-120b-medium`。
- `GET /v0/management/model-definitions/antigravity` 返回二进制内置的这份目录，测试据此
  取得目录，不依赖源码树。

### 改动

- 入口脚本生成的配置增加 `oauth-excluded-models`（`claude-*`、`gpt-*`、
  `gemini-3.1-flash-image`），只针对 Antigravity。
- 插件为生成请求选号时要求 `X-Codex-Gateway-Affinity`。缺少它的请求在 CPA 中共用固定的
  principal，按调用方隔离的状态对它们不起作用。Gateway 的生成请求和模型鉴定探针都带这个
  头，行为不变。
- 端到端 harness 增加 Antigravity 凭证、假的 `fetchAvailableModels`、OAuth 形态的 Claude
  令牌，并使用与入口脚本相同的排除规则。
- 镜像探针和部署静态校验各增加一项检查。

### 测试

插件单测（含 `-race`）：`TestPickRequiresTrustedIdentity` 增加“缺少调用方作用域”一例，
不调用 Gateway 回调，不占用名额。

端到端（真实 CPA 进程，新增 2 组，共 16 组）：

| 测试 | 覆盖 |
|---|---|
| `TestAntigravityModelCatalog` | 从二进制取得 Antigravity 内置目录，确认五个已审模型都在、且目录中确有其他 Provider 的模型；供应商为账号返回整份目录外加已退役的 Gemini ID、Claude 与 Codex 的模型 ID；账号注册的模型恰好是五个已审模型；模型探测由 CPA 发出，使用账号的令牌并经凭证中的出口；数据面的模型列表不含被排除且无其他 Provider 提供的模型；Antigravity 目录与 Claude 目录共有的 `claude-sonnet-4-6` 由 Claude 账号完成，分配只走 Anthropic 回调 |
| `TestProviderSessionsAreCallerScoped` | 同一个上游账号、相同请求、不带任何会话标识：同一个调用方两次请求向上游呈现相同的会话标识，另一个调用方呈现不同的值，调用方作用域本身不出现在上游。分别对 Codex（`Session-Id`）和走 Claude Code 请求路径的 Claude（`X-Claude-Code-Session-Id`）检查 |
| `TestAllocation` | “没有可信身份的请求被拒绝”增加缺少调用方作用域一例，上游零调用 |

反向验证（临时去掉规则，不提交）：

- harness 的配置去掉排除规则后，`TestAntigravityModelCatalog` 失败：账号注册了全部 11 个
  模型；`claude-sonnet-4-6` 的 `/v1/messages` 请求得到 503 `gateway_allocation_unavailable`，
  因为候选同时包含 Claude 与 Antigravity 账号，插件拒绝跨 Provider 的候选。
- 入口脚本去掉排除规则后，镜像探针以
  `Antigravity models are not confined to the reviewed catalog` 失败。

部署静态校验（`test_antigravity_serves_only_the_reviewed_models`）：生成的配置中只有
Antigravity 一组规则；规则不匹配任何已审模型，并覆盖固定目录中的其余六个 ID；已审列表与
`internal/config/antigravity.go` 的 `AntigravityModels()` 及端到端测试一致；harness 中的
规则与入口脚本逐字相同；Dockerfile 按名字断言该测试；镜像探针包含对应检查。

镜像探针：通过 `GET /v0/management/oauth-excluded-models` 确认运行中的 CPA 解析到的规则
恰好是上述三条，且没有针对 Codex 或 Claude 的规则。

缓存键核对中“实测”的三条（不带会话标识时按调用方区分、`prompt_cache_key` 原样转发、
客户端的 `X-Claude-Code-Session-Id` 映射后两个调用方相同）在同一套 harness 上用一次性的
测试观察上游收到的请求头得到，第一条保留为上表的回归测试。

本地结果：

- 按 `plugin.Dockerfile` 的步骤（固定提交、残余补丁、`CGO_ENABLED=1` 编译 CPA 与插件）
  运行端到端测试，16 组全部通过，无跳过。改动前的基线为 14 组通过。
- 插件 module：`gofmt`、`go vet`（含 `-tags e2e`）、`go test -race` 通过；gosec v2.22.8
  为 0 个问题，govulncheck v1.1.4 无发现（两者用 Go 1.26.11 构建）。
- Gateway：`gofmt -l .` 无输出，`go vet ./...`、`go test -count=1 ./...` 通过（去掉会话
  环境中的 `ANTHROPIC_BASE_URL` 与 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST`，原因同上文）。
- 脚本测试共 179 个，178 个通过，1 个需 Docker 守护进程的跳过。
- 镜像探针在本机的私有挂载命名空间中模拟容器挂载、以 UID 10001 运行，`serving` 与
  `no-exec-tmp` 两种模式都通过。

### 未验证

- 镜像构建、`test-cpa-plugin-image.sh` 与 Trivy 只能在 CI 中运行。
- Google 为真实账号返回的模型列表。规则对照的是 CPA 内置的目录，账号实际注册的是两者的
  交集，所以真实列表不会让结果超出五个模型；在第 3 步的验收清单中确认模型列表。
- 缓存键核对表中未标“实测”的项目来自阅读源码。缺口的利用条件没有做攻击性验证。
- 三份上游报告尚未提交。

## 路线图第 8 步第 2、3 项：删除补丁镜像与旧桥接

日期 2026-11-13。应 Owner 要求，在真实账号验收（第 3 步）之前完成。此后只有一种 CPA 镜像，
没有可切换的回退镜像。

### 删除

- 源码补丁镜像：`deploy/codex-compat/Dockerfile`、全部 `cliproxy-v8.0.*-gateway*.patch`
  （含各阶段补丁）、`entrypoint.sh`、`healthcheck.sh`、`smoke.sh`、`legacy-credentials*.test.txt`、
  `source.lock.json`、`deploy/patch-cpa.override.yml`、`scripts/test-sidecar-image.sh`。
- v7 回退镜像：`legacy.Dockerfile`、`legacy-entrypoint.sh`、`cliproxy-v7.3.15-multi-account.patch`、
  `deploy/legacy-cpa.override.yml`、`scripts/test-legacy-sidecar-image.sh`。
- 旧 Antigravity 桥接：`cmd/antigravity-bridge`、`internal/antigravity`、`deploy/antigravity-bridge`、
  `deploy/legacy-bridge.override.yml`、`scripts/antigravity-login.sh`、`scripts/test-antigravity-image.sh`；
  Compose 中的 `antigravity-bridge` 服务、`antigravity_internal` 网络、`antigravity_keyring` 卷与
  两个 secret；Squid A/B 中旧桥接的来源 ACL、目的地清单与日志格式；`THIRD_PARTY_NOTICES` 中
  Antigravity CLI 与为桥接构建的 Expat 两节。
- 只能与旧桥接配套的凭证迁移工具：`cmd/cpa-credentials-migrate`、`internal/cpamigrate`、
  `deploy/cpa-migrate`、`scripts/test-cpa-migration-image.sh`。
- 补丁镜像的命令行登录与冒烟脚本：`oauth-login.sh`、`codex-device-login.sh`、`claude-login.sh`、
  `claude-login.py`、`smoke-sidecar.sh` 及其测试。插件镜像本就拒绝命令行登录。

### Gateway 改动

- 配置：删除 `CPAIntegration`、`AntigravityTransport`、`AntigravityBridgeURL/Token`；
  `CPA_MANAGEMENT_KEY` 改为必填。环境中仍有 `CPA_INTEGRATION`（非 `plugin`）、
  `ANTIGRAVITY_TRANSPORT`（非 `cpa`）或任何 `ANTIGRAVITY_BRIDGE_*` 时拒绝启动，避免运维人员
  以为仍在使用旧链路。删除只为桥接存在的 `LegacyAntigravityModels`。
- 账号接口：`/internal/*` 兼容路径与路径映射删除，账号、并发、额度、状态、出口同步和能力
  探测一律走插件的 `/v0/management/gateway/<namespace>/*` 路由，只用管理密钥；没有管理密钥时
  不发任何请求（`sidecar_unavailable`）。生成请求仍只用数据面密钥。
- 归因：删除读取 `X-Codex-Upstream-Account`/`X-Codex-Conversation-Hash` 响应头的代码，
  归因只来自请求账本；模型目录请求不再产生归因。账本对所有客户端始终启用，`attempt`
  回调不再有“当前集成方式不接收”的分支。
- 模型鉴定：删除补丁镜像的直连探针协议（`model_identification_direct_v1`）及
  `ListUpstreamAccountModels`/`ProbeUpstreamAccount*`；没有账本时探针拒绝发出。顺带修正：
  探针在读取错误响应体时被取消或超时，现在报告取消/超时而不是 HTTP 状态。
- Antigravity：客户端只剩 CPA 原生一种；删除桥接专用的错误映射、增量流协议、请求校验和
  目录合并；`legacyEgressAllowed` 与 `HasProviderEgressBinding` 删除。
- 插件：删除只为桥接回退存在的 `CPA_ANTIGRAVITY_ENABLED` 开关。
- 部署：`scripts/compose.sh` 在 `.env` 选择 `CPA_INTEGRATION=patch` 时拒绝执行；
  `validate-compose.sh` 改为从 `plugin.lock.json` 读取版本与提交，检查插件入口脚本的配置
  （plugin-only、关闭会话亲和、无全局代理、管理 API 独立密钥），并断言旧桥接的服务、卷、
  secret 和 Gateway 变量不再出现。CI 去掉补丁镜像、桥接、v7 回退和迁移工具的构建与扫描，
  CPA 版本与提交改为从锁文件读取。

### 测试

- Gateway：`gofmt -l .` 无输出；`go vet ./...`、`go vet -tags integration ./...`、
  `go test -count=1 ./...`、`go test -race -count=1 ./...` 通过（去掉会话环境中的
  `ANTHROPIC_*`/`CLAUDE_*` 变量，原因同上文）。PostgreSQL 16 本地实例上
  `go test -count=1 -tags=integration ./internal/store ./internal/server` 通过。
- 新增或改写的单测覆盖：退役设置拒绝启动、管理密钥必填；账号路由只带管理密钥且缺少时
  不发请求；任何形态的归因响应头都不被采信（有无账本均如此）；Antigravity 请求不带客户端
  会话头；模型鉴定的能力、模型列表走管理路由、探针走数据面并由账本固定账号。
- 插件 module：`go vet ./...`、`go vet -tags e2e ./...`、`go test -race -count=1 ./...` 通过。
  按 `plugin.Dockerfile` 的步骤在本机构建（v8.0.59 固定提交、残余补丁无 fuzz 应用与其测试、
  CPA 插件宿主测试、`CGO_ENABLED=1` 编译 CPA 与插件），端到端测试 16 组全部通过，无跳过。
- `go mod tidy` 只把 `golang.org/x/sys` 改为间接依赖（原先由旧桥接直接使用）。
- 脚本测试 100 个通过，1 个需要 Docker 镜像构建的跳过。用 CI 的方式生成非生产 `.env` 与
  secret 后，`./scripts/validate-compose.sh` 完整通过（含 Squid A/B 解析和三种出口模式，
  使用本地 Docker 守护进程）；`.env` 追加 `CPA_INTEGRATION=patch` 后 `compose.sh config`
  按预期拒绝；CI 的工具链一致性与上游声明两个步骤在本地按原样执行通过；
  `./scripts/lock-images.sh --check` 通过。

### 未验证

- 镜像构建、`test-cpa-plugin-image.sh`、出口容器测试与 Trivy 在 CI 中运行。
- 真实账号验收（第 3 步）。没有补丁镜像可回退，验收中发现问题只能回到上一个 revision。
