# CPA v8.0.69 升级验证记录

日期 2026-11-13。CLIProxyAPI 从 `v8.0.59`（`c5251b57b3fa5dcbc689f514f4aa166daacb4481`）升级到
当时最新的 tag `v8.0.69`（`f7ae8feeb3710319f524730849fbe32fde1751e4`，上游 100 个提交、382 个
文件）。这是删除源码补丁镜像后的第一次升级，只涉及插件镜像，按
[兼容层升级规程](compatibility-upgrades.md#升级-cpa)进行。

## 改动

- `deploy/codex-compat/plugin.lock.json`：tag、commit、模型目录摘要
  （`internal/registry/models/models.json`，`e47744d8…b98616c`）和镜像标签
  `codex-gateway-compat:v8.0.69-f7ae8fee-8f3381084e4e1055-plugin`。残余补丁未改，摘要不变。
- `docker-compose.yml`、`plugin.Dockerfile`（模型目录摘要）、`THIRD_PARTY_NOTICES`、插件的
  `go.mod`/`go.sum` 跟随锁文件。插件源码未改。
- Antigravity 排除规则增加 `gemini-3.8-flash-tiered`、`gemini-4-carbon-high`、
  `gemini-4-hydrogen-xhigh`（入口脚本、端到端 harness、镜像探针、部署静态校验与文档同步）。
- `scripts/test-cpa-plugin-image.sh` 把锁文件中的版本与提交传给镜像探针，探针不再写死版本。

## 上游变化的评审

**模型目录。** 没有删除任何模型，所以 Gateway 的价格目录、权限和迁移都不需要变。新增：

| Provider | 新增模型 | 处理 |
|---|---|---|
| Antigravity | `gemini-3.8-flash-tiered`、`gemini-4-carbon-high`、`gemini-4-hydrogen-xhigh` | 加入 `oauth-excluded-models`，评审定价前账号不注册 |
| Claude | `claude-fable-5-2` | 未定价，Gateway 不公开 |
| gemini、vertex、aistudio、meta | `gemini-omni-1.2-flash`、`gemini-4-carbon`、`gemini-4-hydrogen`、`muse-ember-1.0` | 这些 Provider 未接入 |

Antigravity 的新模型按排除规则处理而不是改用通配：`gemini-4-*` 会同时遮住已审的
`gemini-4-argon-high`。

**插件接口与残余补丁。** `sdk/pluginapi`、`sdk/pluginabi` 增加了字段，`conductor_selection.go`
改了 648 行；插件源码不改即可编译，残余补丁无 fuzz 应用。上游 `ecebc713` 让调度插件拒绝后
仍保留请求重试轮次；部署的 `request-retry: 0`，行为不变，`TestFailsClosed` 通过。

**Claude Thread 续接（新功能）。** 带 `thread.previous_message_id` 的 `/v1/messages` 续接请求
被固定到上一轮成功的账号。固定只缩小候选，`pickViaPluginScheduler` 仍调用插件，Gateway 仍
核对用户权限和账号池；不允许时请求失败，不会换号也不会绕过分配。新增的两份进程级状态
（账号归属、beta 画像）键都含调用方作用域，已补入
[缓存键核对](cpa-provider-cache-audit.md)第 10、11 项。原有两处缺口的代码未变，仍待提交上游。

**新端点 `/v1/tunnel`。** 多租户 WebSocket 数据交换，无条件挂载，只接受 CPA 的 `api-keys`
（Gateway 的内部数据面密钥），只在兼容网络可达；Gateway 不转发该路径。已写入
[威胁模型](threat-model.md#cpa-插件模式默认)。

**其他。** 执行器与 OAuth 代码中没有新增的供应商主机；插件依赖的管理接口（`auth-files`、
`oauth-excluded-models`、`model-definitions`、授权相关接口）未改；新增的配置项
（`server-side-fallback`、`sensitive-words-replace`、`shutdown-timeout` 等）默认关闭或不影响
生成的配置。安全依赖下限仍然需要：v8.0.69 自带的 `klauspost/compress` v1.17.4、
`golang.org/x/crypto` v0.57.0、`golang.org/x/net` v0.60.0 都低于锁文件的下限，构建按下限升级。

## 测试

按 `plugin.Dockerfile` 的构建步骤在本机原样执行（Go 1.26.x、`CGO_ENABLED=1`）：

- 获取 `v8.0.69` tag 并确认其提交；残余补丁摘要核对、`git apply --check` 与应用、
  `git diff --check`、只改 4 个文件；模型目录摘要核对；安全依赖按下限升级，`go mod verify` 通过。
- 残余补丁的 `TestGatewayPluginOnly*`；CPA 自己的 `internal/pluginhost`、`sdk/pluginapi`、
  `sdk/pluginabi`、`sdk/cliproxy/auth` 测试通过。
- 插件：`gofmt`、`go vet`（含 `-tags e2e`）、`go test -race` 通过；编译 CPA 与插件。
- 端到端测试 16 组全部通过，无跳过，包括 `TestAntigravityModelCatalog` 与
  `TestProviderSessionsAreCallerScoped`。
- 反向验证：去掉新增的三条排除规则后，`TestAntigravityModelCatalog` 失败，报告账号多注册了
  这三个模型。

部署与 Gateway：

- `python3 -m unittest discover -s scripts/tests -p 'test_*.py'` 通过；
  `./scripts/validate-compose.sh` 用 CI 方式生成的非生产配置完整通过；CI 的工具链一致性与
  上游声明步骤在本地按原样执行通过。
- Gateway 的 `go test ./...` 通过（Gateway 代码未因升级改动）。

## 未验证

- 镜像构建、`test-cpa-plugin-image.sh` 与 Trivy 在 CI 中运行。
- 真实账号验收（[路线图第 3 步](cpa-plugin-roadmap.md#3-维护窗口内的真实账号验收)）。部署时会重置
  进程内的会话绑定；凭证卷不需要转换。
