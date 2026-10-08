# Claude Code 接入与账号管理

链路为 Claude Code API 模式 → Gateway → CPA → Claude 订阅 OAuth 账号。Gateway 的提供方 ID 为 `anthropic`，控制台显示“Claude 账号”；CPA 执行器内部名称 `claude` 不构成第二个 Gateway 提供方。

## 管理员准备

先部署数据库迁移、Gateway 和补丁后的 CPA，再在“CPA 授权管理”的“Claude 账号”页授权或导入。导入支持 CPA 的 `refresh_token` / `access_token` 字段，或 Claude Code 的 `claudeAiOauth.refreshToken` / `accessToken`。浏览器只提交必要令牌，不采用导入文件宣称的套餐、身份或额度。身份验证由 CPA 完成；组织不同视为不同账号。Claude 导入至少需要访问令牌或刷新令牌之一；存在刷新令牌时先刷新再验证身份。仅导入访问令牌的账号到期后不能自动刷新，需要重新授权或导入有效凭据。

在“上游账号 → Claude 账号”配置权重、并发和共享/专属权限。用户的 Claude 账号范围默认是“全部账号”；管理员可在“账号权限”选择指定账号，空名单表示全部禁用。账号范围仍受专属授权、模型权限、账号状态与并发限制约束。

管理员应为所需 Claude 精确模型 ID 配置价格并授权。`separate_by_ttl` 在模型定价页分别显示 5 分钟和 1 小时缓存写入价。账单保留总缓存写入以及实际观测的 TTL 分项；缺失分项时显示未知并按请求价格快照中的较高写入价结算，不推算分项。

“刷新状态”读取 CPA 最新被动观测快照，展示五小时/七天窗口、重置时间、冷却时间和观测时间。没有新上游响应时刷新不会改变观测时间；未知套餐和未观测到的额度不会显示为零或满额。冷却沿用上游自动恢复逻辑，手动启用不会跳过冷却。

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
./scripts/validate-cpa-panel.sh
PLAYWRIGHT_MODULE=/path/to/playwright PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chrome \
  node internal/server/testdata/anthropic_accounts_browser.cjs
PLAYWRIGHT_MODULE=/path/to/playwright PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chrome \
  node internal/server/testdata/cpa_admin_browser.cjs
```

以上浏览器验证完全拦截网络，使用合成账号；覆盖账号范围隔离、权重保存、未知与已观测额度、状态刷新后的快照保留、Claude 凭据字段提取、授权域名、移动端布局。配置测试覆盖认证冲突、未授权模型、原子合并、备份和不泄漏凭据。真实 OAuth、凭据刷新后重启、工具续轮、thinking、图片和账单归因仍需部署环境的账号验收。

截图：[Claude 账号桌面](screenshots/anthropic-accounts-desktop.png)、[Claude 账号手机](screenshots/anthropic-accounts-mobile.png)、[配置指导手机](screenshots/claude-guide-mobile.png)、[CPA 桌面](screenshots/cpa-anthropic-desktop.png)、[CPA 手机](screenshots/cpa-anthropic-mobile.png)。

## 定价配置示例

将下面的**模型条目**合并到现有 `GATEWAY_USAGE_PRICING_JSON.models`，保留现有 `schema_version: 2`、其他模型、汇率和兜底策略。`claude-example-exact-id`、上下文限制与全部单价均为演示值，不能直接作为生产模型或官方报价；替换为已核实的精确模型 ID、限制和价格，再按现有部署流程加载。最大输入等于长上下文阈值时只需 `short`；如模型支持超过阈值的上下文，另行配置 `long` 完整五价。

```json
{
  "claude-example-exact-id": {
    "cache_write_mode": "separate_by_ttl",
    "max_input_tokens": 200000,
    "long_context_threshold_tokens": 200000,
    "service_tiers": {
      "standard": {
        "short": {
          "input_usd_per_million": "3",
          "cached_input_usd_per_million": "0.3",
          "cache_write_5m_usd_per_million": "3.75",
          "cache_write_1h_usd_per_million": "6",
          "output_usd_per_million": "15"
        }
      }
    }
  }
}
```

`separate_by_ttl` 不同时设置旧的 `cache_write_usd_per_million`。此条目进入部署价格目录后，Owner 才能在“模型定价”保存数据库覆盖价；“模型倍率”继续对最终请求费用统一应用倍率。

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
