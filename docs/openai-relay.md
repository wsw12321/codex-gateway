# Codex 与 Gemini 双服务器中转：首次部署

本页保留原 OpenAI 中转教程地址。已按旧教程部署“Codex 经 B、Gemini 经 A”的站点，
请使用[现有部署升级指南](relay-upgrade.md)，无需重建 WireGuard 或重新登录账号。
在直连、旧 WireGuard 中转和 Shadowsocks 之间切换，见[出口模式说明](egress.md)。

启用中转后的链路：

```text
Gateway → codex-compat ───────┐
                            ├→ A Squid → WireGuard → B Squid → 对应上游
Gateway → antigravity-bridge ┘
```

A 仍按容器来源分别限制 OpenAI 和 Antigravity 的精确域名。两者共用
`CODEX_RELAY_IP` / `CODEX_RELAY_PORT`；变量名为兼容旧部署而保留。
本教程显式设置 `EGRESS_MODE=relay`，两者均强制经 B，B 故障就失败，不自动回退 A。
旧 `.env` 未设置模式或值为空时，仍按 IP 是否非空选择旧中转或直连。

这会迁移整个 Antigravity Bridge 的获准出口，包括服务器侧登录、刷新、模型查询
和生成请求，不只按 Gemini 模型名分流。用户本地浏览器的授权流量不在此链路内。
B 只转发 HTTPS CONNECT，不终止上游 TLS，也不保存上游凭据。

## 1. 前提与变量

本教程使用 A 的 Debian/Ubuntu + systemd、B 的 Alpine + OpenRC，双方已具备
内核 WireGuard 支持。A 已按 [README](../README.md#部署准备) 准备项目、Docker Compose、
`.env`、服务 secret 和所需镜像；这里不替代网关首次安装流程。若尚未启动网关，
先完成这些准备，再按下文创建出口网络，不要提前执行账号登录。

WireGuard 固定使用 A `10.77.0.1/32`、B `10.77.0.2/32`；B Squid 固定监听
`10.77.0.2:3128`。确认这些网段没有冲突。若使用其他地址，需要同步修改受审查的
部署配置、校验和测试，不能仅改一端命令。

系统配置命令在 root Shell 中执行。A 的项目命令在仓库根目录执行。下列变量仅在
当前 SSH 会话有效，重新登录后需重新设置；不要把私钥粘贴进命令或打印出来。

A（将占位值替换为实际值）：

```bash
set -euo pipefail
B_PUBLIC_IP='填写B公网IPv4'
B_SSH_PORT='填写B的SSH端口'
WIREGUARD_PORT='填写B对外可达的UDP端口'
```

B：

```sh
set -eu
A_PUBLIC_IP='填写A公网IPv4'
WIREGUARD_PORT='与A相同的UDP端口'
```

普通 VPS 可选 `51820/UDP`；NAT VPS 必须选服务商实际转发的 UDP 端口。

## 2. 安装依赖并准备出口网络

A：

```bash
apt update
apt install -y wireguard iptables curl
```

B：在 `/etc/apk/repositories` 启用与本机 Alpine 版本一致的 community 仓库，
然后执行：

```sh
apk update
apk add docker docker-cli-compose wireguard-tools iptables iptables-openrc iproute2
```

在 A 的项目 `.env` 中设置持久配置：

```dotenv
EGRESS_MODE=relay
CODEX_RELAY_IP=10.77.0.2
CODEX_RELAY_PORT=3128
```

现有容器在后续重建时才采用新配置。`scripts/compose.sh` 会清理项目同名环境变量，
所以仅在 Shell 中 `export CODEX_RELAY_IP=...` 不能替代编辑 `.env`。

如果 A 的出口网络尚不存在，先只创建代理容器与网络，不启动服务：

```bash
if ! docker network inspect codex-gateway_egress_external >/dev/null 2>&1; then
    ./scripts/compose.sh create egress-allowlist
fi
EGRESS_SUBNET="$(
    docker network inspect codex-gateway_egress_external \
        --format '{{(index .IPAM.Config 0).Subnet}}'
)"
test -n "$EGRESS_SUBNET"
printf 'EGRESS_SUBNET=%s\n' "$EGRESS_SUBNET"
iptables -nL DOCKER-USER >/dev/null
```

后续规则必须使用这个真实子网，不能照抄示例网段。本教程依赖 Docker 的
`DOCKER-USER` iptables 链；最后一条失败时先核实主机 Docker 防火墙配置，不要继续
安装 WireGuard hooks。

## 3. 复制 B 配置并生成密钥

A 仓库根目录：

```bash
test -r deploy/relay/squid.conf
test -r deploy/relay/wg-codex
test -r deploy/images.lock.env
tar -czf - deploy/relay deploy/images.lock.env |
    ssh -p "$B_SSH_PORT" root@"$B_PUBLIC_IP" \
        'mkdir -p /opt/codex-relay && tar -xzf - -C /opt/codex-relay'
```

A、B 分别在本机执行以下首次初始化命令。若已有这些文件，停止并核实原部署，
不要覆盖密钥或按本页重新初始化：

```sh
install -d -m 700 /etc/wireguard
(
    set -eu
    umask 077
    test ! -e /etc/wireguard/wg-codex.key
    test ! -e /etc/wireguard/wg-codex.pub
    test ! -e /etc/wireguard/wg-codex.conf
    wg genkey > /etc/wireguard/wg-codex.key
    wg pubkey < /etc/wireguard/wg-codex.key > /etc/wireguard/wg-codex.pub
)
cat /etc/wireguard/wg-codex.pub
```

只交换公钥。在 A 设置：

```bash
B_PUBLIC_KEY='填写B的wg-codex.pub内容'
```

在 B 设置：

```sh
A_PUBLIC_KEY='填写A的wg-codex.pub内容'
```

## 4. 配置 WireGuard 与防火墙

A 开启容器流量转发：

```bash
printf 'net.ipv4.ip_forward=1\n' > /etc/sysctl.d/90-codex-relay.conf
sysctl -p /etc/sysctl.d/90-codex-relay.conf
```

A 写入配置；私钥直接从本机文件读取，不经终端输出：

```bash
(
    set -eu
    umask 077
    test ! -e /etc/wireguard/wg-codex.conf
    cat > /etc/wireguard/wg-codex.conf <<EOF
[Interface]
Address = 10.77.0.1/32
PrivateKey = $(cat /etc/wireguard/wg-codex.key)

PostUp = iptables -I DOCKER-USER 1 -s $EGRESS_SUBNET -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j ACCEPT
PostUp = iptables -I DOCKER-USER 1 -i %i -s 10.77.0.2/32 -d $EGRESS_SUBNET -p tcp --sport 3128 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
PostUp = iptables -t nat -I POSTROUTING 1 -s $EGRESS_SUBNET -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j SNAT --to-source 10.77.0.1

PostDown = iptables -D DOCKER-USER -s $EGRESS_SUBNET -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j ACCEPT
PostDown = iptables -D DOCKER-USER -i %i -s 10.77.0.2/32 -d $EGRESS_SUBNET -p tcp --sport 3128 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
PostDown = iptables -t nat -D POSTROUTING -s $EGRESS_SUBNET -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j SNAT --to-source 10.77.0.1

[Peer]
PublicKey = $B_PUBLIC_KEY
Endpoint = $B_PUBLIC_IP:$WIREGUARD_PORT
AllowedIPs = 10.77.0.2/32
PersistentKeepalive = 25
EOF
)
```

B 写入配置：

```sh
(
    set -eu
    umask 077
    test ! -e /etc/wireguard/wg-codex.conf
    cat > /etc/wireguard/wg-codex.conf <<EOF
[Interface]
Address = 10.77.0.2/32
ListenPort = $WIREGUARD_PORT
PrivateKey = $(cat /etc/wireguard/wg-codex.key)

[Peer]
PublicKey = $A_PUBLIC_KEY
AllowedIPs = 10.77.0.1/32
EOF
)
```

B 不设置默认路由，也不需要开启 IPv4 forwarding。以下规则仅添加本方案需要的
规则，不清空既有 SSH 防火墙；按顺序执行一次：

```sh
iptables -I INPUT 1 -p udp --dport "$WIREGUARD_PORT" -j DROP
iptables -I INPUT 1 -p udp -s "$A_PUBLIC_IP/32" --dport "$WIREGUARD_PORT" -j ACCEPT
iptables -I INPUT 1 -p tcp --dport 3128 -j DROP
iptables -I INPUT 1 -i wg-codex -s 10.77.0.1/32 -d 10.77.0.2/32 \
    -p tcp --dport 3128 -j ACCEPT
rc-update add iptables boot
rc-service iptables save
iptables -L INPUT -n -v --line-numbers
```

B 的云安全组或 NAT 面板也须允许 A 公网 IP 访问所选 UDP 端口。
TCP 3128 不开放到公网。

## 5. 启动隧道

A 的 hooks 依赖 Docker，先设置 systemd 顺序：

```bash
mkdir -p /etc/systemd/system/wg-quick@wg-codex.service.d
printf '[Unit]\nRequires=docker.service\nAfter=docker.service\n' \
    > /etc/systemd/system/wg-quick@wg-codex.service.d/docker.conf
systemctl daemon-reload
systemctl enable --now wg-quick@wg-codex
systemctl status wg-quick@wg-codex --no-pager
```

B 安装随仓库提供的 OpenRC 服务，并在 Docker 前启动 WireGuard：

```sh
install -m 755 /opt/codex-relay/deploy/relay/wg-codex /etc/init.d/wg-codex
rc-update add wg-codex default
rc-update add docker default
rc-service wg-codex start
rc-service docker start
rc-service wg-codex status
```

双方检查：

```sh
wg show wg-codex
```

应有近期 `latest handshake` 和传输计数。这个命令默认隐藏私钥；
不要用 `wg showconf`、`wg-quick strip` 或直接输出配置文件代替检查。
没有握手时先核实公钥、UDP 端口、云防火墙和 A 公网 IP。

## 6. 先启动 B，再切换 A

B：

```sh
cd /opt/codex-relay/deploy/relay
unset SQUID_IMAGE
docker compose --env-file ../images.lock.env config -q
docker compose --env-file ../images.lock.env pull relay
docker compose --env-file ../images.lock.env run -T --rm --no-deps \
    --entrypoint /usr/sbin/squid relay -k parse -f /etc/squid/squid.conf </dev/null
docker compose --env-file ../images.lock.env up -d --no-deps --force-recreate relay
docker compose --env-file ../images.lock.env ps
```

先准备镜像锁指定的固定 digest，再用关闭 TTY、stdin 的临时容器解析配置。
这兼容 B 的 Docker Compose v2.23.3，其 `run` 命令不支持 `--pull` 选项。

A 先直接测试 B 的两类 CONNECT。此时不依赖 A Squid：

```bash
for host in chatgpt.com cloudcode-pa.googleapis.com; do
    curl --interface 10.77.0.1 --noproxy '' \
        --proxy http://10.77.0.2:3128 --max-time 20 \
        --silent --show-error --output /dev/null \
        --write-out "$host CONNECT=%{http_connect} upstream=%{http_code}\n" \
        "https://$host/"
done
```

要求 `CONNECT=200` 且 curl 完成 TLS/HTTP 交换。`upstream=403` 或 `404` 可能是
网站或 API 根路径的正常响应，只说明链路可通，不证明账号、模型或生成可用。
`CONNECT=403` 则是代理拒绝；`000`、超时、连接拒绝需排查 Squid、WireGuard 和防火墙。

确认 B 可用后，A 回到仓库根目录：

```bash
./scripts/validate-compose.sh
./scripts/apply-egress.sh
./scripts/compose.sh ps egress-allowlist
```

配置和入口脚本是单文件 bind mount；更新源文件可能替换 inode，因此使用
`--force-recreate` 保证加载新文件及生成路由。重建会中断现有 CONNECT 和 SSE，
已有业务应安排短维护窗口。不要执行 `compose down`；它可能删除出口网络，
让 WireGuard hooks 保存的子网失效。

若 A 的其他网关服务尚未启动，此时继续 README 的服务启动步骤。
尚未登录的账号此时再执行：

```bash
./scripts/codex-device-login.sh
./scripts/antigravity-login.sh
```

Antigravity 必须出现 `Antigravity login persisted; readiness, JSON and SSE passed.`。
然后按 [Gemini 部署说明](gemini-pro.md#部署和登录) 启用精确模型路由并更新 Gateway。
已有有效凭据无需因为切换出口重新登录。

## 7. 连通性与真实上游验收

先区分检查的范围。在 A 仓库根目录执行：

```bash
./scripts/smoke-sidecar.sh
./scripts/compose.sh exec -T -e TERM=dumb antigravity-bridge \
    /usr/local/bin/antigravity-smoke
```

`smoke-sidecar.sh` 只检查 Gateway readiness 和 Sidecar 的账号、模型、访问能力元数据，
**不发起 Codex 生成**，B 停止后也可能成功；它不能证明 Codex 的生成链路或故障策略。
第二条 `antigravity-smoke` 才会实际检查 Gemini JSON/SSE 生成，成功以退出码 0
为准，不输出模型内容。执行时避开其他 Gemini 请求占用 Bridge 的单并发槽。

### Codex 真实生成

Codex 生成必须经 Gateway 的真实用户鉴权、模型权限和计费准入。
在 A 使用用户明确提供的私有 API Key 文件；它须只含一行 Gateway 用户 Key，
权限为 `0600` 或 `0400`。不要用 Sidecar 内部 Key，也不要把 Key 写入命令、
项目 `.env` 或日志。下面只填写文件路径、本站 HTTPS origin 和该用户获准使用的
可用 Codex 模型名；主机需要 `curl`、`jq` 和 Linux `/dev/shm`：

```bash
GATEWAY_URL='https://填写本站域名'
GATEWAY_API_KEY_FILE='/填写用户指定的私有Key文件绝对路径'
CODEX_CHECK_MODEL='填写该用户已获准使用且上游可用的Codex模型'
```

在同一 Bash 会话定义函数，后续用于正常、断链与恢复验证：

```bash
codex_generation_check() (
    set +x
    set -uo pipefail
    umask 077
    test "$#" -eq 1 && test -n "${GATEWAY_URL:-}" &&
        test -n "${GATEWAY_API_KEY_FILE:-}" && test -n "${CODEX_CHECK_MODEL:-}" || exit 2
    case "$1" in false|true) ;; *) exit 2 ;; esac
    test -f "$GATEWAY_API_KEY_FILE" && test ! -L "$GATEWAY_API_KEY_FILE" || exit 2
    case "$(stat -c '%a' "$GATEWAY_API_KEY_FILE")" in 400|600) ;; *) exit 2 ;; esac
    work_dir=$(mktemp -d /dev/shm/codex-relay-check.XXXXXX) || exit 2
    trap 'rm -rf "$work_dir"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    unset gateway_check_key
    gateway_check_key=$(tr -d '\r\n' < "$GATEWAY_API_KEY_FILE") || exit 2
    case "$gateway_check_key" in ''|*[!A-Za-z0-9_-]*) exit 2 ;; esac
    printf 'header = "Authorization: Bearer %s"\n' "$gateway_check_key" \
        > "$work_dir/curl.conf" || exit 2
    unset gateway_check_key
    jq -n --arg model "$CODEX_CHECK_MODEL" --argjson stream "$1" \
        '{model:$model,input:"Reply with exactly OK.",store:false,stream:$stream}' \
        > "$work_dir/request.json" || exit 2
    curl -q --config "$work_dir/curl.conf" --proto '=https' \
        --silent --show-error --fail --connect-timeout 10 --max-time 180 \
        --header 'Content-Type: application/json' \
        --data-binary @"$work_dir/request.json" \
        --output "$work_dir/response" "$GATEWAY_URL/v1/responses" || exit 1
    if test "$1" = false; then
        jq -e '.status == "completed" and .error == null and
            any(.output[]?; .type == "message" and
                any(.content[]?; .type == "output_text" and (.text | length > 0)))' \
            "$work_dir/response" >/dev/null 2>&1 || exit 1
    else
        awk '{ sub(/\r$/, "") }
            /^data: / && $0 != "data: [DONE]" { sub(/^data: /, ""); print }' \
            "$work_dir/response" |
            jq -se 'any(.[]; .type == "response.completed" and
                .response.status == "completed" and
                any(.response.output[]?; .type == "message" and
                    any(.content[]?; .type == "output_text" and (.text | length > 0)))) and
                all(.[]; .type != "error" and .type != "response.failed")' \
                >/dev/null 2>&1 || exit 1
    fi
    printf 'Codex generation passed (stream=%s)\n' "$1"
)
codex_generation_check false
codex_generation_check true
```

凭据只经私有 curl 配置文件传入；请求和响应暂存在私有 tmpfs 目录，函数退出即删除，
不打印 Key 或回复正文。函数返回 0 才代表该模式生成完成；返回 1 表示请求或生成检查失败，
返回 2 表示参数、凭据文件或本地准备失败。JSON 和 SSE 都应成功，再结合父代理日志及
Gateway 用量记录验收。这些调用会实际消耗账号额度；不要启用 Shell 跟踪或 curl 调试输出。

未获得用户 Key 时，明确记录“Codex 真实 JSON/SSE 生成尚未验收”。
成功的 CONNECT、配额查询或本地元数据检查只证明各自覆盖的路径，不能替代生成。
容器 healthy、WireGuard 握手或 Gateway `/readyz` 同样不能替代；Gateway `/readyz`
只检查数据库。

A 查看实际转发路径：

```bash
./scripts/compose.sh exec -T egress-allowlist tail -n 100 /var/log/squid/access.log
```

OpenAI 与 Google 的成功 CONNECT 都应出现 `PARENT/10.77.0.2`，前面的层级名称可能
为 `FIRSTUP_PARENT` 或其他父代理类型；不应出现成功的 `HIER_DIRECT`。
日志只含时间、CONNECT 目标、状态和转发路径，不记录 TLS 内容或认证头。

B 对照同一时间的 CONNECT：

```sh
cd /opt/codex-relay/deploy/relay
docker compose --env-file ../images.lock.env exec -T relay \
    tail -n 100 /var/log/squid/access.log
```

进程启动异常可分别用 A 的 `./scripts/compose.sh logs --tail 100 egress-allowlist`
及 B 的 `docker compose --env-file ../images.lock.env logs --tail 100 relay` 排查。
如果出现新的 Google 目标被拒绝，先核实用途，再同步修改 A/B 的精确清单、校验和
回归测试；不要开放通配 Google 域名。

## 8. 故障、重启与回退

故障测试会同时中断两类上游，放在维护窗口执行。先在 B 停止 relay：

```sh
docker compose --env-file ../images.lock.env stop relay
```

故障验收前，必须先在 B 正常时完成上一节的真实生成成功基线。
在 A 保持已定义 `codex_generation_check` 的同一 Bash 会话，使用相同 Key 和模型：

```bash
if declare -F codex_generation_check >/dev/null &&
    test -n "${GATEWAY_API_KEY_FILE:-}" && test -r "$GATEWAY_API_KEY_FILE"; then
    if codex_generation_check false; then
        printf '%s\n' '验收失败：B 停止后 Codex 真实生成仍成功' >&2
    else
        result=$?
        if test "$result" -eq 1; then
            printf '%s\n' 'Codex 真实生成未完成；结合中转失败日志确认故障原因'
        else
            printf '%s\n' 'Codex 本地准备失败，不能判定中转故障验收通过' >&2
        fi
    fi
else
    printf '%s\n' '未提供用户Key或未准备生成检查：Codex真实生成故障验收未完成'
fi
if ./scripts/compose.sh exec -T -e TERM=dumb antigravity-bridge \
    /usr/local/bin/antigravity-smoke; then
    printf '%s\n' '验收失败：B 停止后 Gemini 仍成功' >&2
else
    printf '%s\n' 'Gemini 生成未完成；结合中转失败日志确认故障原因'
fi
```

`smoke-sidecar.sh` 不用于此处，它在 B 停止后仍成功不是直连回退的证据。
任何真实生成意外成功都算故障验收失败；本地准备失败、无 Key，或鉴权/额度等无关错误
不能算作中转故障验收通过。对照 A 的失败日志，确认没有成功的 `HIER_DIRECT`。
无论结果如何，检查后立即在 B 恢复 relay：

```sh
docker compose --env-file ../images.lock.env up -d relay
```

恢复后在 A 重跑 `codex_generation_check false`、`codex_generation_check true`
和 `antigravity-smoke`，检查生成恢复、父代理日志及用量记录。
若没有用户 Key，报告已完成的 CONNECT/其他检查范围，保留 Codex 生成与故障验收未完成状态。

B 重启后检查 `rc-service wg-codex status`、`wg show wg-codex` 和 relay 的
`docker compose ... ps`。A 的 WireGuard 依赖 Docker；重启 A 的 Docker 后执行：

```bash
systemctl restart wg-quick@wg-codex
wg show wg-codex
```

若重建过 A 的出口网络，应重新查询实际子网。子网变化时，先停止旧 WireGuard
服务使旧 hooks 清理规则，再修正配置中的子网并启动，不能仅重启旧配置。

需要让两类上游都恢复 A 直连时，编辑 A 的 `.env`：

```dotenv
EGRESS_MODE=direct
```

然后执行：

```bash
./scripts/validate-compose.sh
./scripts/apply-egress.sh
```

这会同时改变 Codex 和 Gemini 的出口，不会删除 WireGuard 或登录凭据。
从旧版升级后只想恢复“Codex 经 B、Gemini 经 A”，使用
[升级指南中的配置回退](relay-upgrade.md#6-回退)。
