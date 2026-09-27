# Codex CLI 与 AGY 客户端配置

每台设备在管理界面创建独立 API Key，需要区分项目时为 Key 设置默认项目。

Codex 可使用管理界面“使用指导”的两种一键入口：macOS / Linux 复制命令到终端
执行，Windows 下载并运行 `configure-codex.bat`。脚本会先把已有配置备份为
`config.toml.bak`，再添加或替换顶层 `openai_base_url`；其他配置保持不变。

仓库内的脚本模板位于：

- `internal/server/assets/configure-codex.sh`
- `internal/server/assets/configure-codex.bat`

需要手工配置时使用以下内容：

```toml
openai_base_url = "https://codex.example.com/v1"
```

把域名替换为实际部署域名，然后执行 `codex login --with-api-key` 并按照 Codex
CLI 的输入流程提供 Gateway API Key。不要把 Key 写进可提交的 TOML、shell
profile、命令历史或项目 `.env`。设备丢失时在另一台已认证设备上立即停用或永久
删除该设备的 Key，并撤销对应会话。停用可在设备找回后重新启用；删除不可恢复，
但既有用量和账务历史仍保留安全引用。

脚本不会删除旧版生成的 `model_provider = "gateway"` 或
`[model_providers.gateway]`。如果本机仍显式启用了旧 provider，请手工处理该旧
配置，以免它继续优先于 `openai_base_url` 生效。

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

## AGY 使用 Antigravity 订阅

在现有 `~/.gemini/antigravity-cli/settings.json` 中合并顶层
`"modelProvider": "gemini"`，保留其他设置。将 `GOOGLE_GEMINI_BASE_URL` 设为
Gateway 站点地址（例如 `https://codex.example.com`，**不加 `/v1`**），
`GEMINI_API_KEY` 使用 Gateway 签发的 key，然后运行：

```sh
agy --model gemini-3.1-pro-high
```

AGY 使用 `POST /v1beta/models/{model}:generateContent` 和
`POST /v1beta/models/{model}:streamGenerateContent?alt=sse`，通过 Bridge 使用服务器
订阅账号。管理员需配置路由并授予 `gemini-3.1-pro-preview` 模型权限；同一会话中的
`gemini-3.1-pro-preview-customtools` 归一为该公开模型，共用价格与额度。
首版支持文本和本地编程工具，请求上限 1 MiB，流式响应在完整生成后发送。自动标题
的 Flash Lite 返回 404，不阻断主对话。完整配置、能力边界及验收步骤见
[AGY 接入说明](gemini-pro.md#本机-agy-配置)。
