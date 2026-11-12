# 威胁模型

成员可使用可发现 Passkey 或用户名密码登录。密码以无 pepper 的 Argon2id PHC
保存（64 MiB、3 轮、并行度 2、随机 16 字节 salt、32 字节输出），PHC 在分配
资源前执行严格参数上限检查；哈希最多两个并发槽。未知用户、无密码、禁用用户
和错误密码统一执行同成本验证并返回 `invalid_credentials`，并受每 IP/路径
20 次/分钟及 IP + 规范化用户名摘要 5 次/分钟限制。

恢复码只在新凭据、恢复码轮换和新会话能够原子提交时消费。恢复撤销旧会话但保留未选择的既有登录方式，也不自动停用或删除 API Key。设置或更改密码保留当前会话及其近期验证时间，撤销其他会话。

## 范围和安全目标

受保护资产包括 ChatGPT Plus/Pro OAuth/refresh token、Antigravity Keyring 中的
订阅登录状态、设备 API Key、独立 API Key 加密密钥、Passkey 公钥与 challenge、
恢复码、会话、数据库凭证、配额数据和
安全审计。提示词、源代码和
模型回复属于高敏感瞬时数据：可以在转发内存或受限 tmpfs 中短暂存在，但不得
进入数据库、日志、备份或管理界面。按用户聚合的用量和
“API Token 等价成本”也属于受限管理元数据，Member 只能查看自己的
请求明细，只有 Owner 能查看全员汇总。

部署假设所有上游账号和所有受邀设备都由同一订阅者控制。若让其他真实用户共用
Plus/Pro 凭证，本威胁模型和订阅边界不再成立，必须改为每人独立上游凭证或官方
Platform API。

## 信任边界

```text
Internet
  -> Cloudflare Edge (public TLS)
    -> Cloudflare Tunnel (outbound connector)
      -> Caddy (edge_internal only)
        -> Gateway (edge_internal + data_internal + 两个上游内部网络)
          -> PostgreSQL (data_internal only)
          -> CLIProxyAPI (compat_internal only)
            -> A Squid allowlist (固定 Sidecar /32)
              -> [启用中转时：WireGuard -> B Squid]
              -> auth.openai.com:443 / chatgpt.com:443
          -> Antigravity Bridge (antigravity_internal only)
            -> A Squid allowlist (固定 Bridge /32)
              -> [启用中转时：WireGuard -> B Squid]
              -> 精确审核的 Google OAuth / Cloud Code 域名:443
```

A 的网关容器不发布宿主机端口。`cloudflared` 只连接 Tunnel 出站网络和边缘内部网络，
并把 Dashboard 中的 hostname 转给 `http://caddy:80`。其余带 `internal: true`
的网络没有默认互联网路由。Sidecar 和 Bridge 即使尝试绕过 `HTTP(S)_PROXY` 也没有
直接出口；Squid 只接受两个固定容器地址，并拒绝非 CONNECT、非 443 和不在对应
精确域名列表中的目的地。Gateway 虽加入两个上游网络，但没有 Squid 出站权限。
A/B 的 `dstdomain -n` 规则禁止字面 IP 请求通过反向 DNS 获得域名白名单权限。

配置非空 `CODEX_RELAY_IP` 时，A 为两个上游同时设置强制父代理和禁止直连规则；
B 故障时两者均失败。B 仅在 `10.77.0.2:3128` 监听，只接受
`10.77.0.1/32` 发来的 CONNECT/443，目的地限于同一套经审查的精确域名。
A 保留两类容器的目标隔离；B 看到的是 A 的隧道地址，不再区分原容器。
两个 Squid 均不解密 TLS，日志只记录目标、状态及转发元数据。未启用中转时，
两个上游经 A Squid 直连。部署与升级流程见[双服务器中转](openai-relay.md)。

## 主要威胁与控制

| 威胁 | 控制 | 验证方式 |
| --- | --- | --- |
| 扫描服务器公网 IP 绕过 Cloudflare | A 的网关服务零宿主端口，入站仅管理 SSH；B 另允许来自 A 的 WireGuard UDP，3128 仅在隧道内可达 | `validate-compose.sh`、外部端口扫描 |
| Tunnel token 泄漏 | Dashboard token 仅存 `0640` secret，以 `--token-file` 挂载给非 root、只读的 connector | 文件/mount/进程参数和日志检查 |
| API Key 数据库泄漏 | HMAC 用于认证；新 Key 的版本化 AES-256-GCM 密文使用仅挂载给 Gateway 的独立密钥，AAD 绑定用户和 Public ID，查看要求近期二次验证 | 加密篡改/AAD 测试、管理响应和数据库检查 |
| OAuth 被主服务或备份读取 | OAuth 只挂载到非 root sidecar；不挂载 Gateway/备份任务 | Compose mount 审计、灾备演练 |
| Antigravity 登录状态泄漏 | 完整认证文件经 stdin/stdout 编码存入加密 Keyring，调用前恢复到 UID 10002 的私有 tmpfs HOME；独立口令和 Bearer secret；不持久化设置、会话和缓存 | Compose mount/secret 审计、合成凭据跨容器恢复与刷新、错误口令及明文残留测试 |
| Refresh token 并发复用 | 登录/升级锁；先停唯一实例；禁止共享卷的双实例 | 容器状态检查、运维演练 |
| OAuth 文件权限放宽或 symlink | 启动时要求目录 UID 10001/精确 `0700`、文件 UID 10001/regular file/精确 `0600` | `verify-oauth-permissions.sh` |
| 多账号登录覆盖其他 OAuth 文件 | 登录前后只比较内部 Sidecar Key 加域的文件名/内容 SHA-256；要求恰好一个账号新增或刷新且零删除 | 重复登录、同账号刷新和权限测试 |
| 调用方伪造粘滞或账号归因头 | Gateway 生成每 API Key 的 HMAC 作用域；sidecar 消费作用域头；Gateway 消费账号头且不下发客户端 | Header 清理和跨 API Key session 隔离测试 |
| 即时额度接口被改造成任意 SSRF/令牌出口 | sidecar 仅允许固定 GET `chatgpt.com/backend-api/wham/usage`、禁止重定向并只返回规范化字段；完整管理 API 关闭 | URL/方法/Header/重定向、异常响应和敏感 canary 测试 |
| Sidecar 任意出网/SSRF | internal 网络加 Squid 精确域名和 443 allowlist | 代理 ACL 测试、网络 namespace 测试 |
| Bridge Agent 读取文件或执行工具 | 每请求空目录；strict 权限显式拒绝文件、命令、URL、MCP；工具事件使请求失败并终止进程组 | 假 CLI 协议、取消、子进程与残留测试 |
| Gateway 借上游网络直接出网 | Squid 来源 ACL 只放行 Sidecar `172.28.30.3/32` 和 Bridge `172.28.40.3/32` | Compose/Squid 固定地址校验 |
| B 故障导致出口静默切回 A | 启用中转时两个上游都必须使用同一父代理，禁止直接回退 | 固定 Squid 镜像下双来源断链/恢复测试、生产维护窗口故障验收 |
| B 成为公网或任意目标代理 | 仅监听 WireGuard 地址、仅允许 A 的隧道 /32、CONNECT/443 和精确域名；不对公网开放 3128 | B ACL 负例、Squid 配置校验、公网端口检查 |
| 请求头走私凭证 | Gateway 只接受已知路径/头，替换 Authorization，移除 Cookie、转发头及 hop-by-hop 头 | 代理和 fuzz 测试 |
| 超大正文/资源耗尽 | Caddy 与 Gateway 双重 64 MiB 上限；RPM、并发、日配额和全局流限制 | 限额与并发测试 |
| 邀请/恢复 token 泄漏 | URL fragment、单次/短期 token、HMAC 存储；不启用访问日志 | 邀请复用测试、日志扫描 |
| 会话劫持/CSRF | Secure、HttpOnly、SameSite=Strict；Origin/CSRF 校验；敏感操作 5 分钟内 Passkey 再验证 | 身份安全测试 |
| 伪造来源 IP 绕过限速 | origin 仅 Tunnel 可达；Caddy 与 Gateway 只信任各自上游的固定 `/32`，并重建 XFF | CF/XFF 伪造测试 |
| 供应链 tag 漂移 | 基础镜像 manifest digest；CLIProxy tag 与 full commit 双校验；`agy` 固定版本化 URL、SHA512 和运行时版本；固定 CI 工具版本 | CI、lock diff 与镜像构建审阅 |
| 明文内容进入日志/备份 | Caddy 无访问日志；debug/body 日志关闭；只备份数据库元数据且立即 age 加密 | 敏感字符串 canary 扫描 |
| Owner 上传的图片成为脚本或嗅探载体 | 只收 PNG/JPEG 且声明类型须与解码格式一致；解码后重新编码，丢弃 EXIF、注释和附加数据；以固定 `Content-Type`、`nosniff` 和 `default-src 'none'` 返回；前端只渲染同源路径图片、`https://` 链接（`noopener noreferrer`），文字一律 `textContent` | `site_test.go`、`site_settings_ui_test.cjs` 的 SVG、HTML 伪装、附加脚本、EXIF 与链接负例 |
| 匿名访问登录页接口放大数据库负载或获取收款码 | 交流群接口与群二维码使用 30 秒快照、图片按 SHA-256 缓存；收款码只在登录会话路由提供，关闭的卡片对非 Owner 返回 404 | 缓存读取计数、匿名与 Member 可见性测试 |
| Member 枚举其他用户用量 | 全员接口在查询前强制 Owner 角色，只返回按用户/模型聚合而非其他用户的请求级元数据 | Member 403 与 Owner 聚合测试 |
| Member 借群组用量接口探查上游账号或放大 ChatGPT 请求 | 默认关闭，由 Owner 按群组开启；账号集合只由会话用户的群组成员关系、“指定账号”池推出，请求不带账号 ID；“全部账号”池、Antigravity、已归档和已到期群组不展示；响应只含别名和用量窗口，不含账号 ID、邮箱、套餐和成本；Codex 读取按账号缓存 1 分钟并合并并发，与 Owner 额度查询共用 5 秒限流，实时查询写审计 | `group_account_usage_test.go`、两个 `group_account_usage*_integration_test.go` 的可见性、去标识与缓存测试 |
| 把 API Token 等价成本误当供应商实际账单 | 准入固化规则，ledger 不可变；界面/API 明示 ChatGPT/Antigravity 订阅、内部零价和兜底边界 | 计价、ledger 和文案测试 |
| 意外产生 Platform 费用 | 无 Platform Key、无自动回退；上游失效时 fail closed | 503/502 契约测试 |
| CPA 归因决定扣费层而误扣群组额度 | 准入固化每层账号池；结算只在包含该账号的预授权层中选择，账号缺失时回退第一层并告警；CPA 只能按层内账号选号，个人层仍受每用户资格回调约束 | `TestFundingLayersPostgresIntegration`、`TestFundingLayersGatewayPostgresIntegration`、CPA 分层选号测试 |

## 容器权限

Gateway 和 sidecar 使用 UID 10001，Bridge 使用 UID 10002；三者使用私有部署组、
只读根文件系统、`no-new-privileges` 和 drop-all capabilities。`cloudflared` 同样
以非 root、只读根文件系统和 drop-all capabilities 运行。Sidecar 只有 OAuth
volume 和两个小型 tmpfs 可写，OAuth 文件 umask 是 077。Bridge 只有独立 Keyring
volume 与私有 `/tmp`、运行目录可写，每个请求结束后清理 CLI 工作目录和日志。
Gateway 只有私有 `/tmp` tmpfs
可写，并以 4 个并发槽限制完整请求体的转发前扫描文件。Caddy 仅保留绑定内部
低端口所需的 `NET_BIND_SERVICE`。PostgreSQL 和 Squid 保留各自官方镜像启动
所需权限，但没有公网端口，且位于最小网络集合中。

两个上游分别使用与用户 Key 不同、彼此不同的内部 API Key。Gateway 在转发前丢弃
用户 Authorization 并设置对应 Bearer；sidecar 管理 API 禁止 remote access 且控制
面板关闭。只读账号列表和固定上游额度适配器位于私有兼容网络并使用同一内部
Bearer；它们不接受任意上游请求参数。任何 OAuth 文件下载接口都不经 Caddy 路由。

### CPA 插件模式（可选）

使用 `deploy/cpa-plugin.override.yml` 时，sidecar 运行未修改的 CLIProxyAPI，Gateway 的
账号分配、身份隔离和账号控制由镜像内的插件 `codex-gateway-v1.0.0.so` 执行。与补丁镜像
相比，信任边界有以下变化：

- **插件运行在 CPA 进程内**，与 CPA 共享地址空间和全部凭证，信任等级等同 CPA 本身。
  插件文件由镜像提供，属 root、权限 `0444`，所在目录对服务用户不可写；入口脚本在
  插件可写、目录里出现其他文件或插件缺失时拒绝启动。CPA 的插件商店不可用：插件目录
  只读，出口白名单也不包含任何插件仓库。
- **CPA 管理 API 开启。** 插件的路由挂在 `/v0/management` 之下，因此管理 API 必须
  开启。它只在私有兼容网络可达，使用独立的 `CPA_MANAGEMENT_KEY`，入口脚本拒绝它与数据面
  密钥相同；控制面板及其自动更新仍关闭。持有管理密钥即可通过 CPA 原生接口读写凭证，
  这把密钥的保护等级应与 OAuth 卷相同。Caddy 继续对 `/internal` 返回 404，`/v0` 本就不
  经 Caddy 路由。
- **多一个可执行的可写目录。** CPA 把插件复制到临时目录后再加载，所以 sidecar 多一个
  64 MB、`exec` 的 tmpfs（`/run/cliproxy-plugin`），只有服务用户可写。这削弱了“容器内
  没有可写且可执行的目录”这一加固。`/tmp` 和 OAuth 卷仍是 `noexec`。缺少该目录时插件
  无法加载，表现为拒绝服务。
- **失败时关闭依赖一个 139 行的残余补丁。** 插件未加载、被禁用或给出无效决定时，
  `plugin-only` 选择器拒绝请求；原版 CPA 在同样情况下会改用内置轮询。另外，Gateway
  每次派发前探测插件的能力路由，探测失败即拒绝派发。
- **插件 panic 会终止 CPA 进程。** 插件在每个入口自行捕获 panic 并转成拒绝；逃出插件的
  panic 无法被 CPA 捕获，进程退出后由容器重启，进行中的请求全部中断。
- **归因不再来自响应头。** Gateway 为每次派发生成一次性令牌，插件把选号结果连同令牌
  上报到 `/internal/*-accounts/attempt`。Gateway 只接受进行中、同一用户、同一 Provider
  的派发，并再次核对用户对该账号的权限和本次请求的账号池，所以伪造或错误的上报无法把
  费用归到用户无权使用的账号上。
- **账号出口的路由凭据写入凭证文件。** 插件在使用账号前向 Gateway 确认其出口，并把
  路由（含该路由的访问凭据）写入 OAuth 卷中的凭证文件；补丁镜像只在内存中使用它。上传
  凭证自带的代理一律被覆盖。路由只能指向固定的 `egress-allowlist:3129`；继承全局出口的
  账号写入全局出口地址。
- **账号出口失败时不回退，不靠补丁。** CPA 在凭证代理缺失或无法使用时会退回全局代理、
  再退回直连。插件模式下 CPA 没有全局代理（配置无 `proxy-url`，入口脚本清除代理环境
  变量），所在网络没有外网，所以这条退路必然失败，账号的流量只能经它自己的出口。
- **凭证操作经 CPA 原生管理接口。** 授权、导入、刷新、删除都由 CPA 完成，插件只把
  Owner 选择的出口显式交给 CPA，并在完成后按 CPA 保存的字段识别身份、向 Gateway 提交出口
  登记。授权码与导入的令牌经插件转交给 CPA，插件不保存、不记录，也不回传给 Gateway；
  CPA 刷新接口返回的凭证内容被丢弃。新增的 Antigravity 账号以 CPA 记录的邮箱为身份，弱于
  补丁镜像使用的 Google subject：同一 Google 账号改邮箱后会被当作新账号。
- **模型鉴定探针是普通请求。** Gateway 为每次探针生成一次性身份并在进程内固定到一个
  账号，分配回调与上报只对该身份放行这一个账号，且不检查 Owner 的账号权限。这个身份只由
  Gateway 自己的鉴定流程创建，客户端的身份头在转发前已被替换，探针结束即作废。
- **额度不发上游请求。** 插件不构造任何供应商请求，Codex 与 Claude 的额度只读取 CPA
  对真实请求响应头的被动观测（经 CPA 管理 API），Antigravity 额度不可用。插件不读取
  访问令牌；为核对账号身份只读取凭证文件中的账号标识。
- **CPA 内部的加固不再存在。** 补丁镜像在执行器里拒绝重定向、固定上游主机、按调用方
  和账号隔离各 Provider 缓存。插件接触不到这些代码，相应控制改由出口白名单承担；
  Provider 缓存依赖 CPA 原生的调用方作用域，尚未逐个核对缓存键。完整差异见
  [兼容层升级规程](compatibility-upgrades.md#插件模式可选尚非默认)。

## 数据生命周期

- 请求明细元数据保留 90 天，日/月聚合及不含内容的不可变账务 ledger 长期保留；
  明细过期后仍可按 ledger 对账。
- 安全审计保留 365 天。
- 请求和响应正文不落库，因此不进入 `pg_dump`。
- 站点设置的文字和重新编码后的图片（含收款码）存于数据库，随加密备份保留；
  删除图片即从当前库移除，旧备份按备份保留期过期。
- OAuth volume 永不备份；灾备后重新登录。
- Antigravity Keyring 不进入数据库备份或计划迁机复制；灾备后通过隔离登录脚本重建。
- PostgreSQL 每日 03:00 UTC 生成本地 age 密文，保留最近 14 组；每月至少在
  无网络临时容器中恢复演练一次，升级前追加一次。
- 停用的 API Key 立即拒绝新认证；永久删除移除活动凭证及密文，但数据库保留不含
  秘密的最小历史引用，以维持既有用量、账务、配额和审计完整性。

## 残余风险

- ChatGPT Pro 登录不是官方通用服务端 API，CLIProxyAPI 可能因上游协议改变而
  失效。控制是固定兼容版本、契约测试、人工冒烟和明确 503，而非静默回退。
- Antigravity Bridge 依赖官方 `agy` 的订阅登录和 Headless 输出；CLI 模型、认证或
  事件格式变化会使 readiness 或请求失败。Google 出口域名基线仍须用真实账号流量
  验收，不能把假 CLI 测试视为生产可用证明。
- 公网 TLS 在 Cloudflare Edge 终止，Cloudflare 及转发链路上的
  cloudflared/Caddy/Gateway/sidecar/Bridge 可见必要的瞬时内容。主机 root、Docker
  daemon 或内核被攻破后无法靠容器边界保密。
- 精确域名 allowlist 仍信任这些域名的 DNS、证书和服务端。Squid 不做 TLS
  解密，因此不能检查加密路径，但也不会看到 OAuth 或提示词。
- 单服务器是可用性单点。本地 age 密文、解密 identity 和其他恢复 secret 会随
  服务器或云盘整体丢失，因而只能处理数据库逻辑损坏和计划迁机，既不能提供
  无中断故障转移，也不构成整机灾备。
- 启用双机中转后，B 与 WireGuard 也是 Codex 和 Gemini 的共同可用性依赖；
  重建任一 Squid 会中断活动 CONNECT/SSE。B 管理员可见目的地主机和连接元数据，
  正常 CONNECT 转发不会获得上游 TLS 明文或账号凭据。
- 本版本不支持 API Key 加密密钥轮换。数据库备份不包含该部署 secret；密钥丢失或
  被替换后，HMAC 认证资料仍无法用于还原明文，新 Key 的查看操作会失败。
- 各 Plus/Pro 账号自身配额和服务限制不可由 Gateway 保证；应 fail closed 并告警。
- 插件模式下 CPA 管理 API 开启，管理密钥泄露即等于凭证泄露；sidecar 有一个可执行的
  可写 tmpfs；CPA 执行器内的重定向与主机限制不再存在，只由出口白名单约束。插件模式在
  完成真实账号验收前不是默认部署。
- 充值完全人工：网关不核验付款，也不把付款与账户自动对应。Owner 会话被攻破后
  可替换收款码，引导用户付款给他人；写入要求近期二次验证，每次替换记录
  `site.image_updated` 审计及图片 SHA-256。收款码随数据库进入加密备份。

## 安全验收

上线前至少完成：HTTP/SSE fuzz、CSRF/Origin、WebAuthn challenge 重放、路径
穿越、SSRF、请求走私、伪造 XFF、无效 Key 限速、并发/日配额原子性、客户端
断开取消、上游 401/429/超时映射，以及带 canary 的日志、数据库和备份敏感
内容扫描。还需伪造 `CF-Connecting-IP`/`X-Forwarded-For` 验证来源 IP 边界，
并从公网确认服务器的 80、443、5432、8080、8317、8318 和 3128 均不可达。
