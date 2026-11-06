# 上游账号独立出口

Owner 可在管理网站“出口服务器”页面维护 Shadowsocks 节点，并在 Codex、
Antigravity、Anthropic 的账号表格选择每个账号的出口。多个账号可以共用同一节点；
“继承全局出口”沿用已有 `EGRESS_MODE=direct/relay/shadowsocks`。

节点字段为名称、server、port、cipher、password 和 udp。支持
`aes-128-gcm`、`aes-256-gcm`、`chacha20-ietf-poly1305` 和 Shadowsocks 2022 的
`2022-blake3-aes-128-gcm`。密码加密保存在 PostgreSQL，
普通列表和编辑表单不回显密码；编辑时留空保留原值。节点的 `udp` 传给 SS 运行时，
模型 HTTP(S) 和 SSE 仍使用 TCP，不增加公开 UDP 或 SOCKS 入口。

`2022-blake3-aes-128-gcm` 的“密码”是服务端配置的预共享密钥：标准 Base64（带 `=`
填充）编码的 16 字节随机值，例如 `openssl rand -base64 16` 的输出；服务端启用多用户
身份头（EIH）时，按服务端给出的顺序用英文冒号连接多个密钥。网页、Gateway 和出口
运行时都会检查格式，格式错误的密钥不会保存，也不会下发：Mihomo 遇到一个无效密钥
会拒绝整份配置，导致所有节点的变更都无法应用。把已有节点切换为该加密方式时须重新
填写密钥；留空保存时，Gateway 先解密现有密码并确认它是有效密钥，且只在节点仍是
该版本时写入。SS2022 要求客户端与服务端时钟误差在 30 秒内。

## 生效与故障

保存先持久化，再应用到出口运行时。页面分别显示配置保存和运行时应用状态；应用
失败不会把账号改为继承全局出口。绑定的节点无法连接或密码错误时，该账号的操作
失败；已有调度器仍可以按原规则尝试另一个有权限的账号，并使用该账号自己的出口。
解除绑定后恢复全局出口；删除前需要先解除引用。

推理、额度查询、模型目录、诊断和凭据刷新均使用账号绑定。OAuth 新增/重新授权及
凭据导入先选择出口，使用持久化的短期登记关联验证过程与最终账号，避免先用默认
出口访问 provider 再补绑定。配置变化影响后续操作，已建立的流式连接继续完成。

## 连通性测试

“出口服务器”页面的每个节点都有“测试连通性”按钮，“全部测试”会依次测试所有
已应用的节点（同时最多 3 个）。测试目标可选：

| 选项 | 目标 |
|---|---|
| Codex（默认） | `chatgpt.com:443` |
| Antigravity | `cloudcode-pa.googleapis.com:443` |
| Anthropic | `api.anthropic.com:443` |

测试由出口运行时完成：通过该节点当前已应用版本的本地监听发起 CONNECT，再与目标
完成 TLS 握手并校验证书。Mihomo 在连接 SS 服务端之前就会回应 CONNECT，所以只有
握手成功才能说明节点地址和端口可达、加密方式与密码正确、且该出口能访问目标。
测试不发送 HTTP 请求或模型请求，不消耗额度，不经过 Squid，也不使用任何账号路由
凭据；运行时仍只连接本机 Mihomo 监听，不直连目标。

结果只在当前页面显示，不写数据库，节点修改后需重新测试。结果码如下：

| 结果码 | 含义 |
|---|---|
| `ok` | 连通正常，显示从发起到握手完成的耗时 |
| `handshake_failed` | 握手失败：节点不可达、加密方式或密码错误，或该出口无法访问目标 |
| `timeout` | 10 秒内未完成握手 |
| `certificate_invalid` | 目标证书校验失败，出口可能拦截或篡改了 TLS |
| `tunnel_failed` / `listener_unavailable` | 运行时本地监听异常 |
| `node_not_applied` | 该节点当前版本尚未应用到运行时 |
| `probe_busy` | 运行时已有 4 个测试在进行 |

接口为 `POST /admin/egress-servers/{id}/test`，请求体只能是 `{}` 或
`{"target":"codex|antigravity|anthropic"}`。它需要 Owner 会话并通过同源检查；
测试不修改配置，因此不要求 5 分钟内二次验证。节点尚未应用时返回 409，不会测试
旧版本。响应只含结果码、目标域名、耗时和时间，不含节点地址、密码或底层错误文本。
运行时的 `POST /probe` 只在控制网络上提供，并与 `/config` 使用同一控制密钥。

## 部署

升级前执行 `./scripts/bootstrap-secrets.sh`。它补充 `egress_control_key`，不会覆盖
已有密钥。该文件以 `0640` 挂载给 Gateway 和 `account-egress`，属组仍须匹配
`GATEWAY_SECRET_GID`。密码加密复用现有 API Key 加密根密钥并进行用途隔离，
备份和恢复时必须保留该根密钥。

```sh
./scripts/validate-compose.sh
./scripts/compose.sh build account-egress gateway codex-compat
./scripts/compose.sh up -d account-egress egress-allowlist codex-compat gateway
```

通过 CI 导入镜像时，新清单包含 `account-egress`，见[CI 镜像部署](ci-image-deployment.md)。
首次应用可能短暂显示待应用；Gateway 每 5 秒重新协调一次，运行时或 Gateway 重启后
会自动恢复，不需要重填节点。默认全局出口和 OIDC 不依赖该运行时的就绪状态。

`account-egress` 与可选的 `ss-egress` 使用 Mihomo v1.19.32 的固定源码，
应用已锁定的依赖安全补丁，并由仓库锁定的 Go 工具链构建。源码来源、补丁和
构建脚本见 `deploy/shadowsocks/upstream/`。镜像版本升级为 `1.19.32-v2`，
升级时需重新构建或拉取 CI 产物。

## 网络边界

```text
未绑定：CPA → Squid:3128 → 原全局出口
已绑定：CPA → Squid:3129 → account-egress → 本机 Mihomo listener → 指定 SS
```

Squid 的原精确 provider 域名、443 和 CONNECT 白名单继续生效。3129 是命名的
内部监听，强制使用 `login=PASSTHRU` 的账号父代理；其余父代理显式拒绝该入口，
`never_direct` 禁止直连回退。缺失或错误的内部路由凭据同样无法借用 3128 的路径。
CPA 的普通请求不能自行指定路由或导入凭证中的 proxy URL。

新增容器只有一个 Go helper 和一个锁定版本 Mihomo，不随节点数量增加容器，
不挂载 Docker socket、OAuth volume、数据库密钥或宿主机目录。控制网络
`172.28.70.0/24` 仅连接 Gateway 与运行时；数据网络 `172.28.60.0/24` 仅连接
Squid 与运行时。CPA 无法直接连接后端代理或 Mihomo controller。

Mihomo 的每个节点版本有独立的 loopback HTTP listener，controller 也只监听
loopback。运行时先验证完整配置和新增监听，再原子发布路由。旧版本在活跃隧道
结束前保留；失败时回滚已确认的配置，未变化的可用出口继续服务。明文 SS 配置只在
内存与受限 tmpfs 中出现，不进入环境变量、进程参数、日志或普通浏览器响应。

## 验证

```sh
go test -race ./cmd/account-egress ./internal/egress
python3 -m unittest scripts.tests.test_codex_relay scripts.tests.test_relay_validation
./scripts/test-account-egress-image.sh
```

镜像测试使用无外网的临时 Docker namespace、锁定 Squid 和 Mihomo、三个合成
SS 服务（其中一个为 `2022-blake3-aes-128-gcm`）及不同的回显目标，确认真实出口区别、三个全局模式下的独立绑定、内部
认证透传、配置更新时旧流存活、旧路由拒绝、错误密码/节点停止无直连回退，以及
另一个节点继续可用。它不读取站点 `.env`、生产凭据或生产 volume。

SS2022 与连通性测试的验证记录见[出口服务器 SS2022 与连通性测试验证](account-egress-ss2022-probe-validation.md)。
