# Codex CLI 客户端配置

每台设备在管理界面创建独立 API Key，需要区分项目时为 Key 设置默认项目。

管理界面的“使用指导”会提供两种一键入口：macOS / Linux 复制命令到终端
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

支持的数据请求为：

- `POST /v1/responses`
- `POST /v1/responses/compact`
- `GET /v1/models`

`openai_base_url` 覆盖的是 Codex 内置 `openai` provider 的地址。Codex 可能在新
会话开始时先用受同一 API Key 保护的 `GET /v1/responses` 探测 Responses
WebSocket；Gateway 会返回一次 `426 responses_websocket_unsupported`，客户端随即
改用正常的 `POST /v1/responses` HTTPS/SSE。这是预期的传输协商，不消耗配额、
余额或并发名额，也不会产生 usage 记录或请求 sidecar。

Gateway 不实现真正的 WebSocket、Chat Completions 或任意 URL 代理。Codex 的
文件读取、命令执行和代码修改仍发生在本地设备。
