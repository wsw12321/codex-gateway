# CPA v8.0.77 升级验证记录

日期 2026-11-18。CLIProxyAPI 从 `v8.0.76`（`7df56074b252384d878ec4fca840552c6fb5236e`）升级到
当时最新的 tag `v8.0.77`（`a5e8f69e56309e1ef6d6872e667da81fdfbce2ce`，上游 17 个提交、105 个
文件）。按[兼容层升级规程](compatibility-upgrades.md#升级-cpa)进行。

## 改动

- `deploy/codex-compat/plugin.lock.json`：tag、commit、模型目录摘要
  （`internal/registry/models/models.json`，`2ce8eb01…46a655`）和镜像标签
  `codex-gateway-compat:v8.0.77-a5e8f69e-8f3381084e4e1055-plugin`。残余补丁未改，摘要不变。
- `docker-compose.yml`、`plugin.Dockerfile`（模型目录摘要）、`THIRD_PARTY_NOTICES` 跟随锁文件。
  上游 `LICENSE`、`go.mod`、`go.sum` 均未变。
- 插件的 `go.mod`/`go.sum`：`go get …CLIProxyAPI/v8@v8.0.77 && go mod tidy`，只换了 CPA 版本；
  间接依赖仍只有 `moego v0.1.0-alpha.4`。插件源码未改。
- Gateway 代码、价格目录与数据库迁移都不变。

## 上游变化的评审

**模型目录。** Codex 的 team/plus/pro 三个目录新增 `gpt-6.2-astra` 与 `gpt-6.2-sol`（free 目录不变）；
Claude、Antigravity 以及其他 Provider 的目录与 v8.0.76 完全相同，没有删除或修改任何模型。新增的两个
Codex 模型没有审核过的价格，Gateway 不为它们定价，所以不会出现在 `/v1/models` 中，请求在准入时被拒绝。
Antigravity 目录未变，排除规则不需要调整。上游同时更新了给 Codex CLI 的模型元数据
（`codex_client_models.json`）和离线工具 `fetch_codex_models` 的 CLI 版本号，不影响 CPA 生成的上游请求。

**残余补丁。** `sdk/cliproxy/service_config.go` 未变，补丁仍是 v8.0.76 那处 13 行的上下文偏移，
`git apply` 无 fuzz 应用，改动 4 个文件，补丁内容与摘要不变。

**插件接口。** 均为增量，插件不改即可编译：

- `pluginapi.UsageAttribution` 与 `ExecutorResponse`/`ExecutorStreamResponse` 的 `Attribution` 字段
  （`687caf78`）：只供插件执行器在没有预先选号时报告所用凭证。本插件不注册执行器，不涉及。
- `UsageDetail` 新增 `CacheCreation5mTokens`、`CacheCreation1hTokens`（`f7b3ccdf`）：插件的
  `usage.handle` 只读取 `ResponseHeaders` 中的限额头，不读取 token 明细，额度推送不受影响。

**协议翻译（`34d8fe0f`、`e1384430`、`e21cf8a5`、`f08cb931`、`e3d19b24`）。** 多个翻译器改为
保留工具结果中的 PDF、图片、音频等媒体与文档块。Gateway 能到达的只有 Responses → Claude
（`claude_openai-responses_request.go`），即 Owner 开启 `claude_responses_enabled` 后 `claude-*`
模型走 `/v1/responses` 的路径（默认关闭）；其余翻译方向 Gateway 不转发。翻译仍由 CPA 完成，
Gateway 的计费与准入不读取请求内容，行为不变。

**其他。**

- payload 替换规则支持正则、system/developer 角色与 JSON 路径目标（`cc1d5d78`、`25501b6b`、
  `9f6672f5`）：默认关闭，入口脚本不配置 `payload`。
- 请求日志区分本地错误与上游响应（`1c0ec3ce`）：入口脚本设置 `request-log: false`、
  `logging-to-file: false`，不涉及。
- OAuth 模型别名改为区分大小写（`sdk/cliproxy/auth/oauth_model_alias.go`、`service_models.go`）：
  入口脚本不配置 `oauth-model-alias`，不涉及；`oauth-excluded-models` 的匹配不在此改动范围内，
  `TestAntigravityModelCatalog` 通过。
- 安全依赖下限仍然需要：v8.0.77 自带 `klauspost/compress` v1.17.4、`golang.org/x/crypto` v0.57.0、
  `golang.org/x/net` v0.60.0（与 v8.0.76 相同），构建按锁文件的下限升级。
- 缓存键核对（[升级记录](cpa-provider-cache-audit.md#升级记录)）：`git grep` 191 → 192 处，新增的
  一处是函数内的局部 map；两处缺口的代码未变。

## 测试

**构建步骤（本机原样执行 `plugin.Dockerfile` 的构建阶段，Go 1.26.11、`CGO_ENABLED=1`）：**

- 获取 `v8.0.77` tag 并确认其提交；残余补丁摘要核对、`git apply --check` 与应用、
  `git diff --check`、只改 4 个文件；模型目录摘要核对；安全依赖按下限升级，`go mod verify` 通过。
- 残余补丁的 `TestGatewayPluginOnly*`（三个测试都在）；CPA 自己的 `internal/pluginhost`、
  `sdk/pluginapi`、`sdk/pluginabi`、`sdk/cliproxy/auth` 测试通过。
- 插件：`gofmt`、`go vet`（含 `-tags e2e`）、`go test -race` 通过；编译 CPA 与插件。
- 端到端测试 17 组全部通过，无跳过，包括 `TestAntigravityModelCatalog`、
  `TestProviderSessionsAreCallerScoped` 与 `TestQuotaPush`。

**镜像（`docker buildx build -f deploy/codex-compat/plugin.Dockerfile --target build`，Docker 29.10.0）：**
在锁定的 `golang:1.26.11-bookworm` 镜像中执行同一构建阶段（会话的出网代理使用自签 CA，验证用的
Dockerfile 副本只多了信任该 CA 的几行，其余与仓库中的文件逐字相同）：补丁、CPA 测试、插件测试与
同一组 17 个端到端测试全部通过。

**部署与 Gateway：**

- `python3 -m unittest discover -s scripts/tests -p 'test_*.py'`：100 项通过（1 项按设计跳过），
  其中 `test_cpa_plugin_deploy` 核对锁文件、残余补丁、Dockerfile、入口脚本与 Compose 一致。
- `./scripts/validate-compose.sh`：按 CI 方式生成非生产 `.env`，`direct`、`relay`、`shadowsocks`
  三种出口模式各自加不加 OIDC 均通过（含 Squid 与 Caddy 的镜像内解析），`compose.sh config` 通过；
  `.env` 仍选择 `CPA_INTEGRATION=patch` 时 `compose.sh` 拒绝执行。
- 插件模块：`go vet ./...`、`go test -race -count=1 ./...` 通过；按 CI 方式运行 gosec v2.22.8，0 个问题。
- Gateway 的 `gofmt`、`go vet ./...`、`go test ./...` 通过（Gateway 代码未因升级改动）。

## 未验证

- 运行阶段镜像与 `./scripts/test-cpa-plugin-image.sh`：本次验证环境的网络策略不允许访问 Debian
  软件源，`runtime-base` 的 `apt-get` 无法执行。运行阶段与 v8.0.76 相比只换了 CPA 二进制和插件，
  由 CI 的镜像构建与探针覆盖。
- govulncheck：验证环境无法访问 `vuln.go.dev`，由 CI 运行。
- CI 中的 Trivy 扫描与镜像发布。
- 真实账号验收（[路线图第 3 步](cpa-plugin-roadmap.md#3-维护窗口内的真实账号验收)）。部署时会重置
  进程内的会话绑定；凭证卷不需要转换。
