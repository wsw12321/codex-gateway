# 用 `.env` 切换 Codex 与 Gemini 出口

Codex 和 Gemini 共用所选出口。A Squid 始终保留现有来源和精确域名白名单；
OIDC 认证仍由 A Squid 独立直连配置的 Auth 域名。

| `.env` | 模型出口 |
|---|---|
| `EGRESS_MODE=direct` | A Squid → 上游 |
| `EGRESS_MODE=relay` | A Squid → 原有 WireGuard → B Squid → 上游 |
| `EGRESS_MODE=shadowsocks` | A Squid → A Mihomo → B Shadowsocks → B 的出口 |

模板默认 `direct`。显式模式优先，因此可保留旧的 `CODEX_RELAY_IP` 和端口，
而不启用旧中转。旧 `.env` 未设置模式或值为空时，继续按旧规则：
`CODEX_RELAY_IP` 非空选 `relay`，否则选 `direct`。未知模式拒绝启动。
`relay` 必须配置有效的中转 IP；`relay` 和 `shadowsocks` 都没有直连回退。

## 准备 Shadowsocks

A 需要 Docker Compose v2、Python 3、`jq` 和 util-linux 的 `flock`，以及已配置的网关部署。
保留现有 WireGuard 和 B Squid，即可随时切回 `relay`；SS 本身不使用 WireGuard。
`172.28.50.0/24` 必须与 A 的现有网络不冲突。

下面是所选 B 节点的示例，地址并不保证其出口属性；实际 AT&T 出口需单独验收：

```dotenv
EGRESS_MODE=shadowsocks
CODEX_RELAY_IP=10.77.0.2
CODEX_RELAY_PORT=3128
SHADOWSOCKS_SERVER=23.106.45.205
SHADOWSOCKS_PORT=24019
SHADOWSOCKS_CIPHER=aes-128-gcm
SHADOWSOCKS_PASSWORD_FILE=./deploy/secrets/shadowsocks_password
```

节点地址支持规范 IPv4 或小写 DNS 域名，端口为 `1..65535` 的十进制数。
密码算法支持 `aes-128-gcm`、`aes-256-gcm`、`chacha20-ietf-poly1305`。
密码按 UTF-8 保存、最多 4096 字节，保留空格、引号、反斜杠和 `$` 等特殊字符；
可带一个文件末尾换行，不允许内嵌换行或 NUL。

密码只存于独立文件，不要写入 `.env`。文件必须是普通非符号链接文件、权限
`0640`，组为 `.env` 的 `GATEWAY_SECRET_GID`。该 GID 应是专用部署用户的私有主组。
在该用户的交互终端运行以下命令；输入不会回显，也不进入 Shell 历史或命令参数。
同样的命令可用于密码轮换，确认路径与 `.env` 一致：

```bash
python3 - <<'PY'
import getpass
import os
from pathlib import Path
import tempfile

directory = Path("deploy/secrets")
directory.mkdir(mode=0o700, parents=True, exist_ok=True)
password = getpass.getpass("Shadowsocks password: ")
if not password or any(c in password for c in "\r\n\0"):
    raise SystemExit("Password must be nonempty and contain no CR, LF or NUL")
fd, temporary = tempfile.mkstemp(prefix=".shadowsocks-", dir=directory)
try:
    with os.fdopen(fd, "wb") as output:
        output.write(password.encode("utf-8"))
        os.fchmod(output.fileno(), 0o640)
    os.replace(temporary, directory / "shadowsocks_password")
finally:
    if os.path.exists(temporary):
        os.unlink(temporary)
PY
```

不要提交密码文件，也不要将生成的 Mihomo 配置复制到日志或工单。
直连和旧中转模式不要求 SS 密码文件。

## 应用与回滚

修改 `.env` 后，在项目根目录执行：

```bash
./scripts/apply-egress.sh
```

脚本先由 Compose 解析并校验配置，清理继承的相关 Shell 环境变量，
不会 `source` 或 `eval` `.env`。预校验失败不会改动现有容器。
SS overlay 自动选择，无需手工加 `-f`。若 `.env` 的 `OIDC_ENABLED=true`，
应用命令也会带上既有 OIDC overlay，保留认证出口。

切入 SS 时，先构建并强制重建 `ss-egress`，等待本地代理健康，随后重建 Squid。
每次应用均强制重建，因此密码轮换及单文件挂载的 inode 变化会生效。
切入 `direct` 或 `relay` 时，先重建 Squid 并等待健康，再按项目和服务标签
精确删除旧 `ss-egress` 容器。该操作不执行全项目 `down`，不清理其他服务。

**重建会中断已有 CONNECT/SSE，请安排维护窗口。** 运行中失败不会自动切换出口。
本地健康只表示 Mihomo 已监听代理端口，不证明密码正确、B 在线或模型可用。
失败后检查状态和日志；回滚时恢复之前的 `.env` 模式与节点配置，重新执行
`./scripts/apply-egress.sh`。切回旧中转可保留 SS 参数和密码文件：

```dotenv
EGRESS_MODE=relay
CODEX_RELAY_IP=10.77.0.2
CODEX_RELAY_PORT=3128
```

切回 A 直连只需设置 `EGRESS_MODE=direct`，旧中转参数可保留。
首次设置 WireGuard/B Squid 仍按[双服务器教程](openai-relay.md)完成。

## 隔离与凭据

Mihomo 使用官方 [v1.19.32](https://github.com/MetaCubeX/mihomo/releases/tag/v1.19.32)
的摘要锁定镜像中的二进制；构建工具和运行基础镜像也沿用镜像锁。
SS 服务以非 root 身份运行、根文件系统只读、丢弃全部 capabilities，
不发布宿主机端口，不启用 TUN 或控制 API。

专用内部网络 `ss_internal` 为 `172.28.50.0/24`。Squid 为 `.2`，Mihomo 为 `.3`；
HTTP 代理仅监听 `172.28.50.3:17890`，只接受 `.2/32`。
Mihomo 同时连接现有 `egress_external` 出网，业务容器不接入 SS 网络。
SS 模式中 Squid 只有 Mihomo 一个父代理；Mihomo 只有 B 一个 SS 节点，
所有代理流量都使用该节点。OIDC 的独立直连规则保持不变。

密码仅挂载给 `ss-egress`。启动助手读取文件，安全编码为临时配置（`0600`），
密码不会进入环境变量、进程参数或启动日志。不要通过设置全局代理变量绕过 Squid。

## 验收

部署回归与隔离镜像测试使用合成凭据：

```bash
python3 -m unittest discover -s scripts/tests -p 'test_*.py' -v
./scripts/test-relay-image.sh
./scripts/test-shadowsocks-image.sh
./scripts/validate-compose.sh
# 若启用了 OIDC，用以下校验替代上一行：
./scripts/validate-compose.sh --oidc
```

CI 会渲染并校验三种模式，测试 AEAD 转发、流式响应、错误密码和 B 故障，
以及域名/来源限制、禁止直连回退和切换顺序。部署后在维护窗口分别验收
`direct → shadowsocks → relay → direct`、密码轮换和失败后的回滚。

真实验收需要 A 上的节点密码：先在 B 或供应商控制面核对出口 IP 及 AT&T 归属，
再执行[原中转教程中的真实模型请求](openai-relay.md#codex-真实生成)，
检查 Codex 与 Gemini 的实际生成和 SSE。A 的 Squid 日志应显示父代理为
`172.28.50.3`，不应出现模型请求成功走 `HIER_DIRECT`。
不要为查询公网 IP 临时放宽业务域名白名单；需要链路层出口观测时，
使用 B 的连接/流量记录或受控的隔离验收环境。
