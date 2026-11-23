# 客户端配置

管理界面「使用指导」默认打开 OpenCode 指南。其他客户端的标签先列出公共准备，再按操作系统展示安装步骤。

| 客户端 | CLI | 桌面端 | IDE 扩展 |
| --- | --- | --- | --- |
| OpenCode | OpenCode CLI | OpenCode Desktop | VS Code OpenCode 扩展 |
| OpenAI Codex | Codex CLI | Codex 桌面端（Windows 已验证） | VS Code Codex 扩展 |
| Claude | Claude Code | 第三方工具 CC Switch | VS Code Claude Code 扩展 |
| Google Antigravity | agy CLI | 不提供 | 不提供 |

Codex、Claude 和 Google Antigravity 的操作系统标签为 Windows、macOS 和 Linux，只切换与系统相关的安装命令和路径说明；
配置命令对三种系统相同。页面按浏览器平台（`navigator.userAgentData.platform`、
`navigator.platform`、`userAgent`）选择初始系统，无法识别时使用 Windows。所选标签只
保存在页面内存中，刷新数据不会重置，也不写入浏览器存储。两组标签都支持方向键、
Home 和 End。

复制的一键配置命令会使用当前站点地址，并在终端提示输入 Gateway API Key；Key 不进入
复制命令、命令历史或网页存储。

## OpenCode

1. 在「使用指导 → OpenCode」下载 [OpenCode 最新版 Windows 桌面端](https://opencode.ai/download)，按 Windows 的系统类型选择 x64 或 ARM64。
2. 在「API Keys」创建或查看当前设备的 Gateway Key。点击「下载配置脚本」，可分别选择 Plan 规划和 Build 写代码的模型。Plan 优先预选 `gpt-6.2-astra`，Build 优先预选 `gpt-6-astra`。账号目录没有首选模型时会预选其他可配置模型，两种模式也可以使用同一个模型。
3. 用 Windows PowerShell 运行下载的 `configure-opencode.ps1`，按提示输入 Gateway Key。脚本使用当前 Key 查询 `/v1/models`，确认所选模型可用后备份并合并用户配置。Key 由本机单独保存，下载文件只包含站点地址与所选模型。
4. 完全退出并重新打开 OpenCode，创建会话，在模型选择器核对本次请求的模型。OpenCode V2 的会话会单独记住选中的模型，使用已有会话时也可在模型选择器手动切换。到「使用统计」可以核对实际请求所用模型。

脚本使用 OpenCode V2 的 `agents.plan.model` 和 `agents.build.model`。GPT 和 Gemini
走 OpenAI Responses，Claude 走 Anthropic Messages，两种供给方的 Base URL 都是
`<站点 origin>/v1`。VS Code 的 [OpenCode 扩展](https://marketplace.visualstudio.com/items?itemName=sst-dev.opencode)
在集成终端运行 CLI。Windows 可在 PowerShell 或 CMD 执行 `npm install -g @opencode/cli`。
Linux 的安装和用户级配置示例见 [OpenCode 安装](https://opencode.ai/download)和[供应商配置](https://opencode.ai/docs/providers/#custom)。

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

## Codex 桌面端

Codex 桌面端与 Codex CLI 共用用户配置目录（Windows 为 `%USERPROFILE%\.codex`，
macOS / Linux 为 `~/.codex`，设置 `CODEX_HOME` 时使用该目录）。先安装 Codex CLI 并
执行上节的配置命令，再完全退出并重新打开桌面端。配置器需要调用 Codex CLI 校验配置并
保存凭据，因此只用桌面端也要先安装 CLI。

- Windows：从 Microsoft Store 安装，或在 CMD 运行
  `winget install --id 9PLM9XGG6VKS -s msstore`。管理员已在 Windows 桌面端验证改写
  `config.toml` 的接入方式。若在桌面端设置中把 agent 切换为 WSL，它读取 WSL 内的
  Linux 用户目录，需要在 WSL 中安装 CLI 并执行配置命令。
- macOS：从 [官方页面](https://developers.openai.com/codex/app) 下载。
- Linux：官方桌面端为预览版，支持 Ubuntu / Debian、Fedora 和 Arch，安装方式见
  [官方说明](https://developers.openai.com/codex/linux/linux-app)。

macOS 和 Linux 桌面端读取同一配置文件，但本站尚未在这两个平台验证。

## Codex IDE 扩展

在 VS Code 扩展视图搜索 OpenAI 发布的 “Codex” 扩展并安装，参见
[官方 IDE 说明](https://developers.openai.com/codex/ide)。扩展与 CLI 共用配置目录：
执行 Codex CLI 配置命令后运行 `Developer: Reload Window`。VS Code 通过 WSL 远程连接
时，扩展读取 WSL 内的配置目录，需在 WSL 中执行配置命令。

## Claude Code

安装命令按系统区分：Windows CMD 使用
`curl -fsSL https://claude.ai/install.cmd -o install.cmd && install.cmd && del install.cmd`，
macOS / Linux 使用 `curl -fsSL https://claude.ai/install.sh | bash`。配置器写入的内容、
冲突检查和模型选择见 [Claude Code 接入与账号管理](claude-code.md#用户配置)。

## Claude 桌面端（CC Switch）

页面推荐开源桌面工具 CC Switch，只链接其官网 <https://ccswitch.io>，并提示官方渠道
仅为该网站和 GitHub 仓库 `farion1231/cc-switch`。用户按官网说明自行配置，需要填写：

- 请求地址：当前 Gateway 站点 origin，不加 `/v1`；
- API Key：本站签发的 Gateway Key；
- 模型：中转站目录中已授权的 `claude-*` 精确模型 ID。

CC Switch 不由本站或 Anthropic 提供，Key 由它保存在本机。它和配置命令都会修改 Claude
用户配置，应选择其中一种方式管理，避免互相覆盖。本站没有验证 CC Switch。

## Claude Code IDE 扩展

在 VS Code 安装 Anthropic 发布的 “Claude Code” 扩展（`anthropic.claude-code`），参见
[官方 VS Code 说明](https://code.claude.com/docs/en/vs-code)。扩展与 CLI 共用用户级
`settings.json`，执行 Claude Code 配置命令即可写入 Gateway 地址、Key 和模型；只用
扩展时无需安装 CLI，但仍需 Node.js 运行配置命令。随后在 VS Code 设置中勾选
“Claude Code: Disable Login Prompt”（`claudeCode.disableLoginPrompt`），让扩展跳过
Claude 账号登录。不要把 Gateway Key 写入 VS Code 设置。

## agy CLI

Google Antigravity 标签页只提供 agy CLI。按管理员决定，页面不提供 Antigravity 桌面端
或 IDE 的配置方式。

按 [agy 官方安装说明](https://antigravity.google/docs/cli/install/) 选择系统对应命令。
Windows 使用 CMD：

```bat
curl -fsSL https://antigravity.google/cli/install.cmd -o install.cmd && install.cmd && del install.cmd
```

macOS / Linux：

```sh
curl -fsSL https://antigravity.google/cli/install.sh | bash
```

AGY 1.2.14 实测选择 `gemini-3.8-flash-high` 时，主请求仍发送
`gemini-3.8-flash`，标题请求发送 `gemini-3.1-flash-lite-preview`。Gateway 在原生
Gemini 入口将这两个请求分别解析为 `gemini-3.8-flash-high` 和
`gemini-3.1-flash-lite`，再检查各自的权限、额度并计费。Flash 别名仅接受 High
默认推理参数（预算省略或 `-1`，推理级别省略或 `HIGH`）；Medium 旧模型保持不可用，
其他自定义推理参数必须使用精确原生 ID。标题请求也需要 Lite 模型权限。

其他客户端应保留 CPA 原生模型 ID。旧 AGY 的 Pro preview/customtools 名称仍不兼容，
请使用支持原生 Gemini/Responses 的客户端。配置器仅配置 Gateway 地址和 Key，
不会伪造工具签名；旧 CLI 会话请新建。

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
目录最多公开五个原生 Antigravity ID：`gemini-3-flash`、`gemini-3.8-flash-high`、
`gemini-3.1-flash-lite`、`gemini-3.5-flash-lite` 和 `gemini-4-argon-high`。
基础 `gemini-4-argon` 使用上游 `gemini` provider，Muse Wonder 使用 `meta` provider；
两者本次均未接入，没有价格、默认权限或路由。旧 Pro、3.6 和 3.7 模型只保留历史记录及受控回滚数据，
不出现在当前目录。

原生请求示例：

```http
POST /v1beta/models/gemini-3.8-flash-high:generateContent
Content-Type: application/json
Authorization: Bearer <Gateway Key>

{"contents":[{"role":"user","parts":[{"text":"Reply with OK"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}
```

Responses 示例：

```json
{"model":"gemini-3.8-flash-high","input":"Reply with OK","reasoning":{"effort":"low"},"store":false}
```

支持文本、流式输出和客户端函数工具，请求上限 1 MiB。Argon-high 上游目录中的图片、音频、视频和
web search 能力不会绕过 Gateway 的现有请求校验。原生 Gemini 的 `functionCall`、`functionResponse` 和 `thoughtSignature`
必须完整保留；收到 `antigravity_new_session_required` 时新建会话。不要复用旧 CLI 工具历史，也不要填写
绕过验证的伪签名。Claude 请求使用 Messages API，不通过 Gemini 路径转发。

一次性迁移 `0035_cpa_v8_0_30_model_catalog.sql` 激活 Haiku 5.5 与 Argon-high 两个新增模型并初始化现有用户权限；
`gemini-pro-agent`、`gemini-3.1-pro-low`、`gemini-3.6-flash-high`、`gemini-3.7-flash-high` 和
`gemini-3.7-flash` 退出活动目录。已受限 API Key 的白名单完全不变；仅包含退役名称的 Key 需要 Owner
明确重新配置。退役名称不会自动变成新名称。其他 Codex 设置、汇率和历史账单保持不变。

Gemini 仅 Standard，按 Google API 家族价格进行本地等价计费，不代表 Antigravity 订阅账单。
完整价格、长上下文阈值、Sol 服务档位见 [CPA 原生模型与价格](cpa-native-models.md)。
