# Personal Codex Gateway

面向单台公网 VPS、单个订阅者自有设备的 Codex Responses 网关。它在本地
Codex CLI 与内部 Codex 兼容层之间增加设备级 API Key、项目归属、持久化限额、
纯元数据统计、Passkey 身份管理和安全审计。

> [!WARNING]
> 当前项目已完成代码与可丢弃环境中的自动化部署验收，但真实域名、Passkey 和
> ChatGPT Pro 设备码登录仍需在目标 VPS 上人工验收。在完成这些步骤前尚不应
> 直接用于生产，见[当前状态](#当前状态2026-08-11)。

Codex 的文件读取、命令执行和代码修改仍发生在客户端本地。Gateway 不保存
提示词、源代码或模型回复，也不会在 Pro 凭证失效时自动回退到 Platform API。

## 架构

```text
本地 Codex CLI
      │ HTTPS + 每设备 API Key
      ▼
Cloudflare Edge
      │ Cloudflare Tunnel；服务器无 80/443 入站
      ▼
 cloudflared
      │ 内部 HTTP
      ▼
    Caddy
      │
      ▼
  Go Gateway ───────────── PostgreSQL 17
      │                     身份、配额、审计和 usage 元数据
      │ 内部固定凭证
      ▼
  CLIProxyAPI v7.2.127
      │ HTTP(S)_PROXY；无直接互联网路由
      ▼
  Squid 域名白名单出口
      │
      └─────────────────── auth.openai.com:443 / chatgpt.com:443
```

Compose 中的服务职责如下：

| 服务 | 职责 | 宿主机端口 |
| --- | --- | --- |
| `cloudflared` | 唯一公网入口的出站 Tunnel connector | 无 |
| `caddy` | 内部反向代理、安全头、64 MiB 请求上限和 SSE 刷新 | 无 |
| `gateway` | 身份、Key、配额、统计、管理界面和固定 Responses 代理 | 无 |
| `postgres` | 持久化身份、配额、usage、审计和告警元数据 | 无 |
| `codex-compat` | 持有唯一 Pro OAuth 状态并适配 Codex 协议 | 无 |
| `egress-allowlist` | 只允许目标域名的 443 CONNECT 出口 | 无 |

所有服务均不发布宿主机端口。`cloudflared` 只通过出站连接接入 Cloudflare，
Caddy、Gateway、PostgreSQL 和 sidecar 位于 Docker `internal` 网络；只有
`cloudflared` 和出口代理分别连接其所需的外部网络。OAuth volume 只挂载给
sidecar，不进入数据库或备份。

## 核心能力

### 身份与凭证

- 无密码、无公开注册的邀请制身份系统；Owner 初始化链接只能从 SSH 终端生成。
- 可发现 Passkey 登录、多 Passkey、WebAuthn RP/Origin 校验及 challenge 防重放。
- API Key 创建/撤销、邀请和恢复等敏感操作要求 5 分钟内再次验证 Passkey。
- 每批生成 10 个一次性恢复码；Owner 可签发恢复邀请。
- 服务端随机会话，12 小时空闲和 7 天绝对过期；Cookie 使用 `Secure`、
  `HttpOnly`、`SameSite=Strict`。
- 设备 Key 格式为 `cgk_v1_<public-id>_<256-bit-secret>`，原值只显示一次；
  数据库仅保存 public ID、显示前缀和带服务端 pepper 的 HMAC-SHA256。
- 每个 Key 必须绑定用户和设备，可设置默认项目、到期时间、模型白名单和独立
  限额；默认 90 天，最长 365 天。

### 代理接口

首版只开放 Codex 所需的三个路径：

- `POST /v1/responses`
- `POST /v1/responses/compact`
- `GET /v1/models`

Responses 支持普通 JSON 和 SSE。Gateway 会丢弃客户端的 `Authorization`、
Cookie、转发头及 hop-by-hop headers，使用内部固定 Bearer Token 调用 sidecar。
不开放 WebSocket、Chat Completions、CORS 或任意 URL 反向代理。

每个响应都带 `X-Gateway-Request-ID`。错误采用 OpenAI 风格 JSON，并返回稳定的
`type`、`code`、安全消息和 `request_id`。常见映射包括：

| 状态 | 场景 |
| --- | --- |
| `400 invalid_project` | 显式项目不存在或不属于当前用户 |
| `401` | API Key 无效或已过期 |
| `403` | 用户/Key 已禁用，或模型不在白名单 |
| `413 request_too_large` | 请求超过 64 MiB |
| `429` | RPM、并发、每日请求或 USD 额度不足 |
| `503 upstream_reauthentication_required` | Pro OAuth 需要重新登录 |
| `502/504` | 清洗后的上游网络、协议或超时错误 |

### 配额与统计

配额在 PostgreSQL 中原子预留和结算，进程重启不会清零。长 SSE 流会续租并发
lease，客户端断开会取消上游请求并释放租约，异常退出的遗留租约由后台清理。

默认限额：

| 作用域 | RPM | 并发 | 请求/日 |
| --- | ---: | ---: | ---: |
| 每个 Key | 30 | 4 | 1,000 |
| 每个用户聚合 | 60 | 8 | 2,000 |
| 单 Pro 上游全局 | — | 12 | — |

Token 总量按 input + output 结算；cached input 和 reasoning 是细分指标，不重复
计入总量。管理界面可按时间、用户、设备、Key、项目、模型和状态过滤，展示请求
数、Token、缓存率、错误率、p95 TTFT/耗时，并导出 CSV。

### 额度与订阅计费

所有用户（包括 Owner）都需要可用的 USD 额度才能调用 Responses 和 compact；
`GET /v1/models` 不计费。现金余额和日、周、月三档订阅分别记录，固定滚动周期为
24 小时、7 天和 31 天。实际请求完成后按“日订阅 → 周订阅 → 月订阅 → 现金”
原子分摊，周期余额不结转，也不会出现负现金余额。请求只可使用准入时存在的
周期和现金 credit lot，因此后续充值、续期或修改订阅不会被旧请求追扣。

请求按客户端提交的精确模型在准入时保存输入、缓存输入和输出价格快照；上游返回
不同模型不会改变账单。未配置价格的模型返回 `400 model_pricing_not_found`，没有
正额度来源时返回 `429 insufficient_quota`。单次或并发请求超过准入时绑定额度时，
账务流水会分别记录实际成本、已扣金额和未覆盖金额，不透支也不延后追扣。

管理台的“额度与订阅”区域允许成员只读查看余额、三档周期和长期账务流水。Owner
在近期 Passkey 验证后可按当时充值汇率进行 CNY 充值、执行带原因的正负 USD 调整、
修改充值汇率，以及立即重开或停用任一订阅档。所有写操作要求 UUID
`operation_id`，安全重试会返回原结果；金额在 JSON 中始终使用十进制字符串。

Owner 的全员统计还会按部署时固定的价格目录和 USD/CNY 汇率展示“API 等价费用
估算”。它只按记录中的精确模型名匹配价格；未匹配的模型单独计入未定价 Token
和覆盖率，不会静默按零价形成完整总额。该估算不是真实费用或账单：当前上游是
ChatGPT Pro OAuth，结果不代表实际结算，也不包含 Pro 订阅费、税费、基础设施或
工具费用。历史区间始终按当前部署的价格快照重新估算。

只保存以下调用元数据：身份与项目引用、Key 前缀、模型、端点、状态/错误码、
请求/首 Token/完成时间、TTFT、耗时、各类 Token、字节数和上游请求 ID。请求
明细保留 90 天，安全审计保留 365 天，日/月聚合和不含请求/响应内容的账务流水
长期保留。

## 当前状态（2026-08-11）

### 已实现

- Go Gateway、PostgreSQL schema 与嵌入式校验迁移。
- 中文管理界面、Passkey 邀请/登录/恢复、多设备/项目/API Key 管理。
- Responses、compact、models 固定代理，普通响应和 SSE usage/TTFT 解析。
- PostgreSQL 原子 RPM、并发、每日请求配额及长流 lease 续期。
- usage 日/月聚合、精确筛选、CSV、审计、配额及上游告警。
- Cloudflare Tunnel、Caddy、CLIProxyAPI、Squid 网络隔离、加密备份/恢复脚本
  和供应链锁定。

### 已验证

- `go test ./...`、`go test -race ./...`、`go vet ./...` 和格式检查通过。
- PostgreSQL 17 真实集成测试已覆盖迁移、配额、usage、日/月聚合、审计和告警，
  并已加入使用锁定 PostgreSQL digest 的 CI service。
- Gateway 镜像与固定提交的 CLIProxyAPI 镜像构建成功。
- Sidecar 可在无直接网络、只读根文件系统和非 root 条件下健康启动。
- Compose 暴露面检查确认所有服务均未发布宿主机端口。
- PostgreSQL 空 volume 首次初始化成功；四个后端服务均为 `healthy`，
  `/healthz` 与 `/readyz` 均返回 200，`schema_migrations`、`usage_monthly` 等
  应用表已创建。
- 所有 Shell 脚本语法、Compose 双 env 文件解析及部署安全不变量检查通过。

### 已解决的部署阻塞

原配置曾使用：

```yaml
POSTGRES_INITDB_ARGS: --auth-host=scram-sha-256 --auth-local=peer
```

官方 PostgreSQL entrypoint 初始化时以操作系统用户 `postgres` 连接数据库用户
`gateway`，`peer` 认证失败，导致目标数据库未创建。现在本地和 host 初始化均
固定为 `scram-sha-256`，部署校验会阻止该配置回退。验证时仅删除了已确认没有
目标数据库和业务表的失败测试 volume，随后完成了首次初始化、迁移和健康检查。

同时修复了两个同链路问题：`bootstrap-secrets.sh` 每次都会从当前 PostgreSQL
口令原子重建 DSN，避免残留旧连接串；`scripts/compose.sh` 会清除继承的同名
环境变量，再依次读取 `.env` 和 `deploy/images.lock.env`，既避免遗漏
`GATEWAY_DOMAIN`，也防止宿主环境绕过镜像 digest 锁。

### 仍需目标环境人工验收

- 使用真实域名和 Dashboard 托管的 Cloudflare Tunnel，验证外部 TLS、零宿主
  端口暴露和外部健康检查；
- 执行真实 ChatGPT Pro 设备码登录，验证模型列表和最小流式 Responses；
- 初始化 Owner，完成 Passkey 注册、恢复码离线保存和一台真实 Codex 设备接入；
- 按运维手册完成加密备份、恢复演练和生产监控检查。

## 部署准备

### 前置条件

- 一台可通过 SSH 管理的 Linux VPS；
- Cloudflare 托管的域名和可创建 Dashboard Tunnel 的权限；
- 公网入站只允许固定管理 IP 访问 SSH，80、443 和所有容器端口均关闭；
- Docker Engine 与 Docker Compose v2；
- `openssl`、`jq`、`age`、`age-keygen` 和 util-linux 的 `flock`；镜像升级时另需
  `skopeo`、`crane` 或 Docker Buildx；
- 支持 WebAuthn 的浏览器与安全密钥或平台认证器；
- 可用于设备码登录的个人 ChatGPT Pro 账号。

不固定 CPU、内存或磁盘规格，应按实际负载容量规划。磁盘必须容纳 PostgreSQL、
最近 14 组加密备份、当前和上一 revision 的镜像，并在上述内容全部存在时仍
保留至少 20% 可用空间。

> 自动化部署阻塞已经解决；下列流程仍必须在目标 VPS 使用真实域名、Passkey 和
> Pro 账号完成后，才能视为生产验收通过。

### 1. 固定 revision 并配置站点

生产构建必须在目标服务器的仓库 checkout 中完成。先检出已经审阅的 tag 或
完整 Git commit，再把 `.env` 中的 `GATEWAY_IMAGE_TAG` 设为 release version 或
完整 40 位 revision，`GATEWAY_VERSION` 设为显示版本，`GATEWAY_REVISION` 设为
完整 commit。生产不得使用 `local`、`dev`、`latest` 或 `unknown`，并保留上一
revision 的本地镜像以供无数据库迁移时快速回退。

```sh
cp deploy/env.example .env
chmod 0600 .env
# 编辑 .env：设置真实域名、当前 revision 和 GATEWAY_USAGE_PRICING_JSON
# 把 GATEWAY_SECRET_GID 设置为专用部署用户的主组：id -g
```

价格目录必须按记录中的精确模型名，使用
[OpenAI API Pricing](https://developers.openai.com/api/docs/pricing/) 的每百万 Token
价格填写；同时记录目录日期、固定 USD/CNY 汇率及其日期。示例中的日期、模型名
和价格只是结构占位符，不能直接部署。完整估算口径和升级要求见
[部署与运维手册](docs/operations.md#用量价格快照)。

如 Compose 子网与主机已有网段冲突，必须在首次启动前同步调整内部子网、静态
地址、Caddy 信任的 cloudflared `/32`、`GATEWAY_TRUSTED_PROXY_CIDRS` 及部署
校验中的对应不变量。Gateway 只应信任 Caddy 的固定 `/32` 地址。

### 2. 生成服务 secret 并配置 Tunnel

```sh
./scripts/bootstrap-secrets.sh
```

脚本以 `0640` 创建 PostgreSQL 口令、完整 DSN、API Key/token HMAC pepper 和
Gateway 到 sidecar 的内部 Key，不输出内容。在 Cloudflare Dashboard 创建
Tunnel，把 Public Hostname 的 origin service 固定为 `http://caddy:80`，再将
connector token 以精确 `0640` 保存为
`deploy/secrets/cloudflared_tunnel_token`。Token 只能通过挂载的 secret 文件
传给 `cloudflared`，不得写入 `.env`、命令参数或日志。不要提交 `.env`、
`deploy/secrets/*`、OAuth 文件或备份。

### 3. 校验、构建和启动

基础镜像由 [deploy/images.lock.env](deploy/images.lock.env) 中的 manifest digest
锁定；CLIProxyAPI 固定为 `v7.2.127` / commit
`ecc9aa72b32f34b680d03b0724b531a21ae74472`。

```sh
./scripts/validate-compose.sh
./scripts/compose.sh build gateway codex-compat
./scripts/compose.sh up -d
./scripts/compose.sh ps
```

最终不应有任何 Compose 服务发布宿主机端口。Cloudflare 负责公网 TLS 和
HTTP→HTTPS 跳转；服务器安全组/防火墙只保留固定管理 IP 的 SSH 入站。
`GET /healthz` 是进程存活探针，
`GET /readyz` 当前只检查 PostgreSQL 连接；它不验证 sidecar 或真实上游。

### 4. 登录唯一上游账号

```sh
./scripts/codex-device-login.sh
```

该 SSH 运维脚本会持有本机锁、停止唯一 sidecar、执行设备码登录、检查 OAuth
文件为 UID 10001 且精确 `0600`，然后重启并做模型列表与最小 Responses 冒烟。
任何时刻都不得让两个 sidecar 共享同一个 refresh token。

### 5. 初始化 Owner

```sh
./scripts/compose.sh exec gateway gateway bootstrap-owner
```

命令只在 SSH 终端显示一次 24 小时有效的初始化链接；令牌位于 URL fragment，
不会作为 query string 进入代理日志。Owner 注册 Passkey 并离线保存恢复码后，
即可在管理界面创建设备、项目和 API Key。

完整上线、设备登录、备份、恢复与升级流程见
[部署与运维手册](docs/operations.md)。

## Codex CLI 配置

每台设备创建不同的 API Key，并保存在该设备的安全环境中：

```sh
export CODEX_GATEWAY_API_KEY='cgk_v1_...'
export CODEX_GATEWAY_PROJECT='my-project'
```

在 Codex 配置中加入：

```toml
model_provider = "gateway"

[model_providers.gateway]
name = "Personal Codex Gateway"
base_url = "https://codex.example.com/v1"
env_key = "CODEX_GATEWAY_API_KEY"
wire_api = "responses"
env_http_headers = { "X-Codex-Project" = "CODEX_GATEWAY_PROJECT" }
request_max_retries = 2
stream_max_retries = 2
```

将域名替换为真实部署域名。若没有 `X-Codex-Project`，Gateway 使用 Key 的默认
项目；Key 也没有默认项目时记为 `unassigned`。显式提供未知或跨账号项目会返回
`400 invalid_project`。不要把 Key 写进可提交的 TOML、项目 `.env`、shell
profile 或命令历史。详见 [客户端配置](docs/client-config.md)。

## 开发与验证

项目工具链固定为 Go 1.24.6。普通测试不需要外部数据库：

```sh
gofmt -l .
go test ./...
go test -race ./...
go vet ./...
```

PostgreSQL 集成测试带 `integration` build tag，必须指向可丢弃的独立数据库：

```sh
TEST_DATABASE_URL='postgres://gateway:password@127.0.0.1:5432/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/store
```

部署静态检查：

```sh
./scripts/validate-compose.sh
./scripts/compose.sh config --quiet
for script in scripts/*.sh deploy/codex-compat/*.sh; do
  case "$(sed -n '1p' "$script")" in
    '#!/bin/sh') sh -n "$script" ;;
    '#!/usr/bin/env bash') bash -n "$script" ;;
  esac
done
```

CI 还配置了 `govulncheck`、`gosec`、Trivy 容器扫描、CycloneDX SBOM、许可证
检查以及使用锁定 PostgreSQL 17 镜像的独立集成测试 service。本地集成测试仍
需显式设置 `TEST_DATABASE_URL`，避免误用开发或生产数据库。

## 仓库结构

```text
cmd/gateway/             进程入口和 bootstrap-owner/maintenance 命令
internal/config/         配置与 secret 文件加载
internal/identity/       WebAuthn、会话、邀请与恢复流程
internal/server/         HTTP 路由、管理界面、API 认证与统计
internal/proxy/          Responses/compact/models 转发与 SSE usage 解析
internal/limit/          限额领域逻辑
internal/store/          PostgreSQL store、迁移、配额和 usage 聚合
internal/maintenance/    聚合、保留期和陈旧 lease 清理
deploy/                  Caddy、Squid、sidecar 构建及镜像锁
scripts/                 secret、登录、验证、备份和恢复脚本
docs/                    运维、客户端、安全与兼容层升级文档
```

## 安全边界与限制

- ChatGPT Pro 登录不是官方通用服务端 API。兼容层依赖当前 Codex 协议，可能随
  上游变化失效。
- 项目不会配置 Platform API 自动计费回退。OAuth 失效时应 fail closed、返回
  明确错误并在后台告警。
- 该部署只适用于同一订阅者本人控制的账号与设备。如果向其他真实用户提供
  服务，必须改成每人独立上游凭证或官方 Platform API，并重新评估条款、计费、
  隐私和数据控制。
- 单 VPS 是可用性单点；容器隔离不能抵御宿主机 root、Docker daemon 或内核被
  攻破。
- 当前 age 密文、解密 identity 和恢复所需 secret 只保存在本机；它们能处理
  数据库逻辑损坏和计划迁机，但不能在服务器或云盘整体丢失后恢复数据。
- 请求正文虽然不落库，但在 TLS 终止和转发过程中会短暂存在于相关进程内存。

详细的资产、信任边界、缓解措施和残余风险见
[威胁模型](docs/threat-model.md)。兼容层升级前必须遵循
[CLIProxyAPI 升级规程](docs/compatibility-upgrades.md)。

## 运维命令

Gateway 二进制支持：

```text
gateway serve             启动 HTTP 服务并自动迁移
gateway migrate           只执行嵌入式数据库迁移
gateway bootstrap-owner   生成首次 Owner 初始化链接
gateway maintenance       立即运行一次聚合与清理
gateway version           显示版本与 revision
```

备份和恢复：

```sh
./scripts/init-backup-key.sh
./scripts/backup-postgres.sh
./scripts/restore-drill.sh backups/gateway-YYYYmmddTHHMMSSZ.dump.age
```

OAuth volume 永不备份，灾备后必须重新执行设备码登录。
生产数据库由固定 Compose 命名卷持久化；禁止执行
`./scripts/compose.sh down -v`。每日 03:00 UTC 的本地 age 备份、14 组保留、
每月/升级前恢复演练及计划迁机步骤见[部署与运维手册](docs/operations.md)。

## 许可证

Gateway 源码采用 [MIT License](LICENSE)。CLIProxyAPI 的固定版本、提交和 MIT
版权声明见 [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES)。
