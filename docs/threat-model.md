# 威胁模型

成员可使用可发现 Passkey 或用户名密码登录。密码以无 pepper 的 Argon2id PHC
保存（64 MiB、3 轮、并行度 2、随机 16 字节 salt、32 字节输出），PHC 在分配
资源前执行严格参数上限检查；哈希最多两个并发槽。未知用户、无密码、禁用用户
和错误密码统一执行同成本验证并返回 `invalid_credentials`，并受每 IP/路径
20 次/分钟及 IP + 规范化用户名摘要 5 次/分钟限制。

恢复码只在新凭据、恢复码轮换和新会话能够原子提交时消费。恢复撤销旧会话但保留未选择的既有登录方式，也不自动停用或删除 API Key。设置或更改密码保留当前会话及其近期验证时间，撤销其他会话。

## 范围和安全目标

受保护资产包括 ChatGPT Pro OAuth/refresh token、设备 API Key、独立 API Key
加密密钥、Passkey 公钥与 challenge、恢复码、会话、数据库凭证、配额数据和
安全审计。提示词、源代码和
模型回复属于高敏感瞬时数据：可以在转发内存或受限 tmpfs 中短暂存在，但不得
进入数据库、日志、备份或管理界面。按用户聚合的用量和
“OpenAI API Token 等价成本”也属于受限管理元数据，Member 只能查看自己的
请求明细，只有 Owner 能查看全员汇总。

部署假设账号和所有受邀设备都由同一订阅者控制。若让其他真实用户共用 Pro
凭证，本威胁模型和订阅边界不再成立，必须改为每人独立上游凭证或官方
Platform API。

## 信任边界

```text
Internet
  -> Cloudflare Edge (public TLS)
    -> Cloudflare Tunnel (outbound connector)
      -> Caddy (edge_internal only)
        -> Gateway (edge_internal + data_internal + compat_internal)
          -> PostgreSQL (data_internal only)
          -> CLIProxyAPI (compat_internal only)
            -> Squid allowlist (compat_internal + egress_external)
              -> auth.openai.com:443 / chatgpt.com:443
```

没有容器发布宿主机端口。`cloudflared` 只连接 Tunnel 出站网络和边缘内部网络，
并把 Dashboard 中的 hostname 转给 `http://caddy:80`。其余带 `internal: true`
的网络没有默认互联网路由。Sidecar 即使尝试绕过 `HTTP(S)_PROXY` 也没有直接
出口；Squid 拒绝非 CONNECT、非 443 和不在精确域名列表中的目的地。

## 主要威胁与控制

| 威胁 | 控制 | 验证方式 |
| --- | --- | --- |
| 扫描服务器公网 IP 绕过 Cloudflare | 所有服务零宿主端口；安全组只允许固定管理 IP 的 SSH | `validate-compose.sh`、外部端口扫描 |
| Tunnel token 泄漏 | Dashboard token 仅存 `0640` secret，以 `--token-file` 挂载给非 root、只读的 connector | 文件/mount/进程参数和日志检查 |
| API Key 数据库泄漏 | HMAC 用于认证；新 Key 的版本化 AES-256-GCM 密文使用仅挂载给 Gateway 的独立密钥，AAD 绑定用户和 Public ID，查看要求近期二次验证 | 加密篡改/AAD 测试、管理响应和数据库检查 |
| OAuth 被主服务或备份读取 | OAuth 只挂载到非 root sidecar；不挂载 Gateway/备份任务 | Compose mount 审计、灾备演练 |
| Refresh token 并发复用 | 登录/升级锁；先停唯一实例；禁止共享卷的双实例 | 容器状态检查、运维演练 |
| OAuth 文件权限放宽或 symlink | 启动时要求 UID 10001、regular file、精确 `0600` | `verify-oauth-permissions.sh` |
| Sidecar 任意出网/SSRF | internal 网络加 Squid 精确域名和 443 allowlist | 代理 ACL 测试、网络 namespace 测试 |
| 请求头走私凭证 | Gateway 只接受已知路径/头，替换 Authorization，移除 Cookie、转发头及 hop-by-hop 头 | 代理和 fuzz 测试 |
| 超大正文/资源耗尽 | Caddy 与 Gateway 双重 64 MiB 上限；RPM、并发、日配额和全局流限制 | 限额与并发测试 |
| 邀请/恢复 token 泄漏 | URL fragment、单次/短期 token、HMAC 存储；不启用访问日志 | 邀请复用测试、日志扫描 |
| 会话劫持/CSRF | Secure、HttpOnly、SameSite=Strict；Origin/CSRF 校验；敏感操作 5 分钟内 Passkey 再验证 | 身份安全测试 |
| 伪造来源 IP 绕过限速 | origin 仅 Tunnel 可达；Caddy 与 Gateway 只信任各自上游的固定 `/32`，并重建 XFF | CF/XFF 伪造测试 |
| 供应链 tag 漂移 | 基础镜像 manifest digest；CLIProxy tag 与 full commit 双校验；固定 CI 工具版本 | CI 和 lock diff 审阅 |
| 明文内容进入日志/备份 | Caddy 无访问日志；debug/body 日志关闭；只备份数据库元数据且立即 age 加密 | 敏感字符串 canary 扫描 |
| Member 枚举其他用户用量 | 全员接口在查询前强制 Owner 角色，只返回按用户/模型聚合而非其他用户的请求级元数据 | Member 403 与 Owner 聚合测试 |
| 把 OpenAI API Token 等价成本误当 OpenAI 实际账单 | 准入固化 v2 规则，ledger 不可变；界面/API 明示 Pro OAuth、内部零价和兜底边界 | v2 计价、ledger 和文案测试 |
| 意外产生 Platform 费用 | 无 Platform Key、无自动回退；上游失效时 fail closed | 503/502 契约测试 |

## 容器权限

Gateway 和 sidecar 使用 UID 10001、私有部署组、只读根文件系统、
`no-new-privileges` 和 drop-all capabilities。`cloudflared` 同样以非 root、
只读根文件系统和 drop-all capabilities 运行。Sidecar 只有 OAuth volume 和
两个小型 tmpfs 可写，OAuth 文件 umask 是 077。Gateway 只有私有 `/tmp` tmpfs
可写，并以 4 个并发槽限制完整请求体的转发前扫描文件。Caddy 仅保留绑定内部
低端口所需的 `NET_BIND_SERVICE`。PostgreSQL 和 Squid 保留各自官方镜像启动
所需权限，但没有公网端口，且位于最小网络集合中。

内部 sidecar API Key 与用户 Key 完全不同。Gateway 在转发前丢弃用户
Authorization，设置内部 Bearer；sidecar 管理 API 禁止 remote access 且控制
面板关闭。任何 OAuth 文件下载接口都不经 Caddy 路由。

## 数据生命周期

- 请求明细元数据保留 90 天，日/月聚合及不含内容的不可变账务 ledger 长期保留；
  明细过期后仍可按 ledger 对账。
- 安全审计保留 365 天。
- 请求和响应正文不落库，因此不进入 `pg_dump`。
- OAuth volume 永不备份；灾备后重新登录。
- PostgreSQL 每日 03:00 UTC 生成本地 age 密文，保留最近 14 组；每月至少在
  无网络临时容器中恢复演练一次，升级前追加一次。
- 停用的 API Key 立即拒绝新认证；永久删除移除活动凭证及密文，但数据库保留不含
  秘密的最小历史引用，以维持既有用量、账务、配额和审计完整性。

## 残余风险

- ChatGPT Pro 登录不是官方通用服务端 API，CLIProxyAPI 可能因上游协议改变而
  失效。控制是固定兼容版本、契约测试、人工冒烟和明确 503，而非静默回退。
- 公网 TLS 在 Cloudflare Edge 终止，Cloudflare 及转发链路上的
  cloudflared/Caddy/Gateway/sidecar 可见必要的瞬时内容。主机 root、Docker
  daemon 或内核被攻破后无法靠容器边界保密。
- 精确域名 allowlist 仍信任这些域名的 DNS、证书和服务端。Squid 不做 TLS
  解密，因此不能检查加密路径，但也不会看到 OAuth 或提示词。
- 单服务器是可用性单点。本地 age 密文、解密 identity 和其他恢复 secret 会随
  服务器或云盘整体丢失，因而只能处理数据库逻辑损坏和计划迁机，既不能提供
  无中断故障转移，也不构成整机灾备。
- 本版本不支持 API Key 加密密钥轮换。数据库备份不包含该部署 secret；密钥丢失或
  被替换后，HMAC 认证资料仍无法用于还原明文，新 Key 的查看操作会失败。
- Pro 账号自身配额和服务限制不可由 Gateway 保证；应 fail closed 并告警。

## 安全验收

上线前至少完成：HTTP/SSE fuzz、CSRF/Origin、WebAuthn challenge 重放、路径
穿越、SSRF、请求走私、伪造 XFF、无效 Key 限速、并发/日配额原子性、客户端
断开取消、上游 401/429/超时映射，以及带 canary 的日志、数据库和备份敏感
内容扫描。还需伪造 `CF-Connecting-IP`/`X-Forwarded-For` 验证来源 IP 边界，
并从公网确认服务器的 80、443、5432、8080、8317 和 3128 均不可达。
