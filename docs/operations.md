# 部署与运维手册

本项目按单台 Linux VPS、单个 ChatGPT Pro 上游账号设计。只有 Caddy
发布主机端口；Gateway、PostgreSQL、CLIProxyAPI 和出口代理均不得直接
出现在公网。所有命令都应从仓库根目录执行。

## 1. 前置条件

- 受控域名的 A/AAAA 记录已指向 VPS，80/TCP、443/TCP 和 443/UDP 可达。
- SSH 使用密钥登录；主机防火墙只开放 SSH、80 和 443。
- Docker Engine 和 Docker Compose v2 可用。
- `skopeo`、`crane` 或 Docker Buildx 可用于解析镜像 digest。
- `openssl` 和 util-linux 的 `flock` 用于服务密钥及互斥操作；备份主机另需
  `age` 和 `age-keygen`。
- 不要把 Docker socket 暴露给公网服务，也不要让无关用户进入 `docker`
  组。

先创建本地配置：

```sh
cp deploy/env.example .env
chmod 0600 .env
```

把 `GATEWAY_DOMAIN` 改成真实域名。若 Docker 网段与主机已有网段冲突，
应在首次启动前同时调整 Compose 中的三个内部子网、静态地址和
`GATEWAY_TRUSTED_PROXY_CIDRS`。网关只应信任 Caddy 的单一 `/32` 地址。

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

把 `backup_age_identity` 的离线副本保存到密码管理器或离线介质。recipient
可以交给备份任务，identity 不应留在不负责恢复的备份节点。

## 4. 构建和首次启动

```sh
./scripts/validate-compose.sh
./scripts/compose.sh build gateway codex-compat
./scripts/compose.sh up -d
./scripts/compose.sh ps
```

`scripts/compose.sh` 会先清除宿主环境中与两个 env 文件同名的变量，再依次读取
`.env` 和 `deploy/images.lock.env`，并固定 Compose project 名。这样既不会遗漏
`GATEWAY_DOMAIN`，也不能用导出的环境变量绕过不可变镜像引用。

`validate-compose.sh` 会检查：只有 Caddy 发布 80/443、三个后端网络均为
`internal`、PostgreSQL 本地初始化和 host 连接都强制使用 SCRAM、sidecar 是
只读且非 root、所有基础镜像都有 SHA-256 digest。

确认主机监听：

```sh
ss -lntup
```

除 SSH 外，只应看到 Docker/Caddy 的 80 和 443。不得出现 5432、8080、
8317 或 3128。还应从另一台机器确认这些端口不可达。

数据库迁移由 Gateway 启动过程从嵌入迁移执行。Gateway 的 `/healthz` 是
存活探针，`/readyz` 当前只检查数据库连接，不检查 sidecar 或真实上游；Caddy
仅在 Gateway ready 后启动。

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

创建备份：

```sh
./scripts/backup-postgres.sh
```

脚本把 custom-format `pg_dump` 直接管道到 age，不在磁盘落明文，并为密文
写 SHA-256 校验文件。默认输出到被 Git 忽略的 `backups/`；生产环境应通过
`BACKUP_DIR` 指向加密、受限且会异地复制的目录。

恢复演练必须定期执行：

```sh
./scripts/restore-drill.sh backups/gateway-YYYYmmddTHHMMSSZ.dump.age
```

演练容器使用锁定的 PostgreSQL 镜像、无网络、无发布端口和随机临时 Docker
volume。解密内容只通过管道进入 `pg_restore`，校验应用表数量后显式删除容器、
解密数据 volume 和临时 secret 目录；任一清理失败都会让演练返回失败。
这不是对生产数据库的就地恢复。真正灾备时应先建立隔离的新数据库、完成
同样校验，再在维护窗口切换；不要覆盖仍运行的生产卷。

备份必须包含数据库密文、lock 文件和部署配置，但必须排除：

- `codex_oauth` 卷；
- `.env` 和 `/run/secrets` 明文副本；
- 提示词、代码、响应正文和访问 Cookie。

## 9. 日志、监控和故障处理

Caddy 不启用访问日志。Gateway 只能记录请求 ID、身份/项目引用、状态、时间、
字节数和 usage 元数据；禁止记录 Authorization、Cookie、正文、邀请令牌或
OAuth token。CLIProxyAPI 以 `debug: false`、`logging-to-file: false` 和关闭
内部 usage 统计运行。Squid 只能看到 TLS CONNECT 的目标域名与状态。

常用只读诊断：

```sh
./scripts/compose.sh ps
./scripts/compose.sh logs --since 15m gateway
./scripts/compose.sh logs --since 15m codex-compat
./scripts/compose.sh logs --since 15m egress-allowlist
```

复制日志前先扫描敏感模式。OAuth 重新认证、上游 429、sidecar 不健康和配额
80%/100% 应在管理后台生成告警，不通过含请求内容的外部 webhook 发送。

出现 `upstream_reauthentication_required` 时停止调用并重新执行设备码登录。
不要启用 Platform API 自动回退；这会改变计费边界。

## 10. 升级和回滚

升级 Gateway 前先做数据库密文备份与恢复演练。升级 CLIProxyAPI 前还必须：

1. 审阅新版本、commit、MIT notice 和依赖差异；更新 sources/lock。
2. 在 CI 运行 Responses 普通/SSE、compact、401、429、跨 chunk usage 和刷新
   token 契约测试。
3. 持有 `.device-login.lock` 的 `flock` 并停止生产 sidecar，确保没有两个实例
   共享 token。
4. 用测试 OAuth 状态完成契约验证，再进行一次人工 Pro 冒烟。
5. 单实例滚动替换；失败时停止新实例，再回到旧镜像，不能并行回滚。

若数据库迁移不可向后兼容，禁止仅回滚二进制。应按该版本的迁移说明恢复到
隔离数据库后切换。
