# Claude Code 接入与账号管理

链路为 Claude Code API 模式 → Gateway → CPA → Claude 订阅 OAuth 账号。Gateway 的提供方 ID 为 `anthropic`，控制台显示“Claude 账号”；CPA 执行器内部名称 `claude` 不构成第二个 Gateway 提供方。

## 管理员准备

先部署数据库迁移、Gateway 和补丁后的 CPA，再在“CPA 授权管理”的“Claude 账号”页授权或导入。导入支持 CPA 的 `refresh_token` / `access_token` 字段，或 Claude Code 的 `claudeAiOauth.refreshToken` / `accessToken`。浏览器只提交必要令牌，不采用导入文件宣称的套餐、身份或额度。身份验证由 CPA 完成；组织不同视为不同账号。Claude 导入至少需要访问令牌或刷新令牌之一；存在刷新令牌时先刷新再验证身份。仅导入访问令牌的账号到期后不能自动刷新，需要重新授权或导入有效凭据。

在“上游账号 → Claude 账号”配置权重、并发和共享/专属权限。用户的 Claude 账号范围默认是“全部账号”；管理员可在“账号权限”选择指定账号，空名单表示全部禁用。账号范围仍受专属授权、模型权限、账号状态与并发限制约束。

管理员应为所需 Claude 精确模型 ID 配置价格并授权。`separate_by_ttl` 在模型定价页分别显示 5 分钟和 1 小时缓存写入价。账单保留总缓存写入以及实际观测的 TTL 分项；缺失分项时显示未知并按请求价格快照中的较高写入价结算，不推算分项。

“刷新状态”读取 CPA 最新被动观测快照，展示五小时/七天窗口、重置时间、冷却时间和观测时间。没有新上游响应时刷新不会改变观测时间；未知套餐和未观测到的额度不会显示为零或满额。冷却沿用上游自动恢复逻辑，手动启用不会跳过冷却。

## SSH 终端授权

已有 Docker 操作权限的服务器管理员可以在仓库目录运行以下任一入口，新增 Claude 账号或为已有账号重新授权：

```sh
./scripts/claude-login.sh
# 等价入口：
./scripts/oauth-login.sh claude
```

宿主机需要 Python 3、Docker Compose、`jq` 和 util-linux 的 `flock`，并已按现有 Compose 部署流程配置环境与 secrets。`codex-compat` 必须正在运行，且提供 `anthropic_messages_v1` 能力；脚本会先检查依赖、连通性与能力。它与现有登录、升级流程共用 `.device-login.lock`，使用正在运行的 CPA，不停服、不重建镜像、不创建第二个 CPA 实例，也不修改数据库。首版 SSH 入口仅支持浏览器授权，凭据导入仍使用控制台。

1. 在 SSH 终端启动脚本，将显示的 HTTPS `claude.ai` 授权链接复制到本地浏览器完成登录。
2. 浏览器跳转后，从地址栏复制**完整回调 URL**，回到 SSH 终端粘贴并按回车。回调必须是 HTTP 回环地址（`localhost`、`127.0.0.1` 或 `[::1]`）、端口 `54545`、路径 `/callback`，包含本次授权的 `code` 和 `state`。本地没有回调监听器时，浏览器可能显示连接失败；仍可复制地址栏中的完整 URL，无需转发端口。
3. 输入仅从 `/dev/tty` 读取且不回显，最长等待五分钟。不要把回调放进命令参数、环境变量或文件。无效输入可在剩余时间内重试；重复参数、state 不匹配、错误参数、片段和超限输入都会被拒绝。脚本只解析 URL，不访问回调地址。
4. 脚本最多提交一次授权码，然后每两秒查询状态，最多等待两分钟；每次内部请求最多等待十秒。只有 CPA 返回最终 `status=ok`，才表示身份验证及凭据保存完成。

成功后脚本检查 OAuth 文件的所有权与权限，并显示当前 Claude 账号的脱敏列表。状态接口不返回本次账号 ID，因此列表不标记哪个账号是本次新增或重新授权的账号。请到控制台核对账号状态、精确模型 ID 的价格，以及账号和模型授权。脚本不会自动发送付费生成请求，授权完成不代表已完成生成与账单验收。

管理密钥与内部 API Key 由容器从现有 secret 文件分别读取，不传回宿主机；授权码仅通过标准输入提交。脚本只调用固定的容器内部接口，不输出原始接口响应或容器错误输出。此入口面向可操作 Docker 的管理员，网页端原有 Owner 权限与近期验证要求不变。

| 情况 | 处理方式 |
| --- | --- |
| 依赖缺失、CPA 未运行或能力检查失败 | 按现有部署流程补齐依赖、启动或升级 CPA，确认就绪后重新运行。脚本不会替你停止或启动服务。 |
| 提示锁被占用 | 等待正在进行的登录或升级操作结束；不要删除锁文件绕过互斥。 |
| 回调被拒绝或输入超时 | 确认复制了当前浏览器授权流程的完整回调。五分钟内可重新输入；输入超时后重新运行会创建新的授权流程。 |
| 提交结果不明、状态查询失败或等待超时 | 授权仍可能完成；先到控制台核对账号状态，不要重复提交原回调或立即重试登录。脚本不会自动重发授权码。 |
| 显示“凭据已保存，检查未通过” | CPA 已完成保存，后续权限或账号列表检查失败。检查部署及 OAuth 文件权限，必要时运行 `./scripts/verify-oauth-permissions.sh`；不要删除账号或重新授权来掩盖检查失败。 |
| 按 Ctrl-C 或收到中断 | 脚本恢复终端设置并释放锁。提交前退出由 CPA 原有等待流程自行超时；提交后退出不会撤销授权，仍可能保存成功，需到控制台确认。 |

## 用户配置

按 [Claude Code 官方安装说明](https://code.claude.com/docs/en/setup) 安装客户端和 Node.js。在 Gateway 的“开始使用 → Claude Code”复制配置命令，按终端提示输入 Gateway API Key，再从 `/v1/models` 返回的目录中选择精确模型 ID。目录只应返回当前 Key 已授权、已定价且有可用账号的模型。

配置器原子合并用户级 `~/.claude/settings.json`（Windows 为 `%USERPROFILE%\.claude\settings.json`，设置 `CLAUDE_CONFIG_DIR` 时使用该目录）：

- `env.ANTHROPIC_BASE_URL` 使用当前站点 origin，不附加 `/v1`。
- `env.ANTHROPIC_AUTH_TOKEN` 保存 Gateway Key。
- `model` 使用所选模型的精确 ID。

无关设置保留，已有文件在修改前创建 `.bak-*` 私密备份，凭据文件权限为 `0600`。Key 由终端输入，不拼入命令参数或显示在结果中。配置器发现 `ANTHROPIC_API_KEY`、Claude OAuth token、云供应商开关、`apiKeyHelper`、冲突的继承认证变量或模型覆盖变量时停止写入，并列出变量名称。排除冲突后重新运行。项目级、组织管理配置与启动参数仍可能覆盖用户配置，需按 [官方设置优先级](https://code.claude.com/docs/en/settings) 检查。

已下载配置器的用户也可运行：

```sh
node configure-client.cjs claude https://gateway.example
```

若终端输入被管道接管且目录有多个模型，第三个参数必须指定目录中的精确模型 ID；Key 仍仅从标准输入读取。

退出旧 Claude Code 会话并重新运行 `claude`，发送一次简短请求后在“使用统计”核对模型、账号归因与缓存账单。网关协议参考 [LLM gateway protocol](https://code.claude.com/docs/en/llm-gateway-protocol)，缓存用量参考 [prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)。

## 界面与配置验证

```sh
node internal/server/testdata/configure_client_test.cjs
node internal/server/testdata/model_prices_ui_test.cjs
node internal/server/testdata/user_upstream_access_ui_test.cjs
node internal/server/testdata/overview_ui_test.cjs
sh -n scripts/claude-login.sh && sh -n scripts/oauth-login.sh
python3 -m unittest discover -s scripts/tests -p 'test_*.py' -v
./scripts/validate-cpa-panel.sh
PLAYWRIGHT_MODULE=/path/to/playwright PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chrome \
  node internal/server/testdata/anthropic_accounts_browser.cjs
PLAYWRIGHT_MODULE=/path/to/playwright PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chrome \
  node internal/server/testdata/cpa_admin_browser.cjs
```

以上浏览器验证完全拦截网络，使用合成账号；覆盖账号范围隔离、权重保存、未知与已观测额度、状态刷新后的快照保留、Claude 凭据字段提取、授权域名、移动端布局。配置测试覆盖认证冲突、未授权模型、原子合并、备份和不泄漏凭据。真实 OAuth、凭据刷新后重启、工具续轮、thinking、图片和账单归因仍需部署环境的账号验收。

SSH 脚本回归沿用 CI 的 Python 测试发现入口，使用模拟 CPA 和终端验证授权、重新授权、URL 校验、超时、异常响应、互斥、终端恢复与凭据保护，并检查实际 Shell／`nc` 传输的 HTTP 分帧及响应限制。自动化测试不执行真实账号登录。真实 SSH 授权验收需要已部署 Anthropic 支持的服务器、可用订阅账号和用户浏览器配合；本次脚本交付未执行此项验收，不能以模拟测试结果替代。

截图：[Claude 账号桌面](screenshots/anthropic-accounts-desktop.png)、[Claude 账号手机](screenshots/anthropic-accounts-mobile.png)、[配置指导手机](screenshots/claude-guide-mobile.png)、[CPA 桌面](screenshots/cpa-anthropic-desktop.png)、[CPA 手机](screenshots/cpa-anthropic-mobile.png)。

## 标准 API 定价与目录升级

默认目录 [`deploy/pricing-v2.example.json`](../deploy/pricing-v2.example.json) 和两份环境变量示例已包含以下四款精确模型 ID。价格于 **2026-10-08** 核对 [Anthropic 官方 API 定价](https://platform.claude.com/docs/en/about-claude/pricing)，单位为美元／百万 tokens：

| 精确模型 ID | 输入 | 输出 | 缓存读取 | 5 分钟缓存写入 | 1 小时缓存写入 |
| --- | ---: | ---: | ---: | ---: | ---: |
| `claude-fable-5-1` | 10 | 50 | 0.25 | 12.50 | 20 |
| `claude-opus-5-5` | 4 | 20 | 0.20 | 5 | 8 |
| `claude-sonnet-5-5` | 2 | 10 | 0.10 | 2.50 | 4 |
| `claude-haiku-4-5-20251001` | 1 | 5 | 0.10 | 1.25 | 2 |

模型 ID 和上下文限制对应当前 [CPA 来源锁](../deploy/codex-compat/source.lock.json) 中的 `v8.0.20` 目录（commit `0f96f568e4dbf6f84ad7399a74b78344c5eac7e6`）。Haiku 5.5 尚未进入该锁定目录，本次不加入，也不使用别名或相近型号价格代替。

四款均使用 `cache_write_mode=separate_by_ttl` 和 `service_tiers.standard.short`，同时配置输入、输出、缓存读取、5 分钟写入和 1 小时写入五价，不设置旧的 `cache_write_usd_per_million`。前三款的 `max_input_tokens` 和 `long_context_threshold_tokens` 均为 `1000000`，Haiku 均为 `200000`。前三款按官方规则在完整 1M 上下文内使用统一单价，无需另设 `long`。缓存 TTL 缺失时继续按价格快照中较高的写入价结算，分项数量保持未知；`/v1/messages/count_tokens` 仍不收取 Token 费用。

本目录只覆盖标准 API Token 价格，不支持 Claude Fast、Batch 或地区附加费。普通和流式 `/v1/messages` 请求省略顶层 `speed` 或设置 `"speed":"standard"` 时按标准模式准入；`"speed":"fast"` 及其他未知速度在转发和费用预留前返回 `400 service_tier_not_supported`。重复顶层键、非字符串值等非法形式按请求校验规则拒绝；消息、工具参数等嵌套内容中的 `speed` 不控制服务速度。

已有部署可先将当前 `GATEWAY_USAGE_PRICING_JSON` 的 JSON 值保存为 `pricing-current.json`，再在仓库根目录运行以下命令，生成待审阅的单行配置。命令仅合并这四条模型价格并更新目录日期，保留其他模型、固定汇率、汇率日期和兜底策略：

```sh
jq -ce --slurpfile catalog deploy/pricing-v2.example.json '
  ["claude-fable-5-1", "claude-opus-5-5", "claude-sonnet-5-5",
   "claude-haiku-4-5-20251001"] as $ids |
  if .schema_version != 2 or (.models | type) != "object" then
    error("current pricing must use schema_version 2 with a models object")
  else
    .models += ($catalog[0].models | with_entries(select(.key as $id | $ids | index($id)))) |
    .catalog_as_of = $catalog[0].catalog_as_of
  end
' pricing-current.json > pricing-claude.json
```

核对 `pricing-claude.json` 后，用其单行内容替换 `.env` 的 `GATEWAY_USAGE_PRICING_JSON`，再按现有部署流程校验并重新加载 Gateway。示例文件更新不会自动改变实际部署价格。数据库覆盖价仍优先于部署目录，“模型倍率”继续作用于最终费用；已有覆盖价的模型需在“模型定价”中核对有效价格。此次补目录无需新增数据库迁移，历史请求和账单继续使用原价格快照。

## 上线与回退

先应用 `0032_anthropic_accounts.sql` 和 `0033_anthropic_billing.sql`，部署同一版本的 Gateway 与锁定补丁的 CPA。可继续使用现有 Codex、Antigravity 账号；在数据库与服务都升级后，再导入或启用 Claude 账号。Claude 分配前检查 CPA 的 `upstream_account_access_v1` 与 `anthropic_messages_v1` 能力，旧 CPA 无法满足时拒绝调度。

上线后使用实际订阅账号验收：OAuth 授权、重复身份隔离、刷新并重启后的身份一致性、组织切换、普通与流式工具续轮、thinking 签名、图片、取消、`count_tokens` 和账单账号归因。仓库中的合成测试不代表实际订阅账号验收。

回退前先停用全部 Claude 账号，等待账号实时并发归零，并核对没有未完成的 Messages 请求、费用预留或待结算记录。保留已迁移数据库与账本，不删除历史归因；不要在 Claude 请求或结算仍进行时启动不支持这些端点和计费模式的旧 Gateway。

```sql
SELECT request_id, state FROM usage_requests
WHERE endpoint IN ('messages', 'messages.count_tokens') AND state = 'in_progress';
SELECT request_id, state FROM billing_reservations
WHERE billing_mode = 'anthropic_api_token_equivalent' AND state = 'reserved';
```
