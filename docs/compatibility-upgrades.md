# CLIProxyAPI 兼容层升级规程

兼容层继续使用独立锁定的 Debian slim/glibc 运行镜像。旧第三方 Gemini 插件
和对应补丁已经移除；构建回归检查确认历史 Gemini OAuth 文件被忽略且保留，
Codex 凭证继续加载。Antigravity 使用独立服务、官方 CLI 和 Keyring 卷，详见
[Antigravity 接入说明](gemini-pro.md)。

仓库固定在 2026-09-23 核对的[最新稳定版本 CLIProxyAPI `v7.3.12`](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v7.3.12)、commit
`2eb8dd11d2480c5fd8bc8f2796cec6af534bc3b6`。版本号和 commit 必须作为一组
更新，Docker 构建会验证 tag 指向该 commit。仓库同时固定
`deploy/codex-compat/cliproxy-v7.3.12-multi-account.patch`；本次从 `v7.2.150`
重基，适配上游会话解析、跨优先级选择、凭证更新序号及共享 WebSocket 执行路径，
保留 Gateway 权限分配、账号锁定、并发计数和两账号重试上限。补丁 SHA256 为
`40cc02a0f66b5d28468db7ebc452a31950a2b222bbf40fb8e9ba04b986578134`。
构建必须先用 `git apply --check --ignore-space-change` 验证补丁上下文，
再用 `git apply --ignore-space-change` 应用补丁并运行补丁内的聚焦测试，任一步
失败都不得生成镜像。该选项允许上下文空白差异，不能跳过补丁校验或测试。

上游统一的会话解析器继续识别父会话，Gateway 在解析后的身份上隔离调用方。
重基保留已有模型目录、HTTP/SSE/compact、额度锁定和请求头安全回归，并增加新版
优先级选择、凭证更新序号与 WebSocket 额度信号测试。上游传输使用模拟服务验证。
部署配置显式禁用 `discovery.enabled` 和实验性 `codex.response-steering`，
继续由 Gateway 返回 426 引导客户端使用 HTTPS/SSE。

兼容层镜像标签固定为 `v7.3.12-2eb8dd11-40cc02a0f66b5d28-codex-only`，记录主程序、提交、多账号补丁及仅 Codex 的构建。
Compose、校验脚本和 CI 必须使用同一完整标签，CI 扫描实际构建的镜像。
兼容层构建镜像继续使用 Go 1.26.8；补丁保留 go-git/v6 `v6.0.0-alpha.5`、
go-billy/v6 `v6.0.0-alpha.2` 与 x/text `v0.41.0`，并将 `golang.org/x/crypto`
更新到 `v0.56.0`、`klauspost/compress` 更新到 `v1.18.7`，修复扫描发现的
SSH 死锁及压缩库越界读取问题。Debian 运行时显式安装 `libpcre2-8-0`，
确保继承的基础包获得已发布的安全更新。`CLIPROXY_RUNTIME_IMAGE` 独立锁定 Debian
`bookworm-20260824-slim`，Gateway 的 `RUNTIME_IMAGE` 仍为 Alpine。Responses API
请求契约保持兼容，保留现有价格目录。此次兼容层版本升级没有新增数据库迁移，
Owner 账号状态管理接口仍只管理 Codex。

本次交付范围为仓库升级和构建验证，不代表生产已经切换。真实 OAuth 账号的
Astra 冒烟和生产切换按下文规程执行。

2026-09-23 验证通过：Gateway 单元测试、race、vet，41 项部署脚本回归，
合成配置下的 Compose 校验，三个应用镜像构建及两种 sidecar 隔离运行测试。
CLIProxyAPI 构建覆盖 12 个相关包、账号与 watcher 竞态测试及 13 项执行器安全回归；
容器内二进制报告上述版本和完整 commit。`govulncheck ./cmd/server` 未发现可达或
已导入包漏洞；固定 Trivy 镜像按 CI 的 `--ignore-unfixed --severity HIGH,CRITICAL`
条件扫描，系统包与 Go 二进制均为 0。原版补丁可从升级前的 Git revision 恢复。

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
  `PUT /internal/upstream-accounts/{id}/status` 和固定 URL 的
  `POST /internal/upstream-accounts/{id}/quota`。额度接口只接受精确的
  `{"method":"account/rateLimits/read","id":6}`（不得包含 `params` 或其他字段），
  并且只能请求 `https://chatgpt.com/backend-api/wham/usage`，不能接收调用方提供的
  URL、方法或上游 Header，且 Codex HTTP client 不跟随任何重定向。账号列表只输出严格
  `a***@example.com` 形式的 ASCII 脱敏邮箱；额度只输出 `{id,result}` RPC envelope、
  经过严格标识符校验的限额桶、整数使用百分比、分钟窗口和 Unix 秒重置时间，不能返回
  或记录 token、完整邮箱、任意上游显示文本或原始上游响应。
  CLIProxyAPI 完整管理 API 仍保持关闭。
- 并发接口按稳定账号 ID 返回 `sampled_at` 和 `{id,active_requests}`，只包含实际执行中的
  尝试；首响应等待和长流均计数，每次失败、取消、流关闭与账号切换分别释放。
  禁用账号不会隐藏已有执行。接口只返回计数，缺失账号不代表零。
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

Gateway 必须支持现有用户账号权限、加权分配、账号控制及并发接口；从本次升级前的
仓库版本部署时，无需额外数据库迁移。兼容层
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
