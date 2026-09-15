# 通过 Responses 接入 Gemini Pro

当前默认 Alpine 方案的真实插件加载验证失败；Debian slim 候选已验证通过，运行
镜像调整尚待确认。部署前先阅读 [验证记录](gemini-validation.md)，不要跳过镜像
构建中的 ABI 检查。以下为完成镜像选择后的配置和验收流程。

客户端继续使用原网关地址、API Key 和 `POST /v1/responses`，将模型改为
`gemini-3.1-pro-preview`。支持普通 JSON、SSE 和函数工具调用。沿用现有用户模型
权限、Key allowlist、额度、余额和统计；启动同步价格目录后，还需由 Owner 为使用者
启用该模型。现有 Key 若设置了模型 allowlist，也需加入 Gemini。

本次复用唯一 CLIProxyAPI `v7.2.150` 实例，加载固定提交
[`19d9868ffa24e94a2919ea1d1a761afa634de669`](https://github.com/router-for-me/cpa-plugin-gemini-cli/tree/19d9868ffa24e94a2919ea1d1a761afa634de669)
的 Gemini CLI 插件。凭证存入原 `codex_oauth` volume，不增加数据库结构或 Gemini
账号管理页面。现有“上游账号”控制台及其禁用、额度查询接口仍用于 Codex 账号。

补丁校验由部署脚本固定：主程序 `cliproxy-v7.2.150-gemini.patch` 与插件
`gemini-cli-19d9868-gateway.patch` 分别检查 SHA256，镜像标签也包含二者的组合校验。
变更补丁时必须同步标签和校验值，不能跳过构建内回归测试。

## 升级价格配置

目录只配置 Standard，`max_input_tokens=1048576`，长上下文分界为 `200000`。
缓存写入已包含在输入中；表中的缓存读取 Token 从普通输入 Token 中扣除。

| 输入长度 | 输入 USD/百万 | 缓存读取 USD/百万 | 输出 USD/百万 |
| --- | ---: | ---: | ---: |
| ≤ 200000 | 2 | 0.20 | 12 |
| ≥ 200001 | 4 | 0.40 | 18 |

输出包含回答和思考 Token；`output_tokens_details.reasoning_tokens` 是输出的细分，
不得再次相加。价格依据 [Google 官方 Standard 定价](https://ai.google.dev/gemini-api/docs/pricing#gemini-3.1-pro-preview)，
快照日期为 2026-09-15。Google 发布的其他服务层不在本次目录中。
省略 `service_tier` 或使用 `default`；显式请求未配置的层会按现有规则拒绝。
实际扣款沿用现有 USD/CNY 汇率、取整、预留和结算规则，这不是 Google 订阅账单。

新部署直接使用 `deploy/env.example`。升级现有部署时，先把 `.env` 中
`GATEWAY_USAGE_PRICING_JSON` 的 JSON 值（去掉外围 shell 引号）存为私有临时文件
`/tmp/current-pricing.json`，只合入新模型和目录日期，保留原 GPT 价格、汇率及回退策略：

```sh
umask 077
jq -c --slurpfile reviewed deploy/pricing-v2.example.json '
  if .schema_version != 2 then error("upgrade to pricing schema v2 first") else . end |
  .models["gemini-3.1-pro-preview"] = $reviewed[0].models["gemini-3.1-pro-preview"] |
  .catalog_as_of = $reviewed[0].catalog_as_of
' /tmp/current-pricing.json > /tmp/pricing-with-gemini.json
```

将生成的单行 JSON 作为 `.env` 中 `GATEWAY_USAGE_PRICING_JSON` 的单引号包围值，
复核差异后删除两个临时文件。不要用 `source .env` 读取配置。严格 Compose 校验会要求
完整目录包含 Gemini；只升级镜像但保留旧目录将无法通过部署校验。历史账务保留原快照。

## 构建与登录

```sh
./scripts/validate-compose.sh
./scripts/compose.sh build gateway codex-compat
./scripts/gemini-login.sh
# 若账号需要指定 Google Cloud 项目：
# ./scripts/gemini-login.sh your-project-id
```

构建使用同一 musl 工具链，以 `CGO_ENABLED=1 CC=musl-gcc` 构建主程序和
`-buildmode=c-shared` 插件，在 Alpine 中实际加载 `gemini-cli.so` 并验证登录参数与
合成 OAuth 文件识别。启动也检查插件登录参数，加载失败时拒绝启动。

Gemini 登录与 `codex-device-login.sh` 共用 `.device-login.lock`。脚本先停止唯一
sidecar 并移除其容器以释放固定 IP，保留 OAuth volume，然后执行
`--geminicli-login --no-browser`。在自己的浏览器打开授权 URL；服务器端提示粘贴时，
将浏览器跳转后的完整 `http://127.0.0.1:.../oauth2callback?...` URL 粘贴回 SSH
终端。浏览器显示本地连接失败不妨碍粘贴回调；无需公开 OAuth 监听端口。
授权 URL 和回调仅用于当次交互，不要写入工单或日志。

登录前后的清单覆盖所有 OAuth JSON 文件，要求恰好一个新增或刷新、没有删除；
目录必须为 UID 10001/`0700`，文件为 UID 10001/`0600`。失败保持 sidecar 停止。
成功后重启并验证 Gemini 模型列表、JSON 和 SSE，不输出响应正文。
Gemini 的用户/Key 用量与账务沿用现有记录；Codex 专用账号索引与账号统计不适用于 Gemini。
任何时候都不能启动第二个挂载同一 OAuth volume 的 sidecar。

出口仅增加固定 Google 域名：`accounts.google.com`、`oauth2.googleapis.com`、
`www.googleapis.com`、`cloudresourcemanager.googleapis.com`、
`cloudcode-pa.googleapis.com` 和 `cloudaicompanion.googleapis.com`。
Squid 仍只允许白名单主机的 HTTPS CONNECT。

价格配置更新后重启 Gateway 以同步目录，完成用户授权：

```sh
./scripts/compose.sh up -d --no-deps gateway
./scripts/verify-oauth-permissions.sh
./scripts/smoke-sidecar.sh gemini-3.1-pro-preview
```

## 网关请求与真实账号验收

以下 `$GATEWAY_URL` 为原地址（不含 `/v1`），`$GATEWAY_API_KEY` 为已启用 Gemini 的
用户 Key。不要开启 shell tracing 或将 Key 写入共享日志。

```sh
curl --fail-with-body "$GATEWAY_URL/v1/responses" \
  -H "Authorization: Bearer $GATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gemini-3.1-pro-preview","input":"Reply with OK.","stream":false,"store":false}'

curl --fail-with-body --no-buffer "$GATEWAY_URL/v1/responses" \
  -H "Authorization: Bearer $GATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gemini-3.1-pro-preview","input":"Reply with OK.","stream":true,"store":false}'
```

函数工具仍使用 Responses 的 `tools`、`function_call` 和 `function_call_output`：

```json
{"model":"gemini-3.1-pro-preview","input":"What is the weather in Shanghai?","tools":[{"type":"function","name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"store":false}
```

客户端执行返回的函数后，在下一次 `input` 中保留原请求及完整 `output` 项，再附加
`{"type":"function_call_output","call_id":"返回的 call_id","output":"工具结果"}`。
保留所有不透明调用标识和附带字段，工具结果与调用必须匹配；不依赖服务端
`previous_response_id` 存储。

`POST /v1/responses/compact` 对有权限的 Gemini 请求返回
`501 endpoint_not_supported`，发生在额度、用量和账务预留之前；无权限仍返回拒绝。
客户端须关闭该模型的自动 compact，或使用自己的上下文整理策略。

真实 Pro 账号验收需确认返回模型、JSON 的 `usage` 及 SSE `response.completed` 中
`usage` 一致；`output_tokens` 包含思考且不小于 `reasoning_tokens`。在控制台按
请求记录核对 input/cached/output/reasoning 和扣款，确认一条请求只结算一次，随后
用原 GPT 模型完成一次回归请求。边界价格、重复完成事件、函数往返、Token 刷新、
429、拆包合并与客户端取消由模拟测试覆盖；模拟测试不能替代真实账号验收。
