# CPA 插件模式路线图

本文记录插件模式从“可选”走到“默认”还需要做的事，以及每一步的验收标准。插件模式的
设计与升级规程见[兼容层升级规程](compatibility-upgrades.md#插件模式可选尚非默认)，已完成
的验证见[验证记录](cpa-plugin-migration-validation.md)。

## 现状（2026-11-12）

已经具备：

- 插件 `deploy/codex-compat/plugin/` 承担选号、身份隔离、会话绑定、并发名额、账号控制
  （启用/禁用、额度锁）、账号独立出口、归因上报。
- 残余补丁 139 行，只新增 `routing.strategy: "plugin-only"`，同一份补丁可无 fuzz 应用到
  v8.0.59 和 v8.0.67。
- Gateway 的 `CPA_INTEGRATION=plugin`：请求账本归因、`/v0/management/gateway/*` 路由映射、
  出口推送。默认仍是 `patch`。
- 构建（`plugin.Dockerfile`）、部署（`deploy/cpa-plugin.override.yml`）、CI 步骤、镜像探针、
  部署静态校验。
- 自动化测试：插件单测与竞态检测、9 组端到端测试、Gateway 单测、部署静态校验，均通过。

尚未具备：

- 上游额度查询、模型鉴定、“CPA 授权管理”页面的凭证操作（新增/重新授权、导入、刷新、
  删除，以及先选出口再授权的登记流程）。
- 镜像只在 CI 构建，尚未有一次成功的 CI 运行。
- 没有真实账号验收。
- CPA 内部各 Provider 缓存键是否按调用方隔离尚未逐个核对。

## 步骤

按顺序执行。每一步单独提交，并在[验证记录](cpa-plugin-migration-validation.md)中补充
结果。

### 1. 升级 Go 工具链到 1.26.11，让 CI 恢复绿色

**原因。** govulncheck 报告 GO-2026-6859（`net/http`，1.26.11 修复）。这是既有问题，
Gateway module 在本次迁移之前就会命中；插件 module 的扫描步骤也会因此失败。

**做法。** 按 `CLAUDE.md` 的 “Go or base-image version bump” 清单一次改齐：

- `go.mod` 的 `toolchain`（插件 `go.mod` 的 `go` 指令跟随 CPA 要求的最低版本，不用改）；
- `.github/workflows/ci.yml` 的 `GO_VERSION`；
- `deploy/images.sources`，然后运行 `./scripts/lock-images.sh` 更新 `deploy/images.lock.env`；
- 六个 Dockerfile 的 `ARG GOLANG_IMAGE=`（含 `deploy/codex-compat/plugin.Dockerfile`）；
- `deploy/codex-compat/source.lock.json`、`deploy/codex-compat/plugin.lock.json`、
  `deploy/shadowsocks/upstream/source.lock.env` 中的 Go 版本。

**验收。** `./scripts/lock-images.sh --check` 通过；两个 module 的 govulncheck 均无发现；
CI 的工具链一致性检查通过。

### 2. 让 CI 完整跑通插件镜像

**做法。** 推送后观察 `Build and verify plugin-mode CPA image` 与 Trivy 步骤。本地没有
Docker 守护进程，以下几处只有 CI 能暴露问题，需重点看：

- 构建阶段的 `go test -race` 与端到端测试在 `golang:*-bookworm` 镜像内能否运行（端到端
  需要回环网络，不需要外网）；
- 运行镜像中 `/usr/local/lib/cliproxy/plugins` 的属主与权限是否如预期（`0:0`、`0444`）；
- `scripts/test-cpa-plugin-image.sh` 的两种模式；
- Trivy 对新镜像的结果。

**验收。** CI 全绿，`scripts/test-cpa-plugin-image.sh` 输出两行通过信息。

### 3. 维护窗口内的真实账号验收

**做法。** 按[运维手册](operations.md#cpa-插件模式的切换与回退)切换。验收期间不要使用
尚未迁移的功能；需要维护凭证时先回退到补丁镜像。

**验收清单。**

| 项 | 通过条件 |
|---|---|
| 模型列表 | Codex、Claude、Antigravity 模型与补丁镜像一致 |
| Responses 普通与 SSE、compact | 成功；请求明细中的上游账号、会话哈希、扣费层正确 |
| `/v1/messages`、Claude 模型走 `/v1/responses` | 成功；Claude 账号归因正确 |
| Gemini 原生 `/v1beta/models/*` | 成功；Antigravity 账号归因正确 |
| 群组账号池与扣费层 | 群组请求只落在群组账号上，耗尽后按顺序落到下一层 |
| 并发上限 | 达到上限时返回 `upstream_concurrency_exceeded`，释放后恢复 |
| 禁用与重新启用 | 立即生效；重启 sidecar 后保持 |
| 额度锁 | 真实 `usage_limit_reached` 后账号锁定，Owner 启用后恢复（如验收期间自然出现） |
| 账号独立出口 | 绑定节点后，流量与刷新都经该节点（看出口运行时的连接记录）；解绑后回到全局 |
| 刷新后重启 | 凭证刷新后重启 sidecar，账号仍可用 |
| 回退演练 | 去掉覆盖文件回到补丁镜像，账号状态、禁用与额度锁保持 |

### 4. 迁移额度查询

**原因。** 管理页面的额度列和 Owner 的额度查询依赖它。

**做法。** CPA v8 的 `POST /v0/management/quota/fetch` 把额度查询委托给声明了
`QuotaProvider` 能力的插件。先评估让本插件实现 `QuotaProvider`，并在插件路由
`/v0/management/gateway/{namespace}/quota?id=` 中按补丁镜像的严格格式输出（Codex 的
`account/rateLimits/read` RPC 信封、脱敏规则、固定错误码）。上游请求必须走该账号的出口，
不跟随重定向，不返回原始上游文本。Claude 的被动额度快照需要确认插件能否从 usage 记录的
响应头中取得。

**验收。** Gateway 现有的额度解析测试在插件模式下通过；端到端增加额度场景（假上游返回
各类合法与非法额度响应）。

### 5. 迁移“CPA 授权管理”页面的凭证操作

**做法。** 插件路由包装 CPA 原生管理接口，Gateway 的 `/internal/gateway-management/*`
映射过去，页面不改：

| 操作 | 原生接口 | 插件需补充的部分 |
|---|---|---|
| 新增/重新授权 | `GET /v0/management/{provider}-auth-url`、`/oauth-callback`、`/get-auth-status` | 原生接口接受请求体中的 `proxy_url` 作为本次授权的出口，可实现“先选出口再授权”；完成后按身份登记规则分配稳定账号 ID |
| 导入 | `POST /v0/management/auth-files` | 只接受刷新令牌等必要字段；导入后立即刷新并核验身份（Antigravity 的 Google subject、Anthropic 的账号与组织 UUID） |
| 刷新 | `POST /v0/management/auth-files/refresh` | 只针对单个账号 |
| 删除 | `DELETE /v0/management/auth-files` | 先禁用并等待进行中的请求结束，再删除；保留身份登记与控制状态 |

身份核验必须调用供应商的固定身份端点，且走该账号的出口。重复身份的处理沿用补丁镜像的
规则。

**验收。** Gateway 的 `TestCPAManagement*` 在插件模式下通过；端到端增加授权、导入、刷新、
删除场景（假 OAuth 端点）。

### 6. 迁移模型鉴定

**做法。** 插件路由 `/v0/management/gateway/model-identification/*`。探针用插件自己的
HTTP 客户端，读取该账号的凭证，走该账号的出口，直连固定的 Codex Responses URL；不刷新
凭证、不重放、不跨账号，沿用补丁镜像的单题超时、事件大小、输出长度和错误码限制。也可
评估 CPA 的 `host.model.execute` 回调能否固定到单个凭证，以减少自写的协议处理。

**验收。** Gateway 的模型鉴定测试在插件模式下通过；端到端增加探针场景。

### 7. 补齐剩余差异

- **Provider 缓存键。** 逐个阅读 CPA 中 Claude 工具别名、Gemini 签名、提示缓存等缓存的
  键，确认都包含调用方作用域与账号。有缺口时优先向上游提交修复，否则加入残余补丁并配
  测试。
- **Antigravity 模型限制。** 在 `plugin-entrypoint.sh` 生成的配置中用
  `oauth-excluded-models` 限定为五个已审模型，并加入部署静态校验。
- **后台刷新的出口窗口。** 绑定变更后、账号下一次被选中或收到推送前，后台刷新使用
  凭证中原来的代理。评估在插件中定期对账（例如每分钟对有绑定的账号调用一次
  `resolve-egress`），或在 Gateway 的出口同步循环中推送有变化的绑定。

### 8. 改为默认并清理

前提：第 1–7 步完成，真实账号验收通过，生产稳定运行至少两周。

**做法。**

1. 把插件镜像与 `CPA_INTEGRATION=plugin` 写入 `docker-compose.yml`；补丁镜像改为回退用的
   覆盖文件（参照 `deploy/legacy-cpa.override.yml`），保留至少 7 天。
2. 回退窗口结束后删除 `cliproxy-v8.0.*-gateway*.patch`、`Dockerfile` 中按名字断言的补丁
   测试清单，以及只为补丁镜像存在的 `/internal/*` 兼容代码路径。
3. 更新 `README.md`、`CLAUDE.md`、`docs/compatibility-upgrades.md`、
   `docs/threat-model.md` 与 `THIRD_PARTY_NOTICES`。

**验收。** CI 全绿；默认部署的镜像探针与端到端测试通过；回退覆盖文件可用。

### 9. 减少残余补丁并提前发现上游变化

- **向上游提交** `routing.strategy` 的“插件独占”选项（插件未处理时拒绝而非回退）。
  合并后残余补丁可删除。同时提交本次发现的问题：调度插件 `Reject` 被渲染为 HTTP 500 且
  错误码拼入 message。
- **定时 CI 任务。** 每天针对上游最新 tag 应用残余补丁、编译插件、运行单测与端到端测试，
  失败时开 issue。这样在决定升级前就知道需要改什么。

## 每次上游发版的例行流程

插件模式成为默认后，CPA 升级按以下步骤进行（细节见
[兼容层升级规程](compatibility-upgrades.md#升级-cpa)）：

1. 更新 `plugin.lock.json`、覆盖文件（或默认 Compose）中的 tag、commit、镜像标签，以及
   插件 `go.mod` 中的 CPA 版本。
2. 运行 `python3 -m unittest scripts.tests.test_cpa_plugin_deploy`。
3. 构建镜像。残余补丁应用失败时，按新代码调整插入点并更新锁文件中的 SHA-256；插件编译
   失败时，按插件接口的变化修改插件。
4. 运行 `scripts/test-cpa-plugin-image.sh`，再做真实账号冒烟。

2026-11-12 的 v8.0.67 演练中，第 3 步只需调整一次残余补丁的插入点，插件未改。

## 开放问题

| 问题 | 现状 | 影响 |
|---|---|---|
| 插件 panic 会终止 CPA 进程 | 插件在所有入口自行捕获；逃逸的 panic 仍会使进程退出 | 拒绝服务，不会放行。可观察容器重启次数 |
| 管理 API 开启 | 独立密钥，仅内部网络 | 管理密钥泄露等同凭证泄露，保管要求与 OAuth 卷相同 |
| 可执行的临时目录 | 64 MB tmpfs，仅服务用户可写 | 削弱“容器内无可写可执行目录”的加固 |
| CPA 原生换号规则 | 403 会换账号，补丁镜像不会 | 可用 `oauth-request-scoped-errors` 调整；视验收结果决定 |
| 执行器内的重定向与主机限制 | 只由出口白名单约束 | 若需保留，考虑向上游提交可配置项 |

## 明确不做

- 不迁移“无显式会话标识时按对话内容推导亲和”。它需要解析请求体；Codex CLI 与 Claude Code
  都带显式会话标识。
- 不迁移 Gemini 历史校验返回 `new_session_required`。改为上游原生错误。
- 不迁移 trace 日志脱敏、WebSocket 与 Home 相关改动；对应功能在部署中本就关闭。
