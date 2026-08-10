# 部署与运维手册

本项目按单台 Linux 云服务器、单个 ChatGPT Pro 上游账号设计。Cloudflare
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

## 2. 供应链锁定

`deploy/images.sources` 保存人工审阅的具体版本标签，
`deploy/images.lock.env` 保存仓库返回的不可变 manifest digest。正常部署
直接使用已提交的 lock 文件。只有升级时才重新解析：

```sh
./scripts/lock-images.sh
git diff -- deploy/images.sources deploy/images.lock.env
```

确认版本和 digest 的差异后再提交。不要手写 digest，也不要在生产中使用
`latest`。CLIProxyAPI 的构建还会证明 `v7.2.127` 的 peeled commit 正是
`ecc9aa72b32f34b680d03b0724b531a21ae74472`，不匹配就会失败。

## 3. 服务密钥

```sh
./scripts/bootstrap-secrets.sh
```

脚本以 `0640` 创建 PostgreSQL 口令、完整 DSN、两个 HMAC pepper 和
Gateway 到 sidecar 的内部随机密钥，不会显示密钥。Compose 通过
`/run/secrets` 文件传入，禁止复制到 `.env`、命令行或工单。

本机 Compose 以只读 bind mount 实现 secret，长语法的所有权映射通常会被
忽略。因此 `.env` 中的 `GATEWAY_SECRET_GID` 必须设为专用部署用户的私有
主组（运行 `id -g` 获取）。Gateway 和 sidecar 仍以非 root UID 10001 运行，
仅通过这个组读取 `0640` secret；文件绝不能放宽为全局可读。生产主机不应让
其他交互用户加入该组。OAuth 卷不使用此机制，仍严格为 UID 10001、`0600`。

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
host 连接都强制使用 SCRAM、sidecar 是只读且非 root、secret 权限及所有基础
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
2. 保持域名白名单出口代理运行，执行 `--codex-device-login`。
3. 拒绝任何 symlink、非 UID 10001 或非 `0600` 的 OAuth 文件。
4. 重启唯一 sidecar，等待健康检查。
5. 使用内部固定凭证调用模型列表及最小流式 Responses，且不输出响应正文。

任何一步失败都会让 sidecar 保持停止，防止继续使用不确定的 refresh token。
修复原因后重新执行。严禁手动启动第二个挂载同一 `codex_oauth` 卷的容器。

可独立复查：

```sh
./scripts/verify-oauth-permissions.sh
./scripts/smoke-sidecar.sh
```

OAuth 卷不属于备份。灾备恢复、卷损坏或 refresh token 失效后重新执行设备
码登录，不要从旧快照复制 token。

## 6. Owner 初始化和日常身份运维

服务首次 ready 后，通过 Gateway 的服务器端命令生成一次性初始化链接：

```sh
./scripts/compose.sh exec gateway gateway bootstrap-owner
```

只在 SSH 终端中运行，并把完整链接直接交给 Owner；链接中的令牌必须位于
URL fragment，不能作为 query string。

Owner 完成 Passkey 注册后，邀请、恢复、API Key 创建/撤销和敏感操作都在
HTTPS 管理界面完成。API Key 只显示一次。不要截图、记录或通过聊天系统转发
API Key、恢复码、邀请 fragment 或 Passkey challenge。

## 7. 客户端接入

客户端配置见 [client-config.md](client-config.md)。每台设备使用独立 Key，
通过环境变量传给 Codex CLI。撤销测试应确认只有目标 Key 立即收到 401/403，
其他设备不受影响。

## 8. 加密备份和恢复演练

先手工创建并验证一组备份：

```sh
./scripts/backup-postgres.sh
```

脚本把 custom-format `pg_dump` 直接管道到 age，不在磁盘落明文，并为密文
写 SHA-256 校验文件。默认输出到被 Git 忽略的 `backups/`；也可通过
`BACKUP_DIR` 指向本机另一受限目录。目录必须为部署用户所有、模式 `0700`，
备份与校验文件必须为 `0600`。当前方案不把它们复制到异地。

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

## 10. 升级和回滚

升级 Gateway 前先按第 8 节生成新的数据库密文备份并成功完成恢复演练。然后在
服务器检出已审阅 revision、更新 `.env` 的三个版本字段、验证工作树为空并现场
构建；不要覆盖或清理上一 revision 镜像。升级 CLIProxyAPI 前还必须：

1. 审阅新版本、commit、MIT notice 和依赖差异；更新 sources/lock。
2. 在 CI 运行 Responses 普通/SSE、compact、401、429、跨 chunk usage 和刷新
   token 契约测试。
3. 持有 `.device-login.lock` 的 `flock` 并停止生产 sidecar，确保没有两个实例
   共享 token。
4. 用测试 OAuth 状态完成契约验证，再进行一次人工 Pro 冒烟。
5. 单实例滚动替换；失败时停止新实例，再回到旧镜像，不能并行回滚。

Gateway 启动会写入嵌入式迁移记录；旧二进制检测到未知迁移会明确拒绝数据库
降级。因此，只在确认新 revision **没有写入任何新迁移** 时才可直接把镜像标签
切回上一 revision。只要新迁移已写入，无论 SQL 看起来是否兼容，都禁止仅回滚
二进制；必须停止写入，把升级前已演练的备份恢复到新的隔离数据库卷，验证后
再让旧 revision 切换到该卷。不得对生产卷手工删除 migration ledger 或反向
修改 schema。

## 11. 计划迁机

计划迁机使用数据库逻辑备份，不复制运行中的 PostgreSQL 原始 volume，也不
复制 `codex_oauth` volume。新旧服务器任何时刻只能有一个 Tunnel connector
承载该 hostname，也不能让两个 sidecar 共享或继续使用同一 refresh token。

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
3. 通过经过认证的加密通道，把 `.env`、`deploy/secrets/`（包括 age identity 和
   Tunnel token）以及最终备份文件对复制到新服务器的相同相对路径。恢复
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
5. 完成模型列表、普通 Responses 和 SSE：首个事件必须在请求结束前到达，超过
   两分钟的代表性长流不能被聚合或无故断开，客户端主动断开后上游请求和并发
   lease 会被取消或结算；响应不得被 Cloudflare 缓存。
6. 验证超过 64 MiB 的请求在代理或 Gateway 返回 413，且日志、数据库及备份中
   不出现测试 canary 的正文或凭证。
7. 重启服务器，确认 Docker 与预期容器恢复、PostgreSQL 命名卷数据不变；再
   手工运行一次每日备份任务和恢复演练，确认只保留最近 14 组完整文件对。
