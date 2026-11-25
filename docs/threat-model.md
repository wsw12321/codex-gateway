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

部署假设平台账号（Owner 的上游账号）和所有受邀设备都由同一订阅者控制。若让其他真实用户
共用 Plus/Pro 凭证，本威胁模型和订阅边界不再成立，必须改为每人独立上游凭证或官方 Platform API。
商城开关打开后，成员带来的账号不再满足这个假设，见下一节。

## 多租户（聚合网关）

商城开关 `marketplace_settings.enabled` 默认关闭。关闭时只有 Owner 的平台账号服务请求，
本节的威胁都不存在；设置读取失败时，租户功能按关闭处理（fail closed），平台路径不受影响。
打开后，成员可以添加自己的上游账号、API Key 供应商和出口服务器
（[我的供应商](tenant-providers.md)），并发布用这些供应商服务的套餐
（[套餐商城](marketplace.md)）。各阶段的实现与验证见
[阶段 2](aggregation-gateway-stage2-validation.md)、[阶段 3](aggregation-gateway-stage3-validation.md)、
[阶段 4](aggregation-gateway-stage4-validation.md) 和 [阶段 5](aggregation-gateway-stage5-validation.md)。

### 参与方与信任

| 参与方 | 信任 | 能看到什么 |
| --- | --- | --- |
| Owner（运营者） | 完全信任 | 全部成员的账号、套餐、成交双方、用量、审计；运维权限可读取 CPA 中所有凭证 |
| 卖家（发布套餐的成员） | 不信任 | 自己的账号与套餐；买家只显示匿名编号，用量只按套餐和账号汇总 |
| 买家（购买成员套餐的用户） | 不信任 | 套餐的卖家显示名、说明、价格、供应商构成（官方 OAuth / 第三方 API 及各协议可用账号数） |
| 成员的 API Key 供应商 | 不信任，第三方 | 它处理的每个请求的完整提示词和回复 |
| 成员出口服务器 | 不信任 | 经它出站的连接的目的地与 TLS 密文 |

成员把凭证交给平台托管，就必须信任 Owner：同一个 CPA 进程、同一个 OAuth 卷保存全部成员的
凭证，持有 CPA 管理密钥或主机 root 的人能读取它们。本模型不防御恶意 Owner。

### 单一订阅者假设不再成立

- **平台资金只走平台账号。** 群组、平台套餐（“不限制”范围）、Owner 手工发放的订阅和现金只由
  平台账号服务。准入、逐层资格、`/attempt` 归因和结算四处分别判断，数据库触发器另外拒绝把成员
  账号放进群组账号池、专属授权和用户账号权限名单。成员账号的用量永远不扣个人资金。
- **扣费层由 Gateway 解释。** 插件按层询问资格（`layer_select_v1`），层 ID 只由 Gateway 生成和
  解释：自用层只接受本人的账号，订阅层只接受该订阅来源套餐当前范围内的账号，个人层与群组层只接受
  平台账号。插件或请求无法把成员账号塞进别人的个人层。不支持逐层资格的旧插件收不到任何含成员
  账号的层，请求失败关闭。
- **归因与结算兜底。** `/attempt` 只接受在本次请求某个层里、且此刻在该层仍有资格的账号；结算同时按
  准入层和当前归属判断。缺少归因时不按自用免费；不在任何层里的成员账号不扣任何资金。卖家套餐额度
  不足的部分记为未覆盖（`uncovered_usd`），由卖家承担，绝不扣买家现金。
- **未登记的账号不服务。** 开关打开期间，元数据同步不会把 CPA 中未登记的账号插入为平台账号；没有
  Gateway 记录的账号不被任何资金路径使用，成员中途放弃的登录不会被平台使用。

### 第三方 API 供应商能看到买家的请求内容

成员添加的 API Key 供应商由成员控制 Base URL，Gateway 把买家的完整请求发给它，它也看到全部回复。
这与 OAuth 账号不同：OAuth 请求经 CPA 发往真实的上游服务商，卖家只控制账号本身，看不到请求内容。

- 商城标出含成员 API Key 供应商的套餐（`third_party_api`），买家购买和续费时必须显式确认
  （`acknowledge_third_party=true`，服务端校验，不只是界面提示）。
- 确认记在订阅上（`billing_subscriptions.third_party_acknowledged_at`，迁移 `0056`）。供应商范围的
  修改不经审核、立即生效，所以卖家可以在售出后把 API Key 供应商加进一个原本只含 OAuth 账号的套餐；
  这样的供应商只为已确认的订阅服务。准入的账号池、插件的逐层资格（`/eligible`、`/select`）、
  `/attempt` 归因和 `/v1/models` 都按订阅的确认判断，没有确认时该层只含卖家的 OAuth 账号。买家在
  订阅卡片上随时可以确认或撤回，从下一次请求起生效，每次变更都有审计（`billing.third_party_consent_changed`）。
  只因缺少确认而为空的订阅层不产生 `subscription_layer_empty` 告警：那是买家的选择，不是卖家没有账号。
- 即使是 OAuth 账号，请求也以卖家账号的身份发给上游服务商，受该账号自身的数据设置约束（例如是否
  允许用于改进模型）。Gateway 无法检查或保证这些设置。
- 供应商的 Base URL 经与平台供应商相同的检查：只接受 HTTPS、公网主机名或地址，不允许用户信息、查询串
  和路径穿越；请求经该账号自己的出口（成员节点或全局出口）发出。Gateway 不记录请求或回复内容，这一点
  对成员供应商同样成立。

### 恶意供应商与注入内容

API Key 供应商可以返回任意内容。Codex、Claude Code 等 Agent 客户端会执行回复里的工具调用，恶意
供应商因此可以诱导客户端运行命令、修改或外传本地文件。Gateway 只转发协议，无法判断回复内容是否
可信。

- 控制只有标识、确认和处置：第三方 API 标记与购买确认；Owner 可以停用成员账号
  （`admin_suspended`，成员不能解除）、强制下架套餐、转移账号归属；所有操作有审计。
- 买家应把成员 API 套餐当作不受信任的模型来源，在客户端保持工具调用审批，不在含机密的工作区使用。
- 供应商也可以虚报用量：多报只消耗买家在**该卖家套餐**中的额度（绝不触及现金和其他订阅），少报由
  卖家自己承担。供应商还可能用更便宜的模型冒充请求的模型；模型鉴别只覆盖平台账号，这是剩余风险。

### OAuth 归属冲突

- 登录或导入到已归平台或他人的上游账号时，Gateway 的出口登记返回 409（`account_owned_elsewhere`、
  `provider_limit_reached`、`account_in_platform_use`）。插件只删除这次登录新保存的凭证文件，不停用、
  不删除原主人的账号；被 CPA 原位改写的凭证保留，并恢复原主人的出口。能完成上游登录的人本来就控制
  该上游账号，所以这种改写不扩大权限（e2e `TestCredentialOwnershipConflict`）。
- 归属只能通过两条路径改变：登录认领已释放的账号，或 Owner 转移；数据库触发器只接受带 `claim`、
  `transfer` 标记的变更（阶段 5 增加只用于永久删除成员的 `owner_deleted`，见下文），每次变更都有审计。
- 删除凭证释放归属：账号不再服务任何请求，下一次登录同一上游账号的人可以认领。认领后的账号带着
  原有的用量历史，新主人在“我的供应商”中能看到该账号历史上的聚合用量（不含请求内容和用户身份）。

### 卖家与买家之间的数据隔离

- 成员只能读写自己的账号、出口、套餐和成交记录；访问别人的资源一律 404，不暴露是否存在。成员访问
  审核、商城设置和全部成交接口得到 403。
- 卖家只看到每个买家的顺序编号（每个卖家独立编号）、按账号聚合的订阅扣费和成交金额；卖家的收入流水
  不含买家，也不把买家记为操作人。买家看不到卖家的账号标识、邮箱或出口，只看到供应商构成摘要。
- 粘滞与会话隔离沿用 Gateway 为每个 API Key 生成的调用方作用域，同一个卖家账号服务多个买家时，CPA
  推导的会话和重放状态仍按调用方隔离。[CPA Provider 缓存键核对](cpa-provider-cache-audit.md)记录的
  两处键不含调用方的缓存（Gemini Responses 文本签名、Claude OAuth 工具别名）在多租户下跨越了用户
  边界：利用它需要知道另一调用方的上游消息 ID，得到的是签名或工具名而不是对话内容，仍按该文档接受。
- 卖家能看到自己账号的上游用量（例如服务商后台的额度消耗），由此可以推测买家的使用强度，但不能把它
  对应到具体买家。

### 成员自建出口服务器

账号出口服务在宿主网络上拨号，成员控制的地址可能被用来探测部署或宿主的内部网络。成员节点只接受
公网 IPv4 字面量（拒绝域名，防止 DNS rebinding），数量有上限（`max_egress_servers_per_user`，
0–32），连通性测试按用户限频；成员节点只能绑定成员自己的账号，数据库触发器另外拒绝跨主人的绑定和
登记。成员节点能看到经它出站的连接的目的地和 TLS 密文，看不到 OAuth 令牌或请求内容。成员节点
失效时，该账号的请求、刷新和登录失败，不回退到全局出口或直连。

### CPA 凭证数量增长

每个成员账号在 CPA 的 OAuth 卷里多一个凭证文件，CPA 在内存中加载全部凭证并各自刷新令牌。凭证数量
从“Owner 的几个账号”增长到“成员数 × 每人上限”。

- 每人账号上限 `max_accounts_per_user`（0–256，OAuth 与 API Key 合计，已释放的不计）；OAuth 发起
  每人最多 2 个并发、每分钟 10 次，导入每分钟 10 次，刷新和额度查询每分钟 20 次。
- 一次请求最多 64 个扣费层，每层最多 256 个账号；层数受 CPA 头部大小约束。
- 删除凭证同时删除 CPA 中的文件；已释放账号的数据库记录只保留元数据。OAuth 卷不进入备份。
- 剩余风险：大量成员同时登录会放大 CPA 的令牌刷新流量和上游的异常检测风险；Owner 应按服务器规模
  设置上限，必要时停用异常账号。

### 成员停用与永久删除

- **停用**（用户状态不是 `active`）：卖家套餐立即停售且不能续费，卖家账号不再进入任何订阅层，已购
  订阅的层被跳过；买家仍有额度时触发 `subscription_layer_empty` 告警。停用的成员本人也不能发起请求。
- **永久删除**（信息维护）：只有余额为零、没有任何历史的成员能删除。成员仍持有凭证的账号、出口
  服务器、未删除的套餐、仍被订阅引用的套餐和商城成交记录都会阻止删除，并给出原因（候选列表也
  排除这些成员），不会只返回数据库错误。删除时，成员已释放的账号清空主人并记录
  `owner_deleted_at`：这些账号仍不服务任何请求，也不进入任何人（包括平台）的管理范围，下一次登录
  同一上游账号时按认领处理；成员已删除且无人引用的套餐随之删除。处理流程见
  [运维手册](operations.md#成员的停用与永久删除0055)。
- 停用的成员不能登录，无法自己删除出口服务器；Owner 在“租户供应商”中删除成员节点（与成员自己删除的
  规则相同：仍被账号绑定或被进行中的登录选择的节点会被拒绝），审计记 `egress_server.deleted`，操作人为
  Owner。信息维护的候选搜索会列出与搜索匹配、但不能删除的用户及其原因和处理方法。

### 告警

- `subscription_layer_empty`：买家已付费、订阅仍有额度且覆盖所请求的模型，但套餐此刻没有任何可用
  账号（卖家停用、账号全部被停用或删除、范围不含该协议的账号）。该层被跳过，请求落到后续层或返回
  503。每个订阅、每个协议一条，重复请求合并计数。
- `billing_attribution_missing`：请求缺少账号归因。规则 4 的多层请求按第一层结算；规则 5 不按自用
  免费，按第一个群组层、受限订阅层或个人层结算，都没有时记为未覆盖。告警说明实际的结算方式。

### 剩余风险

- 把 ChatGPT、Claude 订阅账号的使用权转售给他人，很可能违反上游服务条款，账号可能被封；被封后买家的
  订阅层变空（告警），系统没有自动退款，只能由 Owner 手工调整余额。
- 卖家可以随时修改供应商范围（不经审核）。新加入的 API Key 供应商只为已确认的订阅服务；但买家一旦
  确认，就是对该卖家全部 API Key 供应商的确认，之后卖家新增的供应商无需再次确认。
- API Key 供应商的运行时状态（并发、粘滞、冷却）保存在 Gateway 进程内存中，只支持单 Gateway 实例。
- 卖家只看到最近一次审核备注；完整审核历史只在审计事件中。

## 信任边界

```text
Internet
  -> Cloudflare Edge (public TLS)
    -> Cloudflare Tunnel (outbound connector)
      -> Caddy (edge_internal only)
        -> Gateway (edge_internal + data_internal + compat_internal + 出口控制网络)
          -> PostgreSQL (data_internal only)
          -> CLIProxyAPI + Gateway 插件 (compat_internal only)
            -> A Squid allowlist (固定 Sidecar /32)
              -> [启用中转时：WireGuard -> B Squid]
              -> 精确审核的 OpenAI、Google OAuth / Cloud Code 与 Anthropic 域名:443
```

A 的网关容器不发布宿主机端口。`cloudflared` 只连接 Tunnel 出站网络和边缘内部网络，
并把 Dashboard 中的 hostname 转给 `http://caddy:80`。其余带 `internal: true`
的网络没有默认互联网路由。Sidecar 即使尝试绕过代理也没有直接出口；Squid 只接受固定
的 Sidecar 地址（以及可选 OIDC 下的 Gateway 地址），并拒绝非 CONNECT、非 443 和不在对应
精确域名列表中的目的地。Gateway 虽加入兼容网络，但没有 Squid 的模型出站权限。
A/B 的 `dstdomain -n` 规则禁止字面 IP 请求通过反向 DNS 获得域名白名单权限。

配置非空 `CODEX_RELAY_IP` 时，A 为两个上游同时设置强制父代理和禁止直连规则；
B 故障时两者均失败。B 仅在 `10.77.0.2:3128` 监听，只接受
`10.77.0.1/32` 发来的 CONNECT/443，目的地限于同一套经审查的精确域名。
A 保留两类容器的目标隔离；B 看到的是 A 的隧道地址，不再区分原容器。
两个 Squid 均不解密 TLS，日志只记录目标、状态及转发元数据。未启用中转时，
两个上游经 A Squid 直连。部署与升级流程见[双服务器中转](openai-relay.md)。

价格目录随 Gateway 镜像发布，与代码同等审阅。服务器上的 `deploy/pricing/override.json` 是与 `.env`
同级的运维输入：只读挂载进 Gateway，只能修改已有模型的正价格和固定汇率，不能增删模型或改变计费结构。
`validate-compose.sh` 拒绝其他用户可写的覆盖文件，Gateway 拒绝符号链接和无法读取的文件。价格工具
`scripts/pricing.sh` 在没有网络、没有密钥的独立容器中运行。

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
| 多账号登录覆盖其他 OAuth 文件 | 凭证只经 CPA 原生管理接口逐个新增、导入、刷新、删除；删除前确认账号已停用且没有进行中的请求；入口脚本拒绝命令行登录 | 重复登录、同账号刷新和权限测试 |
| 调用方伪造粘滞或账号归因 | Gateway 生成每 API Key 的 HMAC 作用域，插件把它交给 CPA 作为调用方；归因只来自请求账本中插件的上报，任何响应头都不被采信 | Header 清理、伪造归因头被忽略和跨 API Key session 隔离测试 |
| 额度接口被改造成任意 SSRF/令牌出口 | 插件不构造任何供应商请求，额度只读取 CPA 对真实请求的被动观测并只返回规范化字段 | 额度路由不产生上游请求的端到端测试、异常响应和敏感 canary 测试 |
| Sidecar 任意出网/SSRF | internal 网络加 Squid 精确域名和 443 allowlist | 代理 ACL 测试、网络 namespace 测试 |
| Gateway 借上游网络直接出网 | Squid 来源 ACL 只放行 Sidecar `172.28.30.3/32`（旧 Bridge 地址不再被放行） | Compose/Squid 固定地址校验 |
| B 故障导致出口静默切回 A | 启用中转时 Sidecar 的全部 Provider 流量都必须使用同一父代理，禁止直接回退 | 固定 Squid 镜像下双来源断链/恢复测试、生产维护窗口故障验收 |
| B 成为公网或任意目标代理 | 仅监听 WireGuard 地址、仅允许 A 的隧道 /32、CONNECT/443 和精确域名；不对公网开放 3128 | B ACL 负例、Squid 配置校验、公网端口检查 |
| 请求头走私凭证 | Gateway 只接受已知路径/头，替换 Authorization，移除 Cookie、转发头及 hop-by-hop 头 | 代理和 fuzz 测试 |
| 超大正文/资源耗尽 | Caddy 与 Gateway 双重 64 MiB 上限；RPM、并发、日配额和全局流限制 | 限额与并发测试 |
| 邀请/恢复 token 泄漏 | URL fragment、单次/短期 token、HMAC 存储；不启用访问日志 | 邀请复用测试、日志扫描 |
| 会话劫持/CSRF | Secure、HttpOnly、SameSite=Strict；Origin/CSRF 校验；敏感操作 5 分钟内 Passkey 再验证 | 身份安全测试 |
| 伪造来源 IP 绕过限速 | origin 仅 Tunnel 可达；Caddy 与 Gateway 只信任各自上游的固定 `/32`，并重建 XFF | CF/XFF 伪造测试 |
| 供应链 tag 漂移 | 基础镜像 manifest digest；CLIProxy tag 与 full commit 双校验、残余补丁与模型目录 SHA-256；固定 CI 工具版本 | CI、lock diff 与镜像构建审阅 |
| 明文内容进入日志/备份 | Caddy 无访问日志；debug/body 日志关闭；只备份数据库元数据且立即 age 加密 | 敏感字符串 canary 扫描 |
| Owner 上传的图片成为脚本或嗅探载体 | 只收 PNG/JPEG 且声明类型须与解码格式一致；解码后重新编码，丢弃 EXIF、注释和附加数据；以固定 `Content-Type`、`nosniff` 和 `default-src 'none'` 返回；前端只渲染同源路径图片、`https://` 链接（`noopener noreferrer`），文字一律 `textContent` | `site_test.go`、`site_settings_ui_test.cjs` 的 SVG、HTML 伪装、附加脚本、EXIF 与链接负例 |
| 匿名访问登录页接口放大数据库负载或获取收款码 | 交流群接口与群二维码使用 30 秒快照、图片按 SHA-256 缓存；收款码只在登录会话路由提供，关闭的卡片对非 Owner 返回 404 | 缓存读取计数、匿名与 Member 可见性测试 |
| Member 枚举其他用户用量 | 全员接口在查询前强制 Owner 角色，只返回按用户/模型聚合而非其他用户的请求级元数据 | Member 403 与 Owner 聚合测试 |
| Member 借群组用量接口探查上游账号或放大 ChatGPT 请求 | 默认关闭，由 Owner 按群组开启；账号集合只由会话用户的群组成员关系、“指定账号”池推出，请求不带账号 ID；“全部账号”池、Antigravity、已归档和已到期群组不展示；响应只含别名和用量窗口，不含账号 ID、邮箱、套餐和成本；Codex 读取按账号缓存 1 分钟并合并并发，与 Owner 额度查询共用 5 秒限流，实时查询写审计 | `group_account_usage_test.go`、两个 `group_account_usage*_integration_test.go` 的可见性、去标识与缓存测试 |
| 把 API Token 等价成本误当供应商实际账单 | 准入固化规则，ledger 不可变；界面/API 明示 ChatGPT/Antigravity 订阅、内部零价和兜底边界 | 计价、ledger 和文案测试 |
| 意外产生 Platform 费用 | 无 Platform Key、无自动回退；上游失效时 fail closed | 503/502 契约测试 |
| CPA 归因决定扣费层而误扣群组额度 | 准入固化每层账号池；结算只在包含该账号的预授权层中选择，账号缺失时回退第一层并告警；CPA 只能按层内账号选号，个人层仍受每用户资格回调约束 | `TestFundingLayersPostgresIntegration`、`TestFundingLayersGatewayPostgresIntegration`、CPA 分层选号测试 |

## 容器权限

Gateway 和 sidecar 使用 UID 10001；两者使用私有部署组、
只读根文件系统、`no-new-privileges` 和 drop-all capabilities。`cloudflared` 同样
以非 root、只读根文件系统和 drop-all capabilities 运行。Sidecar 只有 OAuth
volume、两个小型 `noexec` tmpfs 和加载插件所需的可执行 tmpfs 可写，OAuth 文件 umask
是 077。Gateway 只有私有 `/tmp` tmpfs
可写，并以 4 个并发槽限制完整请求体的转发前扫描文件。Caddy 仅保留绑定内部
低端口所需的 `NET_BIND_SERVICE`。PostgreSQL 和 Squid 保留各自官方镜像启动
所需权限，但没有公网端口，且位于最小网络集合中。

Gateway 访问 CPA 使用两把与用户 Key 不同、彼此不同的内部密钥：生成请求使用数据面
密钥，插件的账号路由使用 CPA 管理密钥。Gateway 在转发前丢弃用户 Authorization 并设置
对应 Bearer；没有管理密钥时不调用任何账号路由。账号路由位于私有兼容网络，不接受任意
上游请求参数。任何 OAuth 文件下载接口都不经 Caddy 路由。

### CPA 插件模式（默认）

sidecar 运行未修改的 CLIProxyAPI，Gateway 的账号分配、身份隔离和账号控制由镜像内的插件
`codex-gateway-v1.0.0.so` 执行。源码补丁镜像与旧 Antigravity 桥接已删除，不再有其他
部署形态。与已删除的补丁镜像相比，信任边界有以下变化：

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
  和账号隔离各 Provider 缓存。插件接触不到这些代码，重定向与主机的控制改由出口白名单
  承担。完整差异见[兼容层升级规程](compatibility-upgrades.md#与已删除的补丁镜像的行为差异)。
- **调用方之间的隔离依靠 CPA 原生的调用方作用域。** 插件把 Gateway 为每个 API Key 生成的
  作用域作为 principal 交给 CPA，没有作用域的生成请求不选号。CPA 推导的会话身份和按会话
  保存的重放状态都按它隔离，但不按账号隔离；客户端自带的会话标识原样发给上游。逐项核对
  发现两处可达的缓存的键不含调用方（Gemini Responses 文本签名、Claude OAuth 工具别名）：
  知道另一调用方的上游消息 ID 才能利用，得到的是签名或工具名而不是对话内容，在上游修复前
  接受。见 [CPA Provider 缓存键核对](cpa-provider-cache-audit.md)。
- **CPA 自带的数据面端点比 Gateway 用到的多。** 例如 v8.0.69 新增的 `/v1/tunnel`
  （多租户 WebSocket 数据交换）和 v8.0.76 新增的 `/muse-code/*`（Muse Code CLI 的模型列表
  及转发到 Meta 的辅助端点）无条件挂载。它们只接受 CPA 的 `api-keys`，即 Gateway 的内部
  数据面密钥，只在私有兼容网络可达；Gateway 只把固定的几条路径转发给 CPA，不转发这类端点。
  部署中没有 Meta 凭证，出口白名单也不含 Meta 主机，`/muse-code` 的转发无法成功。
- **Antigravity 账号只服务五个已审模型。** CPA 为该 Provider 内置的目录还包含 Claude、
  GPT 和图像模型；入口脚本用 CPA 原生的排除规则去掉它们，镜像构建时对照二进制内置的目录
  核对。Antigravity 凭证因此不会成为其他 Provider 模型的候选。

## 数据生命周期

- 请求明细元数据保留 90 天，日/月聚合及不含内容的不可变账务 ledger 长期保留；
  明细过期后仍可按 ledger 对账。
- 安全审计保留 365 天。
- 请求和响应正文不落库，因此不进入 `pg_dump`。
- 站点设置的文字和重新编码后的图片（含收款码）存于数据库，随加密备份保留；
  删除图片即从当前库移除，旧备份按备份保留期过期。
- OAuth volume 永不备份；灾备后在“CPA 授权管理”页面重新授权。
- PostgreSQL 每日 03:00 UTC 生成本地 age 密文，保留最近 14 组；每月至少在
  无网络临时容器中恢复演练一次，升级前追加一次。
- 停用的 API Key 立即拒绝新认证；永久删除移除活动凭证及密文，但数据库保留不含
  秘密的最小历史引用，以维持既有用量、账务、配额和审计完整性。
- 商城成交记录（`marketplace_sales`）跟随信息清理：早于截止时间、且买家与卖家两条流水都已清理的
  成交记录一并删除。成员删除凭证后，账号记录（不含令牌）为用量历史保留；成员被永久删除后，这些
  记录清空主人并标记 `owner_deleted_at`。

## 残余风险

- ChatGPT Pro 登录不是官方通用服务端 API，CLIProxyAPI 可能因上游协议改变而
  失效。控制是固定兼容版本、契约测试、人工冒烟和明确 503，而非静默回退。
- Antigravity 经 CPA 原生的 Google OAuth 与 Cloud Code 路径提供；Google 出口域名基线仍须
  用真实账号流量验收，假供应商测试不能视为生产可用证明。
- 公网 TLS 在 Cloudflare Edge 终止，Cloudflare 及转发链路上的
  cloudflared/Caddy/Gateway/sidecar 可见必要的瞬时内容。主机 root、Docker
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
  可写 tmpfs；CPA 执行器内的重定向与主机限制不再存在，只由出口白名单约束。源码补丁镜像
  已在真实账号验收（路线图第 3 步）之前删除，不再有可切换的回退镜像：插件或 CPA 出现
  问题时只能回到上一个 revision 整体重新部署。
- 充值完全人工：网关不核验付款，也不把付款与账户自动对应。Owner 会话被攻破后
  可替换收款码，引导用户付款给他人；写入要求近期二次验证，每次替换记录
  `site.image_updated` 审计及图片 SHA-256。收款码随数据库进入加密备份。

## 安全验收

上线前至少完成：HTTP/SSE fuzz、CSRF/Origin、WebAuthn challenge 重放、路径
穿越、SSRF、请求走私、伪造 XFF、无效 Key 限速、并发/日配额原子性、客户端
断开取消、上游 401/429/超时映射，以及带 canary 的日志、数据库和备份敏感
内容扫描。还需伪造 `CF-Connecting-IP`/`X-Forwarded-For` 验证来源 IP 边界，
并从公网确认服务器的 80、443、5432、8080、8317 和 3128 均不可达。
