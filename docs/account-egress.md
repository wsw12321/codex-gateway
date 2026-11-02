# 上游账号独立出口

Owner 可在管理网站“出口服务器”页面维护 Shadowsocks 节点，并在 Codex、
Antigravity、Anthropic 的账号表格选择每个账号的出口。多个账号可以共用同一节点；
“继承全局出口”沿用已有 `EGRESS_MODE=direct/relay/shadowsocks`。

节点字段为名称、server、port、cipher、password 和 udp。支持
`aes-128-gcm`、`aes-256-gcm`、`chacha20-ietf-poly1305`。密码加密保存在 PostgreSQL，
普通列表和编辑表单不回显密码；编辑时留空保留原值。节点的 `udp` 传给 SS 运行时，
模型 HTTP(S) 和 SSE 仍使用 TCP，不增加公开 UDP 或 SOCKS 入口。

## 生效与故障

保存先持久化，再应用到出口运行时。页面分别显示配置保存和运行时应用状态；应用
失败不会把账号改为继承全局出口。绑定的节点无法连接或密码错误时，该账号的操作
失败；已有调度器仍可以按原规则尝试另一个有权限的账号，并使用该账号自己的出口。
解除绑定后恢复全局出口；删除前需要先解除引用。

推理、额度查询、模型目录、诊断和凭据刷新均使用账号绑定。OAuth 新增/重新授权及
凭据导入先选择出口，使用持久化的短期登记关联验证过程与最终账号，避免先用默认
出口访问 provider 再补绑定。配置变化影响后续操作，已建立的流式连接继续完成。

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
go test -race ./cmd/account-egress
python3 -m unittest scripts.tests.test_codex_relay scripts.tests.test_relay_validation
./scripts/test-account-egress-image.sh
```

镜像测试使用无外网的临时 Docker namespace、锁定 Squid 和 Mihomo、两个合成
SS 服务及不同的回显目标，确认真实出口区别、三个全局模式下的独立绑定、内部
认证透传、配置更新时旧流存活、旧路由拒绝、错误密码/节点停止无直连回退，以及
另一个节点继续可用。它不读取站点 `.env`、生产凭据或生产 volume。
