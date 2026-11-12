# CLIProxyAPI 兼容层升级规程

> 以下旧 bridge / CPA v7 内容保留用于历史排障；当前默认部署以 [CPA v8 切换与回滚](cpa-v8-cutover.md) 和 [原生模型目录](cpa-native-models.md) 为准。

兼容层继续使用独立锁定的 Debian slim/glibc 运行镜像。旧第三方 Gemini 插件
和对应补丁已经移除；构建回归检查确认历史 Gemini OAuth 文件被忽略且保留，
Codex 凭证继续加载。Antigravity 使用独立服务、官方 CLI 和 Keyring 卷，详见
[Antigravity 接入说明](gemini-pro.md)。

仓库固定在 2026-09-23 核对的[最新稳定版本 CLIProxyAPI `v7.3.15`](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v7.3.15)、commit
`673131f57484517c3a1eae7e36c4cfa7b9bb4efc`。版本号和 commit 必须作为一组
更新，Docker 构建会验证 tag 指向该 commit。仓库同时固定
`deploy/codex-compat/cliproxy-v7.3.15-multi-account.patch`；本次从 `v7.3.12`
重基，保留 Gateway 权限分配、账号锁定、并发计数和两账号重试上限，并适配
上游 Codex cloaking 请求头签名变化及 compact 模型目录字段修复。补丁 SHA256 为
`2dc404c8b22496a6de935a8fd07ca6006d3681bcf503ea33f2cd529610f3096b`。
构建必须先用 `git apply --check --ignore-space-change` 验证补丁上下文，
再用 `git apply --ignore-space-change` 应用补丁并运行补丁内的聚焦测试，任一步
失败都不得生成镜像。该选项允许上下文空白差异，不能跳过补丁校验或测试。

上游统一的会话解析器继续识别父会话，Gateway 在解析后的身份上隔离调用方。
重基保留已有模型目录、HTTP/SSE/compact、额度锁定和请求头安全回归，并增加新版
优先级选择、凭证更新序号与 WebSocket 额度信号测试。上游传输使用模拟服务验证。
部署配置显式禁用 `discovery.enabled` 和实验性 `codex.response-steering`，
继续由 Gateway 返回 426 引导客户端使用 HTTPS/SSE。

兼容层镜像标签固定为 `v7.3.15-673131f5-2dc404c8b22496a6-codex-only`，记录主程序、提交、多账号补丁及仅 Codex 的构建。
Compose、校验脚本和 CI 必须使用同一完整标签，CI 扫描实际构建的镜像。
兼容层构建镜像使用 Go 1.26.10；补丁保留 go-git/v6 `v6.0.0-alpha.5`、
go-billy/v6 `v6.0.0-alpha.2` 与 x/text `v0.41.0`，并将 `golang.org/x/crypto`
更新到 `v0.56.0`、`klauspost/compress` 更新到 `v1.18.7`，修复扫描发现的
SSH 死锁及压缩库越界读取问题。Debian 运行时显式安装 `libpcre2-8-0`，
确保继承的基础包获得已发布的安全更新。`CLIPROXY_RUNTIME_IMAGE` 独立锁定 Debian
`bookworm-20260824-slim`，Gateway 的 `RUNTIME_IMAGE` 仍为 Alpine。Responses API
请求契约保持兼容，保留现有价格目录。配套的模型鉴定流程需要 Gateway 应用
`0019_model_identification_diagnostics.sql`，新增任务阶段、发起人和安全错误元数据；
Owner 账号状态管理接口仍只管理 Codex。

模型鉴定的直连请求显式遵守 Responses Lite 约束：设置
`reasoning.context=all_turns`、`parallel_tool_calls=false`，避免首题被上游以
HTTP 400 拒绝。上游成功 SSE 可以省略 `Content-Type`，此时仍须通过原有事件、
完成状态、文本和大小校验；显式空值、重复或非 SSE 类型继续拒绝。补丁携带这两项
回归，镜像构建检查测试存在并执行。该修复无需新的 Gateway 或数据库迁移。

本次交付范围为仓库升级和构建验证，不代表生产已经切换。真实 OAuth 账号的
Astra 冒烟和生产切换按下文规程执行。

每次兼容层升级都必须重新执行 Gateway 单元测试、race、vet、部署脚本回归、
合成配置下的 Compose 校验，以及 CLIProxyAPI 补丁覆盖的相关包、账号与 watcher
竞态测试和执行器安全回归；还要确认容器内二进制报告本节锁定的版本与完整 commit。
生产切换前按下文规程完成固定 Trivy 条件扫描、真实 OAuth 冒烟和回滚演练；原版补丁
应能从升级前的 Git revision 恢复。

该补丁属于部署安全边界，而不是可选功能：

- Gateway 在每次生成请求前验证内部 `capabilities` 接口的
  `upstream_account_access_v1` 能力，再以 `X-Codex-Gateway-User` 传递已认证用户 ID。
  兼容层在请求日志记录前消费此头，并在所有上游传输中再次移除它。
  每次派发、绑定复用和重试均先调用 `/internal/upstream-accounts/eligible`，
  冷分配再由数据库按用户权限过滤候选。专属账号仅允许名单内用户；共享账号不受
  专属名单限制。权限查询或能力校验失败时拒绝生成，不使用旧绑定绕过检查。
- 使用 `gateway-allocation` 加一小时 session affinity。新分配由 Gateway 按
  候选账号系数及近 24 小时已结算实际费用选择；系数 0 停止接收新对话，已有
  有效绑定继续使用。单候选、无显式会话、LCP 派生会话和故障切换均受同一规则约束。
  兼容层以 sidecar Bearer 密钥回调固定
  `http://gateway:8080/internal/upstream-accounts/select`，两秒超时，不使用代理、
  不跟随重定向，传播取消。回调期间不持有管理器及绑定锁，返回后重查账号可用性
  和并发绑定；失败返回 503，不回退轮询。插件调度器不能绕过此选择器，Home
  调度与此模式不兼容并拒绝请求。Gateway 发送的
  `X-Codex-Gateway-Affinity` 是每个调用方的 43 字符不透明 HMAC 作用域；sidecar
  校验并消费该头，所有显式及派生 session ID 都以该作用域命名空间隔离，且该头
  永不发送给 OpenAI。
- sidecar 会把主会话、Codex/Claude subagent、fork、LCP parent 和 side 对话解析为
  caller-scoped 的 root session。root 关系只来自已识别的 session/parent/turn 元数据；
  无法解析父关系时使用当前会话自身，缺少会话标识时保留请求级行为。生成给 Gateway
  的 `conversation_hash` 使用 root identity，客户端伪造的同名请求头不可信，也不会
  被转发到真实上游。
  parent registry 在单个 sidecar 实例内保存，TTL 为一小时、容量为 65536；重启、过期
  或逐出后，仅依据当前请求能识别的关系解析 root。亲和绑定仍使用叶子 session、provider
  和 model，不新增 root 级账号绑定。新 hash 继续写入 `usage_requests.conversation_hash`，
  使用独立 root hash domain；不增加数据库列，也不回填历史 leaf hash。
- 每个请求最多触达两个不同 OAuth 账号。仅认证、额度、429、408、网络错误和
  5xx 可在首个下游字节前切换账号；3xx、其他 4xx、客户端取消及首字节后的流式
  错误都不得重放。部署配置必须把 handler 层 `streaming.bootstrap-retries` 固定为
  `0`，防止它为同一请求重新分配一组两账号预算。
- 最终选中的稳定账号索引通过 `X-Codex-Upstream-Account` 返回给 Gateway；Gateway
  必须消费该头，不得转发给 API 客户端。该索引只接受 16 位小写十六进制，基于
  OAuth `account_id` 的不可逆摘要而不是含邮箱的文件名；同一真实账号的重复文件
  只能有一个进入路由池。
- 只开放 Bearer 认证的 `GET /internal/upstream-accounts`、
  `GET /internal/upstream-accounts/capabilities`、
  `GET /internal/upstream-accounts/concurrency`、
  `PUT /internal/upstream-accounts/{id}/status`、固定 URL 的
  `POST /internal/upstream-accounts/{id}/quota`。模型鉴定使用独立的
  `GET /internal/model-identification/capabilities`，协议为 `model_identification_direct_v1`，
  以及 `GET /internal/model-identification/accounts/{id}/models`、
  `POST /internal/model-identification/accounts/{id}/probe`；旧账号权限 capability 的严格
  JSON 保持不变。模型选项是本地 Codex Free/Plus/Team/Pro 原生目录的去重并集，表示
  “可尝试模型”，不依赖套餐元数据、禁用状态、已注册模型、业务别名或价格配置；实际
  支持情况由上游答复确定。Gateway 必须先完成 Owner、来源和近期验证校验，再调用
  内部 Bearer 认证的诊断接口。POST 还必须携带合法 `X-Codex-Gateway-User`、UUID
  `X-Codex-Diagnostic-Run` 和 `X-Codex-Diagnostic-Probe`（1 至 3）；只用于审计，
  不发送给上游。普通 `/v1` 请求不能通过请求头或执行 metadata 获得诊断权限。
  探针仅接受模型、单道纯文本题和 `max_output_tokens`，每题重新取得相同稳定账号的
  最新凭证快照，直接访问固定 Codex Responses URL，保留出口代理并禁止重定向。
  Owner 诊断跳过普通用户账号授权、分配权重、禁用/额度/冷却及并发策略，但绝不临时
  启用账号或修改这些控制。诊断不刷新凭证、不自动重放，也不跨账号；真实凭证缺失、
  上游认证/权限/限流/模型拒绝仍明确失败。请求使用全新会话、空工具和禁止工具选择，
  不进入普通业务提示注入、模型路由、usage 发布、回答缓存或账号结果写回链路。
  每题超时 180 秒；SSE 单事件最多 512 KiB、整流最多 8 MiB，只接受完整 completed，
  `output_tokens` 不超过请求上限（最大 16,384）、文本不超过 16 KiB，且无工具调用。
  错误仅输出固定 code、preflight/probing/validating 阶段以及可选上游状态和重试等待秒数，
  不输出原始上游错误文本。诊断路径无条件排除请求/响应正文及错误日志，审计只含
  actor、run、题号、稳定账号、固定结果码、阶段和耗时；不保存回答。
  Codex 上游不支持 `max_output_tokens`，因此 token/文本上限属于结果验收限制，无法
  保证上游生成或计费不超过该额度。额度接口只接受精确的
  `{"method":"account/rateLimits/read","id":6}`（不得包含 `params` 或其他字段），
  并且只能请求 `https://chatgpt.com/backend-api/wham/usage`，不能接收调用方提供的
  URL、方法或上游 Header，且 Codex HTTP client 不跟随任何重定向。账号列表只输出严格
  `a***@example.com` 形式的 ASCII 脱敏邮箱；额度只输出 `{id,result}` RPC envelope、
  经过严格标识符校验的限额桶、整数使用百分比、分钟窗口和 Unix 秒重置时间，不能返回
  或记录 token、完整邮箱、任意上游显示文本或原始上游响应。
  CLIProxyAPI 完整管理 API 仍保持关闭。
- 兼容服务并发接口按稳定账号 ID 返回 `sampled_at` 和
  `{id,active_requests,concurrent_limit}`。Gateway 接受并校验非负整数
  `concurrent_limit`（未启用分配选择器时为 `0`），也兼容省略该字段的旧快照；
  Owner 接口仅输出 `{id,active_requests}`，其他未知字段仍拒绝解析。其中
  `active_requests` 表示该账号当前活跃的 root 对话数。首响应等待和长流均计数；同一
  root 在同一账号上的重叠执行只占一个名额，root 的最后一个执行结束、失败、取消或流
  关闭后才释放。不同 root 分别计数，同一 root 切换到另一个账号时在另一个账号独立计数。
  禁用账号不会隐藏已有执行。接口只返回计数，缺失账号不代表零；Gateway 自身的请求级
  lease 仍按每个请求计数。
  Gateway 的 `/admin/upstream-accounts/concurrency` 仅 Owner 可访问，并拒绝超出一分钟
  时钟偏差的快照；旧服务不支持或读取失败时页面显示“暂不可用”。
- 状态接口只接受 `{"enabled":true}` 或 `{"enabled":false}`，返回确认后的稳定
  账号 ID、CLIProxyAPI 来源状态、Gateway 手动状态、Gateway 额度状态，以及仅在三者
  均正常时为 `available` 的最终状态。手动禁用与生成请求的结构化
  `usage_limit_reached` 都锁定整个账号，直到 Owner 手动重新启用。普通 429
  仍按现有退避冷却；额度查询只读，认证和网络错误不触发额度锁定。
- 控制状态按稳定 ID 写入 OAuth 持久卷中的 `.gateway-account-state`，通过
  `0600` 临时文件、文件同步、原子替换及目录同步确认持久化。加载器和文件监听
  忽略该文件及其临时文件；现存文件不可读或无效时启动失败。OAuth 刷新、同账号
  重复文件替换及重启均保留锁定；持久化失败返回固定错误，启用失败保持禁止分流。
- 重新启用清除账号及所有模型的冷却，不预查额度；上游再次明确耗尽时重新锁定。
  调度器、模型可用性和会话亲和使用同一控制状态，管理操作前已开始的请求和 SSE
  继续执行；稳定账号 ID 与控制版本保证旧请求结果不能覆盖新的管理操作。
- 额度解析不依赖上游或本地 OAuth 的 `plan_type` 元数据；套餐缺失、名称未知或类型
  变化都不会阻断有效额度。响应必须包含 `rate_limit`（可为 null）或非 null 的
  `additional_rate_limits` 数组，不能将任意 JSON 当成空额度。账号列表仍只输出
  Plus/Pro/unknown 套餐分类。
- 额度解析失败按固定条件返回错误码，区分 JSON、字段类型、额度结构缺失、百分比、窗口、
  重置时间和额外额度标识问题。错误不包含原始上游字段值；Gateway 使用固定白名单
  映射并写入查询审计。百分比、窗口与标识符的校验约束保持不变，收到错误码不等于
  已经证实上游改版；Gateway 仍识别旧 sidecar 的套餐错误以支持滚动更新。

升级候选必须先在隔离环境覆盖以下契约：

- 认证后的 `GET /v1/responses` 返回一次
  `426 responses_websocket_unsupported` 后客户端立即改用 HTTPS/SSE，探测不进入
  usage、配额、计费、并发租约或 sidecar；
- Astra（`gpt-6-astra`）模型列表、非流式和 SSE `POST /v1/responses`；
- Astra 的 `POST /v1/responses/compact`；
- 两账号重试上限、最终账号归因以及内部账号列表和额度接口；
- 系数 0 排空、唯一候选为 0、并发首次绑定、回调超时／失败、绑定到期及重启；
- 安全清理后的 401、429、5xx；
- usage 分散在多个 SSE chunk 时的 input、cached input、output、reasoning；
- 客户端断开取消、首 token 超时和总超时；
- OAuth refresh 成功、refresh 失效和 reauthentication required；
- 禁用／启用后的分流、持续额度锁定、普通 429 到期恢复、再次耗尽；重启、重复文件
  替换、并发控制、旧请求结果、状态读写失败、账号消失及全部账号不可用；
- sidecar 不记录请求/响应正文或 token，完整管理 API 无法从兼容层网络远程访问，
  窄内部接口只返回脱敏、规范化字段。

仓库验证至少执行 `./scripts/validate-compose.sh`、
`./scripts/compose.sh build gateway codex-compat antigravity-bridge`、
`./scripts/test-sidecar-image.sh`、`./scripts/test-antigravity-image.sh`、
`go test -count=1 ./...`、
`go test -race -count=1 ./...` 和 `go vet ./...`，并检查构建内现有安全回归测试通过。
隔离测试的模型目录和 HTTP 契约验证不能替代真实 OAuth 冒烟。

切换生产前运行加密数据库备份，但不要复制 OAuth volume。持有设备登录锁，
停止旧 sidecar，确认其容器状态不是 running，才允许候选版本挂载现有 token。
完成 Astra 模型列表和至少一个已授权 Plus/Pro 账号的普通及 SSE Responses、
compact 人工冒烟后才能恢复 Gateway 流量。

Gateway 必须支持现有用户账号权限、加权分配、账号控制及并发接口。模型鉴定需配套
升级 Gateway 和兼容层，并应用上述 `0019` 迁移；旧兼容层缺少诊断能力时，新 Gateway
会明确返回 `model_identification_protocol_unsupported`。兼容层
启动与健康检查不等待 Gateway，保持 `Gateway -> healthy sidecar` 的启动顺序；
首次部署和登录后的生成冒烟必须等 Gateway `/readyz` 返回 200。
登录脚本在 sidecar 健康后启动 Gateway 并等待就绪；独立
`scripts/smoke-sidecar.sh` 与容器内 `sidecar-smoke` 都会先检查 Gateway。
这两个脚本只验证账号元数据、模型目录和权限能力；普通及 SSE 生成冒烟须通过
Gateway 使用真实用户 API Key，确保经过个人账务和群组额度准入。直接调用兼容层
不会生成可信用户身份，因此拒绝生成请求。
Caddy 对 `/internal` 和 `/internal/*` 返回 404，内部选择接口不能从公网进入。
未同步的新 OAuth 账号自动按系数 1、无历史费用参与首次分配，无须先打开管理员页面。

失败回滚时保持停写，先停止候选实例。本次仅升级兼容层且没有写入新迁移时，
可切回升级前仍支持相同权限、加权分配和账号控制契约的镜像；若同时部署了带新迁移
的 Gateway，则须将升级前数据库备份恢复到新的隔离卷并配套回退。
任何时刻都不允许两个 sidecar
共享同一组 refresh token。若任一 token 已因候选版本失效，保持服务关闭并重新
执行 `scripts/codex-device-login.sh`。

账号控制上线后，回滚镜像也必须理解 `.gateway-account-state`。不支持该文件的
旧镜像会忽略禁用与额度锁定，不能直接恢复流量；保持 Gateway 停流量，使用包含
账号控制补丁的修复镜像。不要通过删除状态文件或复制 OAuth 卷来恢复账号。

## 插件模式（默认）

上文描述的是源码补丁镜像：一个 25,177 行、改动 219 个文件的补丁，每次 CPA 升级都要
重基。CPA v8 提供了正式的插件接口，同一段时间里（v8.0.48 → v8.0.59）补丁改到的 127 个
上游文件中有 85 个被上游修改，而插件接口只变了 48 行。插件模式把 Gateway 需要的功能
移到这个接口之后，残余补丁只剩 139 行。

插件模式是默认部署（`docker-compose.yml`）。源码补丁镜像保留为回退：在 `.env` 中设置
`CPA_INTEGRATION=patch`，`scripts/compose.sh` 就会叠加 `deploy/patch-cpa.override.yml`；
真实账号验收稳定后删除。插件模式覆盖了补丁镜像的功能，差异见下文“与补丁镜像的行为差异”。
上游请求全部由 CPA 生成，插件和 Gateway 不自行构造供应商请求，也不通过补丁改变 CPA 生成
请求的方式。
验证过程见 [CPA 插件迁移验证记录](cpa-plugin-migration-validation.md)，后续步骤见
[CPA 插件模式路线图](cpa-plugin-roadmap.md)。

### 组成

| 部分 | 位置 | 作用 |
|---|---|---|
| 残余补丁 | `deploy/codex-compat/residual/cliproxy-plugin-only-selector.patch` | 新增 `routing.strategy: "plugin-only"`：一个对任何选号都返回 `gateway_allocation_unavailable` 的选择器。纯新增，不删改上游行为 |
| Gateway 插件 | `deploy/codex-compat/plugin/` | 独立 Go module，编译为 `codex-gateway-v1.0.0.so`。除 CPA 的插件接口定义外没有第三方依赖 |
| 构建 | `deploy/codex-compat/plugin.Dockerfile`、`plugin.lock.json` | 固定同一 CPA 提交；安全依赖改用 `go get` 升级；构建时运行插件单测和端到端测试 |
| 运行 | `plugin-entrypoint.sh`、`plugin-healthcheck.sh` | 凭证目录检查沿用 `entrypoint.sh`；健康检查要求插件正在提供服务 |
| Gateway | `CPA_INTEGRATION=plugin` | 用请求账本做归因；账号路由改走 `/v0/management/gateway/*`，使用 `CPA_MANAGEMENT_KEY` |

插件通过 CPA 的以下扩展点工作：

| 扩展点 | 插件做的事 |
|---|---|
| 前端认证（独占） | 只接受 Gateway 的内部密钥；校验 `X-Codex-Gateway-User`、`-Affinity`、`-Groups`、`-Request`；把调用方作用域交给 CPA，使会话身份按调用方隔离 |
| 调度 | 每次选号都调用 Gateway 的 `eligible`，按账号控制状态、账号池范围、并发名额过滤；已绑定的会话复用原账号，否则按资金层顺序调用 `select`；把最终决定上报到 `attempt` |
| 请求拦截 | 选号后移除全部内部请求头 |
| 请求终态 | 释放并发名额；成功的对话保留名额 5 分钟 |
| 用量 | 识别结构化的 `usage_limit_reached`，锁定账号直到 Owner 重新启用 |
| 管理路由 | Codex、Antigravity、Anthropic 三个命名空间的 capabilities、funding-layers、账号列表、并发快照、启用/禁用、出口同步；Codex 额度（CPA 被动观测）；“CPA 授权管理”页面三个 Provider 的额度 |

`.gateway-account-state`、`.gateway-antigravity-identities`、`.gateway-anthropic-identities`
沿用补丁镜像的文件名和格式，稳定账号 ID 的算法也相同，所以两种镜像可以挂载同一个
凭证卷互相替换。

### 账号独立出口

Gateway 仍是账号出口的唯一来源，插件只负责让 CPA 照着执行。CPA 本身不配置任何代理：
生成的配置里没有 `proxy-url`，入口脚本在启动 CPA 前清除 `HTTP_PROXY` 等环境变量。
每个凭证都带显式的 `proxy_url`，包括继承全局出口的账号（写入全局出口地址
`http://egress-allowlist:3128`）。CPA 在凭证代理缺失或无法使用时会退回全局代理、再退回
直连；现在没有全局代理，退路只剩直连，而 `compat_internal` 没有外网，所以账号出口失败时
请求一定失败，不会改走别的出口。

1. 每次选中一个账号后，插件调用 Gateway 现有的 `resolve-egress` 回调，取得该账号此刻
   绑定的路由（继承全局时换成全局出口地址），与凭证文件里的 `proxy_url` 比较。
2. 一致就使用该账号。CPA 对这个凭证的推理、刷新、模型列表都走 `proxy_url`。
3. 不一致时，插件通过 CPA 自己的 `PATCH /v0/management/auth-files/fields` 写入新路由，
   并在本次请求中跳过该账号，改用其他有权限的账号。原因是 CPA 在调用插件之前已经取好了
   凭证快照，本次请求改不了它的代理。
4. 路由无法确认（Gateway 不可达、出口尚未生效）或不是固定的 `egress-allowlist:3129`
   加路由凭据的形式时，该账号同样被跳过。没有其他账号可用时请求返回 503。
5. 账号的出口节点不可用时，该账号这次尝试在出口处失败；CPA 可以按原生规则换另一个有
   权限的账号，并使用那个账号自己的出口。

插件还在后台对账：启动后每 2 秒一轮，直到一轮确认了全部凭证；之后每分钟一轮
（`CPA_PLUGIN_EGRESS_RECONCILE_SECONDS`，10–3600），对每个凭证（含重复凭证）调用
`resolve-egress` 并写入不一致的路由。凭证上传时自带的代理会被覆盖，不会被信任。Owner
修改账号出口或编辑出口节点后，Gateway 立即通知插件同步受影响的账号，所以通常不会出现
上面第 3 步的跳过。通知失败只会导致该账号被跳过一次，不会走错出口。

与补丁镜像相比有两点不同：

- 路由凭据会写进 OAuth 卷里的凭证文件（`0600`），补丁镜像只保存在内存中。
- 绑定变更后、通知或下一轮对账之前（最长约一分钟），CPA 的后台刷新仍使用凭证里原来的
  路由。原来的路由若已失效，刷新失败而不会改走其他出口。

### 额度查询

插件不向任何供应商发请求，额度全部来自 CPA 的被动观测：CPA 对自己发出的真实请求，
把响应里的限额头（Codex 的 `x-codex-*`、Claude 的 `anthropic-ratelimit-unified-*`）记录
在凭证上。插件通过 CPA 管理 API 的 `GET /v0/management/auth-files?auth_index=` 读取
（只取 `quota`、`model_quotas` 和 `next_retry_after`），取该凭证或其各模型中最新的一次
观测。

| 用途 | 插件路由 | 来源 |
|---|---|---|
| Owner 额度查询、额度守卫、群组账号用量（Codex） | `upstream-accounts/quota` | 最新一次含 `x-codex-*` 的观测，映射为补丁镜像的 `account/rateLimits/read` 信封，并附 `observed_at` |
| “CPA 授权管理”页面的 Codex 额度 | `gateway-management/codex/accounts/quota` | 同上，换算成每个限额主窗口的剩余比例 |
| “CPA 授权管理”页面的 Antigravity 额度 | `gateway-management/antigravity/accounts/quota` | CPA 没有 Antigravity 的被动观测，返回 `{"status":"unavailable","quota":[]}` |
| Claude 额度守卫与“CPA 授权管理”页面 | `gateway-management/anthropic/accounts/quota` | 最新一次观测的 `anthropic-ratelimit-unified-*` |

Codex 信号的映射：

- 默认限额（`codex`）读 `X-Codex-Primary-*`、`X-Codex-Secondary-*`；其他限额读带名字的
  `X-Codex-<名字>-Primary-*`（HTTP 响应）或 `X-Codex-Additional-<名字>-…`（WebSocket 事件），
  限额 ID 为名字转小写、连字符换成下划线。名字不能构成合法 ID 的限额被略过，最多 32 个。
- 每个窗口取 `Used-Percent`（小数向上取整，所以不会少报）、`Window-Minutes`、`Reset-At`；
  只有 `Reset-After-Seconds` 时以观测时间为起点换算。`Limit-Reached: true` 或
  `Allowed: false` 记为 `rate_limit_reached`。
- 套餐类型、额度余额、限额显示名都不输出。
- 还没有观测时返回 409 `quota_not_observed`；读取观测失败返回 503
  `quota_observation_unavailable`；数值不合法时返回与补丁镜像相同的结构错误码。
- 凭证文件中的 ChatGPT 账号仍须与稳定账号 ID 的来源一致，否则返回
  `quota_account_identity_unavailable`。这一步只读本地凭证文件。

Gateway 把 `observed_at` 当作这次读数的时间：Owner 页面显示“观测于 …”，额度守卫和群组
账号用量按观测时间记录。额度只在账号处理请求后更新；长时间没有请求的账号显示较旧的观测。

### 失败时关闭的四道检查

1. **选择器。** 插件未加载、在配置中被禁用或返回了无效账号时，CPA 落到 `plugin-only`
   选择器，请求得到 503，不会改用内置轮询。未打残余补丁的原版 CPA 在同样情况下会
   轮询并成功调用上游，端到端测试覆盖了这一差别。
2. **能力探测。** Gateway 每次派发前请求 capabilities 路由。没有插件的 CPA 返回 404，
   Gateway 拒绝派发。
3. **插件自身。** 配置无效、控制文件不可读或格式错误时，插件保持注册但拒绝一切请求，
   容器健康检查失败。
4. **归因复核。** `attempt` 只接受正在进行中的派发，并再次核对该用户是否有权使用被
   上报的账号、账号是否在本次请求的账号池内。不通过则插件放弃这次选号。

### 凭证操作

“CPA 授权管理”页面的新增/重新授权、导入、刷新、删除都由 CPA 自己的管理接口完成，插件
只在之前固定出口、之后识别身份并绑定账号。Gateway 侧的契约与补丁镜像相同，页面不改。

| 操作 | CPA 原生接口 | 插件做的事 |
|---|---|---|
| 新增/重新授权 | `POST /v8/management/oauth/auth-url`（请求体带 `proxy_url`）、`POST /v0/management/oauth-callback`、`GET /v0/management/get-auth-status` | 出口取 Gateway 为本次登记准备的路由，没有选节点时取全局出口地址，二者都显式交给 CPA。CPA 经该出口完成换取令牌，并把出口写进保存的凭证。授权 URL 的主机必须是 `auth.openai.com`、`claude.ai` 或 `accounts.google.com` |
| 导入 | `POST /v0/management/auth-files`、`POST /v0/management/auth-files/refresh` | 只接受刷新令牌（可附访问令牌与 Codex 的 ID 令牌），生成只含 `type`、令牌和 `proxy_url` 的凭证文件上传，再让 CPA 经该出口刷新 |
| 刷新 | `POST /v0/management/auth-files/refresh` | 先写入账号当前的路由 |
| 删除 | `DELETE /v0/management/auth-files` | 删除前再次确认账号已停用、没有进行中的请求；身份登记与控制状态保留 |

授权或导入完成后，插件从 CPA 保存的字段识别身份：Codex 取 `account_id` 与 ID 令牌中的
声明；Claude 取 `account_uuid` 与 `organization_uuid`；Antigravity 优先取补丁镜像记录的
`google_subject`，没有时取 CPA 记录的邮箱（小写，登记在
`.gateway-antigravity-email-identities`）。随后调用 Gateway 的 `register-egress` 把 Owner
选择的出口提交给该账号，并写入最终路由。任何一步失败都不会让账号带着别的出口运行：没有
身份或与已有 Claude/Antigravity 凭证冲突的新凭证会被删除；出口登记失败时账号被停用。

### 模型鉴定

探针是一次普通的 `POST /v1/responses`（`stream: false`），上游请求由 CPA 按处理客户端
请求的同一路径生成。Gateway 为每次探针生成一个一次性身份（随机 UUID）和独立的调用方
作用域，并在请求账本中把它固定到被鉴定的账号：这个身份的 `eligible`/`select` 回调只回答该
账号（不受 Owner 自己的账号权限影响），候选里没有它时回答不可用；`attempt` 上报也只接受该
账号。所以 CPA 无法选到或重试到别的账号。探针不经过准入，不计费，也不记为用量；它和普通
请求一样占用该账号的并发名额。

插件只提供两个管理路由：`model-identification/capabilities` 报告协议
`model_identification_cpa_v1`；`model-identification/accounts/models?id=` 包装 CPA 的
`GET /v0/management/auth-files/models`，列出 CPA 为该凭证注册的模型。

### 与补丁镜像的行为差异

| # | 补丁镜像 | 插件模式 | 原因 |
|---|---|---|---|
| 1 | 自定义“哪些错误可以换账号” | CPA 原生规则，上限仍是 4 个账号。实测 429、500、403 会换账号，400 直接返回 | 判定逻辑在执行循环内部，插件接触不到 |
| 2 | 归因写在响应头里 | Gateway 请求账本 | 只依赖调度这一个扩展点，对所有端点一致 |
| 3 | 执行器内拒绝重定向、固定上游主机 | 不保留，依靠出口白名单 | 属于 CPA 内部代码 |
| 4 | Provider 缓存按调用方和账号隔离 | 依赖 CPA 原生的调用方作用域。会话绑定和会话哈希已验证按调用方隔离；各 Provider 缓存的键尚未逐个核对 | 同上 |
| 5 | Antigravity 只注册五个已审模型 | 只靠 Gateway 的模型权限和价格目录限制 | 尚未配置 CPA 侧的排除列表 |
| 6 | Gemini 历史校验返回 `new_session_required` | 返回上游原生错误 | 需要让完整请求体经过插件 |
| 7 | 重复凭证在加载时隔离 | Codex 重复文件在选号时过滤，只用最近刷新的一份；Antigravity/Claude 同一身份有多份时全部不可用 | 插件没有加载期钩子 |
| 8 | CPA 管理 API 关闭 | 开启，使用独立密钥，仅内部网络可达 | 插件路由挂在管理 API 之下 |
| 9 | 会话绑定和会话哈希跨重启外的升级延续 | 切换到插件模式时重置一次；哈希域从 `v2` 变为 `v3` | 根会话标识的来源不同 |
| 10 | 没有显式会话标识时按对话内容推导亲和 | 不推导，按单次请求处理 | 需要解析请求体。Codex CLI 和 Claude Code 都带显式会话标识 |
| 11 | 没有可验证身份的凭证可路由但无归因 | 直接不可用 | 无法授权也无法计费的账号不应接收请求 |
| 12 | 容器内没有可写且可执行的目录 | 多一个 64 MB 的可执行 tmpfs | CPA 把插件复制到临时目录后再加载 |
| 13 | 账号出口按每次操作向 Gateway 解析，只在内存中使用 | 选号时向 Gateway 解析并写入凭证文件；不一致时本次跳过该账号 | 插件改不了 CPA 已取好的凭证快照 |
| 14 | Codex 额度实时请求 `chatgpt.com/backend-api/wham/usage` | CPA 被动观测的最近一次响应头，附观测时间；没有观测时返回 `quota_not_observed` | 上游请求只由 CPA 生成，CPA 没有主动额度查询 |
| 15 | “CPA 授权管理”页面的 Codex 额度总是失败（门面以 RPC id `1` 调用只接受 `6` 的处理器） | 正常返回 | 补丁镜像的缺陷，插件按正确的 id 调用 |
| 16 | Claude 冷却时间取凭证与各模型冷却中的最晚值 | 取 CPA 管理 API 给出的凭证冷却时间，不含模型级冷却 | 管理 API 不输出模型级冷却 |
| 17 | Antigravity 额度实时请求 `fetchAvailableModels` | 不可用 | CPA 没有 Antigravity 的被动观测；不自行构造供应商请求 |
| 18 | Codex 额度百分比只接受整数 | 小数向上取整 | 响应头可能带小数；向上取整不会少报用量 |
| 19 | 身份由补丁向供应商的固定身份端点核验 | 取 CPA 保存的字段；新增的 Antigravity 账号以邮箱为身份 | 插件不自行发出供应商请求；CPA 的 Antigravity 授权只记录邮箱 |
| 20 | 可导入 Antigravity 刷新令牌、只有访问令牌的 Claude 凭证 | 不支持，提示改用授权 | CPA 刷新时不获取 Antigravity 邮箱；没有刷新令牌的凭证无法由 CPA 核验身份 |
| 21 | — | 插件模式下新增的 Claude 账号与以邮箱为身份的 Antigravity 账号，回退到补丁镜像后不可用 | 补丁镜像只认它自己写入的身份字段；回退期间需在补丁镜像中重新授权 |
| 22 | 模型鉴定探针走补丁中的专用执行路径，不刷新、不重放 | 普通 Responses 请求；CPA 可能按原生规则刷新令牌并在同一账号上重试；停用或额度锁定的账号不能鉴定 | 上游请求只由 CPA 生成；固定账号由 Gateway 的分配回调保证 |

另有一点运行特性：插件内部的 panic 会被插件自己捕获并转成拒绝；如果有 panic 逃出
插件，整个 CPA 进程会退出并由容器重启，进行中的请求全部中断。这是拒绝服务而不是
放行。

### 升级 CPA

1. 在 `deploy/codex-compat/plugin.lock.json`、`docker-compose.yml` 中更新
   tag、commit 和镜像标签；在 `deploy/codex-compat/plugin/go.mod` 中更新 CPA 版本并运行
   `go mod tidy`。补丁镜像与插件镜像必须固定同一个提交，`source.lock.json` 随补丁镜像
   的升级一起更新。
2. 运行 `python3 -m unittest scripts.tests.test_cpa_plugin_deploy`，确认锁文件、补丁、
   Dockerfile、入口脚本和覆盖文件一致。
3. 构建镜像。构建会依次检查：残余补丁无 fuzz 应用、残余补丁的测试、CPA 自己的插件
   宿主与账号管理器测试、插件单测与竞态检测、针对所产出二进制的端到端测试。任何一步
   失败都不产生镜像。
4. 残余补丁应用失败时，说明 `sdk/cliproxy/service_config.go` 中选择策略的那段代码
   变了，按新代码重新生成补丁并更新锁文件中的 SHA-256。插件编译失败时，说明插件接口
   的类型变了，按编译错误修改。
5. 运行 `./scripts/test-cpa-plugin-image.sh`，然后按上文规程做真实账号验收。

作为参照，2026-11-12 对 v8.0.67 做了一次实际演练。v8.0.59 到 v8.0.67 之间上游改了
289 个文件，其中调度与插件接口所在的 `conductor_selection.go` 改了 648 行。插件源码
未作任何修改，针对 v8.0.67 编译后单测、竞态检测和 9 组端到端测试全部通过。残余补丁
第一版无法应用，因为上游把它修改的 `newRoutingSelector` 拆成了两个函数；把插入点移到
两版都相同的 `switch` 之后即可，现在同一份补丁可以无 fuzz 地应用到 v8.0.59 和 v8.0.67。
对比之下，源码补丁镜像在同一区间需要重基 127 个被修改的上游文件中改动过的那部分。

### 启用与回退

见[运维手册](operations.md#cpa-插件模式的切换与回退)。要点：`.env` 中的
`CPA_INTEGRATION`（`plugin` 或 `patch`）决定镜像，Gateway 与 CPA 总是一起切换；不要用 `-f`
手动叠加覆盖文件。启用前阅读上面的行为差异，特别是第 19–22 项。切换会重置会话绑定，进行中
的对话会被重新分配账号。

### 本地验证

```sh
(cd deploy/codex-compat/plugin && go vet ./... && go test -race -count=1 ./...)
python3 -m unittest scripts.tests.test_cpa_plugin_deploy -v
# 端到端：需要一个打了残余补丁的 CPA 二进制和编译好的插件
CPA_E2E_BINARY=/path/to/cli-proxy-api CPA_E2E_PLUGIN_DIR=/path/to/plugins \
  go test -C deploy/codex-compat/plugin -tags e2e -count=1 ./e2e
```
