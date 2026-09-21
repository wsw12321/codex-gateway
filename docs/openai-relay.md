# OpenAI 双服务器中转

链路：`Gateway → codex-compat → A Squid → wg-codex → B Squid → ChatGPT/OpenAI`。
设置 `CODEX_RELAY_IP` 后 Codex 强制经 B；B 故障时请求失败，不回退直连。Antigravity 保持 A 原出口。
A 为已运行本仓库 Compose 的 Debian/Ubuntu；B 为持久化安装的 Alpine 3.19/OpenRC。
两端宿主机内核须支持 WireGuard，`10.77.0.1/32`、`10.77.0.2/32` 无冲突；A 使用 Docker iptables 后端。
以下系统命令在各自主机的 root shell 执行；替换所有 `<…>`，不要传输私钥。

## 1. 准备

```sh
# A
apt update && apt install -y wireguard iptables curl
# B：启用同一 Alpine 3.19 镜像源的 community 仓库
sed -i '/^#.*\/v3\.19\/community\/*$/s/^#//' /etc/apk/repositories
apk update
apk add docker docker-cli-compose wireguard-tools iptables iptables-openrc
```

把本仓库 `deploy/relay/` 整个目录和 `deploy/images.lock.env` 复制至 B，保留为
`/opt/codex-relay/deploy/relay/` 与 `/opt/codex-relay/deploy/images.lock.env`；不复制 `.env`、secrets 或 OAuth 卷。
```sh
tar -czf - deploy/relay deploy/images.lock.env |
    ssh root@B_PUBLIC_IP 'mkdir -p /opt/codex-relay && tar -xzf - -C /opt/codex-relay'
```
Alpine 3.19 的 [WireGuard 打包定义](https://github.com/alpinelinux/aports/blob/3.19-stable/main/wireguard-tools/APKBUILD) 不提供 OpenRC 服务；使用本仓库文件。
两端分别生成本机密钥；只交换 `.pub` 内容：

```sh
install -d -m 700 /etc/wireguard
(umask 077; wg genkey | tee /etc/wireguard/wg-codex.key | wg pubkey > /etc/wireguard/wg-codex.pub)
cat /etc/wireguard/wg-codex.pub
```

## 2. 隧道与 A 转发

A 在仓库目录查询出口网络子网，将结果替换下面全部 `<EGRESS_SUBNET>`：

```sh
docker network inspect codex-gateway_egress_external --format '{{(index .IPAM.Config 0).Subnet}}'
printf 'net.ipv4.ip_forward=1\n' > /etc/sysctl.d/90-codex-relay.conf
sysctl -p /etc/sysctl.d/90-codex-relay.conf
```

A 写入 `/etc/wireguard/wg-codex.conf`；私钥字段填 A 本地 `.key` 内容。
规则仅放行出口子网至 B 的 TCP 3128 和回包；SNAT 必须先于 Docker MASQUERADE，使 B 看到 `10.77.0.1`。
[Docker 要求在 DOCKER-USER 链添加转发策略](https://docs.docker.com/engine/network/firewall-iptables/)。

```ini
[Interface]
Address = 10.77.0.1/32
PrivateKey = <A_LOCAL_PRIVATE_KEY>
PostUp = iptables -I DOCKER-USER 1 -s <EGRESS_SUBNET> -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j ACCEPT
PostUp = iptables -I DOCKER-USER 1 -i %i -s 10.77.0.2/32 -d <EGRESS_SUBNET> -p tcp --sport 3128 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
PostUp = iptables -t nat -I POSTROUTING 1 -s <EGRESS_SUBNET> -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j SNAT --to-source 10.77.0.1
PostDown = iptables -D DOCKER-USER -s <EGRESS_SUBNET> -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j ACCEPT
PostDown = iptables -D DOCKER-USER -i %i -s 10.77.0.2/32 -d <EGRESS_SUBNET> -p tcp --sport 3128 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
PostDown = iptables -t nat -D POSTROUTING -s <EGRESS_SUBNET> -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j SNAT --to-source 10.77.0.1
[Peer]
PublicKey = <B_PUBLIC_KEY>
Endpoint = <B_PUBLIC_IPV4>:51820
AllowedIPs = 10.77.0.2/32
PersistentKeepalive = 25
```

B 写入同名配置；私钥字段填 B 本地 `.key` 内容：

```ini
[Interface]
Address = 10.77.0.2/32
ListenPort = 51820
PrivateKey = <B_LOCAL_PRIVATE_KEY>
[Peer]
PublicKey = <A_PUBLIC_KEY>
AllowedIPs = 10.77.0.1/32
```

两端执行 `chmod 600 /etc/wireguard/wg-codex.conf`。不设置默认路由、不启用 B 的 IP 转发。
B 安全组允许 A 公网 IP 到 UDP 51820；TCP 3128 不开放公网。B 主机添加并持久化规则：

```sh
iptables -I INPUT 1 -p udp -s <A_PUBLIC_IPV4>/32 --dport 51820 -j ACCEPT
iptables -I INPUT 1 -p tcp --dport 3128 -j DROP
iptables -I INPUT 1 -i wg-codex -s 10.77.0.1/32 -d 10.77.0.2/32 -p tcp --dport 3128 -j ACCEPT
rc-update add iptables boot
rc-service iptables save
```

保留已有 SSH、回包和其他必要规则；若使用其他防火墙管理器，将上述规则纳入其持久配置，勿混用恢复机制。

## 3. 启动与启用

A 的 hooks 依赖 Docker 已创建 DOCKER-USER，设置 systemd 顺序后启动：

```sh
mkdir -p /etc/systemd/system/wg-quick@wg-codex.service.d
printf '[Unit]\nRequires=docker.service\nAfter=docker.service\n' > /etc/systemd/system/wg-quick@wg-codex.service.d/docker.conf
systemctl daemon-reload
systemctl enable --now wg-quick@wg-codex
```

B 安装服务；它声明 `need net` 和 `before docker`，先创建隧道地址再让 Squid 绑定：

```sh
install -m 755 /opt/codex-relay/deploy/relay/wg-codex /etc/init.d/wg-codex
rc-update add wg-codex default
rc-update add docker default
rc-service wg-codex start
rc-service docker start
wg show wg-codex
```

`before docker` 只约束启动顺序；手动启动时也遵循先 `wg-codex`、后 Docker/Compose 的顺序。
两端 `wg show wg-codex` 应有近期 handshake 和收发计数；先确认隧道，再在 B 启动代理：

```sh
cd /opt/codex-relay/deploy/relay
unset SQUID_IMAGE
docker compose --env-file ../images.lock.env config -q
docker compose --env-file ../images.lock.env up -d
# A：仅验证 B 的 TLS 隧道；收到上游 HTTP 状态（包括 403）即可区分 TLS 连通与业务权限
curl --interface 10.77.0.1 --noproxy '' -I --max-time 20 --proxy http://10.77.0.2:3128 https://chatgpt.com/
```

最后在 A 仓库 `.env` 设置 `CODEX_RELAY_IP=10.77.0.2`、`CODEX_RELAY_PORT=3128`，执行：

```sh
./scripts/validate-compose.sh
./scripts/compose.sh up -d --no-deps --force-recreate egress-allowlist
./scripts/smoke-sidecar.sh
./scripts/compose.sh logs --tail 100 egress-allowlist
# B（仍在 deploy/relay 目录）
docker compose --env-file ../images.lock.env logs --tail 100 relay
```

## 4. 验收与恢复

A 的 Codex CONNECT 日志应显示父代理及 `10.77.0.2`，B 显示直达目标；CONNECT 结束后才输出完整记录，不含认证头或 TLS 内容。
B 执行 `docker compose --env-file ../images.lock.env stop relay`，A 再运行 `./scripts/smoke-sidecar.sh` 必须失败；用 B 的 `up -d` 恢复后，新请求应成功。
确认 Antigravity 仍走 A；真实 OAuth 按 [运维手册](operations.md) 登录，另在控制台刷新额度并完成一次 SSE 生成、取消、断隧道及恢复后的新连接。
B 重启后检查 `rc-service wg-codex status`、`rc-service docker status`、`wg show wg-codex`、Compose 健康状态及真实请求；此项必须在真实 Alpine 宿主机验收。
A 的 `Requires=docker.service` 会在 Docker 停止时停止隧道；Docker 重启后执行 `systemctl restart wg-quick@wg-codex`。改变出口子网前先停隧道，更新 hooks 后再启动。
回退：A `.env` 清空 `CODEX_RELAY_IP=`，重新执行校验和上面的出口代理重建命令，即恢复 A 直连；不必删除隧道。
