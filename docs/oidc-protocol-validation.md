# OIDC 协议验证记录

验证日期：2026-10-03。此次验证只读取账号站仓库中的公开配置和远程公开 discovery/JWKS，不使用管理令牌，不创建客户端、用户或授权，不修改 Supabase 项目。

## 真实项目公开端点

账号站为 `https://auth.water555.com`，配置的 issuer 为：

```text
https://hqsbxndtzyspkvoaxvid.supabase.co/auth/v1
```

读取下列 HTTPS 端点并校验 JSON：

```sh
curl --fail --silent --show-error --max-time 15 \
  'https://hqsbxndtzyspkvoaxvid.supabase.co/auth/v1/.well-known/openid-configuration'
curl --fail --silent --show-error --max-time 15 \
  'https://hqsbxndtzyspkvoaxvid.supabase.co/auth/v1/.well-known/jwks.json'
```

以上命令是只读公开元数据检查，不含客户端密钥、授权码或用户令牌。生产网关内部的请求仍须经过专用 Squid 代理；开发机直接读取公开端点不能证明容器出口规则已验证。

| 检查 | 2026-10-03 实际结果 | 网关兼容性 |
| --- | --- | --- |
| issuer | 与上述配置精确一致 | 固定 issuer，拒绝令牌或发现文档中的其他 issuer |
| authorization endpoint | issuer 下 `/oauth/authorize` | 同一 HTTPS 主机和 `/auth/v1/` 路径 |
| token endpoint | issuer 下 `/oauth/token` | 同一 HTTPS 主机和 `/auth/v1/` 路径 |
| JWKS URI | issuer 下 `/.well-known/jwks.json` | 同一 HTTPS 主机和 `/auth/v1/` 路径 |
| 签名算法声明 | `RS256`、`HS256`、`ES256` | 网关仅接受 `RS256` 和 `ES256`；不会因发现文档而允许 `HS256` |
| 当前公开 JWKS | 1 枚 `EC`、`ES256`、`use=sig` 密钥 | 与允许的算法兼容；本地测试覆盖未知 kid 后的密钥刷新 |
| 客户端认证方法 | `client_secret_basic`、`client_secret_post`、`none` | 网关固定使用 `client_secret_basic`，登记 confidential 客户端时须选择该方法 |
| PKCE 方法 | `S256`、`plain` | 网关只发起 `S256` |

## 自动化覆盖与验收边界

Go 协议回归使用受控 HTTP transport 和真实 RS256/ES256 签名，覆盖授权码请求参数、S256、nonce、issuer、audience、签名/算法、azp、iat/exp/nbf、hash claims、密钥轮换、失败换码不重试、并发发现缓存、错误信息脱敏，以及出站目标、路径、重定向、响应大小和超时限制。运行：

```sh
go test -count=1 ./internal/config ./internal/identity
```

账号站仓库未提供已登记的 confidential 测试客户端 ID/secret、真实测试账号凭据或独立测试项目；其验收记录也说明完整授权码流程尚未验收。本次未进行真实 Supabase 授权链路中的授权、换码、用户绑定、统一登录或 Strict Cookie 跨站回调测试。浏览器的受控端到端检查另见 [浏览器验证记录](oidc-browser-validation.md)。公开 discovery/JWKS 可读且兼容，不能作为生产启用依据。

用户已确认暂无独立测试配置，并同意本轮维持默认关闭、延后真实授权码联调；这是当前明确的验收边界，未把公开元数据或模拟测试视为完整上线验收。

后续完整联调还需确定隔离测试项目或获准使用的临时客户端与测试账号，登记精确的网关 `/auth/oidc/callback`，通过账号站实际授权页面完成同意/拒绝、首次选择与取消、零余额普通账号开通、绑定已有账号、后续直接登录、SSO 再验证、解绑撤销和账号切换测试，并验证容器专用代理出口。首次开通与再验证扩展不改变已有 issuer、签名、换码和出口校验；可选 `OIDC_AUTHORIZATION_URL` 只改变部署指定的浏览器授权入口。生产开关保持原值；本记录不代表这些新增流程已完成真实客户端验收。

## 真实 Compose 与镜像解析

2026-10-04 首次开通与再验证扩展已另行完成 Docker 补验：默认关闭、原生授权
地址、浏览器代理授权地址三种配置的完整 validator 均通过，并通过运行中的
OIDC 出口隔离和 Caddy 日志脱敏检查。详见 [本次验证记录](oidc-sso-validation.md#docker-补充验收2026-10-04)。

2026-10-03 在一次性部署目录中复制当前 Compose、部署配置与校验脚本，使用保留域名 `oidc-parse.ci.invalid`、假的客户端 ID 和独立生成的占位密钥文件；未复制工作目录的 `.env` 或真实 secrets。分别对默认关闭模式和 `--oidc` 开启模式运行完整 `scripts/validate-compose.sh`，均通过。

此次执行使用真实 Docker daemon 和仓库 digest 锁定的 Squid/Caddy 镜像，全部解析容器均为 `--network none`、只读根文件系统；检查了 A 端直连/中继/OIDC 规则、B 端中继配置和 Caddy 配置。没有启动项目服务、分配项目静态 IP 或访问生产凭据。此结果验证 Compose 安全约束和真实代理配置语法；运行中的代理目的地隔离与真实 Supabase 授权仍需部署环境验收。

## 回调参数日志脱敏

使用锁定的 Caddy 2.10.2 镜像、`--network none` 和不可连接的模拟上游，复现到默认运行时 `http.log.error` 在 502 时会记录完整 `request.uri`，即使未开启 access log。已在全局默认日志编码器中移除请求 URI 与 Referer 的全部查询部分，保留路径、状态码和错误诊断信息。

真实镜像回归确认：模拟回调的 code、state、error_description 和 Referer 查询均不会出现在错误日志；Cookie/Authorization 的默认凭据脱敏仍生效；502 错误日志仍存在且请求路径为 `/auth/oidc/callback`。执行方式（需要 Docker 与已缓存的锁定 Caddy 镜像，不拉取新镜像）：

```sh
RUN_DOCKER_INTEGRATION=1 python3 -m unittest scripts/tests/test_caddy_oidc_logs.py
```

完整 Compose 校验也要求保留该查询脱敏配置。外部 Cloudflare 或额外接入层仍应遵循 [部署说明](oidc.md) 中不采集回调完整 URI 的要求。
