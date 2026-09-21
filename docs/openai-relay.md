# OpenAI 双服务器中转部署教程（临时变量版）

目标链路：

```text
Gateway → codex-compat → A Squid → wg-codex → B Squid → ChatGPT/OpenAI
```

- A：已运行本项目 Compose 的 Debian/Ubuntu
- B：Alpine 3.19 / OpenRC
- WireGuard 内网地址：
  - A：`10.77.0.1/32`
  - B：`10.77.0.2/32`
- Codex 强制经 B；B 故障时请求失败，不回退 A 直连
- Antigravity 保持 A 原出口

> 本教程把需要替换的值统一改成 Shell 临时变量，如 `${B_PUBLIC_IP}`。
>
> 临时变量只在**当前 Shell 会话**中有效。重新登录服务器或打开新终端后，需要重新设置。
>
> 不要把 WireGuard 私钥直接写进命令历史。下面通过读取本机 `/etc/wireguard/wg-codex.key` 的方式取得私钥。

---

# 0. 先设置临时变量

## 0.1 A 服务器变量

在 A 上先设置：

```bash
B_PUBLIC_IP='68.77.201.27'
B_SSH_PORT='21027'
WIREGUARD_PORT='21028'
```

其中：

- `B_PUBLIC_IP`：B 的公网 IPv4
- `B_SSH_PORT`：B 的 SSH 端口
- `WIREGUARD_PORT`：B 对公网开放的 UDP 端口

如果你的 B 是普通 VPS，可以使用：

```bash
WIREGUARD_PORT='51820'
```

如果像 NAT VPS 一样只开放了 `21028-21029`，并且 `21028/UDP` 可用，则可以：

```bash
WIREGUARD_PORT='21028'
```

检查：

```bash
printf 'B_PUBLIC_IP=%s\n' "$B_PUBLIC_IP"
printf 'B_SSH_PORT=%s\n' "$B_SSH_PORT"
printf 'WIREGUARD_PORT=%s\n' "$WIREGUARD_PORT"
```

---

## 0.2 B 服务器变量

在 B 上设置：

```sh
A_PUBLIC_IP='填写A服务器的公网IPv4'
WIREGUARD_PORT='21028'
```

例如：

```sh
A_PUBLIC_IP='1.2.3.4'
WIREGUARD_PORT='21028'
```

检查：

```sh
printf 'A_PUBLIC_IP=%s\n' "$A_PUBLIC_IP"
printf 'WIREGUARD_PORT=%s\n' "$WIREGUARD_PORT"
```

> 如果重新登录 A 或 B，请重新执行对应的变量设置命令。

---

# 1. 安装依赖

## 1.1 A

```bash
apt update
apt install -y wireguard iptables curl
```

---

## 1.2 B

启用 Alpine 3.19 的 community 仓库：

```sh
sed -i '/^#.*\/v3\.19\/community\/*$/s/^#//' /etc/apk/repositories
apk update
```

安装：

```sh
apk add docker docker-cli-compose wireguard-tools iptables iptables-openrc
```

---

# 2. 把 relay 文件复制到 B

这一部分在 **A 的项目仓库根目录**执行。

先确认：

```bash
ls -l deploy/relay/wg-codex
ls -l deploy/images.lock.env
```

然后：

```bash
tar -czf - deploy/relay deploy/images.lock.env | \
ssh -p "$B_SSH_PORT" root@"$B_PUBLIC_IP" \
'mkdir -p /opt/codex-relay && tar -xzf - -C /opt/codex-relay'
```

到 B 检查：

```sh
find /opt/codex-relay -maxdepth 4 -type f | sort
```

至少应当看到：

```text
/opt/codex-relay/deploy/images.lock.env
/opt/codex-relay/deploy/relay/wg-codex
```

如果只有 `images.lock.env` 而没有 `deploy/relay/`，说明复制不完整，不要继续启动 `wg-codex` 服务。

---

# 3. 生成 WireGuard 密钥

A、B **分别在本机执行**：

```sh
install -d -m 700 /etc/wireguard

(
    umask 077
    wg genkey | tee /etc/wireguard/wg-codex.key | \
        wg pubkey > /etc/wireguard/wg-codex.pub
)
```

查看本机公钥：

```sh
cat /etc/wireguard/wg-codex.pub
```

只交换 `.pub` 的内容。

不要传输：

```text
/etc/wireguard/wg-codex.key
```

---

# 4. 设置双方公钥临时变量

## 4.1 A

A 本机公钥和私钥直接从文件读取：

```bash
A_PUBLIC_KEY="$(cat /etc/wireguard/wg-codex.pub)"
A_PRIVATE_KEY="$(cat /etc/wireguard/wg-codex.key)"
```

把 B 的公钥粘贴到：

```bash
B_PUBLIC_KEY='填写B的wg-codex.pub内容'
```

不要打印 `A_PRIVATE_KEY`。

可以检查公钥：

```bash
printf 'A_PUBLIC_KEY=%s\n' "$A_PUBLIC_KEY"
printf 'B_PUBLIC_KEY=%s\n' "$B_PUBLIC_KEY"
```

---

## 4.2 B

B 本机：

```sh
B_PUBLIC_KEY="$(cat /etc/wireguard/wg-codex.pub)"
B_PRIVATE_KEY="$(cat /etc/wireguard/wg-codex.key)"
```

填入 A 的公钥：

```sh
A_PUBLIC_KEY='填写A的wg-codex.pub内容'
```

检查：

```sh
printf 'A_PUBLIC_KEY=%s\n' "$A_PUBLIC_KEY"
printf 'B_PUBLIC_KEY=%s\n' "$B_PUBLIC_KEY"
```

不要打印：

```text
$B_PRIVATE_KEY
```

---

# 5. A 查询 Docker 出口子网

在 A 的项目环境中执行：

```bash
EGRESS_SUBNET="$(
    docker network inspect codex-gateway_egress_external \
    --format '{{(index .IPAM.Config 0).Subnet}}'
)"
```

检查：

```bash
printf 'EGRESS_SUBNET=%s\n' "$EGRESS_SUBNET"
```

例如可能得到：

```text
EGRESS_SUBNET=172.20.0.0/24
```

---

# 6. A 开启 IPv4 转发

```bash
printf 'net.ipv4.ip_forward=1\n' > /etc/sysctl.d/90-codex-relay.conf
sysctl -p /etc/sysctl.d/90-codex-relay.conf
```

应看到：

```text
net.ipv4.ip_forward = 1
```

---

# 7. 写入 A 的 WireGuard 配置

执行前确认这些变量已经设置：

```bash
printf 'B_PUBLIC_IP=%s\n' "$B_PUBLIC_IP"
printf 'WIREGUARD_PORT=%s\n' "$WIREGUARD_PORT"
printf 'EGRESS_SUBNET=%s\n' "$EGRESS_SUBNET"
printf 'B_PUBLIC_KEY=%s\n' "$B_PUBLIC_KEY"
```

然后直接生成配置：

```bash
cat > /etc/wireguard/wg-codex.conf <<EOF
[Interface]
Address = 10.77.0.1/32
PrivateKey = ${A_PRIVATE_KEY}

PostUp = iptables -I DOCKER-USER 1 -s ${EGRESS_SUBNET} -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j ACCEPT
PostUp = iptables -I DOCKER-USER 1 -i %i -s 10.77.0.2/32 -d ${EGRESS_SUBNET} -p tcp --sport 3128 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
PostUp = iptables -t nat -I POSTROUTING 1 -s ${EGRESS_SUBNET} -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j SNAT --to-source 10.77.0.1

PostDown = iptables -D DOCKER-USER -s ${EGRESS_SUBNET} -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j ACCEPT
PostDown = iptables -D DOCKER-USER -i %i -s 10.77.0.2/32 -d ${EGRESS_SUBNET} -p tcp --sport 3128 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
PostDown = iptables -t nat -D POSTROUTING -s ${EGRESS_SUBNET} -d 10.77.0.2/32 -o %i -p tcp --dport 3128 -j SNAT --to-source 10.77.0.1

[Peer]
PublicKey = ${B_PUBLIC_KEY}
Endpoint = ${B_PUBLIC_IP}:${WIREGUARD_PORT}
AllowedIPs = 10.77.0.2/32
PersistentKeepalive = 25
EOF
```

设置权限：

```bash
chmod 600 /etc/wireguard/wg-codex.conf
```

检查配置时注意不要把私钥发给其他人：

```bash
wg-quick strip wg-codex
```

---

# 8. 写入 B 的 WireGuard 配置

执行前确认：

```sh
printf 'WIREGUARD_PORT=%s\n' "$WIREGUARD_PORT"
printf 'A_PUBLIC_KEY=%s\n' "$A_PUBLIC_KEY"
```

生成：

```sh
cat > /etc/wireguard/wg-codex.conf <<EOF
[Interface]
Address = 10.77.0.2/32
ListenPort = ${WIREGUARD_PORT}
PrivateKey = ${B_PRIVATE_KEY}

[Peer]
PublicKey = ${A_PUBLIC_KEY}
AllowedIPs = 10.77.0.1/32
EOF
```

权限：

```sh
chmod 600 /etc/wireguard/wg-codex.conf
```

B 不设置默认路由，也不需要开启 IPv4 forwarding。

---

# 9. B 配置防火墙

确认变量：

```sh
printf 'A_PUBLIC_IP=%s\n' "$A_PUBLIC_IP"
printf 'WIREGUARD_PORT=%s\n' "$WIREGUARD_PORT"
```

允许 A 访问 WireGuard：

```sh
iptables -I INPUT 1 \
    -p udp \
    -s "${A_PUBLIC_IP}/32" \
    --dport "$WIREGUARD_PORT" \
    -j ACCEPT
```

禁止公网直接访问 Squid：

```sh
iptables -I INPUT 1 \
    -p tcp \
    --dport 3128 \
    -j DROP
```

允许 A 通过 WireGuard 访问 Squid：

```sh
iptables -I INPUT 1 \
    -i wg-codex \
    -s 10.77.0.1/32 \
    -d 10.77.0.2/32 \
    -p tcp \
    --dport 3128 \
    -j ACCEPT
```

持久化：

```sh
rc-update add iptables boot
rc-service iptables save
```

查看：

```sh
iptables -L INPUT -n -v --line-numbers
```

另外，B 的云服务商 / NAT VPS 控制面板也必须允许：

```text
A 公网 IP → B 的 UDP $WIREGUARD_PORT
```

不要把 TCP 3128 开放到公网。

---

# 10. A 配置 WireGuard 的 systemd 启动顺序

A 的 WireGuard hooks 依赖 Docker 已经创建 `DOCKER-USER` 链，因此设置依赖：

```bash
mkdir -p /etc/systemd/system/wg-quick@wg-codex.service.d
```

```bash
printf '[Unit]\nRequires=docker.service\nAfter=docker.service\n' \
    > /etc/systemd/system/wg-quick@wg-codex.service.d/docker.conf
```

```bash
systemctl daemon-reload
```

启动：

```bash
systemctl enable --now wg-quick@wg-codex
```

检查：

```bash
systemctl status wg-quick@wg-codex --no-pager
```

---

# 11. B 安装并启动 wg-codex OpenRC 服务

先确认文件存在：

```sh
ls -l /opt/codex-relay/deploy/relay/wg-codex
```

安装：

```sh
install -m 755 \
    /opt/codex-relay/deploy/relay/wg-codex \
    /etc/init.d/wg-codex
```

确认：

```sh
ls -l /etc/init.d/wg-codex
```

加入启动项：

```sh
rc-update add wg-codex default
rc-update add docker default
```

按照正确顺序启动：

```sh
rc-service wg-codex start
rc-service docker start
```

检查：

```sh
rc-service wg-codex status
rc-service docker status
```

---

# 12. 验证 WireGuard 隧道

A、B 都执行：

```sh
wg show wg-codex
```

成功时应看到类似：

```text
latest handshake: 10 seconds ago
transfer: ... received, ... sent
```

B 还可以确认监听端口：

```sh
ss -lunp | grep "$WIREGUARD_PORT"
```

如果没有 handshake，可在 B 临时抓包：

```sh
tcpdump -ni any "udp port ${WIREGUARD_PORT}"
```

然后重新启动 A 的 WireGuard，观察 B 是否收到 UDP 数据包。

---

# 13. 在 B 启动 Squid relay

B：

```sh
cd /opt/codex-relay/deploy/relay
```

```sh
unset SQUID_IMAGE
```

检查 Compose：

```sh
docker compose --env-file ../images.lock.env config -q
```

启动：

```sh
docker compose --env-file ../images.lock.env up -d
```

查看：

```sh
docker compose --env-file ../images.lock.env ps
```

日志：

```sh
docker compose --env-file ../images.lock.env logs --tail 100 relay
```

---

# 14. A 测试 B 的代理

A：

```bash
curl \
    --interface 10.77.0.1 \
    --noproxy '' \
    -I \
    --max-time 20 \
    --proxy http://10.77.0.2:3128 \
    https://chatgpt.com/
```

如果收到正常的上游 HTTP 状态，例如：

```text
HTTP/2 200
```

或：

```text
HTTP/2 403
```

都可以说明：

```text
A → WireGuard → B Squid → Internet
```

已经连通。

如果出现：

```text
Connection refused
```

优先检查 B 的 Squid。

如果超时，则检查：

```sh
wg show wg-codex
```

和：

```sh
iptables -L INPUT -n -v --line-numbers
```

---

# 15. 让 A 的 Codex 正式经 B 出口

回到 A 的项目仓库。

这里的 `.env` 是项目持久配置，不是前面的 Shell 临时变量。

设置：

```env
CODEX_RELAY_IP=10.77.0.2
CODEX_RELAY_PORT=3128
```

然后：

```bash
./scripts/validate-compose.sh
```

重建出口代理：

```bash
./scripts/compose.sh up -d \
    --no-deps \
    --force-recreate \
    egress-allowlist
```

测试：

```bash
./scripts/smoke-sidecar.sh
```

A 日志：

```bash
./scripts/compose.sh logs --tail 100 egress-allowlist
```

B 日志：

```sh
cd /opt/codex-relay/deploy/relay

docker compose \
    --env-file ../images.lock.env \
    logs --tail 100 relay
```

---

# 16. 故障测试

目标是确认：

```text
B 故障 → Codex 请求失败
```

而不是自动回退 A 直连。

B：

```sh
cd /opt/codex-relay/deploy/relay
```

停止：

```sh
docker compose --env-file ../images.lock.env stop relay
```

A：

```bash
./scripts/smoke-sidecar.sh
```

此时应该失败。

B 恢复：

```sh
docker compose --env-file ../images.lock.env up -d
```

A 再测试：

```bash
./scripts/smoke-sidecar.sh
```

应该恢复。

---

# 17. 重启 B 后检查

B：

```sh
rc-service wg-codex status
```

```sh
rc-service docker status
```

```sh
wg show wg-codex
```

```sh
cd /opt/codex-relay/deploy/relay
docker compose --env-file ../images.lock.env ps
```

---

# 18. Docker 重启后的 A

因为 A 的：

```text
wg-quick@wg-codex
```

依赖 Docker，Docker 停止时 WireGuard 服务也可能停止。

Docker 重启后执行：

```bash
systemctl restart wg-quick@wg-codex
```

然后：

```bash
wg show wg-codex
```

---

# 19. 回退到 A 直连

A 项目 `.env` 中改为：

```env
CODEX_RELAY_IP=
```

然后：

```bash
./scripts/validate-compose.sh
```

```bash
./scripts/compose.sh up -d \
    --no-deps \
    --force-recreate \
    egress-allowlist
```

这样即可恢复 A 直连，不需要删除 WireGuard。

---

# 20. 临时变量速查

## A

每次重新登录 A 后，根据实际环境重新设置：

```bash
B_PUBLIC_IP='68.77.201.27'
B_SSH_PORT='21027'
WIREGUARD_PORT='21028'

A_PUBLIC_KEY="$(cat /etc/wireguard/wg-codex.pub)"
A_PRIVATE_KEY="$(cat /etc/wireguard/wg-codex.key)"

B_PUBLIC_KEY='填写B的公钥'

EGRESS_SUBNET="$(
    docker network inspect codex-gateway_egress_external \
    --format '{{(index .IPAM.Config 0).Subnet}}'
)"
```

---

## B

每次重新登录 B 后：

```sh
A_PUBLIC_IP='填写A公网IPv4'
WIREGUARD_PORT='21028'

B_PUBLIC_KEY="$(cat /etc/wireguard/wg-codex.pub)"
B_PRIVATE_KEY="$(cat /etc/wireguard/wg-codex.key)"

A_PUBLIC_KEY='填写A的公钥'
```

---

# 21. 变量使用说明

Shell 中：

```sh
NAME='value'
```

表示定义临时变量。

读取变量：

```sh
echo "$NAME"
```

在较长字符串中推荐：

```sh
echo "${NAME}"
```

例如：

```sh
Endpoint="${B_PUBLIC_IP}:${WIREGUARD_PORT}"
```

临时变量不会永久写入系统。

退出当前 SSH：

```sh
exit
```

之后再次登录时：

```sh
echo "$WIREGUARD_PORT"
```

通常会变成空值，因此需要重新设置。

WireGuard 的正式配置、项目 `.env`、iptables 持久化规则等则不会因为 Shell 退出而消失。
