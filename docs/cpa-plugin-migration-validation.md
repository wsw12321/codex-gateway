# CPA 插件迁移验证记录

本文记录把 `codex-compat` 从整份源码补丁迁移到 CPA 官方插件的验证过程。迁移目标、
架构与取舍见 [兼容层升级规程](compatibility-upgrades.md) 的“插件模式”一节。
默认部署仍是补丁镜像；插件模式在完成真实账号验收前不作为默认。

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
