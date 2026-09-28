# 通过 Antigravity 订阅接入 Gemini

Gateway 使用 Google 官方 AGY 的精确模型名，独立 `antigravity-bridge` 将请求模型
原样传给 `agy --model`，并校验 CLI 返回的模型名。CLI 固定为 `1.2.4`；安装包 URL
和官方 SHA512 位于 [agy.lock.json](../deploy/antigravity-bridge/agy.lock.json)，构建验证
校验值和 `agy --version`。运行时 `AGY_CLI_DISABLE_AUTO_UPDATE=true`，二进制位于
只读文件系统。当前镜像仅支持 `linux/amd64`。

实现依据 Google 官方的[安装与认证](https://antigravity.google/docs/cli/install/)、
[Headless 协议](https://antigravity.google/docs/cli/headless/)、
[权限配置](https://antigravity.google/docs/cli/permissions/)及
[禁用自动更新](https://antigravity.google/docs/cli/troubleshooting/)说明。
不实现或模拟 Antigravity 私有网络协议。Google 上游使用服务器的订阅登录；客户端配置中的
Gateway API key 只用于本服务鉴权。

## 模型权限和结算

模型进入现有模型目录、用户模型权限、API Key 模型范围及价格目录。管理员继续通过
现有界面/API 设置默认和个人模型权限。Owner 可在新增的「Antigravity 账号」页面设置
账号启停、共享或专属用户、分配系数和并发对话上限；普通用户仍可使用获得授权的模型与
账号。正常的请求次数、并发、Token 配额、余额、结算和审计流程继续生效。

固定模型目录如下；同一 ID 用于 `/v1/models`、Responses 请求、Gemini 原生路径、
响应和账单。Bridge 按每个账号的 `agy models` 结果公布可用集合，请求只分配给支持
该模型且通过用户权限检查的账号，不要求每个账号支持全部七个模型。

| AGY 模型 ID | Gemini API 家族 | 每百万未缓存输入 / 缓存读取 / 输出 Token（USD） |
| --- | --- | --- |
| `gemini-3.8-flash-high` | Gemini 3.8 Flash | $0.75 / $0.075 / $3.75 |
| `gemini-3.8-flash-medium` | Gemini 3.8 Flash | $0.75 / $0.075 / $3.75 |
| `gemini-3.7-flash-high` | Gemini 3.7 Flash | $0.75 / $0.075 / $3.75 |
| `gemini-3.7-flash-medium` | Gemini 3.7 Flash | $0.75 / $0.075 / $3.75 |
| `gemini-3.6-flash-high` | Gemini 3.6 Flash | $0.75 / $0.075 / $3.75 |
| `gemini-3.6-flash-medium` | Gemini 3.6 Flash | $0.75 / $0.075 / $3.75 |
| `gemini-3.1-pro-high` | Gemini 3.1 Pro | 输入 ≤200K：$2 / $0.20 / $12；输入 >200K：$4 / $0.40 / $18 |

价格依据 [Gemini API Standard 付费价格](https://ai.google.dev/gemini-api/docs/pricing)，
账单模式为 `gemini_api_token_equivalent`。每个 ID 有独立价格快照，输出包括思考 Token；
这些是按对应 API 家族价格计算的本地等价费用，不表示 Antigravity 订阅实际收费。
统计包含 Agent 系统提示和上下文开销，可能明显高于客户端提供的文本 Token 数量。
六个 Flash 模型的上述价格有效至 **2026-12-31**；运维须提前安排，在 **2027-01-01** 新价格生效时手动将
价格目录更新为每百万输入 / 缓存读取 / 输出 **$1.50 / $0.15 / $7.50** 并更新目录日期。
现有配置不会自动按日切价；改价只作用于新准入请求，已固化快照和历史账单不重算。

迁移 `0023` 将七个模型的默认权限及现有用户权限设为禁用，管理员须重新授权。
旧 `gemini-3.1-pro-preview` 权限、受限 API Key 白名单与倍率均不继承；新模型倍率默认
为 `1`。受限 Key 需要重新签发包含新 ID 的白名单，旧模型退出可管理目录。

AGY stream-json 的 `input_tokens` 是未缓存输入，`cache_read_tokens` 是单独的
缓存输入，允许缓存量大于未缓存量；原始 `total_tokens` 为未缓存输入加输出。
Bridge 在读取 CLI 结果时归一一次：Gateway 输入量为
`input_tokens + cache_read_tokens`，总量为 `total_tokens + cache_read_tokens`，
缓存量同时保留为输入明细。`output_tokens` 已包含 `thinking_tokens`，因此输出量
不再次相加，思考量另写入 reasoning 明细。必填字段、非负数、原始总量关系及
整数溢出检查均保留；不截断或丢弃缓存量。200,000 Token 的价格阈值按包含缓存的
输入总量判断。历史结算和在途请求的价格快照保留原值。

Gemini 原生响应将答案输出映射为 `candidatesTokenCount`，思考量映射为
`thoughtsTokenCount`。Gateway 按候选输出加思考计入输出 Token；
`cachedContentTokenCount` 是输入的子集，不再加到输入总量。两种协议共用原有账单，
同一请求只结算一次。

## 使用精确模型名请求 Gateway

链路为 `API 客户端 → Gateway → Antigravity Bridge → 服务器订阅账号 → Google`。
先由管理员配置同名路由并重新授予用户模型权限，再以 Gateway key 查询 `GET /v1/models`，
选择已获授权且有可用账号的精确 ID。以下示例使用 `gemini-3.1-pro-high`，可替换为
目录中的任一已授权模型。

在 Bash 中运行；Key 在提示后输入，避免写入命令历史或 curl 进程参数：

```bash
export GATEWAY_BASE_URL='https://gateway.example.com'
read -r -s -p 'Gateway API Key: ' GATEWAY_API_KEY
printf '\n'
printf 'header = "Authorization: Bearer %s"\n' "$GATEWAY_API_KEY" |
  curl --fail-with-body --silent --show-error --config - \
    -H 'Content-Type: application/json' \
    --data '{"model":"gemini-3.1-pro-high","input":"Reply with exactly OK.","store":false}' \
    "$GATEWAY_BASE_URL/v1/responses"
```

原生 Gemini 请求示例（沿用上述终端变量）：

```bash
printf 'header = "Authorization: Bearer %s"\n' "$GATEWAY_API_KEY" |
  curl --fail-with-body --silent --show-error --config - \
    -H 'Content-Type: application/json' \
    --data '{"contents":[{"role":"user","parts":[{"text":"Reply with exactly OK."}]}]}' \
    "$GATEWAY_BASE_URL/v1beta/models/gemini-3.1-pro-high:generateContent"
```

管理界面“使用指导”也提供 agy CLI 的官方安装和一键持久配置，以官方 AGY `1.2.12`
为验收基线。启动命令为 `agy --model gemini-3.1-pro-high`，完整步骤见
[客户端配置](client-config.md#agy-cli)。

仅原生 Gemini 入口将 `gemini-3.1-pro-preview`、`gemini-3.1-pro-preview-customtools`
和标题请求 `gemini-3.1-flash-lite-preview` 统一映射到 `gemini-3.1-pro-high`。
权限、额度、路由、响应校验及计费都使用实际模型，标题等辅助请求同样计入用量。
这些兼容名称不加入模型目录，Responses 接口仍拒绝别名。
AGY `1.2.12` 的 `gemini-3.8-flash` 原生请求按 `generationConfig.thinkingConfig.thinkingBudget`
选择实际模型：`4000` 对应 `gemini-3.8-flash-medium`，省略或 `-1` 对应
`gemini-3.8-flash-high`。接受该版本 Flash 默认的 `maxOutputTokens: 65536`；
其他自定义生成控制仍按原规则校验。权限、路由、响应模型名及计费均使用选中的实际 ID。
服务器内部使用官方 AGY 调用订阅。Gateway key 不发送给 Bridge 或 Google，
Gateway→Bridge 使用独立凭据。

## Gemini 原生 API

支持以下接口，仅转发给 Antigravity Bridge：

```text
POST /v1beta/models/{model}:generateContent
POST /v1beta/models/{model}:streamGenerateContent?alt=sse
```

使用 `x-goog-api-key: <Gateway key>` 或 `Authorization: Bearer <Gateway key>`，
只能选一种；重复凭据或同时从多个来源提供凭据会被拒绝。错误使用 Gemini JSON
格式，保留 HTTP 状态及适用的 `Retry-After`。鉴权、模型授权、并发租约、配额准入、
用量记录和结算与 Responses 共用，统计分别记录 `gemini.generateContent` 和
`gemini.streamGenerateContent`。

支持文本、系统提示、完整客户端工具参数 schema（`parametersJsonSchema` 或
`parameters`）、`functionCall` 和 `functionResponse`，保留工具名称、参数、调用 ID
与结果关联。兼容 AGY 将工具结果放在 `role=model` 的格式。返回调用必须来自客户端
声明的工具；工具在用户本机执行，不需要伪造思考签名。服务器隔离进程仍禁止执行工具。

原生请求沿用 1 MiB 上限，按账号执行并发对话限制（默认每账号 1 个名额；无法识别对话的请求各占一个）。接受实测 AGY `1.2.4` 主会话默认生成参数，
推理行为由所请求的 AGY 模型决定；无法兑现的自定义生成控制会返回明确错误。首版不支持
图片附件、精确 `countTokens` 或云端内置工具。新版 Gateway 与 Bridge 协商增量 Gemini
SSE：CLI 初始化并验证模型及严格权限后开始响应，普通文本按 `text_delta` 逐段发送，
不再等待完整生成；等待思考或完整工具参数时，每 10 秒发送一个空 Gemini candidate
保活。AGY `1.2.12` 不接受 `: keepalive` 注释，因此不能改用 SSE 注释心跳。

潜在工具调用的 JSON 参数继续缓冲，完整校验并确认进程成功退出、凭据写回成功后才
交给客户端执行。最终结果必须与已发送文本前缀一致，用量只在最终结果及流尾验证通过
后结算；保活不会被当作首 Token。成功流以 EOF 结束，没有 Responses 的 `[DONE]`。
流开始后的错误发送脱敏的 `event: error`，网关记录失败并释放配额后中止 HTTP 流，
避免 AGY 忽略普通 JSON 错误或把半份结果视为成功。取消或超时会结束隔离进程并清理
请求数据。已经显示的文本可能属于随后失败的请求，不能只凭 HTTP 200 判断完成。

增量协议通过内部头协商，不向客户端暴露；任意一端仍为旧版时沿用原先的完整结果
缓冲路径。JSON `generateContent` 和 Antigravity 的 Responses 路径仍返回最终结果。
部署 Gateway 与 Bridge 两个新镜像后，AGY 原生接口才会启用上述流式行为，无新增配置
或数据库迁移。排查证据与本地验证见 [499 与流式修复记录](agy-499-investigation.md)。

## API 范围

`POST /v1/responses` 支持文本字符串、消息数组、`instructions`、`stream`、`tools` 工具列表、
以及 Codex CLI 客户端的会话参数与历史 `function_call` / `function_call_output` 结构。Bridge
会自动转换客户端请求格式：将声明的客户端工具注入会话提示词，若模型输出工具调用结构则转换为标准 Responses
API `function_call` 事件；非文本内容（文件、图片）仍拒绝并返回明确的 `400 antigravity_input_unsupported`。
Antigravity 模型的 compact 返回 `501 endpoint_not_supported`。

每个请求创建独立进程和空工作目录，提示词只经 stdin NDJSON 传入；使用
`--input-format stream-json --output-format stream-json --model <请求中的精确模型 ID>
--print-timeout 5m --disable-slash-commands`。CLI 日志、HOME、会话和缓存留在请求
临时目录，退出后删除。每次调用只从加密 Keyring 恢复完整认证文件；CLI 退出且进程组
清理后写回合法的凭据更新，随后删除临时目录。同账号的凭据恢复和写回分别串行，
不同账号使用独立的加密凭据命名空间。全部授权账号均无法接收新的并发对话时返回
`429 upstream_concurrency_exceeded`；Bridge 另设最多 64 个同时处理请求的资源保护上限。
文件、命令、URL、非沙箱执行和 MCP 权限全部显式拒绝。内部工具事件导致整个进程组
终止并返回 `502 upstream_protocol_error`，不会转发工具内容。

JSON 只取最终成功 `result`。首版 SSE 在 CLI 退出并验证完整协议后发送标准 Responses
事件，包含 `response.completed` 和 `[DONE]`，因此客户端收到首个事件的时间接近完整
生成完成时间。这保证错误或尾部工具事件仍可返回明确 HTTP 502。取消和超时终止进程组
并清理请求数据。认证失效映射 503，订阅额度耗尽映射 429，超时映射 504，进程或输出
协议失败映射 502；原始 stderr 不发送给客户端。

## 部署和登录

先更新现有配置中的价格目录，保留 `ANTIGRAVITY_MODEL_ROUTES_JSON={}`；默认不会
把任何模型转向尚未验收的 Bridge。Bridge 服务本身不使用 Compose profile。

```sh
./scripts/bootstrap-secrets.sh
./scripts/validate-compose.sh
./scripts/compose.sh build gateway codex-compat antigravity-bridge
./scripts/antigravity-login.sh
```

新增两个独立 `0640` secret：`antigravity_bridge_api_key` 用于 Gateway→Bridge Bearer
认证，`antigravity_keyring_password` 用于解锁加密 Keyring。不得复用 Sidecar Key。
脚本持有 `.antigravity-login.lock`，停止 Bridge，使用同一镜像和独立 Keyring 卷启动
一次性登录容器。按官方远程登录流程在浏览器授权并粘贴验证码，成功后输入 `/exit`。
内部 `auth-login` 命令随后保存认证文件。脚本再启动独立容器，由 `auth-verify` 恢复
凭据并检查固定 CLI 版本、`agy models`、`agy --print /usage` 和最小文本生成。
全部通过后才启动 Bridge，并执行 readiness、账号模型目录及每个实际可用模型的 JSON 和 SSE 验收。
最终成功标志只有以下一行；浏览器授权成功或 CLI 显示已登录不代表整个验收完成：

```text
Antigravity login persisted; readiness, JSON and SSE passed.
```

仅交互授权阶段保留 TTY；后验容器使用 `compose run -T`、`TERM=dumb`，无需输入的
命令连接 `/dev/null`，生成请求仍使用 stdin 协议管道。脚本退出、失败或收到信号时
恢复原终端设置。失败时 Bridge 保持停止，Codex 不受影响。

错误输出使用固定阶段、类别和退出码，例如：

```text
antigravity: stage=credential_restore category=keyring_failed exit_code=1
```

`credential_restore`、`credential_save` 分别定位恢复、写回；`models`、`usage`、
`generation` 定位 CLI 检查；`readiness`、`http_models`、`http_json`、`http_sse`
定位服务验收。诊断不会打印令牌、授权 URL、模型回复或原始 CLI stderr；授权阶段
所需的交互界面仍由 CLI 显示。保存失败应先检查 Keyring 解锁、卷权限和可写性，再
重新运行登录脚本，不能把脚本的非零退出当成登录成功。

容器以 UID 10002 运行，根文件系统只读；D-Bus 和 GNOME Secret Service 在容器内启动。
Keyring 唯一持久挂载为 `antigravity_keyring:/var/lib/antigravity/keyrings`，仅 Bridge
持有，文件为 `0600`，目录为 `0700`。启动必须确认持久 login collection 已解锁。
没有宿主目录、Docker Socket、原 OAuth 卷挂载；临时 HOME 和运行目录使用私有 tmpfs。

`agy 1.2.4` 的认证来源是 `$HOME/.gemini/antigravity-cli/antigravity-oauth-token`
文件，并不会自动把该文件保存到 Keyring。Bridge 将完整文件编码后，通过
`secret-tool` 的 stdin/stdout 读写已解锁的持久 `login` 集合；固定应用属性和格式
版本标记用于找到凭据记录。为兼容工具的输入上限，文件分块保存，最后提交完整性
清单，避免半次写入替换原凭据。编码本身不提供保密性，磁盘保密由带独立口令的加密
Keyring 提供。文件中的 OAuth 令牌、项目、地区、订阅信息完整保留，兼容当前包装
结构和旧版直接 OAuth Token 结构；设置、聊天记录、其他文件和缓存不持久化。
每次 CLI 调用都恢复到新的 HOME，目录为 `0700`、文件为 `0600`，拒绝符号链接、
超过 1 MiB 或结构无效的认证文件。

刷新完成后即使请求失败、超时或取消，也在清理 CLI 子进程后以独立的最多 5 秒
收尾时限保存合法更新。文件缺失或损坏不会覆盖 Keyring 中已有的凭据；写回失败
会关闭 readiness，成功模型结果也不会返回给客户端。请求和健康检查仍串行执行，
登录由停服及文件锁排他保护。修复前若认证仅留在已退出容器的临时 HOME，现有
Keyring 卷不会凭空补回它；应重新执行 `./scripts/antigravity-login.sh`，直到看到
上述最终成功标志。不得通过共享 HOME 或复制 settings/history 来修复登录。

Keyring 不进入数据库备份或计划迁机复制；灾备和迁机后重新运行隔离登录脚本。
若仅为受控故障排查临时导出 Keyring，必须同时保护对应口令，不能单独重生成密码。

登录及健康检查通过后，在 `.env` 中配置精确路由并重建 Gateway 容器：

```dotenv
ANTIGRAVITY_MODEL_ROUTES_JSON={"gemini-3.8-flash-high":"gemini-3.8-flash-high","gemini-3.8-flash-medium":"gemini-3.8-flash-medium","gemini-3.7-flash-high":"gemini-3.7-flash-high","gemini-3.7-flash-medium":"gemini-3.7-flash-medium","gemini-3.6-flash-high":"gemini-3.6-flash-high","gemini-3.6-flash-medium":"gemini-3.6-flash-medium","gemini-3.1-pro-high":"gemini-3.1-pro-high"}
```

```sh
./scripts/compose.sh up -d gateway codex-compat antigravity-bridge
```

`/v1/models` 合并健康上游目录；重复模型 ID 拒绝处理。Bridge 不可用时其模型临时隐藏，
已有直接请求返回 `503 upstream_unavailable`，其他模型继续使用 Codex。账号只公布
实际可用模型，所有账号都没有可用目标模型或认证不可用时 readiness 失败。Gateway
启动不依赖 Bridge 健康状态。Gateway 可只启用上述同名路由的子集，默认保持空目录；
Bridge 的 Compose 配置固定完整七个同名路由。

## 多账号管理和轮换

Owner 控制台新增「Antigravity 账号」页面，支持历史区间／全部历史的请求、错误、Token
和 API 等价费用统计，近 24 小时费用占比与分配系数、启停、实时并发对话名额及上限、
共享／专属授权用户。该页面和原「上游账号」页面的数据、权限设置及分配费用窗口分别
按 Antigravity 与 Codex 隔离。敏感修改使用已有的二次验证和审计流程。

在服务器项目目录依次添加账号；名称只允许 1–32 位小写字母、数字、下划线及连字符，
首位必须为字母或数字：

```sh
./scripts/antigravity-login.sh team-alpha
./scripts/antigravity-login.sh team-beta
```

每次执行官方远程授权及独立容器验收。无参数时使用 `default`，现有单账号加密凭据
自动归入此名称。再次使用同一名称更新该位置的登录；历史归属按稳定的本地账号名称
保留。添加／重新登录期间脚本会停止整个 Bridge，其他模型继续使用 Codex。网页只显示
名称和可取得的脱敏邮箱，OAuth 凭据仍只保存在服务器加密 Keyring 中。

新账号会在下一次推理请求或页面刷新时自动同步。每次分配先验证 API Key 所属用户的
账号权限、请求模型可用性与当前并发上限，再按近 24 小时已结算费用趋近分配系数比例；无历史或差额相同
时按系数随机分配。系数为 0 的账号不再接收新请求。请求遇到认证失败、额度／速率限制
或暂时不可用时，在输出前尝试其他符合权限的账号；限流账号冷却 60 秒后可再次尝试。
网关回调失败或旧版 Bridge 缺少账号权限协议时拒绝请求。返回的最终／最后尝试账号
写入现有用量及账单归属，不向客户端暴露内部账号头。

AGY `1.2.12` Gemini 模式实测会在 `systemInstruction.parts[].text` 中携带完整行
`Conversation ID: <UUID>`；主请求和工具续轮共享 UUID，新对话更换 UUID。Bridge 仅从
这些系统提示文本提取标记，将有效 UUID 统一为小写；重复的相同 UUID 可以合并，缺失、
格式错误或多个 UUID 冲突时按独立请求处理。该标记属于提示词文本，是版本兼容规则，
并非官方保证稳定的协议字段；不从普通消息、请求头或工具调用 ID 推断对话。

账号并发按**活跃对话名额**计数。同一 API Key、同一 UUID 的重叠请求在同一账号共用
一个名额，最后一个请求结束后立即释放；不同 API Key 分别计数。不含 UUID 的标题请求、
其他无法识别对话的请求及 Responses 请求各占一个名额，即使正文完全相同也不合并。
同一活跃对话优先复用正在使用的账号，每次仍检查账号权限、模型可用性、启停和冷却。
故障切换导致一个对话同时使用多个账号时，每个账号分别占一个名额。空闲对话不占名额，
也不保留长期账号绑定；服务器端每个请求仍创建独立的临时 CLI 会话。

管理员降低上限不会中断已开始的请求，已有活跃对话仍可追加请求；占用降到上限以下
后才接收新对话。停用账号或将分配系数设为 0 会阻止该账号后续请求，包括活跃对话的
续轮。Gateway 的请求级配额和 Bridge 的 64 请求保护上限仍按实际请求数计数。

实时并发接口保留 `active_requests` 字段名，数值为上述名额占用数；无需数据库迁移。
用量记录与监控使用 API Key 隔离后的对话哈希归组，不新增原始 UUID 日志或持久化字段，
内部归因响应头不向客户端透出。

官方 CLI 未提供本实现可以可靠归一为 Codex 配额窗口的数据，因此页面不提供剩余
百分比或即时额度查询，套餐显示 Antigravity；仍显示限流冷却、账号状态和网关实际
统计。上线前未归属到账号的历史请求没有可靠的供应商维度，不回填为 Antigravity
账号用量。账号统计与权限随 PostgreSQL 备份保留，Google 登录仍须在灾备后重新授权。

升级时同时更新 Bridge 与执行 `0021` 数据迁移的 Gateway。内部账号选择回调使用
`ANTIGRAVITY_GATEWAY_URL`（默认 `http://gateway:8080`），只走内部网络，不经过出口
代理；回调和管理接口使用独立的 Bridge secret。部署后的真实 Google 多账号登录和
额度切换仍需使用站点账号验收。本地测试使用合成凭据和确定性的 CLI 替身。

## 出口验收

两个上游使用独立内部网络，只有 Squid 能访问外部网络。Codex 仅允许现有两个 OpenAI
域名。Antigravity 出口允许 `accounts.google.com`、`oauth2.googleapis.com`、
`www.googleapis.com`、`cloudcode-pa.googleapis.com`、`daily-cloudcode-pa.googleapis.com`、
`aicode.googleapis.com`、`businessaicode.googleapis.com`、`generativelanguage.googleapis.com`
等模型交互与订阅鉴权必需域名。

配置非空 `CODEX_RELAY_IP` 后，Codex 与整个 Antigravity Bridge 共用同一 B 服务器
出口，包括 Bridge 的登录、刷新、模型检查和生成请求；B 故障时请求失败，不回退 A。
变量名为兼容旧部署而保留。未启用中转时两者均从 A 直连。首次配置见
[双服务器中转](openai-relay.md)，旧版仅 Codex 经 B 的站点见
[升级指南](relay-upgrade.md)。用户本地浏览器的授权流量不随服务器代理改变。

Squid 为 Antigravity 网络记录 CONNECT 目标、时间、状态及实际转发路径，不记录
TLS 内容或认证头。验收登录、刷新、模型检查、`/usage` 和生成请求时检查：

```sh
./scripts/compose.sh exec -T egress-allowlist tail -n 100 /var/log/squid/access.log
```

启用中转时，成功 CONNECT 应显示 `PARENT/10.77.0.2`，并可在 B 对照同一目标和时间；
不能仅凭 Bridge 健康状态断言流量已经经 B。按升级指南执行 JSON/SSE 冒烟和故障验收。
对被拒绝目标核对实际 DNS/CONNECT/SNI 和官方用途，再将必要的精确主机名同步加入
`deploy/egress/squid.conf` 与 `deploy/relay/squid.conf`，更新
`scripts/validate-compose.sh` 的固定清单及 relay 回归测试并审核差异。
不要开放 `*.googleapis.com` 或通用 Google 子域；安装包和自动更新域名不需要运行时出口。
在真实流量完成前不能宣称出口清单已经充分或生产验收通过。

## 验收和回滚

开发验证覆盖假 CLI 的 JSON、SSE、NDJSON 分段、认证、限额、协议错误、工具事件、取消、
超时、usage、路由和共用权限/结算，以及认证恢复、刷新写回和失败后的收尾。
`./scripts/test-antigravity-image.sh <bridge-image>` 使用无网络容器、全新的 HOME 和
一次性 Keyring 卷，经真实凭据管理代码导入合成认证文件、刷新后跨容器恢复，并检查
错误口令拒绝、卷中没有明文或仅编码的凭据，以及输出不泄露凭据或模型内容。
该脚本用假 CLI 代替 Google，不证明真实账号登录或生产出口可用。
部署前运行完整 Go suite、race、vet、Compose 校验与
三个镜像构建。真实账号还需验证重启、续期、重新登录、限额、取消后无残余进程、磁盘残留
和出口域名。真实 Google 上游验收须独立执行，不应把单元测试或本地 AGY 协议探测视为替代。

本次模型与计费升级须在维护窗口协调切换新 Bridge 与执行 `0023` 迁移的 Gateway，
避免旧 Gateway 的 preview 路由请求新 Bridge，或新 Gateway 向旧 Bridge 发送新模型名。
按下列步骤执行：

1. 停止新增 Gemini 请求，等待在途请求完成；完成 PostgreSQL 加密备份和恢复演练，
   保存旧 revision、镜像和配置。不得把登录凭据纳入仓库或普通备份。
2. 更新价格 JSON 与同名路由，构建配套镜像；先保持 Gateway Gemini 路由为空，
   在停服窗口切换 Bridge 和 Gateway，确认 `0023` 迁移成功。
3. 逐账号查询实际模型目录并运行 `antigravity-smoke <账号名>`。脚本只遍历该账号
   实际可用的受支持模型，对每个模型验证 JSON、SSE 和同名回显，不要求每账号全部七个。
4. 管理员重新授权新模型，按需设置倍率并重新签发受限 API Key，启用已验收的同名路由。
   使用新 Key 对实际可用模型完成 Responses 和原生 API 冒烟，核对 Standard 计费、
   Token 与账单；检查原生别名按实际模型扣费、Responses 拒绝别名，取消后无残余进程或租约。
5. 对照前后历史账单和已固化 reservation，确认没有用新价格重算旧费用。
   建立 2027-01-01 前的 Flash 手动改价提醒。

迁移是 forward-only。只关闭 Gemini 时可清空路由并停止 Bridge；这不会撤销 `0023`。
若需恢复旧二进制，须停止全部数据库写入，把升级前备份恢复到新的隔离数据库卷，
再切回匹配的 Gateway、Bridge、配置与 revision；不能只切旧镜像，也不能保留升级后的
写入同时恢复旧权限。升级后的新增账务写入必须先完成核账和保全。
真实订阅生成、Google 出口及生产部署需在站点环境独立验收。

旧第三方 Gemini 插件、补丁和登录脚本已移除，Codex Sidecar 禁用插件，历史 Gemini OAuth
文件不再加载且不会自动删除。`codex_oauth` 卷继续保留原内容。

紧急关闭 Gemini 时把路由恢复为 `{}`，重建 Gateway 并停止 Bridge；Codex 路由不受影响，
数据库仍保留 `0023`，旧版本恢复遵循上述备份步骤：

```sh
./scripts/compose.sh up -d --no-deps gateway
./scripts/compose.sh stop antigravity-bridge
```

若真实环境中 Secret Service 无法稳定解锁，先停止 Bridge；在专用 Linux 用户下部署同一
桥接程序和同样的权限配置，以受限内部监听接入。不能通过共享桌面 Keyring、挂载宿主 HOME
或放宽容器权限临时绕过隔离。
