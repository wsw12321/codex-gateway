# CLIProxyAPI 兼容层升级规程

Gemini 的默认 Alpine 运行方案目前未通过真实 ABI 加载检查；已验证的 Debian
候选及待确认事项见 [Gemini 验证记录](gemini-validation.md)。不要跳过构建检查部署。

Gemini CLI 插件固定到 `19d9868ffa24e94a2919ea1d1a761afa634de669`，与原主程序
一同使用 musl/CGO 构建，并在 Alpine 中加载真实共享库完成 ABI、登录参数和
OAuth 识别验证。新增 SSE 组帧及回答/思考 Token 映射补丁，详见
[Gemini Pro 接入说明](gemini-pro.md)。升级须同步新价格目录，继续保留原 GPT
价格和历史账务。

仓库固定在 CLIProxyAPI `v7.2.150`、commit
`c77b13694318b0897f2c74104ef48aebdf8c34d6`。版本号和 commit 必须作为一组
更新，Docker 构建会验证 tag 指向该 commit。仓库同时固定
`deploy/codex-compat/cliproxy-v7.2.150-multi-account.patch`；旧补丁不能仅靠忽略空白
应用到新版本，本次重基保留安全契约，并适配上游请求头参数、session 亲和与重试
规则的变化。补丁 SHA256 为
`00633c2417755730b8abe7c5d273135a43d449d1952c3a1d489b7fbae9e32e7f`。
构建必须先用 `git apply --check --ignore-space-change` 验证补丁上下文，
再用 `git apply --ignore-space-change` 应用补丁并运行补丁内的聚焦测试，任一步
失败都不得生成镜像。该选项允许上下文空白差异，不能跳过补丁校验或测试。

重基同时保留上游父会话识别，隔离不同调用方，并让 OAuth 与内部请求头防护覆盖
同名头的大小写变体。补丁保留原安全回归，新增 Astra 模型目录、HTTP/SSE/compact
和调用方 session 隔离测试；Astra 上游传输使用模拟服务验证。

兼容层镜像标签固定为 `v7.2.150-c77b1369-00633c2417755730-gemini19d9868-708d3052c0caad8a`，由主程序版本和提交、多账号补丁 SHA256 前 16 位、Gemini 插件提交前 8 位
及两份新增补丁（主程序、插件顺序拼接）的 SHA256 前 16 位组成。Compose、校验脚本和 CI 必须使用同一完整
标签，CI 扫描实际构建的镜像。继续使用现有 Go 1.26 构建镜像和 Alpine 运行镜像。Responses API 与数据库结构
保持兼容，新增 Gemini 价格目录；原有 Owner 账号状态管理接口仍只管理 Codex。

本次交付范围为仓库升级和构建验证，不代表生产已经切换。真实 OAuth 账号的
Astra 冒烟和生产切换按下文规程执行。

该补丁属于部署安全边界，而不是可选功能：

- 使用 `round-robin` 加一小时 session affinity。Gateway 发送的
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
  `PUT /internal/upstream-accounts/{id}/status` 和固定 URL 的
  `POST /internal/upstream-accounts/{id}/quota`。额度接口只接受精确的
  `{"method":"account/rateLimits/read","id":6}`（不得包含 `params` 或其他字段），
  并且只能请求 `https://chatgpt.com/backend-api/wham/usage`，不能接收调用方提供的
  URL、方法或上游 Header，且 Codex HTTP client 不跟随任何重定向。账号列表只输出严格
  `a***@example.com` 形式的 ASCII 脱敏邮箱；额度只输出 `{id,result}` RPC envelope、
  经过严格标识符校验的限额桶、整数使用百分比、分钟窗口和 Unix 秒重置时间，不能返回
  或记录 token、完整邮箱、任意上游显示文本或原始上游响应。
  CLIProxyAPI 完整管理 API 仍保持关闭。
- 状态接口只接受 `{"enabled":true}` 或 `{"enabled":false}`，返回确认后的稳定
  账号 ID 与 `available`／`unavailable`。手动禁用与生成请求的结构化
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
- 安全清理后的 401、429、5xx；
- usage 分散在多个 SSE chunk 时的 input、cached input、output、reasoning；
- 客户端断开取消、首 token 超时和总超时；
- OAuth refresh 成功、refresh 失效和 reauthentication required；
- 禁用／启用后的分流、持续额度锁定、普通 429 到期恢复、再次耗尽；重启、重复文件
  替换、并发控制、旧请求结果、状态读写失败、账号消失及全部账号不可用；
- sidecar 不记录请求/响应正文或 token，完整管理 API 无法从兼容层网络远程访问，
  窄内部接口只返回脱敏、规范化字段。

仓库验证至少执行 `./scripts/validate-compose.sh`、
`./scripts/compose.sh build gateway codex-compat`、`go test -count=1 ./...`、
`go test -race -count=1 ./...` 和 `go vet ./...`，并检查构建内现有安全回归测试通过。
隔离测试的模型目录和 HTTP 契约验证不能替代真实 OAuth 冒烟。

切换生产前运行加密数据库备份，但不要复制 OAuth volume。持有设备登录锁，
停止旧 sidecar，确认其容器状态不是 running，才允许候选版本挂载现有 token。
完成 Astra 模型列表和至少一个已授权 Plus/Pro 账号的普通及 SSE Responses、
compact 人工冒烟后才能恢复 Gateway 流量。

失败回滚时先停止候选实例，再启动旧实例。任何时刻都不允许两个 sidecar
共享同一组 refresh token。若任一 token 已因候选版本失效，保持服务关闭并重新
执行 `scripts/codex-device-login.sh`。

账号控制上线后，回滚镜像也必须理解 `.gateway-account-state`。不支持该文件的
旧镜像会忽略禁用与额度锁定，不能直接恢复流量；保持 Gateway 停流量，使用包含
账号控制补丁的修复镜像。不要通过删除状态文件或复制 OAuth 卷来恢复账号。
