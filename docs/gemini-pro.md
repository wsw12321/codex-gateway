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
不实现或模拟 Antigravity 私有网络协议，不使用 Gemini API Key。

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

## API 范围

`POST /v1/responses` 支持文本字符串、纯文本消息数组、`instructions`、`stream`、
缺省或 `false` 的 `store`、缺省或默认 `service_tier`。函数工具、非文本内容、文件、
图片、会话续接、`previous_response_id`、`max_output_tokens` 及其他无法可靠转换的
参数返回明确的 `400 antigravity_*_unsupported`。Antigravity 模型的 compact 返回
`501 endpoint_not_supported`。

每个请求创建独立进程和空工作目录，提示词只经 stdin NDJSON 传入；使用
`--input-format stream-json --output-format stream-json --model gemini-3.1-pro-high
--print-timeout 5m --disable-slash-commands`。CLI 日志、HOME、会话和缓存留在请求
临时目录，退出后删除。全局并发为 1，忙时立即返回 `429 upstream_concurrency_exceeded`。
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
随后检查 `agy models`、`agy --print /usage` 和最小文本请求，重新启动独立容器重复检查
以验证持久登录，最后启动服务并验证 JSON、SSE。失败时 Bridge 保持停止，Codex 不受影响。

容器以 UID 10002 运行，根文件系统只读；D-Bus 和 GNOME Secret Service 在容器内启动。
Keyring 唯一持久挂载为 `antigravity_keyring:/var/lib/antigravity/keyrings`，仅 Bridge
持有，文件为 `0600`，目录为 `0700`。启动必须确认持久 login collection 已解锁。
没有宿主目录、Docker Socket、原 OAuth 卷挂载；临时 HOME 和运行目录使用私有 tmpfs。
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
域名。Antigravity 基线仅保留原部署已经允许的 `accounts.google.com`、
`oauth2.googleapis.com`、`www.googleapis.com`、`cloudcode-pa.googleapis.com`，并移除
旧项目发现/配置域名；该基线尚未经过真实 Antigravity 账号流量验收。

Squid 为 Antigravity 网络记录 CONNECT 目标、时间和状态，不记录 TLS 内容或认证头。
验收登录、刷新、模型检查、`/usage` 和生成请求时检查：

```sh
./scripts/compose.sh logs --since 10m egress-allowlist
```

对被拒绝目标核对实际 DNS/CONNECT/SNI 和官方用途，再将必要的精确主机名加入
`deploy/egress/squid.conf`，同步 `scripts/validate-compose.sh` 的固定清单并审核差异。
不要开放 `*.googleapis.com` 或通用 Google 子域；安装包和自动更新域名不需要运行时出口。
在真实流量完成前不能宣称出口清单已经充分或生产验收通过。

## 验收和回滚

开发验证覆盖假 CLI 的 JSON、SSE、NDJSON 分段、认证、限额、协议错误、工具事件、取消、
超时、usage、路由和共用权限/结算。部署前运行完整 Go suite、race、vet、Compose 校验与
三个镜像构建。真实账号还需验证重启、续期、重新登录、限额、取消后无残余进程、磁盘残留
和出口域名。当前环境未登录真实账号，不应把单元测试视为这些验收的替代。

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
