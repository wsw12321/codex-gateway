# 我的供应商（租户供应商）

聚合网关阶段 2 的功能说明。成员可以登录或导入自己的上游账号、添加 API Key 供应商，并运行自己的出口服务器；Owner 可以停用成员账号、转移账号归属。

整个功能由商城开关控制，默认关闭。关闭时成员看不到这些页面，接口返回 404，线上行为与升级前一致。

> 阶段 3 起，成员的账号为成员自己服务（自用，见下文），也可以为卖家套餐的买家服务（阶段 4 起成员可以发布套餐，见[套餐商城](marketplace.md)）。成员账号永远不为平台资金（群组、平台套餐、现金）服务。

## 开启前提

- 部署已配置账号出口服务（`EGRESS_CONTROL_KEY`，Compose 默认配置）。没有账号出口时，`PUT /admin/market/settings` 拒绝打开开关（409 `marketplace_requires_account_egress`）。成员的登录要通过出口登记写入归属，所以账号出口是必需的。
- CPA 插件镜像已升级到支持凭据归属协议的版本（`credential_ownership_v1`）。旧插件下，成员的登录和导入返回 503 `tenant_providers_unavailable`。
- Owner 在 `/admin/market/settings` 设置：
  - `enabled`：商城开关；
  - `max_accounts_per_user`：每人账号上限（0–256，默认 20），OAuth 账号和 API Key 供应商合计，已删除的不计；
  - `max_egress_servers_per_user`：每人出口服务器上限（0–32，默认 5）；
  - `allow_member_suppliers`：是否允许成员添加 API Key 供应商（还需 `SUPPLIERS_ENABLED=true`）。

  这个接口要求提交全部字段和当前 `revision`。阶段 4 起可以在“管理 › 商城管理”页修改，还可以设置抽成比例和每人套餐上限。

## 成员

侧栏“我的供应商”分组有两个页面。

### 我的供应商

- **添加账号**：先选协议（Codex、Claude、Antigravity）和出口（全局出口或自己的出口服务器），然后：
  - **授权登录**：打开供应商授权页面，授权完成后浏览器会跳到一个打不开的本地地址（如 `http://localhost:1455/auth/callback?...`），把完整地址粘贴回来提交。页面每 2 秒查询一次结果，授权有效期 5 分钟，同时最多进行两个授权流程。
  - **导入凭据**：粘贴 JSON 凭据。只提交其中的令牌：Codex 需要 `refresh_token`（也接受 Codex CLI 的 `auth.json`），Claude 接受 `claudeAiOauth` 格式或 `refresh_token`/`access_token`。Antigravity 只能授权登录。
  - **添加 API Key 供应商**：填写兼容所选协议的 HTTPS Base URL 和 API Key。供应商没有模型列表时，要勾选它支持的模型。
- **账号卡片**：启用/停用、刷新凭据、查看额度、删除，以及在“账号设置”中修改别名、并发上限和出口。
- **被拒绝的情况**：
  - 该上游账号已归平台或其他成员所有：拒绝，提示“已归平台或其他用户所有”。这次登录新保存的凭据会被删除；原主人的账号不受影响，也不会被停用。
  - 达到账号上限：拒绝。
  - 该账号已释放，但仍在平台的群组账号池、专属授权或用户账号权限名单里：拒绝。
- **被管理员停用**：卡片显示“管理员已停用这个账号”，成员不能自行启用。
- **删除**：先停用并等待在途请求结束，再删除 CPA 中的凭据，然后释放归属（见下文）。

### 自用

- 商城开关打开后，成员自己的账号是每个请求的第一个扣费层：请求优先由自己的账号服务，只记录用量，不扣费、不记未覆盖。账务流水显示“自用：使用自己的供应商，不扣费”。
- 自己的账号满载、额度保护暂停或不支持该模型时，请求依次落到群组、指定供应商的订阅和个人余额，由平台账号服务并照常扣费。
- 只有自用时也能发起请求：没有余额和订阅的成员，可以只用自己的账号。
- “额度与订阅”页的“自用”开关可以关闭（`PUT /admin/billing/me/self-use`，`{"enabled": false}`），关闭后自己的账号不再为自己服务；从下一次请求起生效。
- Owner 设置的模型权限照样适用于自用请求。
- 被管理员停用、已删除（释放）的账号不参与自用。自用的 OAuth 账号同样受额度保护：开始服务后定时检查，额度不足时暂停使用。

### 我的出口

与 Owner 的出口服务器页面相同（Shadowsocks、SOCKS5、连通性测试），但只显示自己的节点，也只能绑定自己的账号。

- 服务器地址必须是**公网 IPv4 地址**，不接受域名、私有地址、回环、CGNAT（100.64.0.0/10）和文档地址段。域名可能在任何时候解析到部署内部或宿主机网络，所以不接受。
- 数量受 `max_egress_servers_per_user` 限制。
- 不选出口的账号使用全局出口。绑定节点故障时不会回退全局出口或直连。

## Owner

- **租户供应商**页面（`GET /admin/tenant-providers`）：列出全部成员账号（成员、协议、状态、出口）和成员出口服务器。
- **停用 / 恢复**（`PUT /admin/tenant-providers/{id}/suspension`，`{"suspended": true}`）：Owner 的审核停用，成员不能解除。
- **删除成员出口服务器**（`DELETE /admin/tenant-providers/egress-servers/{id}`，请求体 `{}`）：用于成员已停用、不能自己删除，或永久删除成员之前。规则与成员自己删除相同：仍被账号绑定、或被 24 小时内未完成的登录选择的节点返回 409 `tenant_egress_in_use`，平台节点返回 404。列表标出已停用的成员。
- **转移归属**（`PUT /admin/{upstream|antigravity|anthropic}-accounts/{id}/owner`，`{"owner_user_id": "<成员 ID>" 或 null, "expected_owner_user_id": "<当前主人 ID>" 或 null}`）：
  - 只有 Owner 能修改归属；目标必须是正常状态的成员，`null` 表示转为平台账号。
  - `expected_owner_user_id` 与当前主人不符时返回 409，防止旧页面覆盖。
  - 平台账号要先移出群组账号池、专属授权和用户账号权限名单，才能转给成员（409 `account_in_platform_use`）。
  - 账号原先绑定的出口节点若不属于新主人，改回全局出口。
  - 界面入口：租户供应商页的“转移归属”，以及上游账号卡片“账号详情与设置”中的“转给成员…”。
- Owner 的上游账号页、CPA 授权管理页、出口服务器页只管理平台账号和平台节点；对成员账号的修改返回“不存在”。
- **套餐供应商范围**（阶段 3）：套餐页每张卡片有“供应商范围”，可设为“不限制”（全部平台账号，默认）或“指定”（只用所选平台账号）。指定范围的订阅单独成为一个扣费层，只为所选账号付费，额度不足的部分再由同样包含该账号的后续层（个人余额）支付。修改从下一次请求起生效，不提升套餐版本，不影响续费。接口：`GET /admin/billing/plan-provider-scopes`、`PUT /admin/billing/plans/{id}/provider-scope`（`{"mode": "all"|"selected", "account_ids": [...], "revision": n}`）。

## 归属规则

| 情况 | 结果 |
|---|---|
| 登录或导入到一个新账号 | 归发起人（成员或平台） |
| 发起人重新授权自己的账号 | 允许 |
| 账号已归其他成员或平台 | 拒绝（409 `account_owned_elsewhere`），原账号不受影响 |
| 账号已删除（已释放） | 被这次登录认领，归发起人 |
| 删除凭据 | 释放归属：账号不再服务任何请求，下一次登录同一上游账号的人可以认领 |
| Owner 转移 | 归新主人 |

释放的账号保留原主人记录，用于历史用量；它不计入账号上限，也不出现在成员的列表里。

成员被永久删除时（阶段 5，迁移 `0055`），其已释放账号清空主人并标记 `owner_deleted_at`：账号仍不服务请求，
不出现在任何人（包括 Owner 的平台账号）的列表里，下一次登录同一上游账号时按认领处理。成员仍持有凭据的账号
和出口服务器会阻止删除，要先删除或由 Owner 转给平台，见[运维手册](operations.md#成员的停用与永久删除0055)。

商城开关打开期间，Gateway 不再根据 CPA 的账号列表自动创建账号记录：账号只能通过出口登记（登录、导入）进入数据库。这样，成员中途放弃的登录不会变成平台账号。Owner 添加平台账号也要通过管理台的授权登录或导入；直接放进 CPA 凭据目录的文件，在开关打开期间不会被使用。

## 审计

- `tenant_provider.operation`：成员的每次写操作（路由、方法、协议、HTTP 状态、账号 ID）。
- `upstream_account.owner_changed`：认领（`claim`）或转移（`transfer`），含前后主人。
- `upstream_account.released`：删除后释放。
- `upstream_account.suspended` / `upstream_account.unsuspended`。
- `gateway.marketplace_settings_changed`：商城设置变更。
- `billing.plan_provider_scope_changed`：套餐供应商范围变更（前后模式、账号数、版本）。
- `billing.self_use_changed`：成员开关自用。
