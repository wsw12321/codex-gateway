# GPT-5.6 服务端配置

本指南使用 [`deploy/env.gpt-5.6.example`](../deploy/env.gpt-5.6.example) 生成
生产 `.env`。完整可读目录是
[`deploy/pricing-v2.example.json`](../deploy/pricing-v2.example.json)，模型单价和
缓存语义于 2026-08-20 对照以下官方文档核对：

- [OpenAI API Pricing](https://developers.openai.com/api/docs/pricing/)
- [GPT-5.5](https://developers.openai.com/api/docs/models/gpt-5.5)
- [GPT-5.4](https://developers.openai.com/api/docs/models/gpt-5.4)
- [Prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching/)

本系统计算的是“OpenAI API Token 等价成本”，不是 OpenAI 实际账单。上游使用
ChatGPT Pro OAuth，而且内部零价和保守兜底都属于本地策略；Pro 订阅费、工具、
区域、Batch、Ultrafast、税费和基础设施成本不在范围内。

## 计价口径

模板包含当前 Codex API 模型目录中的 `gpt-5.6-sol`、`gpt-5.6-terra`、
`gpt-5.6-luna`、`gpt-5.5`、`gpt-5.4`、`gpt-5.4-mini`，以及隐藏的
`codex-auto-review`。GPT-5.2 等未加入完整规则的模型会在转发前拒绝，不能用别名
或相近模型价格代替。

配置中的每百万 Token 单价如下；每格依次为“普通输入 / 缓存读取 / 缓存写入 /
输出”。“包含”表示缓存写入不单独收费，仍属于普通非缓存输入。

| 模型 / 服务层 | `<=272000` Token | `>272000` Token |
| --- | --- | --- |
| GPT-5.6 Sol Standard | `5 / 0.5 / 6.25 / 30` | `10 / 1 / 12.5 / 45` |
| GPT-5.6 Sol Flex | `2.5 / 0.25 / 3.125 / 15` | `5 / 0.5 / 6.25 / 22.5` |
| GPT-5.6 Sol Fast | `10 / 1 / 12.5 / 60` | `20 / 2 / 25 / 90` |
| GPT-5.6 Terra Standard | `2 / 0.2 / 2.5 / 12` | `4 / 0.4 / 5 / 18` |
| GPT-5.6 Terra Flex | `1 / 0.1 / 1.25 / 6` | `2 / 0.2 / 2.5 / 9` |
| GPT-5.6 Terra Fast | `4 / 0.4 / 5 / 24` | `8 / 0.8 / 10 / 36` |
| GPT-5.6 Luna Standard | `0.2 / 0.02 / 0.25 / 1.2` | `0.4 / 0.04 / 0.5 / 1.8` |
| GPT-5.6 Luna Flex | `0.1 / 0.01 / 0.125 / 0.6` | `0.2 / 0.02 / 0.25 / 0.9` |
| GPT-5.6 Luna Fast | `0.4 / 0.04 / 0.5 / 2.4` | `0.8 / 0.08 / 1 / 3.6` |
| GPT-5.5 Standard | `5 / 0.5 / 包含 / 30` | `10 / 1 / 包含 / 45` |
| GPT-5.5 Flex | `2.5 / 0.25 / 包含 / 15` | `5 / 0.5 / 包含 / 22.5` |
| GPT-5.5 Fast | `12.5 / 1.25 / 包含 / 75` | 未公布；保守兜底同左 |
| GPT-5.4 Standard | `2.5 / 0.25 / 包含 / 15` | `5 / 0.5 / 包含 / 22.5` |
| GPT-5.4 Flex | `1.25 / 0.13 / 包含 / 7.5` | `2.5 / 0.25 / 包含 / 11.25` |
| GPT-5.4 Fast | `5 / 0.5 / 包含 / 30` | 未公布；保守兜底同左 |
| GPT-5.4-mini Standard | `0.75 / 0.075 / 包含 / 4.5` | 不适用 |
| GPT-5.4-mini Flex | `0.375 / 0.0375 / 包含 / 2.25` | 不适用 |
| GPT-5.4-mini Fast | `1.5 / 0.15 / 包含 / 9` | 不适用 |

`input_tokens <= 272000` 使用短档，`input_tokens > 272000` 使用长档。GPT-5.4-mini
的最大输入为 272000，只配置短档；若上游仍报告更大输入，会记录缺失组合并用该
模型最高已公布分量兜底。

GPT-5.6 的 `cache_write_mode` 是 `separate`：

```text
ordinary = input - cached - cache_write
cost = ordinary * input_price
     + cached * cached_price
     + cache_write * cache_write_price
     + output * output_price
```

GPT-5.5、GPT-5.4 和 GPT-5.4-mini 使用 `included_in_input`：

```text
ordinary = input - cached
cost = ordinary * input_price
     + cached * cached_price
     + output * output_price
```

两种公式最后都除以 `1,000,000`，并按现有规则保留 12 位小数。GPT-5.6 若缺失
`cache_write_tokens` 字段，会把全部非缓存输入视为缓存写入并记录
`missing_cache_write_tokens`。

服务层处理规则：请求/响应 `default` 或 `standard` 映射 Standard，`flex` 映射
Flex，`priority` 或 `fast` 映射 Fast。显式请求 Ultrafast 或其他未配置层级会在
转发前拒绝；响应缺失或未知层级时，在当前上下文档位逐分量取最高已公布价格并
记录原因。缺少指定组合时在该模型全部已公布组合中逐分量取最高值。GPT-5.5 和
GPT-5.4 的 Fast 长上下文因此分别用 `12.5 / 1.25 / 75` 与 `5 / 0.5 / 30`
兜底，管理台会单独统计，不显示成官方精确价格。

`codex-auto-review` 是自动审批使用的独立 reviewer 模型名。官方文档没有为这个
内部名称公布独立 API 价格，模板将其设为零价，表示审批属于不向用户扣费的内部
治理流量。这是本部署的计费策略，不是 OpenAI 官方报价。它可以在无余额时创建
无资金来源的 reservation，仍写 Token 和零金额 ledger，也仍占用普通请求与 Token
配额。

## v2 JSON 与账务快照

完整 JSON 的三个保守策略必须保持为：

```json
{
  "schema_version": 2,
  "fallback_policy": {
    "unknown_service_tier": "max_published",
    "missing_price_combination": "max_published",
    "missing_cache_write_tokens": "all_uncached_as_write"
  }
}
```

这只是字段说明，不能代替完整模板。v2 严格禁止混用旧模型级三价字段；无版本
配置仍视为 v1，仅用于过渡及结算升级时已经执行中的旧 reservation。

准入时会保存请求模型的完整 v2 规则。结算后不可变 ledger 保存实际模型、请求/
实际/最终计价服务层、短/长档、cache-write Token 和模式、规则版本、目录日期、
最终四价以及兜底原因。管理台 USD 汇总来自 ledger；`estimated_usd` 保留为
`actual_cost_usd` 的兼容别名，并另列 `charged_usd`、`uncovered_usd`。修改当前
价格 JSON 不会改变历史 ledger USD。

## 新服务器初始化

所有命令都在服务器的仓库根目录，以专用部署用户执行。

```sh
cp deploy/env.gpt-5.6.example .env
chmod 0600 .env

gateway_host='codex.example.com'
gateway_revision=$(git rev-parse HEAD)
gateway_gid=$(id -g)

sed -i \
  -e "s/replace-with-real-cloudflare-hostname/${gateway_host}/" \
  -e "s/replace-with-full-git-sha/${gateway_revision}/g" \
  -e "s/replace-with-deployment-user-gid/${gateway_gid}/" \
  .env
```

把 `gateway_host` 改成 Cloudflare 中已配置的真实主机名。然后打开 `.env`，把
`usd_cny_rate` 和 `fx_as_of` 改成你在部署当天审阅并决定冻结的汇率及日期；价格
JSON 必须保持在同一行。

生成服务 secret：

```sh
./scripts/bootstrap-secrets.sh
```

Cloudflare Tunnel token 不得写入 `.env`。把它保存为
`deploy/secrets/cloudflared_tunnel_token`，属主为部署用户、属组为部署组、权限为
`0640`。然后执行：

```sh
./scripts/validate-compose.sh
./scripts/compose.sh build gateway codex-compat
./scripts/compose.sh up -d
./scripts/compose.sh ps
```

完成 sidecar OAuth 登录后验证模型：

```sh
./scripts/smoke-sidecar.sh
```

首次启动后确认 `schema_migrations` 包含
`0005_official_token_pricing.sql`，再按本页模型集合做结算冒烟。已有旧数据库升级
不能直接套用上述首次启动流程，必须执行运维手册的
[7 步停写迁移](operations.md#升级到-0005_official_token_pricingsql)。

## 已运行服务器只更新价格目录

本节只适用于已经确认应用 `0005`、且二进制已经支持 v2 的服务器。首次从 v1
升级必须使用上述停写迁移。后续只调整官方价格或固定汇率时，先备份现有配置，
然后只把模板中的 `GATEWAY_USAGE_PRICING_JSON` 合并到服务器 `.env`；不要覆盖
服务器自己的域名、revision、GID、网段或并发参数。

```sh
cp .env ".env.backup.$(date -u +%Y%m%dT%H%M%SZ)"
./scripts/validate-compose.sh
./scripts/compose.sh up -d --no-deps --force-recreate gateway
./scripts/compose.sh ps
```

仅修改环境变量不需要重新构建镜像，但必须重建 Gateway 容器，因为目录只在进程
启动时读取。新目录只影响之后创建的 reservation；正在执行的请求继续使用原准入
快照，已经结算的 ledger USD 不变。变更后应抽查新 ledger 的目录日期、最终四价
和兜底统计。

## Codex 客户端自动审批

每台客户端在 `~/.codex/config.toml` 中设置：

```toml
model = "gpt-5.6-sol"
openai_base_url = "https://codex.example.com/v1"
approval_policy = "on-request"
approvals_reviewer = "auto_review"
default_permissions = ":workspace"
```

替换域名后执行 `codex login --with-api-key`，通过交互输入 Gateway API Key。不要
把 API Key 写进 TOML、shell history 或项目文件。

如果 API Key 配置了模型白名单，必须至少允许主模型和自动审批模型：

```text
gpt-5.6-sol
codex-auto-review
```

价格目录只负责计价，不会自动修改 API Key 白名单。

## 故障判断

- `model_pricing_not_found`：请求中的精确模型名不在价格 JSON 中。
- `service_tier_not_supported`：请求显式指定了 Ultrafast 或模型未配置的服务层；
  请求没有转发到上游。
- `model_not_allowed`：API Key 模型白名单未包含该模型。
- `insufficient_quota`：非零价请求没有可用于准入的余额或订阅额度；内部零价
  `codex-auto-review` 不要求资金来源。
- 管理台出现 `missing_service_tier`、`unknown_service_tier`、
  `missing_price_combination` 或 `missing_cache_write_tokens`：请求已按保守规则
  结算，需要核查 sidecar/upstream usage 字段或官方目录覆盖，不得手工改 ledger。
- 上游返回模型不存在：CLIProxyAPI/上游版本不支持该模型，需要按兼容层升级规程
  升级，而不是伪造另一个价格名称。

以后 Codex 模型目录出现新模型时，应先从官方价格页核对该模型所有允许服务层、
上下文档位和缓存写入语义，在价格 JSON 加入完整规则并同步 API Key 白名单，经过
边界与结算测试后再允许客户端切换。
