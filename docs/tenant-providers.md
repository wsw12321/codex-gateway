# 我的供应商（租户供应商）

聚合网关阶段 2 的功能说明。成员可以登录或导入自己的上游账号、添加 API Key 供应商，并运行自己的出口服务器；Owner 可以停用成员账号、转移账号归属。

整个功能由商城开关控制，默认关闭。关闭时成员看不到这些页面，接口返回 404，线上行为与升级前一致。

> 现阶段只做“添加与管理”：成员的账号还不服务任何请求。自用和套餐（阶段 3）开放后，这些账号才会被使用。

## 开启前提

- 部署已配置账号出口服务（`EGRESS_CONTROL_KEY`，Compose 默认配置）。没有账号出口时，`PUT /admin/market/settings` 拒绝打开开关（409 `marketplace_requires_account_egress`）。成员的登录要通过出口登记写入归属，所以账号出口是必需的。
- CPA 插件镜像已升级到支持凭据归属协议的版本（`credential_ownership_v1`）。旧插件下，成员的登录和导入返回 503 `tenant_providers_unavailable`。
- Owner 在 `/admin/market/settings` 设置：
  - `enabled`：商城开关；
  - `max_accounts_per_user`：每人账号上限（0–256，默认 20），OAuth 账号和 API Key 供应商合计，已删除的不计；
  - `max_egress_servers_per_user`：每人出口服务器上限（0–32，默认 5）；
  - `allow_member_suppliers`：是否允许成员添加 API Key 供应商（还需 `SUPPLIERS_ENABLED=true`）。

  这个接口要求提交全部字段和当前 `revision`，目前还没有界面（阶段 4 加入）。

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

### 我的出口

与 Owner 的出口服务器页面相同（Shadowsocks、SOCKS5、连通性测试），但只显示自己的节点，也只能绑定自己的账号。

- 服务器地址必须是**公网 IPv4 地址**，不接受域名、私有地址、回环、CGNAT（100.64.0.0/10）和文档地址段。域名可能在任何时候解析到部署内部或宿主机网络，所以不接受。
- 数量受 `max_egress_servers_per_user` 限制。
- 不选出口的账号使用全局出口。绑定节点故障时不会回退全局出口或直连。

## Owner

- **租户供应商**页面（`GET /admin/tenant-providers`）：列出全部成员账号（成员、协议、状态、出口）和成员出口服务器。
- **停用 / 恢复**（`PUT /admin/tenant-providers/{id}/suspension`，`{"suspended": true}`）：Owner 的审核停用，成员不能解除。
- **转移归属**（`PUT /admin/{upstream|antigravity|anthropic}-accounts/{id}/owner`，`{"owner_user_id": "<成员 ID>" 或 null, "expected_owner_user_id": "<当前主人 ID>" 或 null}`）：
  - 只有 Owner 能修改归属；目标必须是正常状态的成员，`null` 表示转为平台账号。
  - `expected_owner_user_id` 与当前主人不符时返回 409，防止旧页面覆盖。
  - 平台账号要先移出群组账号池、专属授权和用户账号权限名单，才能转给成员（409 `account_in_platform_use`）。
  - 账号原先绑定的出口节点若不属于新主人，改回全局出口。
  - 界面入口：租户供应商页的“转移归属”，以及上游账号卡片“账号详情与设置”中的“转给成员…”。
- Owner 的上游账号页、CPA 授权管理页、出口服务器页只管理平台账号和平台节点；对成员账号的修改返回“不存在”。

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

商城开关打开期间，Gateway 不再根据 CPA 的账号列表自动创建账号记录：账号只能通过出口登记（登录、导入）进入数据库。这样，成员中途放弃的登录不会变成平台账号。Owner 添加平台账号也要通过管理台的授权登录或导入；直接放进 CPA 凭据目录的文件，在开关打开期间不会被使用。

## 审计

- `tenant_provider.operation`：成员的每次写操作（路由、方法、协议、HTTP 状态、账号 ID）。
- `upstream_account.owner_changed`：认领（`claim`）或转移（`transfer`），含前后主人。
- `upstream_account.released`：删除后释放。
- `upstream_account.suspended` / `upstream_account.unsuspended`。
- `gateway.marketplace_settings_changed`：商城设置变更。
