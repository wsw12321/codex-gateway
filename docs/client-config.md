# Codex CLI 与 agy CLI 客户端配置

管理界面“使用指导”分为公共准备、Codex CLI、agy CLI 三部分，每个客户端都有安装、
配置和启动步骤。复制的一键配置命令会使用当前站点地址，并在终端提示输入 Gateway
API Key；Key 不进入复制命令、命令历史或网页存储。

## 公共准备

1. 安装 [Node.js LTS](https://nodejs.org/en/download)，其中包含 npm。
2. Windows 按 Win+R，输入 `cmd` 并回车；macOS / Linux 打开终端。安装后重新打开
   终端，运行 `node --version` 和 `npm --version` 确认安装成功。
3. 为当前设备创建独立 API Key，并确认账号与 Key 的模型权限。需要区分项目时设置
   Key 的默认项目；没有默认项目时记为 `unassigned`。

配置器从同站 `GET /setup/configure-client.cjs` 下载，响应禁止缓存。页面生成的
Node.js 启动命令将脚本保存到独立临时目录，传入客户端名称和站点 origin，保留终端
输入供 Key 提示使用，并在执行结束后清理临时文件。Node.js 处理 UTF-8、中文和空格
路径。旧版配置脚本下载入口已停用，返回 `410 Gone`。

## Codex CLI

按 [Codex 官方安装说明](https://developers.openai.com/codex/cli/) 安装：

```sh
npm install -g @openai/codex
codex --version
```

复制管理界面 Codex CLI 部分的配置命令，在终端执行并按提示输入 Gateway Key。
配置器尊重 `CODEX_HOME`，默认修改 `~/.codex/config.toml`；修改前备份，保留无关设置，
将以下顶层配置设置为当前站点对应值：

```toml
model_provider = "openai"
openai_base_url = "https://codex.example.com/v1"
cli_auth_credentials_store = "file"
```

配置器通过 stdin 调用 `codex login --with-api-key`，由 Codex 保存持久凭据。
已有自定义 provider 的配置块会保留，但当前 provider 切换到内置 `openai`。
认证行为参见 [Codex 官方认证说明](https://developers.openai.com/codex/auth/)。

配置器先在独立临时目录调用已安装 Codex 校验原配置及新配置，再执行登录；全部成功
后才备份并保存到用户目录。无法解析、登录失败或旧默认 `profile` 可能覆盖设置时，
会明确报错并保留原文件。旧 `profile` 需先按当前 Codex 的提示迁移，再运行配置器。

关闭并重新打开终端，运行 `codex`，发送“请回复 OK”，再到“使用统计”确认请求。
不要把 Key 写入 Git、项目 `.env` 或命令行参数。设备丢失后应在另一台已认证设备上
停用或删除对应 Key，并撤销会话；删除不影响既有用量与账务历史。

Codex 支持的数据请求为：

- `POST /v1/responses`
- `POST /v1/responses/compact`
- `GET /v1/models`

发往 Codex sidecar 的上述请求支持 `Session-Id` 和 `Session_id` 会话请求头，
头名不区分大小写。Gateway 保留两种拼写及其原值；同时提供时分别透传，由
sidecar 判断优先级和有效性。含回车或换行的值会被过滤。这两种头不会转发给
Antigravity。

同一会话的请求应保持稳定的会话 ID，不同会话应使用不同 ID。会话亲和按已认证
API Key 隔离；即使不同 Key 使用相同会话 ID，也不会共享绑定。实际账号绑定还
受模型、亲和有效期和账号可用性影响，不能保证始终使用同一账号。内部
`X-Codex-Gateway-Affinity` 由 Gateway 生成，客户端无法覆盖；内部账号归因头也
不会从客户端透传。

`openai_base_url` 覆盖的是 Codex 内置 `openai` provider 的地址。Codex 可能在新
会话开始时先用受同一 API Key 保护的 `GET /v1/responses` 探测 Responses
WebSocket；Gateway 会返回一次 `426 responses_websocket_unsupported`，客户端随即
改用正常的 `POST /v1/responses` HTTPS/SSE。这是预期的传输协商，不消耗配额、
余额或并发名额，也不会产生 usage 记录或请求 sidecar。

Gateway 不实现真正的 WebSocket、Chat Completions 或任意 URL 代理。Codex 的
文件读取、命令执行和代码修改仍发生在本地设备。

## agy CLI

按 [agy 官方安装说明](https://antigravity.google/docs/cli/install/) 选择系统对应命令。
Windows 使用 CMD：

```bat
curl -fsSL https://antigravity.google/cli/install.cmd -o install.cmd && install.cmd && del install.cmd
```

macOS / Linux：

```sh
curl -fsSL https://antigravity.google/cli/install.sh | bash
```

客户端必须保留 CPA 原生模型 ID。旧 AGY 1.2.12 会重写 Pro/标题等请求名称，
因此旧配置不能作为 CPA v8 的兼容性保证；如果客户端无法原样发送目录中的 ID，请使用
支持原生 Gemini/Responses 的客户端。配置器仅配置 Gateway 地址和 Key，不替客户端
伪造签名或替换模型。

配置器保留用户级 `~/.gemini/antigravity-cli/settings.json` 的其他字段并合并
`{"modelProvider":"gemini"}`，保存 `GOOGLE_GEMINI_BASE_URL=<站点 origin>`（不加 `/v1`）
和 `GEMINI_API_KEY`。Windows 使用当前用户环境变量；macOS / Linux 使用 0600 凭据文件，
并幂等更新 Bash / Zsh 加载配置。修改前备份；重开整个终端后生效。

## Gemini API

链路为客户端 → Gateway → 同一 CPA → Antigravity。Gateway 支持
`POST /v1/responses`、`POST /v1beta/models/{model}:generateContent` 及
`POST /v1beta/models/{model}:streamGenerateContent?alt=sse`。使用 Gateway Key；原生 Gemini
可以用 `X-Goog-Api-Key` 或 Bearer，不能同时提供两种凭据，也不接受 URL 中的 Key。

先查询 `GET /v1/models`。经过价格、用户权限、受限 Key 和可用账号交集筛选后，
目录最多公开八个原生 Gemini ID：`gemini-pro-agent`、`gemini-3.1-pro-low`、`gemini-3-flash`、
`gemini-3.6-flash-high`、`gemini-3.7-flash-high`、`gemini-3.8-flash-high`、
`gemini-3.1-flash-lite`、`gemini-3.5-flash-lite`。

原生请求示例：

```http
POST /v1beta/models/gemini-pro-agent:generateContent
Content-Type: application/json
Authorization: Bearer <Gateway Key>

{"contents":[{"role":"user","parts":[{"text":"Reply with OK"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}
```

Responses 示例：

```json
{"model":"gemini-pro-agent","input":"Reply with OK","reasoning":{"effort":"low"},"store":false}
```

支持文本、流式输出和客户端函数工具，请求上限 1 MiB。原生 Gemini 的 `functionCall`、
`functionResponse` 和 `thoughtSignature` 必须完整保留；收到 `antigravity_new_session_required`
时新建会话。不要复用旧 CLI 工具历史，也不要填写绕过验证的伪签名。不开放 Claude、图片、
音视频、供应商搜索或代码执行，Antigravity compact 返回 501。

一次性迁移将八个 Gemini ID 和 `gpt-6.1-sol` 的用户权限/未来用户默认权限设为允许，倍率设为 1。
已受限 API Key 的白名单完全不变；仅包含退役名称的 Key 需要 Owner 明确重新配置。退役名称
不会自动变成新名称。其他 Codex 设置、汇率和历史账单保持不变。

Gemini 仅 Standard，按 Google API 家族价格进行本地等价计费，不代表 Antigravity 订阅账单。
完整价格、长上下文阈值、Sol 服务档位见 [CPA 原生模型与价格](cpa-native-models.md)。
