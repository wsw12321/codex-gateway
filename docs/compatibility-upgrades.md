# CLIProxyAPI 兼容层升级规程

仓库固定在 CLIProxyAPI `v7.2.150`、commit
`c77b13694318b0897f2c74104ef48aebdf8c34d6`。版本号和 commit 必须作为一组
更新，Docker 构建会验证 tag 指向该 commit。仓库同时固定
`deploy/codex-compat/cliproxy-v7.2.150-multi-account.patch`；旧补丁不能仅靠忽略空白
应用到新版本，本次重基保留安全契约，并适配上游请求头参数、session 亲和与重试
规则的变化。补丁 SHA256 为
`6be9eef68861a4bb05fa2c4ca42b3826d588e90ea87e0ff4c58dd0cb08783fc6`。
构建必须先用 `git apply --check --ignore-space-change` 验证补丁上下文，
再用 `git apply --ignore-space-change` 应用补丁并运行补丁内的聚焦测试，任一步
失败都不得生成镜像。该选项允许上下文空白差异，不能跳过补丁校验或测试。

重基同时保留上游父会话识别，隔离不同调用方，并让 OAuth 与内部请求头防护覆盖
同名头的大小写变体。补丁保留原安全回归，新增 Astra 模型目录、HTTP/SSE/compact
和调用方 session 隔离测试；Astra 上游传输使用模拟服务验证。

兼容层镜像标签固定为 `v7.2.150-c77b1369-6be9eef68861a4bb`，由版本号、commit
前 8 位和补丁 SHA256 前 16 位组成。Compose、校验脚本和 CI 必须使用同一完整
标签，CI 扫描实际构建的镜像。继续使用现有 Go 1.26 构建镜像；本次不修改公共
API、数据库结构和模型价格配置。

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
- 只开放 Bearer 认证的 `GET /internal/upstream-accounts` 和固定 URL 的
  `POST /internal/upstream-accounts/{id}/quota`。后者只接受精确的
  `{"method":"account/rateLimits/read","id":6}`（不得包含 `params` 或其他字段），
  并且只能请求 `https://chatgpt.com/backend-api/wham/usage`，不能接收调用方提供的
  URL、方法或上游 Header，且 Codex HTTP client 不跟随任何重定向。账号列表只输出严格
  `a***@example.com` 形式的 ASCII 脱敏邮箱；额度只输出 `{id,result}` RPC envelope、
  经过严格标识符校验的限额桶、整数使用百分比、分钟窗口和 Unix 秒重置时间，不能返回
  或记录 token、完整邮箱、任意上游显示文本或原始上游响应。
  CLIProxyAPI 完整管理 API 仍保持关闭。
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
