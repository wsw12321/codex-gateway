# 首次开通与 SSO 再验证验证记录

2026-10-04，本次扩展沿用 `OIDC_ENABLED`，没有数据库迁移，也没有修改实际部署
`.env`、secrets 或开关。功能及部署配置见 [OIDC 说明](oidc.md)。

## 已执行

使用 Go 1.26.8 和临时目录内的 GCC 13；PostgreSQL 16.15 仅监听本机回环地址，
使用新建空测试库。工具、缓存和数据库均位于仓库外，没有连接生产数据库。

- `go build ./cmd/gateway`：通过，产物写入临时目录。
- `go test -count=1 ./...`：通过，包含 Go 调用的 Node 管理台回归。
- `go test -race -count=1 ./...`：通过。
- `go vet ./...`：通过。
- `go test -count=1 -tags=integration ./internal/store`：通过。
- `go test -count=1 -tags=integration ./internal/server`：通过。
- store/server 的 OIDC 集成回归加 `-race`：通过。
- `gofmt -l .`、`git diff --check`：无输出。
- 28 项 Python OIDC 出口、Compose 渲染与安全策略回归：通过。
- Chromium 154 桌面 1440 px、手机 390 px 回归：通过；六张新增截图和复现命令见
  [浏览器记录](oidc-browser-validation.md)。

store 和 server 全套集成测试使用不同的新建测试库，避免旧测试固定用户名冲突。
Go 工具未预装，使用独立下载的规定版本；没有改变系统工具链。

## 新增回归重点

- 选择前和取消后均不创建用户；绑定已有账号需取消开通并重新本地登录、授权。
- 原子创建用户、绑定及会话；重复和并发开通、绑定竞争、会话插入失败全部回滚。
- 普通 active 会员、唯一用户名、零余额、默认模型权限、无群组、无本地凭据。
- 后续直接登录；禁用、待审批绑定账号及绑定存储故障不进入开通流程。
- 创建时保留交换时间；等待六分钟后创建不会获得新的敏感操作窗口。
- 纯 SSO 用户创建与查看 API Key、建立浏览器客户端接入凭据，计费购买仍校验
  余额；测试明确充值后可正常购买，本地密码和 Passkey 始终可选。
- 再验证匹配原用户、原会话和原绑定；拒绝错误身份、切号、退出、替换绑定、
  超时及重放，不改变会话来源。
- 取消期间和完成响应之后的迟到结果只条件清理对应验证时间；保留原本地会话
  和较新的验证。事务 Cookie 已清除时，原会话仍可取消自己的确切再验证流程。
- 授权临近十分钟期限才完成时，取消记录保留至该次五分钟窗口结束；不会延长
  授权流程的消费期限。
- 唯一登录方式不可解绑；添加本地凭据后可解绑，撤销 SSO 会话并清空其他会话
  的验证窗口；解绑与再验证竞争不会留下有效窗口。
- 浏览器只暂存白名单功能页名称，返回后刷新状态并提示重试，不保存授权凭据
  或待执行操作，不自动重放购买、删除或密码保存。

## Docker 补充验收（2026-10-04）

用户提供可用的 Docker 环境后，已重新完成此前被 daemon 权限阻止的检查。
Docker Engine 29.8.2、Compose v5.6.0；基础镜像均使用
`deploy/images.lock.env` 中的 digest，包括 Go 1.26.8、PostgreSQL 17.6、Squid
6.6、Caddy 2.10.2 和 Gateway 的 Alpine 运行时。

### Compose 与真实代理镜像

在一次性部署目录复制部署定义、校验脚本和 CPA 静态资源，生成占位 `.env`
及仅供格式检查的独立 secret 文件，没有复制实际部署凭据。三个配置均完整通过：

| 配置 | 实际命令 | 结果 |
| --- | --- | --- |
| OIDC 关闭 | `scripts/validate-compose.sh` | 通过 |
| OIDC 启用，原生授权地址 | `scripts/validate-compose.sh --oidc` | 通过 |
| OIDC 启用，指定浏览器代理授权地址 | `scripts/validate-compose.sh --oidc` | 通过 |

检查包含 A/B Squid 真实解析、direct/relay/shadowsocks 三种模式的 OIDC 路由、
Caddy 真实解析、Compose 网络和 secret 安全约束、镜像及源码锁。
解析容器使用 `--network none` 和只读根文件系统，不分配项目静态 IP。

真实 Caddy 回调错误日志回归通过：合成 502 日志保留路径与状态码，删除 code、
state、error_description、Referer 查询及认证凭据。

```sh
RUN_DOCKER_INTEGRATION=1 python3 -m unittest -v scripts/tests/test_caddy_oidc_logs.py
```

### 运行中的 Squid 出口隔离

新增可重复执行的验收脚本：

```sh
./scripts/test-oidc-egress-image.sh --log-dir /tmp/oidc-egress-evidence
```

使用锁定的 Squid 镜像及实际部署 entrypoint，九个组合全部通过：三种出口模式，
分别测试 OIDC 关闭、自定义 issuer 域名和 Supabase 项目域名。

- Gateway 来源仅能 CONNECT 配置的精确 issuer 的 443 端口。
- 其他项目、父域、子域、模型上游、IP 字面量、80/8443 端口及普通 GET 均拒绝。
- Codex、Antigravity 及未知来源不能借用 Gateway 的 OIDC 出口权限。
- 代理日志证明 OIDC 始终直连；模型流量按原 direct/relay 规则路由。
- 模型中继停止后 OIDC 仍可用，模型请求失败；shadowsocks 父代理不可达时模型
  请求不回退直连，目标连接计数不增加，OIDC 仍直连。

容器使用 `--network none`，以独立回环地址模拟来源和上游。仅替换测试副本中的
源地址、监听地址并缩短超时；域名、端口和方法 ACL 及生产渲染逻辑保持原样。
CONNECT 后的双向合成数据验证隧道可用性，不访问真实账号中心或外部模型服务。
这证明代理规则的运行行为，不代替生产网络连通性和真实 TLS/SSO 联调。

### 容器内应用与数据库

Go 镜像提供 GCC 12.2.0，并安装 Node 18.20.4 执行管理台测试。仓库只读挂载，
缓存与产物使用临时目录；PostgreSQL 使用独立网络及临时数据库，未发布宿主端口。
以下检查全部通过，无因缺少数据库或 Node 而跳过的目标验收：

- Gateway 构建、全量单元、全量 race、`go vet ./...`。
- PostgreSQL 17.6 全量 store/server 集成测试，分别使用新建空库。
- store 的 External 集成及 server 的 OIDC HTTP 集成加 `-race`。
- 使用仓库 Dockerfile 构建生产 Gateway 镜像；构建上下文仅包含源码、Go 模块文件
  和 Dockerfile，不包含 `.env` 或部署 secret。
- 生产镜像以 UID 10001、只读根文件系统和 `--cap-drop ALL` 启动；OIDC 关闭及
  启用两种配置的健康、就绪、功能配置、回调页面、脚本和首页检查通过。

启用模式使用合成客户端和 issuer 配置，验证启动及功能开关行为，未执行真实
授权交换。临时容器和网络在验收后删除，实际部署开关及数据保持原值。
定向 race 分别执行 11 个 External、22 个 OIDC HTTP 顶层测试，全部通过且无跳过。
启动检查完成 28 项已有迁移，用户数仍为零。验收镜像保留为
`cg-sso-gateway:20261004`，镜像 ID 为
`sha256:8df002a39940f0db3f8bd878662881f916a348898fffa7d9d886bca420f8af0c`，
仅用于本次工作区验收，未作为发布版本部署。

本次原始证据保存在仓库外：`/tmp/cg-sso-docker-compose/`、
`/tmp/codex-oidc-egress-acceptance-20261004/` 和
`/tmp/cg-sso-docker-go-20261004/`；包含配置校验、代理访问、测试及启动日志。
临时目录可能随环境清理，持久验收结论以本记录为准。

## 仍需真实账号中心验收

用户确认本轮先不依赖真实账号中心配置，因此真实授权、换码、首次开通、绑定及
再验证链路另行联调，本次未部署上线。Docker 验收通过不替代该步骤；
上线前仍须完成 [真实环境验收](oidc.md#上线验收)。
