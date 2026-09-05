# 部署与运维手册

`0003_password_credentials.sql`、`0004_subscription_period_limits.sql`、
`0005_official_token_pricing.sql`、`0006_api_key_lifecycle.sql` 和
`0007_upstream_accounts.sql` 都是 forward-only 迁移。部署前先备份
PostgreSQL；迁移后旧二进制会因未知迁移保护而拒绝启动，不能只切回旧镜像。
`0004` 会把所有当时仍启用且未到期的订阅设为 `1/1`，保留现有额度、余额和周期
起止时间，并在原 `period_ends_at` 自动失效；已经越过结束时间但仍标记启用的
记录会按原结束时间关闭。`0005` 增加 OpenAI API Token 等价成本 v2 所需的服务层、
上下文、缓存写入和不可变 ledger 元数据，不重算或更新历史流水。`0006` 为既有 Key
回填不含秘密的历史引用，迁移账务、用量、配额和审计外键，增加可空密文字段，删除旧
`revoked` 凭证并把活动状态收紧为 `active`/`disabled`；迁移前 Key 的密文保持为空。
`0007` 增加不含 OAuth secret 的上游账号索引，并为请求明细、日/月聚合和不可变
ledger 增加可空账号归因；迁移前历史保持 `NULL`，不得猜测或回填所属账号。
密码哈希属于敏感数据，
不得写入日志、审计 metadata 或支持工单。Argon2id 固定使用 64 MiB 内存、3 轮和
并行度 2，不需要新增部署 secret。

本项目按单台 Linux 云服务器、单个隔离 sidecar 中的多个 ChatGPT Plus/Pro 上游
账号设计。Cloudflare
Tunnel 是唯一公网入口，connector 只建立出站连接；所有 Compose 服务均不得
发布宿主机端口。服务器只接受固定管理 IP 的 SSH 入站。所有命令都应从目标
服务器上的仓库根目录执行。

## 1. 前置条件

- 域名由 Cloudflare 托管，操作者有权在 Dashboard 创建 Tunnel 和 Public
  Hostname；不需要把 A/AAAA 记录指向服务器公网 IP。
- SSH 使用密钥登录；云安全组和主机防火墙只允许固定管理 IP 访问 SSH，明确
  拒绝公网 80、443、5432、8080、8317 和 3128 入站。
- 若主机限制出站，允许 DNS、Cloudflare Tunnel 所需的 TCP/UDP 7844，以及
  拉取镜像和管理 Cloudflare 所需的 HTTPS；不要为 Tunnel 增加任何入站规则。
- Docker Engine 和 Docker Compose v2 可用。
- `skopeo`、`crane` 或 Docker Buildx 可用于解析镜像 digest。
- `openssl`、`age`、`age-keygen` 和 util-linux 的 `flock` 用于服务密钥、备份
  及互斥操作。
- 不要把 Docker socket 暴露给公网服务，也不要让无关用户进入 `docker`
  组。

不对泛 Linux 主机硬编码 CPU、内存或磁盘规格，应按请求并发和实际数据库增长
测量容量。磁盘必须同时容纳数据库、最近 14 组完整加密备份、当前及上一
revision 的镜像，并持续保留至少 20% 可用空间；还要监控 inode。空间不足时
先停止升级并扩大磁盘，不得删除唯一可恢复备份或正在使用的数据库卷。

先创建本地配置：

```sh
cp deploy/env.example .env
chmod 0600 .env
```

把 `GATEWAY_DOMAIN` 改成真实域名，并按[构建和首次启动](#4-构建和首次启动)
设置不可变版本字段。若 Docker 网段与主机已有网段冲突，应在首次启动前同时
调整 Compose 中的内部子网与静态地址、Caddy 信任的 cloudflared `/32`、
`GATEWAY_TRUSTED_PROXY_CIDRS`，以及 `validate-compose.sh` 中验证这些地址的
对应不变量。网关只应信任 Caddy 的单一 `/32` 地址。

### 用量价格快照

`GATEWAY_USAGE_PRICING_JSON` 是必填的非 secret 部署配置。Gateway 不在运行时
联网抓取价格或汇率；操作者必须在启动前从
[OpenAI API Pricing](https://developers.openai.com/api/docs/pricing/) 人工核对所有
允许模型、服务层和上下文档位的价格，并选择、记录固定的 USD/CNY 汇率。可读的
完整目录是 [`deploy/pricing-v2.example.json`](../deploy/pricing-v2.example.json)，
`deploy/env.example` 和 `deploy/env.gpt-5.6.example` 已包含其单行副本。生产
`.env` 中的 JSON 必须保持一行；v2 的结构如下（片段不能单独部署）：

```json
{
  "schema_version": 2,
  "catalog_as_of": "2026-09-05",
  "fx_as_of": "2026-08-20",
  "usd_cny_rate": "7.20",
  "fallback_policy": {
    "unknown_service_tier": "max_published",
    "missing_price_combination": "max_published",
    "missing_cache_write_tokens": "all_uncached_as_write"
  },
  "models": {
    "gpt-5.6-sol": {
      "cache_write_mode": "separate",
      "max_input_tokens": 1050000,
      "long_context_threshold_tokens": 272000,
      "service_tiers": {
        "standard": {
          "short": {
            "input_usd_per_million": "4",
            "cached_input_usd_per_million": "0.4",
            "cache_write_usd_per_million": "5",
            "output_usd_per_million": "20"
          },
          "long": {
            "input_usd_per_million": "8",
            "cached_input_usd_per_million": "0.8",
            "cache_write_usd_per_million": "10",
            "output_usd_per_million": "30"
          }
        }
      }
    }
  }
}
```

完整模板覆盖 `gpt-6-astra`、`gpt-5.6-sol`、`gpt-5.6-terra`、
`gpt-5.6-luna`、`gpt-5.5`、`gpt-5.4`、`gpt-5.4-mini` 和内部零价
`codex-auto-review`。GPT-5.2 等不在目录
中的模型必须在转发前拒绝，直到加入完整的官方模型、服务层和上下文规则；不得
用别名、通配符或相近模型价格代替。设备码登录后应把 `/v1/models` 与该集合和
用户模型权限、API Key 白名单逐一核对。

v2 是严格的 tagged union：不得在同一配置中混入旧的模型级三价字段；未写
`schema_version` 的配置仍按 v1 解析，只用于过渡和升级时结算已经执行中的 v1
reservation。日期使用 `YYYY-MM-DD`；汇率和价格必须是带引号的非负十进制字符串，
汇率必须大于零。每次变更都应保存审阅来源、完整 JSON、目录日期和汇率日期。

服务层和上下文选择规则如下：

- 请求或响应 `default`/`standard` 对应 Standard，`flex` 对应 Flex，
  `priority`/`fast` 对应 Fast。显式请求 Ultrafast 或其他未配置层级会在转发前
  返回 `service_tier_not_supported`。
- `input_tokens <= 272000` 使用 `short`，`input_tokens > 272000` 使用 `long`。
  GPT-5.4-mini 的官方最大输入为 272000，只配置短档；若上游仍报告更大输入，
  作为缺失价格组合使用最高已公布分量并明确标记兜底。
- 响应缺失服务层时记录 `missing_service_tier`，响应层级未知时记录
  `unknown_service_tier`；二者都在当前上下文档位按各价格分量的最高已公布值
  结算。指定模型/层级/档位缺少组合时记录 `missing_price_combination`，并在该
  模型所有已公布组合中逐分量取最高值。
- GPT-5.5 和 GPT-5.4 的 Fast 长上下文未公布，分别兜底为
  `12.5 / 1.25 / 75` 和 `5 / 0.5 / 30`（输入/缓存读取/输出，每百万 Token）。
  这属于本地保守兜底，不伪装成官方精确价格。

GPT-6 和 GPT-5.6 使用 `cache_write_mode=separate`，缓存写入是独立计费类别：

```text
ordinary = input_tokens - cached_input_tokens - cache_write_tokens
cost = (ordinary * input_price
      + cached_input_tokens * cached_input_price
      + cache_write_tokens * cache_write_price
      + output_tokens * output_price) / 1,000,000
```

GPT-5.5、GPT-5.4 和 GPT-5.4-mini 使用
`cache_write_mode=included_in_input`，cache-write Token 仍包含在普通非缓存输入中，
不额外收费：

```text
ordinary = input_tokens - cached_input_tokens
cost = (ordinary * input_price
      + cached_input_tokens * cached_input_price
      + output_tokens * output_price) / 1,000,000
```

若 GPT-6 或 GPT-5.6 响应没有给出 cache-write 字段，结算会把全部非缓存输入视为
缓存写入，
记录 `missing_cache_write_tokens`。`reasoning_tokens` 已包含在 output 中，不能再次
计费。最终金额沿用系统规则保留 12 位小数。

准入时会把请求模型的完整 v2 规则和目录日期保存在 reservation；结算后把实际
模型、请求/实际/最终计价服务层、上下文档位、cache-write 模式和 Token、最终应用
四价及兜底原因写入不可变 `billing_ledger_entries`。全员报表的 USD 金额汇总该
ledger，修改当前价格 JSON 不会改变历史 USD；Token 数量继续来自 usage 明细和
日/月聚合。接口中的 `estimated_usd` 是 `actual_cost_usd` 的兼容别名，另有
`charged_usd` 和 `uncovered_usd`。CNY 只按当前配置的固定汇率换算 ledger USD。

界面和接口中的金额只能称为“OpenAI API Token 等价成本”，不能称为 OpenAI
实际账单：当前上游是 ChatGPT Plus/Pro OAuth，而且 `codex-auto-review` 零价与上述
保守兜底都是本地策略；Pro 订阅费、工具、区域、Batch、Ultrafast、税费和基础
设施成本均不在范围内。完整价格表和缓存语义见
[GPT-6 与 GPT-5.6 服务端配置](gpt-5.6-server-configuration.md)。

## 2. 供应链锁定

`deploy/images.sources` 保存人工审阅的具体版本标签，
`deploy/images.lock.env` 保存仓库返回的不可变 manifest digest。正常部署
直接使用已提交的 lock 文件。只有升级时才重新解析：

```sh
./scripts/lock-images.sh
git diff -- deploy/images.sources deploy/images.lock.env
```

确认版本和 digest 的差异后再提交。不要手写 digest，也不要在生产中使用
`latest`。CLIProxyAPI 的构建还会证明 `v7.2.150` 的 peeled commit 正是
`c77b13694318b0897f2c74104ef48aebdf8c34d6`，不匹配就会失败。兼容层镜像标签为
`v7.2.150-c77b1369-771903fb47f59bda`；最后一段为固定多账号补丁 SHA256 的
前 16 位，校验脚本会检查它，CI 使用实际构建的完整标签执行扫描。

## 3. 服务密钥

```sh
./scripts/bootstrap-secrets.sh
```

脚本以 `0640` 创建 PostgreSQL 口令、完整 DSN、两个 HMAC pepper、独立的 API Key
加密密钥和 Gateway 到 sidecar 的内部随机密钥，不会显示密钥。API Key 加密密钥
位于 `deploy/secrets/gateway_api_key_encryption_key`，是恰好 32 个随机字节的无填充
URL-safe Base64 编码。脚本只在文件不存在时创建它；已有文件会被保留并校验，不能
通过重新运行脚本轮换密钥。Compose 通过
`/run/secrets` 文件传入，禁止复制到 `.env`、命令行或工单。

本机 Compose 以只读 bind mount 实现 secret，长语法的所有权映射通常会被
忽略。因此 `.env` 中的 `GATEWAY_SECRET_GID` 必须设为专用部署用户的私有
主组（运行 `id -g` 获取）。Gateway 和 sidecar 仍以非 root UID 10001 运行，
仅通过这个组读取 `0640` secret；文件绝不能放宽为全局可读。生产主机不应让
其他交互用户加入该组。OAuth 卷不使用此机制，仍严格为 UID 10001、`0600`。

`API_KEY_ENCRYPTION_KEY`/`API_KEY_ENCRYPTION_KEY_FILE` 是 Gateway 启动必填配置，
部署固定使用 `_FILE`。它必须与 HMAC pepper 独立生成，仅挂载给 Gateway，禁止写入
`.env`、日志、审计 metadata、命令行或工单。本版本不支持密钥轮换；丢失或替换该
文件会使数据库中的 API Key 密文无法解密。数据库备份和恢复必须配套保管并恢复同一
份 secret。

初始化 age 备份密钥：

```sh
./scripts/init-backup-key.sh
```

`backup_age_identity` 是恢复所必需的解密 secret，只允许部署用户读取；
`backup_age_recipient` 可供备份任务读取。当前方案把数据库密文、identity 和
其他恢复 secret 都保存在同一服务器，仅覆盖数据库逻辑损坏与计划迁机，不
覆盖服务器或云盘整体丢失。若日后需要整机灾备，必须另行设计并验收异地密文
与异地 identity 托管，不能只复制其中一项。

在 Cloudflare Dashboard 的 **Networks → Tunnels** 创建托管 Tunnel，并复制
connector token。不要把 token 粘贴到命令参数或 shell 历史；在交互式 SSH
终端隐藏读取后写入 secret 文件：

```bash
read -r -s -p 'Cloudflare Tunnel token: ' CLOUDFLARED_TOKEN_INPUT
printf '\n'
printf '%s' "$CLOUDFLARED_TOKEN_INPUT" > deploy/secrets/cloudflared_tunnel_token
unset CLOUDFLARED_TOKEN_INPUT
chmod 0640 deploy/secrets/cloudflared_tunnel_token
chgrp "$(id -g)" deploy/secrets/cloudflared_tunnel_token
```

文件的 owner 和 group 必须与其他 `deploy/secrets` 文件一致，并仅挂载给
`cloudflared`。Token 不得进入 `.env`、Compose 命令、日志、工单或备份输出。
Dashboard 中为 `GATEWAY_DOMAIN` 创建 Public Hostname，origin service 精确设为
`http://caddy:80`，并把 origin 的 **HTTP Host Header** 明确设为同一个
`GATEWAY_DOMAIN`。该 hostname 必须设置 Cache Bypass 和 Always Use HTTPS；
不要启用 Cloudflare Access、Worker、Rocket Loader、Pseudo IPv4 或会修改客户
端 IP 头的 Transform Rule。外部 TLS 与 HTTP→HTTPS 跳转由 Cloudflare 负责。
Caddy 只信任固定 `cloudflared` 地址 `172.28.10.4/32` 提供的单值
`CF-Connecting-IP`，丢弃客户端的 `Forwarded`/`X-Forwarded-For` 后重建发给
Gateway 的 `X-Forwarded-For`；缺失或非法值回退为代理地址。Gateway 再只信任
Caddy 的 `172.28.10.2/32`。Caddy 的 HSTS 不声明 `preload`；不要在未单独评估
整个域名生命周期时申请浏览器 preload。

## 4. 构建和首次启动

部署必须从目标服务器现场构建，不从开发机复制未标识镜像。选择已审阅的 tag
或 commit，检出 detached revision 并记录完整 SHA：

```sh
git fetch --tags --prune
git checkout --detach <reviewed-tag-or-commit>
git rev-parse --verify HEAD
git status --short
```

工作树必须为空。把 `.env` 中 `GATEWAY_IMAGE_TAG` 设为审阅过的 release
version 或完整 40 位 Git SHA，`GATEWAY_REVISION` 设为完整 SHA，
`GATEWAY_VERSION` 设为审阅过的 release/tag 名称。生产禁止
`GATEWAY_IMAGE_TAG=local`、`GATEWAY_VERSION=dev` 或
`GATEWAY_REVISION=unknown`。构建新 revision 后保留前一个 revision 的本地镜像，
至少到升级和恢复验收完成。

```sh
./scripts/validate-compose.sh
./scripts/compose.sh build gateway codex-compat
./scripts/compose.sh up -d
./scripts/compose.sh ps
```

`scripts/compose.sh` 会先清除宿主环境中与两个 env 文件同名的变量，再依次读取
`.env` 和 `deploy/images.lock.env`，并固定 Compose project 名。这样既不会遗漏
`GATEWAY_DOMAIN`，也不能用导出的环境变量绕过不可变镜像引用。

`validate-compose.sh` 会检查：没有服务发布宿主机端口、内部网络隔离、
`cloudflared` 与 Squid 只能连接各自需要的出站网络、PostgreSQL 本地初始化和
host 连接都强制使用 SCRAM、v2 价格目录结构和必要占位符、sidecar 是只读且非
root、API Key 加密 secret 的格式和仅 Gateway 挂载、其他 secret 权限及所有基础
镜像的 SHA-256 digest。

确认主机监听：

```sh
ss -lntup
```

Compose 不应增加任何监听。除固定管理 IP 可访问的 SSH 外，公网不应看到 80、
443、5432、8080、8317 或 3128；必须从另一网络扫描服务器公网 IP 验证。随后
从公网域名验证 Cloudflare Tunnel 返回 `/healthz` 和 `/readyz`。

数据库迁移由 Gateway 启动过程从嵌入迁移执行。Gateway 的 `/healthz` 是
存活探针，`/readyz` 当前只检查数据库连接，不检查 sidecar 或真实上游；Caddy
仅在 Gateway ready 后启动，`cloudflared` 再把 Dashboard hostname 转到 Caddy
的内部 HTTP 端口。

## 5. 上游设备码登录

登录只能通过 SSH 执行：

```sh
./scripts/codex-device-login.sh
```

脚本持有本机互斥锁并执行以下顺序：

1. 停止唯一的 `codex-compat` 实例，确认已停止后移除容器以释放固定 IP；命名
   OAuth volume 始终保留。
2. 通过 sidecar 的本地命令记录现有 Codex OAuth 文件名和内容的 secret-keyed
   SHA-256；不输出可离线猜测的裸文件名哈希、文件名、邮箱或 token。
3. 保持域名白名单出口代理运行，执行一次 `--codex-device-login`，添加一个新账号
   或刷新同一账号。
4. 拒绝任何 symlink、非 UID 10001、目录非 `0700` 或文件非 `0600` 的 OAuth
   状态；再次取得哈希清单，并要求恰好一个账号新增或变化、零账号消失。
5. 重启唯一 sidecar，等待健康检查。
6. 使用内部固定凭证调用脱敏账号列表、模型列表及最小流式 Responses，验证最终
   账号归因头，且不输出响应正文。

每次执行只处理一个 ChatGPT Plus/Pro 账号；重复执行可逐个加入更多账号。刷新
已有账号时，其他 OAuth 文件必须保持原样。差异标识由内部 Sidecar Key 加域，不能
在不知道该 Key 时对常见邮箱做离线字典匹配，仅用于阻止登录工具意外覆盖或删除其他
账号。

任何一步失败都会让 sidecar 保持停止，防止继续使用不确定的一组 refresh token。
修复原因后重新执行。严禁手动启动第二个挂载同一 `codex_oauth` 卷的容器。

可独立复查：

```sh
./scripts/verify-oauth-permissions.sh
./scripts/smoke-sidecar.sh
```

OAuth 卷不属于备份。灾备恢复、卷损坏或 refresh token 失效后，按账号逐次重新
执行设备码登录，不要从旧快照复制 token。

## 6. Owner 初始化和日常身份运维

服务首次 ready 后，通过 Gateway 的服务器端命令生成一次性初始化链接：

```sh
./scripts/compose.sh exec gateway gateway bootstrap-owner
```

只在 SSH 终端中运行，并把完整链接直接交给 Owner；链接中的令牌必须位于
URL fragment，不能作为 query string。

Owner 完成 Passkey 注册后，邀请、恢复、API Key 创建、查看、启停、删除和其他
敏感操作都在 HTTPS 管理界面完成并要求近期二次验证。新创建的 API Key 可在再次
验证后查看；迁移前 Key 因没有密文会明确显示不可查看。不要截图、记录或通过聊天系统转发
API Key、恢复码、邀请 fragment 或 Passkey challenge。

“模型权限”仅对 Owner 可见。每个当前定价目录模型分别维护新用户默认值和已有用户
快照：修改默认值只影响之后注册的用户；单用户、选中用户和“全部现有用户”操作只
影响操作事务中已经存在的账号，包括 Owner 与已停用账号。所有写入都要求同源浏览器、
近期二次验证和 1–500 字原因。禁用后应在下一次请求立即得到
`403 permission_error / model_not_allowed`，且不得产生 usage、配额、账务预留或
sidecar 请求。内部 `codex-auto-review` 不出现在该目录中。

## 7. 客户端接入

客户端配置见 [client-config.md](client-config.md)。每台设备使用独立 Key，
通过 Codex CLI 的安全输入流程配置。停用测试应确认只有目标 Key 立即收到
`403 key_disabled`，重新启用后恢复；永久删除后列表中不再出现且新认证立即失败，
其他设备以及既有用量和账务历史不受影响。

## 8. 加密备份和恢复演练

先手工创建并验证一组备份：

```sh
./scripts/backup-postgres.sh
```

脚本把 custom-format `pg_dump` 直接管道到 age，不在磁盘落明文，并为密文
写 SHA-256 校验文件。默认输出到被 Git 忽略的 `backups/`；也可通过
`BACKUP_DIR` 指向本机另一受限目录。目录必须为部署用户所有、模式 `0700`，
备份与校验文件必须为 `0600`。当前方案不把它们复制到异地。

数据库备份包含新 Key 的 AES-GCM 密文，但不包含
`deploy/secrets/gateway_api_key_encryption_key`。恢复演练只能证明数据库内容可恢复；
生产恢复和计划迁机还必须通过单独受保护的 secret 交接恢复完全相同的加密密钥。
使用另一把格式正确的 32 字节密钥可能通过启动配置检查，但查看 Key 时会完整性校验失败。

生产要求每日 03:00 UTC 备份，并且**只有新备份成功后**才删除旧文件，最终
保留按 UTC 文件名排序的最近 14 组完整 `.dump.age`/`.sha256` 对。为 cron 和
systemd 共用同一安全行为，可将以下 Bash wrapper 安装为
`/usr/local/sbin/codex-gateway-backup`。这是管理员手工安装的主机配置，不是
仓库中的新脚本；把两个绝对路径改成实际部署位置，并将文件设为
`root:<部署组>`、模式 `0750`，使部署用户可执行但不可修改：

```bash
#!/usr/bin/env bash
set -eu
set -o pipefail
umask 077

repo=/srv/codex-gateway
backup_dir=/srv/codex-gateway/backups

fail() {
    printf '%s\n' "codex-gateway-backup: $*" >&2
    exit 1
}

case "$repo:$backup_dir" in
    /*:/*) ;;
    *) fail 'repo and BACKUP_DIR must be absolute paths' ;;
esac
test -d "$backup_dir" && test ! -L "$backup_dir" ||
    fail "BACKUP_DIR is not a real directory: $backup_dir"

BACKUP_DIR="$backup_dir" "$repo/scripts/backup-postgres.sh"

shopt -s nullglob
backup_files=("$backup_dir"/gateway-*.dump.age)
checksum_files=("$backup_dir"/gateway-*.dump.age.sha256)
test "${#backup_files[@]}" -gt 0 || fail 'successful backup produced no dump'
for backup_path in "${backup_files[@]}"; do
    checksum_path=$backup_path.sha256
    test -f "$backup_path" && test ! -L "$backup_path" ||
        fail "invalid backup path: $backup_path"
    test -f "$checksum_path" && test ! -L "$checksum_path" ||
        fail "missing or invalid checksum: $checksum_path"
done
for checksum_path in "${checksum_files[@]}"; do
    backup_path=${checksum_path%.sha256}
    test -f "$checksum_path" && test ! -L "$checksum_path" ||
        fail "invalid checksum path: $checksum_path"
    test -f "$backup_path" && test ! -L "$backup_path" ||
        fail "orphan checksum: $checksum_path"
done

ordered_backups=$(printf '%s\n' "${backup_files[@]}" | LC_ALL=C sort -r) ||
    fail 'could not order backup set'
mapfile -t ordered_backup_files <<< "$ordered_backups"
stale_backups=("${ordered_backup_files[@]:14}")

for backup_path in "${stale_backups[@]}"; do
    if ! rm -- "$backup_path" "$backup_path.sha256"; then
        fail "could not remove complete backup pair: $backup_path"
    fi
done
```

在部署用户的 crontab 中可使用以下条目；先确认本机 cron 支持 `CRON_TZ`，否则
改用下方 systemd timer。cron daemon 应保存并告警非零退出和标准错误：

```cron
CRON_TZ=UTC
0 3 * * * /usr/local/sbin/codex-gateway-backup
```

使用 systemd 时，将以下两个单元分别保存到 `/etc/systemd/system/`；`User`、
`Group`、工作目录和 wrapper 路径必须替换为实际值：

```ini
# codex-gateway-backup.service
[Unit]
Description=Encrypted Codex Gateway PostgreSQL backup
Requires=docker.service
After=docker.service

[Service]
Type=oneshot
User=codex-gateway
Group=codex-gateway
WorkingDirectory=/srv/codex-gateway
UMask=0077
ExecStart=/usr/local/sbin/codex-gateway-backup
```

```ini
# codex-gateway-backup.timer
[Unit]
Description=Daily Codex Gateway backup at 03:00 UTC

[Timer]
OnCalendar=*-*-* 03:00:00 UTC
Persistent=true
AccuracySec=1min
Unit=codex-gateway-backup.service

[Install]
WantedBy=timers.target
```

启用前先手工运行 service，确认成功后只启用 cron 或 timer 其中一个：

```sh
sudo systemctl daemon-reload
sudo systemctl start codex-gateway-backup.service
sudo systemctl enable --now codex-gateway-backup.timer
systemctl list-timers codex-gateway-backup.timer
```

持续监控任务退出状态、最近备份时间、14 组文件对和剩余空间。不要为了达到
数量而自动删除缺少 checksum 的孤立文件；wrapper 对任何校验或删除失败都以
非零状态退出，应告警并人工调查。

每月至少恢复演练一次，每次 Gateway/数据库升级前还必须额外执行一次，并记录
所用备份、校验结果、表数量、时间和操作者：

```sh
./scripts/restore-drill.sh backups/gateway-YYYYmmddTHHMMSSZ.dump.age
```

演练容器使用锁定的 PostgreSQL 镜像、无网络、无发布端口和随机临时 Docker
volume。解密内容只通过管道进入 `pg_restore`，校验应用表数量后显式删除容器、
解密数据 volume 和临时 secret 目录；任一清理失败都会让演练返回失败。
这不是对生产数据库的就地恢复。真正灾备时应先建立隔离的新数据库、完成
同样校验，再在维护窗口切换；不要覆盖仍运行的生产卷。

本地备份集只包含数据库密文及对应 checksum；同一 revision 的 Git checkout
提供 lock 文件和部署定义。它必须排除：

- `codex_oauth` 卷；
- `.env`、`deploy/secrets` 和 `/run/secrets` 明文副本；
- 提示词、代码、响应正文和访问 Cookie。

数据库位于固定 Compose 命名卷，普通 `stop`/`down` 不会删除它。任何环境都
禁止执行 `./scripts/compose.sh down -v`，也禁止复制运行中的 PostgreSQL 原始
volume；恢复和迁机必须使用 `pg_dump`/`pg_restore`。

## 9. 日志、监控和故障处理

Caddy 不启用访问日志。`cloudflared` 日志只用于 connector 状态，禁止提高到会
泄露 header 的调试级别，也不得出现 Tunnel token。Gateway 只能记录请求 ID、
身份/项目引用、状态、时间、字节数和 usage 元数据；禁止记录 Authorization、
Cookie、正文、邀请令牌或 OAuth token。CLIProxyAPI 以 `debug: false`、
`logging-to-file: false` 和关闭内部 usage 统计运行。Squid 只能看到 TLS CONNECT
的目标域名与状态。

常用只读诊断：

```sh
./scripts/compose.sh ps
./scripts/compose.sh logs --since 15m cloudflared
./scripts/compose.sh logs --since 15m caddy
./scripts/compose.sh logs --since 15m gateway
./scripts/compose.sh logs --since 15m codex-compat
./scripts/compose.sh logs --since 15m egress-allowlist
```

复制日志前先扫描敏感模式。Tunnel connector 离线、备份过期/失败、磁盘低于
20% 可用空间、OAuth 重新认证、上游 429、sidecar 不健康和配额 80%/100%
都应告警；不要通过含请求内容的外部 webhook 发送。

出现 `upstream_reauthentication_required` 时停止调用并重新执行设备码登录。
不要启用 Platform API 自动回退；这会改变计费边界。

即时额度查询失败时，查看浏览器 Network 中
`POST /admin/upstream-accounts/{id}/quota` 的 `error.code`、`error.message`
和 `error.request_id`。相同分类也写入 `upstream_account.quota_queried` 审计事件的
`metadata.result_code`，可用请求 ID 关联排查。

| 错误码 | 失败环节与排查方向 |
| --- | --- |
| `sidecar_unavailable` | Gateway 无法连接 sidecar；检查容器状态、服务地址和内部网络 |
| `sidecar_timeout` | Gateway 等待 sidecar 超时；尚不能确定是内部网络还是 sidecar 等待上游 |
| `sidecar_auth_failed` | sidecar 拒绝内部认证；核对 Gateway 与 sidecar 的内部密钥配置 |
| `sidecar_auth_unavailable` | sidecar 内部认证未就绪；检查认证配置和服务状态 |
| `sidecar_account_registry_unavailable` | sidecar 账号管理未就绪 |
| `sidecar_quota_protocol_error` | sidecar 拒绝固定额度 RPC 请求；检查两端版本是否匹配 |
| `invalid_upstream_account` / `upstream_account_disabled` | 账号不存在或已停用；刷新账号列表并检查账号状态 |
| `upstream_quota_account_identity_unavailable` | OAuth 账号缺少有效账号标识 |
| `upstream_quota_credential_unavailable` | OAuth 账号缺少有效访问令牌；重新登录该账号 |
| `upstream_reauthentication_required` | ChatGPT 返回 401/403；检查账号登录状态与访问限制 |
| `upstream_quota_request_failed` | sidecar 无法构造上游请求；检查配置和版本 |
| `upstream_quota_unavailable` | sidecar 请求 ChatGPT 时发生网络错误；检查出站代理、DNS、TLS 和网络 |
| `upstream_quota_timeout` | sidecar 明确报告请求 ChatGPT 超时 |
| `upstream_quota_rate_limited` | ChatGPT 返回 429；稍后重试 |
| `upstream_quota_upstream_error` | ChatGPT 返回其他非成功状态；现有 sidecar 不提供原始状态和正文 |
| `upstream_quota_invalid_response` | sidecar 读取上游响应失败或响应超过大小限制 |
| `upstream_quota_schema_changed` | 上游 JSON 无法解析或字段未通过校验；检查额度适配器兼容性 |
| `sidecar_invalid_response` | Gateway 校验 sidecar 成功响应失败；检查内部响应协议与版本 |
| `sidecar_request_failed` | 收到了无法识别的 sidecar 错误响应；不据此推断 sidecar 连接中断 |
| `upstream_quota_query_rate_limited` | Gateway 本地并发/五秒防抖限制；等待后重试 |

Gateway 只识别固定 HTTP 状态与错误码组合，错误 JSON 最多读取 1025 字节，
超过 1024 字节即不解析；只接受单一 `error` 字段。未知字段、重复字段、异常 JSON、
不匹配的状态码和未知错误码均保留状态级通用分类，原始正文和解析异常不会写入
浏览器响应或审计。未识别的 5xx 使用 `sidecar_request_failed`，不会再合并成
`sidecar_unavailable`。Gateway 先到达超时时限时仍返回 `sidecar_timeout`。

这些分类利用现有 sidecar 已输出的固定错误码，只需重新构建并部署 Gateway
即可生效，无需修改额度查询地址、重建 sidecar 或执行额外数据库迁移。

新 Codex 会话可能先记录一次 `GET /v1/responses` 的
`426 responses_websocket_unsupported`，紧接着以 `POST /v1/responses` 的
HTTPS/SSE 继续。这是预期的传输协商，不是 Cloudflare 缓冲、上游中断或需要告警
的 Gateway 故障。合法探测只应有一次 426，且不应出现对应的 usage、配额预留、
计费或 sidecar 请求；若客户端仍显示 `Reconnecting... n/5`，先核对新 revision
是否已部署、GET 是否到达 Gateway，以及返回体错误码是否被中间代理改写。

## 10. 升级和回滚

升级 Gateway 前先按第 8 节生成新的数据库密文备份并成功完成恢复演练。升级到
首次要求价格配置的 revision 前，必须先在 `.env` 填妥
`GATEWAY_USAGE_PRICING_JSON`，否则新 Gateway 会拒绝启动。以后每次升级也应核对
精确模型集合、官方价格目录日期、固定汇率及其日期。v2 价格变更只影响变更后
创建的 reservation；已经结算的 USD 成本来自不可变 ledger，不会按新 JSON
重算。当前固定汇率仅影响 CNY 展示。升级到 `0006` 的 revision 还必须在任何新
Gateway 命令（包括 `gateway migrate`）运行前执行新版 `bootstrap-secrets.sh`，生成
并保管必填的 `gateway_api_key_encryption_key`；配置加载发生在迁移前，缺少该文件时
迁移命令也会拒绝启动。然后在服务器检出已审阅 revision、更新
`.env` 的三个版本字段、验证工作树为空并现场构建；不要覆盖或清理上一 revision
镜像。升级 CLIProxyAPI 前还必须：

1. 审阅新版本、commit、MIT notice 和依赖差异；更新 sources/lock。
2. 将固定多账号补丁重放到新 commit；对 `v7.2.150` 使用
   `git apply --check --ignore-space-change` 校验，再用
   `git apply --ignore-space-change` 应用。审阅完整 diff，并在 CI 运行调用方作用域
   隔离、轮询/粘滞、两账号失败切换、SSE 首字节边界、窄内部接口、Responses
   普通/SSE、compact、401、429、跨 chunk usage 和刷新 token 契约测试；同时确认
   认证后的 Responses WebSocket 探测返回一次 426 后立即降级到 HTTPS/SSE。
3. 持有 `.device-login.lock` 的 `flock` 并停止生产 sidecar，确保没有两个实例
   共享 token。
4. 用测试 OAuth 状态完成契约验证，再用至少一个已授权 Plus/Pro 账号进行人工冒烟。
5. 单实例滚动替换；失败时停止新实例，再回到旧镜像，不能并行回滚。

`v7.2.150` 仓库升级沿用现有 Go 1.26 构建镜像，不更改公共 API、数据库结构或模型
价格配置。仓库交付覆盖 Compose、两个镜像构建、构建内安全回归及 Gateway 单元
测试、race 和 vet；生产切换与真实 OAuth 账号的 Astra 模型列表、普通/SSE
Responses、compact 冒烟仍按上述步骤及
[兼容层升级规程](compatibility-upgrades.md) 执行。

Gateway 启动会写入嵌入式迁移记录；旧二进制检测到未知迁移会明确拒绝数据库
降级。因此，只在确认新 revision **没有写入任何新迁移** 时才可直接把镜像标签
切回上一 revision。只要新迁移已写入，无论 SQL 看起来是否兼容，都禁止仅回滚
二进制；必须停止写入，把升级前已演练的备份恢复到新的隔离数据库卷，验证后
再让旧 revision 切换到该卷。不得对生产卷手工删除 migration ledger 或反向
修改 schema。

### 升级到 `0004_subscription_period_limits.sql`

`0004` 改变订阅运行语义，不能直接二进制回滚。升级时按以下顺序执行：

1. 运行 `./scripts/backup-postgres.sh` 生成新的加密备份，并立即对该备份运行
   `./scripts/restore-drill.sh <backup>`；恢复演练失败时停止升级。
2. 检出已审阅 revision，确认工作树为空；更新 `.env` 中的
   `GATEWAY_IMAGE_TAG`、`GATEWAY_VERSION` 和完整 `GATEWAY_REVISION`。
3. 执行 `./scripts/validate-compose.sh` 和
   `./scripts/compose.sh build gateway`。
4. 执行 `./scripts/compose.sh up -d --no-deps gateway`。新 Gateway 会在监听前
   以单个数据库事务应用 `0004`。
5. 检查 `./scripts/compose.sh ps`、Gateway 日志、公开 `/healthz` 和 `/readyz`，
   并确认 `schema_migrations` 中存在 `0004_subscription_period_limits.sql`。
6. 在管理台抽查升级前的启用订阅：应显示为第 `1/1` 个周期，当前周期起止时间、
   周期剩余额度均与升级前一致，最终失效时间等于原周期结束时间。
7. 若必须回滚，先停止所有写入，将升级前已演练的备份恢复到新的隔离数据库卷，
   验证后再让旧 revision 切换到该卷。不得让旧二进制连接已经应用 `0004` 的卷。

### 升级到 `0005_official_token_pricing.sql`

`0005` 是从旧三价 reservation 过渡到 OpenAI API Token 等价成本 v2 的
forward-only 迁移。它只增加列、约束和索引，不更新历史 ledger；旧 reservation
仍按准入时的 v1 三价结算。必须在一个停写窗口内一次完成以下 7 步：

1. 停止公网入口和 Gateway 写入；保持 PostgreSQL 运行，不得让旧 Gateway 继续
   创建 reservation：

   ```sh
   ./scripts/compose.sh stop cloudflared
   ./scripts/compose.sh stop caddy gateway
   ```

2. 运行 `./scripts/backup-postgres.sh` 生成新的加密备份，并立即执行
   `./scripts/restore-drill.sh <backup>`。恢复演练成功后，记录
   `billing_ledger_entries` 中 `usage_charge` 的行数以及 `actual_cost_usd`、
   `charged_usd`、`uncovered_usd` 三项合计，作为迁移前核账基线；任何一项无法
   取得或备份无法恢复都停止升级。在受控的 PostgreSQL 管理会话中执行并保存：

   ```sql
   SELECT count(*) AS usage_rows,
          COALESCE(sum(actual_cost_usd), 0) AS actual_cost_usd,
          COALESCE(sum(charged_usd), 0) AS charged_usd,
          COALESCE(sum(uncovered_usd), 0) AS uncovered_usd
   FROM billing_ledger_entries
   WHERE entry_type = 'usage_charge';
   ```

3. 检出已审阅 revision，确认工作树为空；将 `.env` 的
   `GATEWAY_USAGE_PRICING_JSON` 一次性替换为
   `deploy/pricing-v2.example.json` 的完整单行矩阵，并复核目录/汇率日期。同步
   更新三个版本字段，运行 `./scripts/validate-compose.sh`，再执行
   `./scripts/compose.sh build gateway`。不要分批部署只有部分模型或部分服务层的
   目录。
4. PostgreSQL 保持 healthy、旧 Gateway 保持停止，用新镜像显式执行迁移：

   ```sh
   ./scripts/compose.sh run --rm --no-deps gateway migrate
   ```

5. 确认 `schema_migrations` 中存在 `0005_official_token_pricing.sql`，再次查询步骤
   2 的旧 ledger 行数和三项金额合计，必须逐项完全相同。抽查历史
   `usage_charge` 的 `pricing_rule_version=1`，新增 v2 元数据保持空值；不得为追求
   “完整”而更新历史流水。
6. 先启动 Gateway 和 Caddy，确认健康后分别用 GPT-6、GPT-5.6、GPT-5.5、GPT-5.4、
   GPT-5.4-mini 做受控 Responses 结算冒烟，并验证 Standard/Flex/Fast、
   `272000`/`272001` 边界、cache-write 与兜底统计。用无余额测试用户验证
   `codex-auto-review` 能写入 Token 和零金额 ledger、但不扣任何 USD 额度；全部
   通过后最后启动 Tunnel，恢复公网调用并再做一次外部健康和最小请求验证。
7. `0005` 写入后，禁止任何旧二进制直接连接该数据库卷。若必须回退，先停止
   Tunnel、Caddy、Gateway 和其他写入，把步骤 2 的升级前备份恢复到**新的**
   PostgreSQL 卷，核对 migration ledger 和金额后再让旧 revision 连接新卷；
   不得删除迁移记录、反向修改 schema 或让旧二进制连接已应用 `0005` 的卷。

### 升级到 `0006_api_key_lifecycle.sql`

`0006` 把可认证的活动凭证与长期账务/用量引用分开，并为新创建 Key 增加加密保存。
迁移前 Key 仍使用原 HMAC 正常认证，但没有可恢复的明文，因此管理界面会标记为不可
查看。迁移会永久删除原先状态为 `revoked` 的活动凭证行；对应安全历史引用继续保留。
按以下顺序升级：

1. 停止 Tunnel、Caddy 和 Gateway 写入，生成新的数据库加密备份并完成恢复演练。
   记录 `api_keys` 总数、各状态数量，以及用量、账务、配额和审计表中 API Key 引用
   的行数，作为迁移前基线。
2. 检出已审阅 revision 后，先执行新版 `./scripts/bootstrap-secrets.sh`。确认
   `deploy/secrets/gateway_api_key_encryption_key` 是非 symlink regular file、模式
   `0640`、组与 `GATEWAY_SECRET_GID` 一致，并将同一份文件纳入受保护的恢复 secret
   交接；不得从 HMAC pepper 派生、复制或替换它。
3. 更新 `.env` 中三个版本字段，执行 `./scripts/validate-compose.sh`，再构建 Gateway。
   校验必须确认加密密钥只挂载给 Gateway，且内容是表示 32 字节的无填充 URL-safe
   Base64。
4. PostgreSQL 保持 healthy、旧 Gateway 保持停止，显式执行迁移：

   ```sh
   ./scripts/compose.sh run --rm --no-deps gateway migrate
   ```

5. 确认 `schema_migrations` 中存在 `0006_api_key_lifecycle.sql`；每个迁移前 Key 都有
   对应历史引用，活动状态只剩 `active`/`disabled`，旧 `revoked` 行已从活动凭证表
   删除，迁移前活动 Key 的密文字段为空。再次核对步骤 1 的用量、账务、配额和审计
   引用，历史行不得丢失或变成孤儿。
6. 启动 Gateway 和 Caddy 后，验证迁移前 Key 仍可认证但查看返回
   `api_key_secret_unavailable`；创建新 Key 后验证查看、停用、重新启用和永久删除，
   并确认删除不会移除既有用量或账务历史。全部通过后最后恢复 Tunnel。
7. `0006` 写入后禁止旧二进制连接该数据库卷。需要回退时停止所有写入，将升级前
   备份恢复到新的隔离数据库卷，再切换旧 revision；不得删除迁移记录或手工反向
   修改生产 schema。

### 升级到 `0007_upstream_accounts.sql`

`0007` 增加上游账号的非 secret 元数据和最终成功账号归因。它不会读取 sidecar
OAuth 文件，也不会反推迁移前请求；旧明细、聚合和 ledger 的账号列必须保持
`NULL`，在管理台统一显示为“未归因”。按以下顺序升级：

1. 停止 Tunnel、Caddy 和 Gateway 写入，生成新的数据库加密备份并完成恢复演练。
   记录 `usage_requests`、`usage_daily`、`usage_monthly` 和
   `billing_ledger_entries` 的行数及账务金额合计作为迁移前基线。
2. 检出已审阅 revision，确认 CLIProxyAPI 固定多账号补丁仍能对固定 commit
   `git apply --check --ignore-space-change`，并确认 sidecar 配置保持两账号尝试上限、
   一小时粘滞和流式 handler 层零 bootstrap retry。运行 `./scripts/validate-compose.sh`，
   再构建 Gateway 与 `codex-compat`。
3. PostgreSQL 保持 healthy、旧 Gateway 保持停止，用新镜像显式执行迁移，并确认
   `schema_migrations` 中存在 `0007_upstream_accounts.sql`。
4. 确认 `upstream_accounts` 只含稳定账号索引、严格脱敏邮箱、套餐、可用状态和同步
   时间；数据库中不得出现完整邮箱、OAuth token、refresh token 或管理员归属。
   再次核对步骤 1 的行数和金额，迁移前行的 `upstream_account_id` 必须全部为
   `NULL`。
5. 启动唯一 sidecar 和 Gateway，逐个账号执行设备码登录或刷新。Owner 的“上游
   账号”视图应只显示 `a***@example.com` 形式的邮箱，并能按按钮实时查询固定
   ChatGPT 用量地址；异常 JSON、超时、重认证和上游字段变化只能让即时额度查询
   失败，不得影响代理请求。
6. 用两个以上账号验证新会话近似轮询、同一会话一小时粘滞、账号不可用时重新绑定，
   以及普通和 SSE 请求都只归因到最终成功账号。未知或缺失归因头必须进入“未归因”，
   不得伪造账号关联；非 Owner 和跨站额度请求必须拒绝。
7. `0007` 写入后禁止旧二进制连接该数据库卷。需要回退时停止所有写入，将升级前
   备份恢复到新的隔离数据库卷，再切换旧 revision；不得删除迁移记录、手工回填
   历史账号或反向修改生产 schema。

### 升级到 `0008_model_access.sql`

`0008` 新增模型权限默认值、用户快照和注册触发器。迁移本身不知道运行时价格目录；
新 Gateway 在监听端口前会用 `GATEWAY_USAGE_PRICING_JSON` 同步可管理模型，为新模型
创建启用默认值并给全部已有用户补齐启用记录。按以下顺序验收：

1. 停止公网入口和旧 Gateway，生成加密备份并记录用户、usage、配额、账务与审计行数。
2. 用新镜像显式执行迁移，再启动 Gateway；若价格目录同步失败，进程必须拒绝监听。
3. 确认 `model_access_defaults` 包含除 `codex-auto-review` 外的当前价格目录模型，且
   `user_model_access` 对每个现有用户/可管理模型恰有一行。已从目录移除的历史记录可
   保留，但 `catalog_active` 必须为 false，管理台和数据面均不得采用。
4. 在非生产账号验证默认值注册快照、默认修改不追溯、选中/全部批量、无效目标整批
   回滚和审计原因/影响数量。再验证 `/v1/models` 只返回价格目录、用户权限与 Key
   白名单交集，畸形或超过 1 MiB 的上游目录返回 502 且没有部分响应。
5. `0008` 写入后禁止旧二进制连接该数据库卷；回退必须恢复升级前备份，不能手工删除
   权限表、触发器或 migration ledger。

## 11. 计划迁机

计划迁机使用数据库逻辑备份，不复制运行中的 PostgreSQL 原始 volume，也不
复制 `codex_oauth` volume。新旧服务器任何时刻只能有一个 Tunnel connector
承载该 hostname，也不能让两个 sidecar 共享或继续使用同一组 refresh token。

1. 在新服务器安装相同依赖，配置仅固定管理 IP 可访问的 SSH，并关闭所有其他
   入站端口。检出旧服务器正在运行的**同一完整 revision**，确认工作树为空；
   暂不启动任何 Compose 服务。
2. 在旧服务器先停止公网入口，再停止会写数据库或使用 OAuth 的服务：

   ```sh
   ./scripts/compose.sh stop cloudflared
   ./scripts/compose.sh stop caddy gateway codex-compat
   ./scripts/backup-postgres.sh
   ./scripts/compose.sh stop postgres egress-allowlist
   ```

   记录最后一组 `.dump.age` 及其 `.sha256`，旧服务器保持停止状态。
3. 通过经过认证的加密通道，把 `.env`、`deploy/secrets/`（包括 age identity、
   Tunnel token 和完全相同的 `gateway_api_key_encryption_key`）以及最终备份文件对
   复制到新服务器的相同相对路径。恢复
   `.env` 的 `0600`、`deploy/secrets` 目录的 `0700`、服务/Tunnel secret 的
   `0640`、age key 文件与备份文件的 `0600`。把 `.env` 中
   `GATEWAY_SECRET_GID` 更新为新部署用户的主组并将 service secret 设为该组；
   不复制 `.device-login.lock`、容器、PostgreSQL volume 或任何 OAuth 文件。
4. 在新服务器运行静态校验并从同一 revision 现场构建 Gateway 和兼容层。核对
   `.env` 中版本字段与 `git rev-parse HEAD`，不要使用 `local/dev/unknown`：

   ```sh
   ./scripts/bootstrap-secrets.sh
   ./scripts/validate-compose.sh
   ./scripts/compose.sh build gateway codex-compat
   ```

5. 确认新服务器的 `postgres_data` 是首次创建的空命名卷，只启动 PostgreSQL，
   校验最终备份后将其恢复。下面的 `BACKUP_FILE` 必须替换为最终密文的绝对
   路径；若该卷已有任何业务表，立即停止，不得覆盖：

   ```sh
   BACKUP_FILE=/srv/codex-gateway/backups/gateway-YYYYmmddTHHMMSSZ.dump.age
   (cd "$(dirname "$BACKUP_FILE")" && sha256sum -c "$(basename "$BACKUP_FILE").sha256")
   ./scripts/compose.sh up -d postgres
   age --decrypt --identity deploy/secrets/backup_age_identity "$BACKUP_FILE" |
     ./scripts/compose.sh exec -T postgres sh -eu -c '
       PGPASSWORD=$(cat /run/secrets/postgres_password)
       export PGPASSWORD
       exec pg_restore --host=127.0.0.1 --username=gateway --dbname=gateway \
         --exit-on-error --no-owner --no-privileges
     '
   ```

6. 不恢复旧 OAuth volume。在新服务器重新执行
   `./scripts/codex-device-login.sh`，完成权限检查、模型列表和最小 Responses
   冒烟；旧服务器的 sidecar 必须继续停止。
7. 先启动除 Tunnel 外的服务并确认全部 healthy、数据库 migration ledger 和
   管理数据正确；按第 8 节在新机重新安装备份 wrapper 及 cron 或 systemd timer，
   手工成功执行一次后，再单独启动新 connector。最后从公网验证健康检查、
   Passkey、普通 Responses、首事件及时到达及超过两分钟的 SSE：

   ```sh
   ./scripts/compose.sh up -d postgres egress-allowlist codex-compat gateway caddy
   ./scripts/compose.sh ps
   ./scripts/compose.sh up -d cloudflared
   ```

8. 验证审计中的客户端 IP、零宿主端口和每日备份 timer 后再退役旧服务器。迁机
   失败时保持新 connector 停止；如需返回旧服务器，应在旧机重新做设备码登录，
   不能同时启动两边 sidecar。只有确认新机稳定且无需回退后，才按云厂商流程
   安全销毁旧数据库卷和 secret。

## 12. 上线验收清单

生产启用前必须记录以下结果；任一项失败都保持 Tunnel 或调用入口停用：

1. 从另一网络扫描服务器公网 IP，确认只有固定管理 IP 可访问 SSH，80、443、
   5432、8080、8317 和 3128 均不可达；Compose 渲染结果中也没有 `ports`。
2. Cloudflare Dashboard 显示唯一 connector healthy；公网 `/healthz`、`/readyz`
   返回 200，响应为 `DYNAMIC`/`BYPASS` 而非缓存命中，HTTP 会在边缘跳转 HTTPS。
3. 分别伪造 `CF-Connecting-IP` 和 `X-Forwarded-For`，确认 Cloudflare/Caddy
   重建来源链，Gateway 审计只记录真实客户端 IP；缺失或非法来源头应收敛为
   代理地址，不能采用伪造值。
4. 使用真实域名完成 Owner Passkey 注册、退出、重新登录和敏感操作再验证；
   错误 Origin 与跨站请求必须被拒绝，Cookie 保持 `Secure`、`HttpOnly`、
   `SameSite=Strict`。
5. 创建新 API Key，确认普通管理状态不含明文或密文、近期二次验证后可重复查看，
   停用立即返回 `403 key_disabled`、重新启用恢复、永久删除后列表和新认证均不可见；
   升级环境还应确认迁移前 Key 显示为不可查看。关闭查看弹窗和退出登录后页面不得
   保留 Key 明文。
6. 以 Owner 检查全员使用统计：目录/汇率日期正确，USD 等于不可变 ledger 合计，
   `actual_cost_usd = charged_usd + uncovered_usd`，`estimated_usd` 与
   `actual_cost_usd` 相同。分别核对服务层、短/长上下文、cache-write Token 和
   兜底原因；修改测试环境当前价格 JSON 后，历史 ledger USD 必须不变。未配置
   模型必须在转发前返回 `model_pricing_not_found`。确认所有金额均标为
   “OpenAI API Token 等价成本”，没有呈现为 OpenAI 实际账单。
7. 在“模型权限”中验证新用户默认值与已有用户批量操作互不追溯；禁用一个测试用户
   后，其下一次请求必须在 usage、配额、账务预留和 sidecar 调用前返回
   `model_not_allowed`。`/v1/models` 必须同时受用户权限和 Key 白名单过滤。
8. 完成模型列表、普通 Responses 和 SSE：首个事件必须在请求结束前到达，超过
   两分钟的代表性长流不能被聚合或无故断开，客户端主动断开后上游请求和并发
   lease 会被取消或结算；响应不得被 Cloudflare 缓存。
9. 验证超过 64 MiB 的请求在代理或 Gateway 返回 413，且日志、数据库及备份中
   不出现测试 canary 的正文或凭证。
10. 重启服务器，确认 Docker 与预期容器恢复、PostgreSQL 命名卷数据不变；再
   手工运行一次每日备份任务和恢复演练，确认只保留最近 14 组完整文件对。
