# CLIProxyAPI 兼容层升级规程

兼容层是未修改的 CLIProxyAPI（CPA）加 Gateway 插件，在一个进程中服务 Codex、Claude 和
Antigravity。CPA 的版本、提交、残余补丁与模型目录摘要统一记录在
`deploy/codex-compat/plugin.lock.json`；`docker-compose.yml`、插件的 `go.mod`、
`plugin.Dockerfile` 与 `THIRD_PARTY_NOTICES` 必须与它一致，部署静态校验和 CI 都会核对。

2026-11-13 起，源码补丁镜像（`cliproxy-v8.0.*-gateway.patch`、`deploy/codex-compat/Dockerfile`、
`deploy/patch-cpa.override.yml`）、旧 Antigravity 桥接（`cmd/antigravity-bridge`、
`internal/antigravity`、`legacy-bridge` profile）以及 v7 回退镜像和凭证迁移工具都已删除，
`CPA_INTEGRATION`、`ANTIGRAVITY_TRANSPORT` 两个开关随之取消（[路线图第 8 步](cpa-plugin-roadmap.md#8-改为默认并清理)）。
`.env` 仍选择 `CPA_INTEGRATION=patch` 时，`scripts/compose.sh` 拒绝执行；Gateway 环境中仍有
`CPA_INTEGRATION`（非 `plugin`）、`ANTIGRAVITY_TRANSPORT`（非 `cpa`）或任何 `ANTIGRAVITY_BRIDGE_*`
时拒绝启动。旧补丁镜像的设计与历次重基记录保留在 Git 历史以及各 `docs/cpa-v8.0.*-validation.md` 中。

残余补丁只有 139 行：CPA v8 提供了正式的插件接口，同一段时间里（v8.0.48 → v8.0.59）原补丁改到的
127 个上游文件中有 85 个被上游修改，而插件接口只变了 48 行。上游请求全部由 CPA 生成，插件和
Gateway 不自行构造供应商请求，也不通过补丁改变 CPA 生成请求的方式。
验证过程见 [CPA 插件迁移验证记录](cpa-plugin-migration-validation.md)，后续步骤见
[CPA 插件模式路线图](cpa-plugin-roadmap.md)。

### 组成

| 部分 | 位置 | 作用 |
|---|---|---|
| 残余补丁 | `deploy/codex-compat/residual/cliproxy-plugin-only-selector.patch` | 新增 `routing.strategy: "plugin-only"`：一个对任何选号都返回 `gateway_allocation_unavailable` 的选择器。纯新增，不删改上游行为 |
| Gateway 插件 | `deploy/codex-compat/plugin/` | 独立 Go module，编译为 `codex-gateway-v1.0.0.so`。除 CPA 的插件接口定义外没有第三方依赖 |
| 构建 | `deploy/codex-compat/plugin.Dockerfile`、`plugin.lock.json` | 固定同一 CPA 提交；安全依赖改用 `go get` 升级；构建时运行插件单测和端到端测试 |
| 运行 | `plugin-entrypoint.sh`、`plugin-healthcheck.sh` | 凭证目录检查沿用 `entrypoint.sh`；健康检查要求插件正在提供服务 |
| Gateway | `CPA_MANAGEMENT_KEY`（必填） | 用请求账本做归因（不读取任何响应头）；账号路由走 `/v0/management/gateway/*`，使用管理密钥；生成请求使用数据面密钥 |

插件通过 CPA 的以下扩展点工作：

| 扩展点 | 插件做的事 |
|---|---|
| 前端认证（独占） | 只接受 Gateway 的内部密钥；校验 `X-Codex-Gateway-User`、`-Affinity`、`-Groups`、`-Request`；把调用方作用域（`-Affinity`）交给 CPA，使会话身份按调用方隔离。生成请求缺少其中的用户、作用域或请求令牌时不选号 |
| 调度 | 每次选号都调用 Gateway 的 `eligible`，按账号控制状态、账号池范围、并发名额过滤；已绑定的会话复用原账号，否则按资金层顺序调用 `select`；把最终决定上报到 `attempt` |
| 请求拦截 | 选号后移除全部内部请求头 |
| 请求终态 | 释放并发名额；成功的对话保留名额 5 分钟 |
| 用量 | 识别结构化的 `usage_limit_reached`，锁定账号直到 Owner 重新启用 |
| 管理路由 | Codex、Antigravity、Anthropic 三个命名空间的 capabilities、funding-layers、账号列表、并发快照、启用/禁用、出口同步；Codex 额度（CPA 被动观测）；“CPA 授权管理”页面三个 Provider 的额度 |

`.gateway-account-state`、`.gateway-antigravity-identities`、`.gateway-anthropic-identities`
沿用已删除的补丁镜像的文件名和格式，稳定账号 ID 的算法也相同，所以它写下的凭证卷无需
转换即可继续使用。

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

### Antigravity 模型限制

Antigravity 账号只注册 Gateway 已审并定价的五个模型：`gemini-3-flash`、
`gemini-3.1-flash-lite`、`gemini-3.5-flash-lite`、`gemini-3.8-flash-high`、
`gemini-4-argon-high`。CPA 为该 Provider 内置的目录还列有 Claude、GPT 和图像模型
（v8.0.59 中另有六个），账号实际注册的是这份目录与 Google 为该账号返回的模型列表的交集。
入口脚本在生成的配置中用 CPA 原生的 `oauth-excluded-models` 排除其余部分：

```yaml
oauth-excluded-models:
  antigravity:
    - "claude-*"
    - "gpt-*"
    - "gemini-3.1-flash-image"
```

规则是排除列表，不是允许列表，所以由三处检查保证它与目录保持一致：

- 端到端测试 `TestAntigravityModelCatalog` 在镜像构建中运行。它向正在测试的 CPA 二进制
  查询其内置目录（`GET /v0/management/model-definitions/antigravity`），让假的供应商为
  账号返回整份目录外加已退役和属于其他 Provider 的 ID，然后要求该账号注册的模型恰好是
  这五个。CPA 升级后目录里多出一个模型时，构建失败。
- 部署静态校验核对入口脚本与端到端测试使用同一段配置，规则不遮住任何已审模型，并且已审
  列表与 `internal/config/antigravity.go` 一致。
- 镜像探针通过 CPA 的管理 API 确认运行中的二进制解析到了这条规则，且没有针对其他 Provider
  的规则。

不加限制时，Antigravity 账号会注册 `claude-sonnet-4-6` 等模型。CPA 把同一模型的候选账号
一起交给调度插件，而插件要求一次选号的候选属于同一个 Provider，于是这个模型的请求得到
503。Gateway 不为这些模型定价，本来就不会发出这类请求；排除规则让 CPA 一侧也不依赖这一点。

目录新增模型导致构建失败时，二选一：把它加入排除规则；或者评审后为它定价
（`deploy/pricing-v2.example.json` 及其各份副本），并加入 `AntigravityModels()` 与测试中的
已审列表，目录的范围见 [CPA 原生模型与定价迁移](cpa-native-models.md)。

### Provider 缓存与会话标识

补丁镜像把各 Provider 的缓存键改成了“调用方 + 用户 + 账号”的哈希。插件模式依靠 CPA 原生的
调用方作用域：CPA 推导的会话身份和按会话保存的重放状态都按调用方隔离，但不按账号隔离；
客户端自带的会话标识（如 `prompt_cache_key`）原样发给上游；另有两处可达的缓存的键不含
调用方。逐项结果、影响和准备提交上游的报告见
[CPA Provider 缓存键核对](cpa-provider-cache-audit.md)。

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

### 与已删除的补丁镜像的行为差异

下表保留供排查从补丁镜像迁移过来的部署时参考。补丁镜像已删除，第 21 项不再适用。

| # | 补丁镜像 | 插件模式 | 原因 |
|---|---|---|---|
| 1 | 自定义“哪些错误可以换账号” | CPA 原生规则，上限仍是 4 个账号。实测 429、500、403 会换账号，400 直接返回 | 判定逻辑在执行循环内部，插件接触不到 |
| 2 | 归因写在响应头里 | Gateway 请求账本 | 只依赖调度这一个扩展点，对所有端点一致 |
| 3 | 执行器内拒绝重定向、固定上游主机 | 不保留，依靠出口白名单 | 属于 CPA 内部代码 |
| 4 | Provider 缓存按调用方和账号隔离；发给上游的会话标识换成含用户和账号的哈希 | CPA 原生按调用方隔离，不按账号隔离；客户端自带的会话标识原样发给上游；两处缓存的键不含调用方，利用需要知道对方的上游消息 ID（见[核对记录](cpa-provider-cache-audit.md)） | 属于 CPA 内部代码；缺口提交上游，不进残余补丁 |
| 5 | Antigravity 只注册五个已审模型 | 相同，改用 CPA 原生的 `oauth-excluded-models` 实现，构建时按二进制内置的目录核对 | 排除列表需要随目录维护，由构建中的测试把关 |
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

1. 在 `deploy/codex-compat/plugin.lock.json` 中更新 tag、commit、模型目录摘要
   （上游 `internal/registry/models/models.json` 的 SHA-256）和镜像标签，在
   `docker-compose.yml` 中更新 tag、commit 和镜像标签，在 `plugin.Dockerfile` 中更新模型
   目录摘要，在 `THIRD_PARTY_NOTICES` 中更新版本与提交；在
   `deploy/codex-compat/plugin/go.mod` 中更新 CPA 版本并运行 `go mod tidy`。
2. 运行 `python3 -m unittest scripts.tests.test_cpa_plugin_deploy` 和
   `./scripts/validate-compose.sh`，确认锁文件、残余补丁、Dockerfile、入口脚本和 Compose 一致。
   比较新旧两版的模型目录：Gateway 已定价的模型若被上游删除，按
   [CPA 原生模型与定价迁移](cpa-native-models.md) 处理。
3. 构建镜像。构建会依次检查：残余补丁无 fuzz 应用、残余补丁的测试、CPA 自己的插件
   宿主与账号管理器测试、插件单测与竞态检测、针对所产出二进制的端到端测试。任何一步
   失败都不产生镜像。
4. 残余补丁应用失败时，说明 `sdk/cliproxy/service_config.go` 中选择策略的那段代码
   变了，按新代码重新生成补丁并更新锁文件中的 SHA-256。插件编译失败时，说明插件接口
   的类型变了，按编译错误修改。
5. `TestAntigravityModelCatalog` 失败时，说明 CPA 的 Antigravity 目录变了，按上文
   “Antigravity 模型限制”处理。`TestProviderSessionsAreCallerScoped` 失败时，说明 CPA
   推导会话身份的方式变了。无论是否失败，都按
   [缓存键核对的升级清单](cpa-provider-cache-audit.md#cpa-升级时)比较新旧版本的缓存代码。
6. 运行 `./scripts/test-cpa-plugin-image.sh`，然后按[路线图第 3 步](cpa-plugin-roadmap.md#3-维护窗口内的真实账号验收)
   的清单做真实账号冒烟。

作为参照，2026-11-12 对 v8.0.67 做了一次实际演练。v8.0.59 到 v8.0.67 之间上游改了
289 个文件，其中调度与插件接口所在的 `conductor_selection.go` 改了 648 行。插件源码
未作任何修改，针对 v8.0.67 编译后单测、竞态检测和 9 组端到端测试全部通过。残余补丁
第一版无法应用，因为上游把它修改的 `newRoutingSelector` 拆成了两个函数；把插入点移到
两版都相同的 `switch` 之后即可，现在同一份补丁可以无 fuzz 地应用到 v8.0.59 和 v8.0.67。
对比之下，源码补丁镜像在同一区间需要重基 127 个被修改的上游文件中改动过的那部分。

### 回退

不再有源码补丁镜像可以切换。CPA 升级出现问题时，回到上一个 Gateway revision（它固定了
上一个 CPA 提交）重新构建并部署，见[运维手册](operations.md#cpa-插件镜像的升级与回退)。凭证卷
在 CPA 版本之间不需要转换；不要复制 OAuth 卷，也不要让两个 CPA 实例同时挂载同一个卷。

### 本地验证

```sh
(cd deploy/codex-compat/plugin && go vet ./... && go test -race -count=1 ./...)
python3 -m unittest scripts.tests.test_cpa_plugin_deploy -v
# 端到端：需要一个打了残余补丁的 CPA 二进制和编译好的插件
CPA_E2E_BINARY=/path/to/cli-proxy-api CPA_E2E_PLUGIN_DIR=/path/to/plugins \
  go test -C deploy/codex-compat/plugin -tags e2e -count=1 ./e2e
```
