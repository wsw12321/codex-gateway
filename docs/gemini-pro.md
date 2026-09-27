# 通过 Antigravity 订阅接入 Gemini Pro

Gateway 对外模型仍为 `gemini-3.1-pro-preview`，独立 `antigravity-bridge` 使用
Google 官方 `agy` 调用 `gemini-3.1-pro-high`。CLI 固定为 `1.2.4`；安装包 URL
和官方 SHA512 位于 [agy.lock.json](../deploy/antigravity-bridge/agy.lock.json)，构建验证
校验值和 `agy --version`。运行时 `AGY_CLI_DISABLE_AUTO_UPDATE=true`，二进制位于
只读文件系统。当前镜像仅支持 `linux/amd64`。

实现依据 Google 官方的[安装与认证](https://antigravity.google/docs/cli/install/)、
[Headless 协议](https://antigravity.google/docs/cli/headless/)、
[权限配置](https://antigravity.google/docs/cli/permissions/)及
[禁用自动更新](https://antigravity.google/docs/cli/troubleshooting/)说明。
不实现或模拟 Antigravity 私有网络协议。Google 上游使用服务器的订阅登录；客户端配置中的
`GEMINI_API_KEY` 是 Gateway 签发的 key。

## 模型权限和结算

模型进入现有模型目录、用户模型权限、API Key 模型范围及价格目录。管理员继续通过
现有界面/API 设置默认和个人权限；没有 Owner 限制、Antigravity 用户白名单或独立
启用开关。正常的请求次数、并发、Token 配额、余额、结算和审计流程继续生效。

沿用当前 Gemini API 等价价格：输入不超过 200,000 Token 时，每百万输入/缓存输入/
输出 Token 为 `$2 / $0.20 / $12`，超过时为 `$4 / $0.40 / $18`。这些是本地成本
统计价格，不表示 Google 对订阅的实际收费。统计包含 Antigravity Agent 系统提示和
上下文开销，可能明显高于客户端提供的文本 Token 数量。

`input_tokens` 映射输入量，`cache_read_tokens` 写入缓存明细；`agy 1.2.4` 的
`output_tokens` 已包含 `thinking_tokens`，因此输出量不再次相加，思考量另写入
reasoning 明细。总量为输入加输出。历史结算快照保持不变。

Gemini 原生响应将答案输出映射为 `candidatesTokenCount`，思考量映射为
`thoughtsTokenCount`。Gateway 按候选输出加思考计入输出 Token；
`cachedContentTokenCount` 是输入的子集，不再加到输入总量。两种协议共用原有账单，
同一请求只结算一次。

## 本机 AGY 配置

链路为 `本机 AGY → Gateway 地址与 key → Antigravity Bridge → 服务器订阅账号 → Google`。
管理员须已启用下文的 Bridge 路由，并给用户及 API Key 授予公开模型
`gemini-3.1-pro-preview` 的权限。客户端兼容范围以 AGY `1.2.4` 的文本和本地编程工具为准。

在现有 `~/.gemini/antigravity-cli/settings.json` 中合并下列顶层字段，保留其他设置：

```json
{
  "modelProvider": "gemini"
}
```

在 Bash 中设置当前终端会话的地址和 key，然后启动。地址使用 Gateway 站点 origin，
**不加 `/v1`**；将示例域名换成自己的站点。Key 在提示后输入，避免写入命令历史：

```bash
export GOOGLE_GEMINI_BASE_URL='https://gateway.example.com'
read -r -s -p 'Gateway API Key: ' GEMINI_API_KEY
printf '\n'
export GEMINI_API_KEY
agy --model gemini-3.1-pro-high
```

`GEMINI_API_KEY` 必须是本 Gateway 签发的 key。它只用于 Gateway 鉴权，不发送给
Bridge 或 Google；Gateway→Bridge 使用独立凭据。

AGY 在同一会话中会使用 `gemini-3.1-pro-preview` 和
`gemini-3.1-pro-preview-customtools`。Gateway 仅将这两个精确名称归一为公开模型
`gemini-3.1-pro-preview`，再执行模型权限、价格和额度检查。不会通过模糊前缀匹配
开放其他模型。自动标题使用的 Flash Lite 返回 404；已完成的本地客户端探测中，
此错误不影响主对话或本地工具循环。其他未配置模型同样返回 Gemini 格式的 404。

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

原生请求沿用 1 MiB 上限及全局单进程并发。接受实测 AGY `1.2.4` 主会话默认生成参数，
推理行为由服务器固定模型决定；无法兑现的自定义生成控制会返回明确错误。首版不支持
图片附件、精确 `countTokens` 或云端内置工具。Gemini SSE 在完整生成并验证上游结果后
发送 `data` JSON 对象，并以 EOF 结束，没有 Responses 的 `[DONE]` 标记。因此首个事件
仍需等待完整生成。请求取消或超时会结束隔离进程并清理请求数据。

## API 范围

`POST /v1/responses` 支持文本字符串、消息数组、`instructions`、`stream`、`tools` 工具列表、
以及 Codex CLI 客户端的会话参数与历史 `function_call` / `function_call_output` 结构。Bridge
会自动转换客户端请求格式：将声明的客户端工具注入会话提示词，若模型输出工具调用结构则转换为标准 Responses
API `function_call` 事件；非文本内容（文件、图片）仍拒绝并返回明确的 `400 antigravity_input_unsupported`。
Antigravity 模型的 compact 返回 `501 endpoint_not_supported`。

每个请求创建独立进程和空工作目录，提示词只经 stdin NDJSON 传入；使用
`--input-format stream-json --output-format stream-json --model gemini-3.1-pro-high
--print-timeout 5m --disable-slash-commands`。CLI 日志、HOME、会话和缓存留在请求
临时目录，退出后删除。每次调用只从加密 Keyring 恢复完整认证文件；CLI 退出且进程组
清理后写回合法的凭据更新，随后删除临时目录。全局并发为 1，忙时立即返回 `429 upstream_concurrency_exceeded`。
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
全部通过后才启动 Bridge，并执行 readiness、模型目录、JSON 和 SSE 验收。
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
ANTIGRAVITY_MODEL_ROUTES_JSON={"gemini-3.1-pro-preview":"gemini-3.1-pro-high"}
```

```sh
./scripts/compose.sh up -d gateway codex-compat antigravity-bridge
```

`/v1/models` 合并健康上游目录；重复模型 ID 拒绝处理。Bridge 不可用时其模型临时隐藏，
已有直接请求返回 `503 upstream_unavailable`，其他模型继续使用 Codex。目标 CLI 模型
缺失或认证不可用时 readiness 失败。Gateway 启动不依赖 Bridge 健康状态。

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

增加 Gemini 原生协议时，先部署新版 `antigravity-bridge`，再部署执行 `0020` 迁移的
Gateway。上线验收须使用真实订阅账号，从本机按上述配置完成一次文本对话和一次本地
工具任务，检查两种模型别名切换、标题 404 不阻断主任务及实际账单。该验收尚未由本次
实现验证；在完成前不能宣称 Google 上游已通过。

旧第三方 Gemini 插件、补丁和登录脚本已移除，Codex Sidecar 禁用插件，历史 Gemini OAuth
文件不再加载且不会自动删除。`codex_oauth` 卷继续保留原内容。

回滚时把路由恢复为 `{}`，重建 Gateway 并停止 Bridge；Codex 路由不受影响：

```sh
./scripts/compose.sh up -d --no-deps gateway
./scripts/compose.sh stop antigravity-bridge
```

若真实环境中 Secret Service 无法稳定解锁，先停止 Bridge；在专用 Linux 用户下部署同一
桥接程序和同样的权限配置，以受限内部监听接入。不能通过共享桌面 Keyring、挂载宿主 HOME
或放宽容器权限临时绕过隔离。
