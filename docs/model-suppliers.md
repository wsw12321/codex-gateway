# API Key 模型供应商

在 A 的 `.env` 里设置 `SUPPLIERS_ENABLED=true`，按现有流程更新 Gateway 和 `account-egress`。全局出口是 Shadowsocks 时也要更新 `ss-egress`。数据库迁移自动新增供应商表，不迁移官方账号或 OAuth 凭据。

Owner 完成近期身份验证后，在 `/admin/cpa/` 选择协议并添加 API Key 供应商。输入公网 HTTPS Base URL 和 API Key，可选出口节点；不选则继承全局出口。Gateway 会用该出口查询模型列表，只接纳当前网关配置的已定价模型。模型列表接口不支持或没有匹配项时，可手动选择已定价的模型；网络、鉴权、重定向错误不能以手动选择绕过。未提供列表的供应商需发起一次小额调用，确认所选模型和 Key 可用。

| 账号池 | 供应商接口 |
|---|---|
| Codex | `/v1/models`、`/v1/responses` |
| Claude | `/v1/models`、`/v1/messages`、`/v1/messages/count_tokens` |
| Gemini | `/v1beta/models`、`/v1beta/models/{model}:generateContent` 或 `:streamGenerateContent` |

Base URL 可包含供应商的路径前缀，例如 `https://api.example.com/compatible/v1`。同一供应商仅用于相应原生协议，不参与跨协议转换、WebSocket 或 Compact。`/v1/models` 只从 Codex 供应商追加响应接口可用的模型；仍按用户和 API Key 权限过滤。

供应商与官方账号共用账单、权限、分组、权重、并发限制和出口绑定。Key 加密保存，换 Key 保留账号索引；删除先停用、等待进行中的请求，然后删除凭据和出口绑定，保留历史账单和权限记录。API Key 供应商没有官方订阅额度查询。并发、短期会话与冷却状态由单个 Gateway 实例维护；不要以多副本部署此功能。

Gateway 核验并转发供应商请求，不向 CPA 传送供应商 URL 或凭据。官方账号仍使用原有 CPA 路径。混合池使用 CPA 现有账号列表和权限接口，官方账号的实时模型可用性仍由 CPA 在每次派发时确认；CPA 管理接口不可用时不会阻断已配置的供应商。已开始输出的请求不会切换账号。

## 出口

- `direct`：Gateway 通过隔离的 `account-egress` 直连公网。仅允许 HTTPS 443，解析结果必须全部是公网地址，建立连接时使用已核验的 IP，TLS 仍以原域名核验，不跟随重定向。
- `shadowsocks` 或绑定账号节点：请求仍按现有出口走代理。Gateway 的出口服务先核验域名及解析结果，再将固定的目标 IP 交给代理；代理不可用时不回退到直连。
- `relay`：已有 B Squid 继续服务官方账号，API Key 供应商另走 `10.77.0.2:3130` 的认证 CONNECT 服务。这只支持部署文档中的 WireGuard 地址 `10.77.0.1`（A）和 `10.77.0.2`（B）。B 必须先加载 `deploy/relay/supplier.override.yml`，配合原 `deploy/relay/docker-compose.yml` 启动；使用同版本已构建的 `account-egress` 镜像（通过 `SUPPLIER_RELAY_IMAGE` 指定），仅允许来自 A 的 WireGuard 流量访问 TCP 3130。A、B 均以 `SUPPLIER_RELAY_KEY_FILE` 指向同一份独立的随机密钥（建议 `openssl rand -hex 32`，至少 32 字节，不含空白），文件权限 `0640`，组为各自的服务组；B 的组可用 `SUPPLIER_RELAY_SECRET_GID` 指定。A 的 `scripts/compose.sh` 会自动加载供应商中转配置。

回滚时设 `SUPPLIERS_ENABLED=false` 并重启 Gateway。已有凭据和历史不会被清除，官方账号的路由恢复为原流程。
