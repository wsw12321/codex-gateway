# CPA v8.0.76 升级验证记录

日期 2026-11-17。CLIProxyAPI 从 `v8.0.69`（`f7ae8feeb3710319f524730849fbe32fde1751e4`）升级到
当时最新的 tag `v8.0.76`（`7df56074b252384d878ec4fca840552c6fb5236e`，上游 60 个提交、661 个
文件）。按[兼容层升级规程](compatibility-upgrades.md#升级-cpa)进行。

## 改动

- `deploy/codex-compat/plugin.lock.json`：tag、commit、模型目录摘要
  （`internal/registry/models/models.json`，`fbca148e…6d319`）和镜像标签
  `codex-gateway-compat:v8.0.76-7df56074-8f3381084e4e1055-plugin`。残余补丁未改，摘要不变。
- `docker-compose.yml`、`plugin.Dockerfile`（模型目录摘要）、`THIRD_PARTY_NOTICES` 跟随锁文件。
  `plugin.Dockerfile` 另在运行阶段预先创建插件目录（见下文“镜像”）。
  上游 `LICENSE` 未变。
- 插件的 `go.mod`/`go.sum`：`go get …CLIProxyAPI/v8@v8.0.76 && go mod tidy`。插件源码未改。
  CPA 的 `sdk/pluginapi`、`sdk/pluginabi` 改用 `github.com/MoeForge/moego`（MIT，自身没有依赖）
  编解码 JSON，插件 `go.mod` 因此多出 `github.com/MoeForge/moego v0.1.0-alpha.4 // indirect`。
  v8.0.69 的 CPA 二进制已经通过 `replace` 使用同一库的 fork，新版改为直接依赖上游。
  `scripts/tests/test_cpa_plugin_deploy.py` 的依赖断言改为：直接依赖只有 CPA，间接依赖只有
  moego。

## 上游变化的评审

**模型目录。** Codex、Claude、Antigravity 三个已接入 Provider 的目录（ID 与元数据）与 v8.0.69
完全相同；只有未接入的 aistudio 新增 `gemini-4-argon`、`gemini-4-carbon`、`gemini-4-hydrogen`，
vertex 的 `gemini-4-hydrogen` 改了思考参数。价格、权限、迁移和 Antigravity 排除规则都不需要变。

**残余补丁。** `sdk/cliproxy/service_config.go` 在补丁插入点之前多了 3 行（配置应用后的回调），
补丁一处上下文偏移 13 行，`git apply` 仍无 fuzz 应用，改动 4 个文件，补丁内容与摘要不变。

**插件接口。** 均为增量，插件不改即可编译：

- 可选的 `SchedulerContinuity`（`7fcdfcf`）：Codex 账号都在等额度恢复，或输出前收到 429 时，
  让调度插件决定是否在 CPA 内等待（默认 30 秒、最长 120 秒）。只有调度插件声明了该能力时才
  运行（`canRunCodexContinuity` 要求 `HasSchedulerContinuity()`）；本插件不声明，行为不变，
  请求照常由 Gateway 的分配决定。
- `SchedulerPickResponse.ExcludeCandidates`（`22d4070`）：插件放弃处理或委托内置调度时可排除
  候选。本插件从不放弃或委托（要么选定账号，要么返回错误），不设置该字段。
- `pluginapi`/`pluginabi` 的 JSON 改用 moego，字段名与兼容的反序列化规则不变。

**新增数据面端点 `/muse-code/*`（`7fd90c7`）。** Muse Code CLI 的模型列表与转发到 Meta 的
三个辅助端点，挂在 CPA 的 `AuthMiddleware` 之后，与 `/v1/tunnel` 一样只接受 CPA 的
`api-keys`（Gateway 的内部数据面密钥），只在兼容网络可达；Gateway 不转发该路径。部署中没有
Meta 凭证，出口白名单也不含 Meta 主机。已写入[威胁模型](threat-model.md#cpa-插件模式默认)。

**Antigravity 违反服务条款（`5a351d9`、`e96d4c4`）。** 上游返回 `403 TOS_VIOLATION` 时，CPA
把该凭证标为停用并持久化；可选的“池级保护暂停”默认关闭（`antigravity.tos-pause-window` 为空），
入口脚本不配置。停用只影响出问题的那个账号，与 Gateway 的失败关闭方向一致。

**其他。**

- 默认关闭、入口脚本不启用的新配置：`sensitive-patterns-replace`（`905e70c`）、payload 用户文本
  替换（`f00a3d3`、`b4410dd`）。
- 管理密钥校验结果缓存 1 小时（`51b98e7`）：缓存绑定当前的 bcrypt 哈希，更换密钥时清空，无效
  密钥不缓存，失败计数与封禁不变。部署中更换 `CPA_MANAGEMENT_KEY` 本来就要重启容器。
- 管理接口上传/删除时保留插件凭证身份（`95ebe39`）、SIWC 绑定使用目标账号的代理（`3b48077`）、
  `MaxRetryCredentials`/`SelectAuthExcluding` 等调度辅助函数：端到端测试
  `TestCredentialOperations`、`TestPerAccountEgress`、`TestDeadEgressNeverFallsBack` 覆盖相关路径。
- 安全依赖下限仍然需要：v8.0.76 自带 `klauspost/compress` v1.17.4、`golang.org/x/crypto` v0.57.0、
  `golang.org/x/net` v0.60.0，构建按锁文件的下限升级。
- 缓存键核对（[升级记录](cpa-provider-cache-audit.md#升级记录)）：`git grep` 187 → 191 处，新增的
  两处进程级状态不含请求或调用方数据；两处缺口的代码未变。

## 测试

**构建步骤（本机原样执行 `plugin.Dockerfile` 的构建阶段，Go 1.26.11、`CGO_ENABLED=1`）：**

- 获取 `v8.0.76` tag 并确认其提交；残余补丁摘要核对、`git apply --check` 与应用、
  `git diff --check`、只改 4 个文件；模型目录摘要核对；安全依赖按下限升级，`go mod verify` 通过。
- 残余补丁的 `TestGatewayPluginOnly*`；CPA 自己的 `internal/pluginhost`、`sdk/pluginapi`、
  `sdk/pluginabi`、`sdk/cliproxy/auth` 测试通过。
- 插件：`gofmt`、`go vet`（含 `-tags e2e`）、`go test -race` 通过；编译 CPA 与插件。
- 端到端测试 17 组全部通过，无跳过，包括 `TestAntigravityModelCatalog` 与
  `TestProviderSessionsAreCallerScoped`。

**镜像（`docker buildx build -f deploy/codex-compat/plugin.Dockerfile`，Docker 29.9.2）：**
会话的出网代理只接受 HTTPS 且使用自签 CA，所以验证用的 Dockerfile 副本只多了信任该 CA 的几行
（并把 apt 源换成 HTTPS），其余与仓库中的文件逐字相同。构建内的全部检查通过，包括同一组 17 个
端到端测试；随后 `./scripts/test-cpa-plugin-image.sh` 两种模式都通过：正常挂载时插件完整性、
密钥分离、刷新锁、三个 Provider 的账号路由、账号控制与失败关闭检查通过；没有可执行临时目录时
插件不加载、容器不健康、能力探测失败、生成请求被拒绝。镜像内 `cli-proxy-api` 报告
`v8.0.76`／`7df56074…`。

第一次构建时探针报告插件不可读：这一版 Docker 的 BuildKit 会把 `COPY --chmod=0444` 同时作用于
它为目标创建的上级目录，`/usr/local/lib/cliproxy/plugins` 因此成为 `0444`，服务用户无法进入。
这与 CPA 版本无关（`COPY` 行未变），取决于构建器的版本；CI 用的构建器若不这样处理就不会暴露，
但用较新的 Docker 在服务器本地构建时，会得到一个插件无法加载、按设计失败关闭的镜像。`plugin.Dockerfile` 现在在 `runtime-base` 中以
`root:root 0555` 预先创建该目录，`COPY` 只设置文件本身的权限；修正后重新构建并通过探针。

**部署与 Gateway：**

- `python3 -m unittest discover -s scripts/tests -p 'test_*.py'`：100 项通过（1 项按设计跳过）。
- `./scripts/validate-compose.sh`：按 CI 方式生成非生产 `.env`，`direct`、`relay`、`shadowsocks`
  三种出口模式各自加不加 OIDC 均通过（含 Squid 与 Caddy 的镜像内解析），`compose.sh config` 通过。
- 插件模块：`go vet ./...`、`go test -race -count=1 ./...` 通过；按 CI 方式运行 govulncheck
  v1.1.4（无漏洞）与 gosec v2.22.8（0 个问题）。
- Gateway 的 `go test ./...` 通过（Gateway 代码未因升级改动）。

## 未验证

- CI 中的 Trivy 扫描与镜像发布。
- 真实账号验收（[路线图第 3 步](cpa-plugin-roadmap.md#3-维护窗口内的真实账号验收)）。部署时会重置
  进程内的会话绑定；凭证卷不需要转换。
