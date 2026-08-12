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
profile、命令历史或项目 `.env`。设备丢失时在另一台已认证设备上立即撤销该
设备的 Key 和会话。

脚本不会删除旧版生成的 `model_provider = "gateway"` 或
`[model_providers.gateway]`。如果本机仍显式启用了旧 provider，请手工处理该旧
配置，以免它继续优先于 `openai_base_url` 生效。

首版只支持：

- `POST /v1/responses`
- `POST /v1/responses/compact`
- `GET /v1/models`

不支持 WebSocket、Chat Completions 或任意 URL 代理。Gateway 仅转发模型请求；
Codex 的文件读取、命令执行和代码修改仍发生在本地设备。
