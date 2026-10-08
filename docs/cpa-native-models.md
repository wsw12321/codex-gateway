# CPA v8 原生模型与定价迁移

Gateway 继续公开已审核的 8 个 Gemini 原生 ID，以及 `gpt-6.1-sol`。
CPA v8.0.20 使用固定提交自带的模型目录，已原生包含 Sol，不再叠加本地目录条目；
新增的上游模型不会自动扩大 Gateway 的公开范围，本次升级不改现有授权、价格或数据库结构。
Gateway 仅展示同时满足价格、用户权限、API Key 白名单和可用账号能力的模型。
旧 `gemini-3.1-pro-high`、Flash `-medium` 和旧 preview 别名退出默认目录；客户端必须
主动选择新的精确 ID，并通过请求参数设置推理档位。旧 CLI 会话须新建，不迁移或伪造签名。

| 模型 ID | Standard 输入 | 缓存读取 | 输出 |
| --- | ---: | ---: | ---: |
| `gemini-pro-agent`、`gemini-3.1-pro-low` | 2 | 0.20 | 12 |
| `gemini-3-flash` | 0.50 | 0.05 | 3 |
| `gemini-3.6-flash-high`、`gemini-3.7-flash-high`、`gemini-3.8-flash-high` | 0.75 | 0.075 | 3.75 |
| `gemini-3.1-flash-lite` | 0.25 | 0.025 | 1.50 |
| `gemini-3.5-flash-lite` | 0.30 | 0.03 | 2.50 |
| `gpt-6.1-sol` | 2 | 0.10 | 10 |

单位为美元／百万 tokens。Gemini 是对应 Google API 的本地等价计费，不代表
Antigravity 订阅产生了按量费用；缓存写入包含在普通输入价格中，仅开放 Standard。
两项 Pro 的总输入超过 200,000 时，整请求采用 `4 / 0.40 / 18`。
Flash-high 三项的当前价格有效至 **2026-12-31**；运维须在到期前复核
[Google 官方价格](https://ai.google.dev/gemini-api/docs/pricing)，更新配置并保存新快照，
服务不会自动调价。

Sol 的缓存写入单独收费：短上下文为 2.50。总输入超过 272,000 时，整请求采用
输入／缓存读取／缓存写入／输出 `4 / 0.20 / 5 / 15`。Flex 为 Standard 的 0.5 倍，
Fast/priority 为 2 倍，见 [OpenAI 官方价格](https://developers.openai.com/api/docs/pricing/)。
价格配置中的最大输入只用于计费边界，CPA 渠道的实际上下文能力须通过账号验收确认。
本次实现核对了此前保存的官方页面；当前环境再次抓取返回 HTTP 405，未声称实时复核。

完整配置示例在 [pricing-v2.example.json](../deploy/pricing-v2.example.json)。现有部署
**不要用示例覆盖整个价格环境变量**，否则会覆盖其他 Codex 价格与自定义汇率。先把当前
`GATEWAY_USAGE_PRICING_JSON` 作为纯 JSON 保存到受保护文件，再运行离线合并工具：

```sh
umask 077
go build -o /tmp/cpa-pricing-migrate ./cmd/cpa-pricing-migrate
/tmp/cpa-pricing-migrate < pricing.before.json > pricing.applied.json
```

审核后将 `pricing.applied.json` 的完整 JSON 写回原部署配置；不要只同步新增模型列表。
工具保留其他模型、汇率和 fallback 策略，仅替换九个目标模型并移除明确退役 Gemini ID。
v1 配置需先人工转换到 v2，工具会拒绝隐式改变旧计费语义。

数据库迁移 `0025_cpa_v8_native_models.sql` 仅执行一次：目标模型默认权限和所有现有用户
权限置为允许、倍率重置为 1，后续新用户继承默认值。重启不会再次覆盖管理员操作。
所有受限 API Key 的白名单逐项保留，包含旧名称的 Key 不会自动获得新模型，也不会被清空
变成无限制；需要 Owner 显式重新配置。其他 Codex 权限和倍率、余额、历史用量、账单及
已预约请求的价格快照均不更改。默认允许仍然受 API Key、账号权限、余额与配额约束。

Responses 客户端采用精确模型名，例如：

```json
{"model":"gemini-pro-agent","input":"Reply with OK","store":false}
```

原生 Gemini 入口为 `/v1beta/models/gemini-pro-agent:generateContent` 或
`:streamGenerateContent?alt=sse`。本次只开放文本与客户端函数工具；函数返回时完整保存
并回传供应商提供的 `thoughtSignature`，不要通过改名续接旧 bridge 会话。

回滚必须使用识别全部新迁移的兼容构建。价格回滚只恢复仍等于切换快照的目标条目，
保留切换后管理员修改、当前汇率与其他模型：

```sh
/tmp/cpa-pricing-migrate -rollback-before pricing.before.json \
  -rollback-applied pricing.applied.json < pricing.current.json > pricing.rollback.json
```

维护窗口内执行 [rollback-cpa-model-config.sql](../scripts/rollback-cpa-model-config.sql)
恢复未被后续修改的目标默认权限、原用户权限与倍率。该脚本不恢复全库、不改账单与余额；
切换后新用户的记录保留。旧链路无法执行的新模型保持不可用，最后以完整回滚价格目录
同步可用目录。`cpa_v8_model_config_backup` 保存比较所需的切换前后值，7 天稳定期内保留。
