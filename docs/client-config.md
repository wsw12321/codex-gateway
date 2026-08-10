# Codex CLI 客户端配置

每台设备在管理界面创建独立 API Key，并把 Key 只保存到该设备的凭证环境中：

```sh
export CODEX_GATEWAY_API_KEY='cgk_v1_...'
export CODEX_GATEWAY_PROJECT='my-project'
```

Codex 配置：

```toml
model_provider = "gateway"

[model_providers.gateway]
name = "Personal Codex Gateway"
base_url = "https://codex.example.com/v1"
env_key = "CODEX_GATEWAY_API_KEY"
wire_api = "responses"
env_http_headers = { "X-Codex-Project" = "CODEX_GATEWAY_PROJECT" }
request_max_retries = 2
stream_max_retries = 2
```

把域名替换为实际部署域名。不要把 Key 写进可提交的 TOML、shell profile、
命令历史或项目 `.env`。设备丢失时在另一台已认证设备上立即撤销该设备的 Key
和会话。

首版只支持：

- `POST /v1/responses`
- `POST /v1/responses/compact`
- `GET /v1/models`

不支持 WebSocket、Chat Completions 或任意 URL 代理。Gateway 仅转发模型请求；
Codex 的文件读取、命令执行和代码修改仍发生在本地设备。

