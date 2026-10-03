# 吾水阁账号绑定与统一登录

此功能默认关闭。现有网关用户先用密码或 Passkey 登录，在“账号安全”完成近期
二次验证后绑定吾水阁账号；此后可通过该绑定统一登录。未绑定的统一账号不能
注册网关用户，也不会按邮箱或用户名合并。用户 ID、余额、权限、用量和 API Key
仍属于原网关用户；密码、Passkey、恢复流程及原有会话继续有效。

## 账号中心登记

每个环境登记独立的 confidential OAuth 客户端，认证方式使用
`client_secret_basic`。精确登记回调地址
`https://<GATEWAY_DOMAIN>/auth/oidc/callback`，申请 `openid email`，启用 RS256
或 ES256 签名。吾水阁已有授权页面，基础接入无需修改账号站代码。
登记与签名要求见 [Supabase OAuth Server 官方说明](https://supabase.com/docs/guides/auth/oauth-server/getting-started)。

账号中心的“已授权应用”可链接到
`https://<GATEWAY_DOMAIN>/?login=water5`。网关先移除一次性登录提示并检查现有
会话；已有会话直接进入控制台，未登录且 OIDC 已启用时自动通过站内 POST
发起统一登录。登录提示不包含用户凭据，也不指定 OAuth 回调或跳转目标。
OIDC 未启用或暂不可用时保留密码与 Passkey 登录入口；未绑定账号仍需先在
“账号安全”完成绑定。

从实际项目确认可信 issuer，通常为
`https://<PROJECT_REF>.supabase.co/auth/v1`。自定义 Auth 域名以该项目实际发现
文档为准，不能用吾水阁前端站点地址、publishable key 或 service-role key 代替。
网关不需要 Supabase 管理密钥或数据库凭据。部署默认配置不填写实际 issuer、
客户端 ID 或客户端 secret；下列示例均为占位值。已只读确认的账号站公开 issuer
与 discovery/JWKS 结果见[协议验证记录](oidc-protocol-validation.md)，这不代表
完整登录联调已通过。

## 配置与可选 Compose 部署

| 配置 | 含义 |
| --- | --- |
| `OIDC_ENABLED` | 默认 `false`；启用时设为 `true` |
| `OIDC_ISSUER` | 固定可信 HTTPS issuer；仅 443，不带查询或 fragment |
| `OIDC_CLIENT_ID` | 本环境独立 confidential 客户端 ID |
| `OIDC_CLIENT_SECRET_FILE` | 客户端 secret 文件，Compose 固定为 `/run/secrets/oidc_client_secret` |
| `OIDC_CLIENT_SECRET` | 非 Compose 部署可直接提供；不要与文件配置同时使用 |
| `OIDC_PROXY_URL` | 专用 HTTP 代理，必须使用私有 IP 字面量及端口；Compose 固定为 `http://172.28.30.4:3128` |
| `OIDC_AUTH_HOST` | 仅供 Squid：与 issuer 完全一致的小写主机名，不带协议、路径、端口或通配符 |

基础 `docker-compose.yml` 固定关闭 OIDC，不引用任何新增 secret。启用时，先完成
数据库备份与恢复演练，再在 `.env` 配置：

```dotenv
OIDC_ENABLED=true
OIDC_ISSUER=https://PROJECT_REF.supabase.co/auth/v1
OIDC_CLIENT_ID=replace-with-staging-client-id
OIDC_AUTH_HOST=PROJECT_REF.supabase.co
```

替换为真实的小写项目主机名。通过已有的受保护 secret 配置流程，把客户端 secret
写入 `deploy/secrets/oidc_client_secret`，要求普通非符号链接文件、权限 `0640`、
组与 `GATEWAY_SECRET_GID` 一致。不要将 secret 放入 `.env`、命令行参数、日志或
版本控制；`bootstrap-secrets.sh` 不会替你生成账号中心签发的客户端 secret。

```sh
./scripts/validate-compose.sh --oidc
./scripts/compose.sh -f deploy/oidc.override.yml build gateway
./scripts/compose.sh -f deploy/oidc.override.yml up -d egress-allowlist gateway
./scripts/compose.sh -f deploy/oidc.override.yml ps
```

启用后的 Compose 操作均保留 `-f deploy/oidc.override.yml`，避免后续 `up` 意外
恢复基础配置。该 overlay 只增加 Gateway 专用 secret 和环境变量，不新增网络、
宿主端口、全局 `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` 或启动健康依赖。

Squid 仅允许 `172.28.30.2/32` 的 Gateway 对指定 Auth 主机发起 HTTPS 443
CONNECT。`-n` 禁止通过反向 DNS 将 IP 请求匹配为主机；不允许子域通配符。
Codex、Antigravity 的现有来源与目标列表保持独立。OIDC 直接使用本机 Squid
出口，不使用可选的模型上游 WireGuard 中转；B 端无需扩大允许列表。
网关协议客户端同时限制 discovery、token 和 JWKS 请求的来源、路径、重定向、
超时及响应大小；配置允许的发行者与发现端点必须符合这些约束。

发现文档按需加载并缓存。账号中心不可用不阻止 Gateway 启动、原有登录或已有
会话使用，只影响新的绑定和统一登录。不要在 Squid、Caddy、Cloudflare 或监控
中开启完整回调 URL、请求正文或认证响应记录；代理只记录 CONNECT 主机、状态
与路径类型，审计只记录操作结果及必要标识。

## 绑定、解绑与会话行为

绑定在同一标签页完成。返回网关后，用户先看到本地网关账号和统一账号的脱敏
邮箱，再明确确认。确认仍要求原用户、原会话有效且本地二次验证在五分钟内；
期间退出、切换本地账号、禁用或验证过期，都要重新开始。邮箱仅用于展示，唯一
身份依据是经过验证的 `(issuer, subject)`。

统一登录只允许已有有效绑定的 active 用户；pending、disabled 及未绑定身份
被拒绝。已有本地登录时必须先退出再发起，不能静默切换。统一登录会话不会自动
取得近期验证资格；查看 API Key、改密码、解绑仍使用原密码或 Passkey 验证。

解绑撤销该次绑定创建的全部统一登录会话，保留密码、Passkey 等原登录会话。
若当前会话来自统一登录，解绑后回到登录页。重新绑定生成新的绑定 ID，不能恢复
已撤销会话。永久删除本站用户只清理本站映射，不删除 Supabase 全局账号。

中心退出、中心改密码或撤销应用授权不自动撤销网关已经建立的会话；全局退出与
封禁同步不在本期范围内。网关本地禁用继续立即控制本站业务访问。

现有 Cookie 保持 `Secure`、`HttpOnly`、`SameSite=Strict`。跨站回调的 GET
仅提供同源落地页，不建立会话或写入绑定；页面先清除 URL 授权参数，再以同源
POST 完成处理。登录还要求短期 HttpOnly 事务 Cookie，绑定同时要求原会话。
无需修改 Origin 白名单、CSP、跨域管理接口或浏览器持久存储。

登录与绑定事务分开保存，十分钟过期、容量受限、原子一次性消费；换码失败不
自动重试。重启会丢弃未完成事务，应重新开始；单实例部署之外需保证整个流程
落到同一 Gateway 实例。Supabase 令牌只在请求处理中使用，不保存刷新令牌。

## 升级与回退

追加式迁移创建 `external_identities` 并为 `sessions` 添加可空来源字段。
旧会话的来源仍为空，不轮换会话/API Key 密钥，不导入旧密码或迁移 Passkey。
备份恢复副本先验证迁移、旧会话及原业务数据，再更新全部 Gateway 实例。

功能回退时把 `.env` 的 `OIDC_ENABLED` 改为 `false`，保持 overlay，重新创建
`egress-allowlist` 和 `gateway`。新绑定和统一登录入口关闭，Squid 的专用授权
规则同时移除；已有会话和新增数据结构保留。也可在恢复基础 Compose 配置时
移除 overlay，并重新创建这两个服务；基础部署不再挂载 OIDC secret。
不要删除迁移记录或直接回退到不认识新迁移的旧二进制。必须回退程序时，按既有
流程停写并将升级前备份恢复到隔离数据库卷，评估备份后数据损失。

## 上线验收

以下真实环境验收必须在独立测试 Supabase 项目和真实测试域名完成并留存结果。
自动化模拟服务、Compose 渲染或单元测试通过不能替代它们；未取得客户端凭据
时保持生产功能关闭。

1. 使用不同站点的授权页，在桌面和手机浏览器完成 Strict Cookie 回调；GET
   回调不写会话，URL 参数被清除，同源 POST 后再处理。检查拒绝授权、重复回调、
   刷新、后退、缺少事务 Cookie、账号中心登出及不同账号选择。
2. 验证密码和 Passkey 二次验证后绑定、明确确认、错绑取消、切号、退出后迟到
   响应、会话失效、用户禁用、超过五分钟确认及十分钟事务过期。
3. 验证未绑定/pending/disabled 身份无法登录，已绑定身份保持原用户 ID、余额、
   权限、用量和 Key；已有登录不能静默切换，统一登录不能直接执行敏感操作。
4. 同时登录与解绑，确认没有遗漏撤销的统一登录会话；旧登录会话仍有效。
   争抢同一外部身份、重复确认、重新绑定均保持唯一性，旧已撤销会话不能恢复。
5. 在真实项目轮换签名密钥，确认 JWKS 刷新和旧/新有效令牌处理；验证错误 issuer、
   audience、签名、nonce、state、PKCE 和重放均失败，不创建本地用户。
6. 从 Gateway 验证只有指定 Auth 主机的 443 CONNECT 可通；其他 Supabase 项目、
   模型上游、IP 地址、80 端口均拒绝。从模型容器验证不能借用 Gateway 的 OIDC
   权限。中断 Supabase 或 Squid 后，原登录、旧会话和 `/readyz` 仍可用。
7. 检查日志、审计与浏览器存储没有授权码、ID/access/refresh token、客户端
   secret 或完整回调查询。确认 Cloudflare 不缓存 `/auth/*` 和 `/admin/*`。
8. 运行 Go 1.26.8 构建、全量测试、race、vet、store/server 集成测试、账号页面
   浏览器回归，以及 `./scripts/validate-compose.sh --oidc`。出口与部署回归可
   独立运行：

   ```sh
   python3 -m unittest scripts.tests.test_oidc_egress scripts.tests.test_codex_relay scripts.tests.test_relay_validation
   ```

上述 Python 部署回归使用真实 Compose 渲染与假 Docker daemon 验证安全策略；
生产 validator 还用锁定镜像实际解析 Squid/Caddy 配置，两者不能互相替代。
本地跨站 HTTPS Strict Cookie 与桌面/手机页面回归、截图和复现命令见
[浏览器验证记录](oidc-browser-validation.md)。

## 本次实现验证（2026-10-03）

Go 1.26.8 构建、全量 uncached 单元测试、全量 race、`go vet ./...`、
store/server PostgreSQL 集成测试及集成 race 均通过。数据库为临时容器中的
独立测试库；旧的 `TestPostgresIntegration` 使用固定用户名，重复运行全套前
需更换空测试库。主机未安装 C 编译器，因此 race 使用本地缓存的
`golang:1.26.8-bookworm`，仓库只读挂载，并挂载 Node 以执行管理台回归。
`gofmt -l .` 与 `git diff --check` 均无输出。

HTTP/数据库回归还覆盖换码期间本地登录或退出、迟到会话撤销、绑定原始验证
期限，以及迟到失败响应不清除新事务 Cookie。部署安全回归、默认关闭和可选
开启两种 Compose 的实际锁定镜像解析、Caddy 错误日志脱敏回归通过。

[浏览器验证与桌面/手机截图](oidc-browser-validation.md)记录真实 Chromium
跨站 Strict Cookie 和页面行为；[协议与部署验证](oidc-protocol-validation.md)
记录真实公开 discovery/JWKS、代理配置及日志检查。本轮没有真实 confidential
测试客户端与测试账号，已按确认维持默认关闭；真实授权码联调保留为上线前
待验收项。
