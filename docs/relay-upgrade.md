# 现有双服务器中转升级：让 Gemini 同样经 B

适用于 A 上网关与 Gemini 已正常运行、Codex 已按旧版
[双服务器教程](openai-relay.md) 经 WireGuard 和 B Squid 出网的部署。
首次配置双机中转请使用该教程。

本次沿用现有 `CODEX_RELAY_IP` / `CODEX_RELAY_PORT`。升级后 Codex 和整个
Antigravity Bridge 的获准出口都经 B；B 故障时两者均失败，不自动回退 A。
这包括 Bridge 的服务器侧认证与刷新，不影响用户本地浏览器的网络设置。

只部署本次代理配置变更，无需重建应用镜像、变更数据库、重建 WireGuard、
重新生成 secret 或重新登录。若目标版本还包含其他应用变更，另按其升级说明处理。
配置更新要先 B 后 A，两次代理重建均会中断现有 CONNECT/SSE，应安排短维护窗口。
不要执行整套 `compose down`，也不要删除 OAuth 或 Keyring 卷。

## 1. 记录并备份旧配置

更新 A 仓库文件前，在 A 的仓库根目录执行。示例系统命令以 root Shell 运行：

```bash
set -euo pipefail
B_PUBLIC_IP='填写现有B公网IPv4'
B_SSH_PORT='填写现有B的SSH端口'
A_UPGRADE_BACKUP="/var/backups/codex-relay/$(date -u +%Y%m%dT%H%M%SZ)"
install -d -m 700 "$A_UPGRADE_BACKUP"
tar -czf "$A_UPGRADE_BACKUP/a-config.tar.gz" \
    deploy/egress deploy/relay deploy/images.lock.env scripts/validate-compose.sh
chmod 600 "$A_UPGRADE_BACKUP/a-config.tar.gz"
printf 'A_UPGRADE_BACKUP=%s\n' "$A_UPGRADE_BACKUP"
wg show wg-codex
./scripts/compose.sh ps egress-allowlist codex-compat antigravity-bridge
```

检查项目 `.env` 的中转 IP/端口仍是当前有效值，通常为 `10.77.0.2:3128`；
不要用示例覆盖经过调整的配置。本页按默认隧道地址展示探测命令。
保留已有 `.env`、服务 secret、WireGuard 配置和应用镜像标签。
备份中包含与旧配置匹配的校验脚本，便于回退时通过旧规则校验。

B 在覆盖文件前另做备份：

```sh
set -eu
B_UPGRADE_BACKUP="/var/backups/codex-relay/$(date -u +%Y%m%dT%H%M%SZ)"
install -d -m 700 "$B_UPGRADE_BACKUP"
tar -czf "$B_UPGRADE_BACKUP/relay-config.tar.gz" -C /opt/codex-relay \
    deploy/relay deploy/images.lock.env
chmod 600 "$B_UPGRADE_BACKUP/relay-config.tar.gz"
printf 'B_UPGRADE_BACKUP=%s\n' "$B_UPGRADE_BACKUP"
wg show wg-codex
```

记录两端输出的备份目录；这些 Shell 变量不会跨 SSH 会话保留。
备份只包含部署文件，不包含账号凭据或 WireGuard 私钥。

## 2. 更新并校验待部署文件

按站点既有发布方式，把 A 仓库更新到包含本次修改的版本。
保留正在运行的应用版本与站点配置。A 仓库根目录执行：

```bash
./scripts/validate-compose.sh
./scripts/test-relay-image.sh
```

前者检查部署策略和固定版本 Squid 语法；后者使用隔离容器和合成服务检查强制中转、
故障不回退、流式连接和 ACL，不访问真实上游账号。测试通过不等于生产出口已验收。

将同一版本的 B 配置和镜像锁同步到 B：

```bash
tar -czf - deploy/relay deploy/images.lock.env |
    ssh -p "$B_SSH_PORT" root@"$B_PUBLIC_IP" \
        'tar -xzf - -C /opt/codex-relay'
```

B 的配置现在允许既有 OpenAI 域名和经审查的 Antigravity 精确域名，
仍只接受 A 的 WireGuard 地址、HTTPS CONNECT 和 443 端口。无需改 WireGuard
路由、原防火墙或应用的代理环境变量。

A/B 的域名规则均使用 `dstdomain -n`：按允许主机名发起的 CONNECT 保持可用，
直接请求 IP 不能再通过反向 DNS 命中域名白名单。

## 3. 先更新 B

B：

```sh
cd /opt/codex-relay/deploy/relay
unset SQUID_IMAGE
docker compose --env-file ../images.lock.env config -q
docker compose --env-file ../images.lock.env run --rm --no-deps \
    --entrypoint /usr/sbin/squid relay -k parse -f /etc/squid/squid.conf
docker compose --env-file ../images.lock.env up -d --no-deps --force-recreate relay
docker compose --env-file ../images.lock.env ps
```

`config -q` 检查 Compose，下一条命令才检查 Squid 配置。
A、B 都使用单文件 bind mount，更新文件可能替换 inode；必须强制重建以加载新文件，
不能把普通 `up -d`、`restart` 或 Squid reload 当作同等保证。

A 直接经 B 测试两类域名：

```bash
for host in chatgpt.com cloudcode-pa.googleapis.com; do
    curl --interface 10.77.0.1 --noproxy '' \
        --proxy http://10.77.0.2:3128 --max-time 20 \
        --silent --show-error --output /dev/null \
        --write-out "$host CONNECT=%{http_connect} upstream=%{http_code}\n" \
        "https://$host/"
done
```

确认 `CONNECT=200` 且 curl 完成 TLS/HTTP 交换，再切换 A。
`upstream=403` / `404` 可能是目标网站/API 对根路径的正常响应，不代表真实生成成功；
`CONNECT=403` 则是代理拒绝。此时若失败，保持 A 原运行配置，先排查 B 日志与隧道。

## 4. 再更新 A

A 仓库根目录：

```bash
./scripts/validate-compose.sh
./scripts/compose.sh up -d --no-deps --force-recreate egress-allowlist
./scripts/compose.sh ps egress-allowlist
```

不需要修改现有中转环境变量。A 的生成配置现在同时为
`codex_clients` 与 `antigravity_clients` 选择父代理并禁止直连。
现有 Google CONNECT 会随代理重建断开，Bridge 后续连接将经 B。

## 5. 验收与故障恢复

在 A 依次运行真实账号冒烟，执行时避开其他 Gemini 请求占用单并发槽：

```bash
./scripts/smoke-sidecar.sh
./scripts/compose.sh exec -T -e TERM=dumb antigravity-bridge \
    /usr/local/bin/antigravity-smoke
./scripts/compose.sh exec -T egress-allowlist tail -n 100 /var/log/squid/access.log
```

Codex 与 Gemini 均应退出码为 0。Gemini 冒烟检查模型目录、JSON 和 SSE，
会产生真实生成和额度消耗，不输出回复内容。A 的 OpenAI 和 Google 成功 CONNECT
都应显示 `PARENT/10.77.0.2`，不能出现成功的 `HIER_DIRECT`。

B 对照目标与时间：

```sh
cd /opt/codex-relay/deploy/relay
docker compose --env-file ../images.lock.env exec -T relay \
    tail -n 100 /var/log/squid/access.log
```

维护窗口内做故障测试，先在 B 停止 relay：

```sh
docker compose --env-file ../images.lock.env stop relay
```

A 用条件分支分别检查预期失败，避免 `set -e` 跳过第二个模型：

```bash
if ./scripts/smoke-sidecar.sh; then
    printf '%s\n' '验收失败：B 停止后 Codex 仍成功' >&2
else
    printf '%s\n' '符合预期：Codex 请求失败'
fi
if ./scripts/compose.sh exec -T -e TERM=dumb antigravity-bridge \
    /usr/local/bin/antigravity-smoke; then
    printf '%s\n' '验收失败：B 停止后 Gemini 仍成功' >&2
else
    printf '%s\n' '符合预期：Gemini 请求失败'
fi
```

任何一条意外成功都算验收失败。无论结果如何，检查后立即在 B 恢复 relay，
再在 A 验证两类冒烟成功及对应父代理日志：

```sh
docker compose --env-file ../images.lock.env up -d relay
```

`/readyz`、容器 healthy 或成功握手都不能替代生成及父代理日志验收。
如有新 Google 域名被拒绝，按用途审核后同步更新 A/B 的精确清单及校验/测试；
不放宽到通配域名。完整重启检查见[首次部署指南](openai-relay.md#8-故障重启与回退)。

## 6. 回退

**只撤销本次 Gemini 中转，恢复 Codex 经 B、Gemini 经 A：**

先在 A 仓库根目录恢复升级前的匹配配置和校验脚本，并强制重建 A 代理。
如果换过 SSH 会话，先将 `A_UPGRADE_BACKUP` 设置为第 1 节记录的实际目录：

```bash
tar -xzf "$A_UPGRADE_BACKUP/a-config.tar.gz"
./scripts/validate-compose.sh
./scripts/compose.sh up -d --no-deps --force-recreate egress-allowlist
```

这里保留 `.env` 中原有非空中转地址。A 回退并验证两类真实请求后，再在 B
恢复旧的 OpenAI 专用 allowlist；不要先收紧 B 而使仍经 B 的 Gemini 请求失败：

```sh
tar -xzf "$B_UPGRADE_BACKUP/relay-config.tar.gz" -C /opt/codex-relay
cd /opt/codex-relay/deploy/relay
unset SQUID_IMAGE
docker compose --env-file ../images.lock.env config -q
docker compose --env-file ../images.lock.env run --rm --no-deps \
    --entrypoint /usr/sbin/squid relay -k parse -f /etc/squid/squid.conf
docker compose --env-file ../images.lock.env up -d --no-deps --force-recreate relay
```

**让 Codex 和 Gemini 都改回 A 直连：**

保留新版配置，清空 A 项目 `.env` 的 `CODEX_RELAY_IP`，然后执行：

```bash
./scripts/validate-compose.sh
./scripts/compose.sh up -d --no-deps --force-recreate egress-allowlist
```

这是两类流量一起切回 A，不是只回退 Gemini。两种回退都会中断当前代理连接，
均无需删除 WireGuard、重新登录或重建数据库。
