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

重新打开终端并运行 `agy --version`。当前网关兼容验收以官方 AGY `1.2.12` 为基线，
官方安装器可能安装更新版本。

复制管理界面 agy CLI 部分的配置命令，在终端执行并按提示输入 Gateway Key。
配置器保留用户级 `~/.gemini/antigravity-cli/settings.json` 的其他字段并合并：

```json
{"modelProvider": "gemini"}
```

同时持久保存 `GOOGLE_GEMINI_BASE_URL=<当前站点 origin>`（不加 `/v1`）和
`GEMINI_API_KEY`。Windows 使用当前用户的环境变量；macOS / Linux 使用权限为 `0600`
的独立凭据文件，并幂等添加 Bash / Zsh 的加载配置。修改前备份，可重复配置。
配置解析失败、取消输入或 Codex 登录失败时会明确报错，按错误提示修复后可重试。

关闭并重新打开终端；Windows Terminal 用户应退出整个应用后重开，确保读到用户
环境变量。然后运行：

```sh
agy --model gemini-3.1-pro-high
```

发送“请回复 OK”，再到“使用统计”查看请求。管理员需先启用路由，并为账号及 Key
授予 `gemini-3.1-pro-high` 权限。

原生 Gemini 入口兼容 AGY `1.2.12` 的以下请求名称：

| AGY 请求名称 | 实际模型 |
| --- | --- |
| `gemini-3.1-pro-preview` | `gemini-3.1-pro-high` |
| `gemini-3.1-pro-preview-customtools` | `gemini-3.1-pro-high` |
| `gemini-3.1-flash-lite-preview`（标题） | `gemini-3.1-pro-high` |

权限、额度、路由、响应校验和计费均使用实际模型；标题等辅助请求同样计入该模型用量。
兼容名称不加入模型目录，Responses 接口仍要求使用目录中的精确 ID。

## Gemini API

Gateway 也支持 `POST /v1/responses` 及原生 Gemini 的
`POST /v1beta/models/{model}:generateContent` 和
`POST /v1beta/models/{model}:streamGenerateContent?alt=sse`。
客户端使用 Gateway 签发的 Key；服务器内部通过官方 AGY 调用订阅账号。

以 Key 查询 `GET /v1/models` 可获取实际可用模型。目录包含
`gemini-3.8-flash-high`、`gemini-3.8-flash-medium`、`gemini-3.7-flash-high`、
`gemini-3.7-flash-medium`、`gemini-3.6-flash-high`、`gemini-3.6-flash-medium` 和
`gemini-3.1-pro-high`；实际可用性取决于用户权限、Key 白名单及订阅账号。
新模型默认禁用，管理员需重新授权并重新签发受限 Key。

完整 Responses 与原生 Gemini 请求示例见
[精确模型名请求](gemini-pro.md#使用精确模型名请求-gateway)。支持文本和客户端函数工具，
请求上限 1 MiB；流式响应在完整生成并校验后发送。计费使用 Gemini API Standard
家族价格对应的 Token 等价费用，不代表 Antigravity 订阅实际账单。
