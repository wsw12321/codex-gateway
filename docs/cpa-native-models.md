# CPA v8.0.59 原生模型与定价迁移

Gateway 固定使用 CLIProxyAPI **v8.0.59**（commit `c5251b57b3fa5dcbc689f514f4aa166daacb4481`）的模型目录，
公开范围限于已经接入的供应商和审核过的模型。
目录同步由 Gateway 启动时执行，只有同时满足价格、用户权限、API Key 白名单和上游账号能力的模型才会在
`/v1/models` 中返回。Antigravity 默认模型为 `gemini-3.8-flash-high`；模型 ID 必须精确匹配，不能用
别名替代。

## v8.0.59 目录变更

v8.0.59 未修改上游 `internal/registry/models/models.json`，SHA-256 仍为
`99037d47cc67751a02847f8a5ed3463c694de88319ee458e527844821b9a9bd0`。公开模型、价格和权限均不变。

## v8.0.58 目录变更

v8.0.58 在既有目录基础上新增 `gpt-6-chat-thinking`。模型只有在 Gateway 价格目录和用户权限策略中完成审核后才会公开。

## v8.0.48 基线目录变更

上游 v8.0.48 从全部四个 Codex 目录（free/team/plus/pro）移除 `gpt-5.5`，并新增 `gpt-6.1-terra`、
Kimi 与 xAI 模型。Gateway 只退役 `gpt-5.5`，不新增公开模型：`gpt-6.1-terra` 尚无审核过的价格，
Kimi 与 xAI 没有 Gateway 路由和计费策略，均保持不可用。完整差异见
[目录审计](cpa-v8.0.48-model-catalog-audit.md)。

- 价格：离线工具 `cpa-pricing-migrate` 同时移除 `gpt-5.5`。Gateway 启动时若价格目录仍包含
  `gpt-5.5` 会拒绝启动，避免旧配置在下次目录同步时把退役模型重新标记为活动。
- 数据库：`0038_cpa_v8_0_48_model_catalog.sql` 只执行一次，将 `gpt-5.5` 标为非活动目录，并保存默认
  权限、用户权限和倍率的 before/applied 快照；历史 usage、账单、余额、API Key allowlist 和订阅
  模型范围不变，历史记录仍可读取，新请求会被拒绝。
- 回滚：条件恢复脚本只应用最新的快照表；存在 `0038` 快照时只恢复本次退役，不叠加 `0035` 或 `0025`。

## v8.0.30 目录变更

v8.0.30 新增以下两个模型（v8.0.48 保持不变）：

| 模型 ID | 最大输入上下文 | 最大完成 | Standard 输入 / 缓存读取 / 输出（美元／百万 tokens） |
| --- | ---: | ---: | ---: |
| `claude-haiku-5-5` | 1,000,000 | 128,000 | 输入 ≤100,000：0.10 / 0.01 / 0.50；输入 >100,000：0.50 / 0.05 / 2.50 |
| `gemini-4-argon-high` | 1,048,576 | 1,048,576 | 2 / 0.10 / 10 |

当前 Antigravity 公开目录共五个 ID：`gemini-3-flash`、`gemini-3.8-flash-high`、
`gemini-3.1-flash-lite`、`gemini-3.5-flash-lite` 和 `gemini-4-argon-high`。
Haiku 5.5 走 Anthropic Messages。上游目录将 Haiku 5.5 标记为支持文本、图片、thinking 和原生 web search，
Argon-high 标记为支持文本、图片、音频、视频和 web search；Gateway 的 Antigravity 请求校验仍只开放
文本与客户端函数工具，上游能力描述不代表这些媒体或内置工具已开放。

基础 `gemini-4-argon` 属于上游 `gemini` provider，四个 Muse Wonder ID 属于 `meta` provider；
这两个 provider 本次均未接入。基础 Argon 和 Muse 不配置价格、默认权限或新请求路由，也不会出现在
Gateway 的公开目录中，不能通过改名路由到 Argon-high。

以上价格于 2026-10-17 核对 [Anthropic 官方定价](https://platform.claude.com/docs/en/about-claude/pricing)
和 [Google 官方定价](https://ai.google.dev/gemini-api/docs/pricing)。Haiku 5.5 使用 TTL 缓存模式，
5 分钟／1 小时写入价在短档为 `0.125 / 0.20`，长档为 `0.625 / 1`。总输入包含普通输入、缓存读取
和缓存写入；超过 100,000 后整个请求按长档五价结算。Argon-high 对应 `gemini-4-argon` API 家族，
使用 `included_in_input` 缓存模式，现价持续至 2026-11-30；2026-12-01 起官方已公布的输入／缓存读取／输出为
`4 / 0.20 / 20`，需届时复核并更新。实际 CPA 账号上下文能力仍须通过真实账号验收。

v8.0.30 从活动目录移除以下上游删除的 ID：

- `gemini-pro-agent`
- `gemini-3.1-pro-low`
- `gemini-3.6-flash-high`
- `gemini-3.7-flash-high`
- `gemini-3.7-flash`

删除只会将 `model_access_defaults.catalog_active` 设为 `false`。旧名称保留在历史 usage、账单快照和受限
API Key allowlist 中，因此历史记录仍可读取；新请求会被拒绝。受限 Key 不会被清空，也不会因目录同步而
获得新模型。旧桥接时期的其它 ID 只保留为退役名称，不能通过 CPA 原生路由重新启用。

完整示例在 [pricing-v2.example.json](../deploy/pricing-v2.example.json)。现有部署不要直接以示例覆盖整个
`GATEWAY_USAGE_PRICING_JSON`，应先保存当前纯 JSON，再使用离线工具合并：

```sh
umask 077
go build -o /tmp/cpa-pricing-migrate ./cmd/cpa-pricing-migrate
/tmp/cpa-pricing-migrate < pricing.before.json > pricing.applied.json
```

工具保留其它 Codex/Claude 模型、汇率和 fallback 策略，只替换其七个目标模型价格（五款 Gemini、
Haiku 5.5 和 GPT-6.1 Sol），并移除上述退役 Gemini ID 和 v8.0.48 退役的 `gpt-5.5`。它不覆盖 GPT-6 Sol/Luna 和 GPT-5.6 Sol，
同步全部现价还须执行[官方价格同步流程](operations.md#同步当前官方-api-价格)。
v1 配置必须先人工转换到 v2；工具不会隐式改变旧计费语义。审核后将完整 `pricing.applied.json` 写回
部署配置，并保存 before/applied 文件供回滚比较。

数据库迁移 `0035_cpa_v8_0_30_model_catalog.sql` 只执行一次。它会：

- 将五个删除模型标记为非活动目录，保留历史权限和数据；
- 初始化 Haiku 5.5 和 Argon-high 两个新模型的活动目录和现有用户授权，新用户通过默认目录继承；
- 保存默认权限、用户权限和倍率的 before/applied 快照；缺失倍率行继续按 `1` 计费，不伪造管理员归属；
- 不修改 API Key allowlist、历史 usage、余额、账单、已结算记录或 admission pricing snapshot。

## 条件恢复快照

迁移具有 forward-only 语义。旧 Gateway 二进制会因未知迁移保护而拒绝启动，回滚时必须使用包含已应用迁移
（含 `0038`）的兼容构建。数据库配置回滚使用 [rollback-cpa-model-config.sql](../scripts/rollback-cpa-model-config.sql)：
脚本只恢复仍等于切换 applied 快照的行，保留切换后 Owner 修改、新用户记录和业务数据。
脚本只选择最新的快照表：存在 `0038` 时只恢复 v8.0.48 退役，否则存在 `0035` 时只恢复 v8.0.30
切换，均不叠加更早的回滚。价格回滚则比较
`pricing.before.json` 与 `pricing.applied.json`：

```sh
/tmp/cpa-pricing-migrate -rollback-before pricing.before.json \
  -rollback-applied pricing.applied.json < pricing.current.json > pricing.rollback.json
```

`pricing.rollback.json` 是完整的条件恢复快照，须保留用于审计和后续比较，**不要直接加载为运行配置**。
它可能恢复 v8.0.20 的 `gemini-pro-agent` 等价格；本次 Gateway 在普通 `cpa` 模式下仍拒绝退役 ID，
不会因为价格恢复而重新开放。价格快照恢复与选择可运行的回滚传输是两个步骤。

旧 Antigravity 桥接与 legacy 路由已于 2026-11-13 删除（[路线图第 8 步](cpa-plugin-roadmap.md#8-改为默认并清理)），
不再能用恢复快照为它生成 `pricing.legacy.json`；Gateway 在 Antigravity 价格目录中出现未审 ID 时拒绝启动。
若继续使用普通 CPA 传输，则保持当前审核过的 CPA 模型集合，不能用完整恢复快照或 legacy 路由重新开放旧 ID。

## 当前 CPA 请求示例

CPA 原生请求使用精确模型名，例如：

```json
{"model":"gemini-4-argon-high","input":"Reply with OK","store":false}
```

原生 Gemini 入口为 `/v1beta/models/gemini-4-argon-high:generateContent` 或
`:streamGenerateContent?alt=sse`。Claude Messages 使用 `/v1/messages` 并携带 `claude-haiku-5-5`。
上线前必须使用真实账号验收普通/流式请求、工具调用、失败冷却、强制刷新和重启后的身份保持。
