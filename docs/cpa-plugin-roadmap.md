# CPA 插件模式路线图

本文记录插件模式从“可选”走到“默认”还需要做的事，以及每一步的验收标准。插件模式的
设计与升级规程见[兼容层升级规程](compatibility-upgrades.md#插件模式可选尚非默认)，已完成
的验证见[验证记录](cpa-plugin-migration-validation.md)。

## 原则

以下三条约束所有步骤，与之冲突的旧方案已经改写。

1. **上游请求只由 CPA 生成并发出。** 推理、令牌刷新、OAuth 授权、模型列表和额度信息
   都来自 CPA 原生代码路径。插件和 Gateway 不构造 provider 请求（不写死 provider 的 URL、
   头部或 User-Agent，不自带 TLS 客户端），也不通过补丁改变 CPA 生成请求的方式。CPA 不
   提供的能力宁可不提供，也不自行实现。
2. **只保留残余补丁。** 补丁镜像的大补丁会被删除。残余补丁
   （`residual/cliproxy-plugin-only-selector.patch`）不改请求的内容和格式，只在插件没有
   选号时拒绝请求；它会提交给上游，合并后删除。
3. **账号出口失败时拒绝，不回退。** “出口服务器”功能保留。账号绑定的出口不可用时，
   该账号的请求、刷新和授权直接失败，绝不改走全局出口或直连。调度器仍可按原规则换另一个
   有权限的账号，并使用那个账号自己的出口。

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
- 自动化测试：插件单测与竞态检测、12 组端到端测试、Gateway 单测、部署静态校验，均通过。
- 账号出口不回退（第 7 步的出口部分）：每个凭证都带显式出口，CPA 不配置任何代理，
  插件后台对账。
- 额度只读取 CPA 的被动观测（第 4 步），插件不再自行发出供应商请求。
- “CPA 授权管理”页面的凭证操作经 CPA 原生管理接口完成，先选出口再授权（第 5 步）。
- 模型鉴定的探针是由 CPA 生成的普通请求，由 Gateway 固定到被鉴定的账号（第 6 步）。
- Go 工具链 1.26.11（第 1 步），两个 module 的 govulncheck 与 gosec 在本地均无发现。
- 插件镜像在 CI 中构建、测试、探针与 Trivy 扫描全部通过（第 2 步，PR #4）。

尚未具备：

- 没有真实账号验收。
- CPA 内部各 Provider 缓存键是否按调用方隔离尚未逐个核对。

## 步骤

第 1、2 步已完成。其余按 7（出口部分）→ 4 → 5 → 6 → 8 的顺序执行：凭证操作和模型鉴定
都依赖“每个凭证都带显式出口”。每一步单独提交，并在[验证记录](cpa-plugin-migration-validation.md)
中补充结果。

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

**状态（2026-11-12）。** 已完成，结果见
[验证记录](cpa-plugin-migration-validation.md#路线图第-1-步go-12611)。同时修复了插件
module 加入后仓库级 gosec 扫描必然失败的问题（根目录扫描排除插件目录）。CI 中的确认
并入第 2 步。

### 2. 让 CI 完整跑通插件镜像

**做法。** 推送后观察 `Build and verify plugin-mode CPA image` 与 Trivy 步骤。本地没有
Docker 守护进程，以下几处只有 CI 能暴露问题，需重点看：

- 构建阶段的 `go test -race` 与端到端测试在 `golang:*-bookworm` 镜像内能否运行（端到端
  需要回环网络，不需要外网）；
- 运行镜像中 `/usr/local/lib/cliproxy/plugins` 的属主与权限是否如预期（`0:0`、`0444`）；
- `scripts/test-cpa-plugin-image.sh` 的两种模式；
- Trivy 对新镜像的结果。

**验收。** CI 全绿，`scripts/test-cpa-plugin-image.sh` 输出两行通过信息。

**状态（2026-11-12）。** 已完成，PR #4 的 CI 第一次运行即全绿，结果见
[验证记录](cpa-plugin-migration-validation.md#路线图第-2-步插件镜像首次通过-ci)。

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
| 出口失败不回退 | 停掉绑定的节点后，该账号的请求失败，Squid 3128 上没有该账号的连接；其他账号照常 |
| 凭证中的出口 | 每个凭证都带显式 `proxy_url`（未绑定的账号为全局出口地址） |
| 刷新后重启 | 凭证刷新后重启 sidecar，账号仍可用 |
| 额度 | 有请求后，Codex 与 Claude 显示最近一次观测及观测时间；“CPA 授权管理”页面三个 Provider 的额度列可用（Antigravity 显示不可用） |
| 回退演练 | 去掉覆盖文件回到补丁镜像，账号状态、禁用与额度锁保持 |

### 4. 额度改为 CPA 的被动观测

**原因。** 第一次实现（[验证记录](cpa-plugin-migration-validation.md#路线图第-4-步额度查询)）
由插件自己向 `chatgpt.com/backend-api/wham/usage` 和 `cloudcode-pa` 发请求，URL、头部和
User-Agent 都是写死的，用的是 Go 标准 TLS 客户端，违反原则 1。

**做法。** CPA 会把 Codex（`x-codex-*`）和 Claude（`anthropic-ratelimit-unified-*`）
响应头记录进凭证的被动观测（`QuotaState.Signals`），这些头来自 CPA 自己发出的真实请求。

- 插件删除全部主动查询代码，改为读取 CPA 管理列表中的观测：Codex 映射成补丁镜像的
  `account/rateLimits/read` 信封并附加 `observed_at`，还没有观测时返回固定码
  `quota_not_observed`；Claude 维持现有的被动快照；Antigravity 没有被动来源，返回固定码
  `quota_unavailable`。
- Gateway 接受 `observed_at` 与上述错误码；控制台显示“最近观测于 …”或“暂无观测”。
  请求后的额度检查（`upstream_quota_guard`）读取的是该请求刚产生的观测。

**验收。** 插件的“信号 → 信封”表驱动测试（含非法值）；端到端测试中，假上游在普通
`/v1/responses` 上返回 `x-codex-*` 头后额度路由返回快照，且额度路由本身不产生任何上游请求；
Gateway 的额度解析与界面测试通过。

**状态（2026-11-12）。** 已完成，结果见
[验证记录](cpa-plugin-migration-validation.md#路线图第-4-步改写额度改为-cpa-被动观测)。

### 5. 迁移“CPA 授权管理”页面的凭证操作

**做法。** 插件路由经回环调用 CPA 原生管理接口，Gateway 的 `/internal/gateway-management/*`
映射过去，页面不改：

| 操作 | 原生接口 | 出口与身份 |
|---|---|---|
| 新增/重新授权 | `GET /v0/management/{provider}-auth-url?proxy_url=…`、`/get-auth-status` | 出口由 Gateway 的出口登记解析；CPA 经该出口完成授权，并把 `proxy_url` 写入凭证。解析失败时拒绝开始授权 |
| 导入 | `POST /v0/management/auth-files`（凭证带 `proxy_url`），然后 `POST /auth-files/refresh` | 刷新由 CPA 经该出口完成 |
| 刷新 | `POST /v0/management/auth-files/refresh` | 只针对单个账号 |
| 删除 | `DELETE /v0/management/auth-files` | 先禁用并等待进行中的请求结束，再删除；保留身份登记与控制状态 |

身份只取 CPA 保存在凭证中的字段（Codex 的 id_token 声明、Antigravity 由 CPA 获取的
用户信息与项目、Claude 的账号与组织），插件不调用 provider 的身份端点。缺少必要字段时拒绝
登记。重复身份的处理沿用补丁镜像的规则。

**验收。** Gateway 的 `TestCPAManagement*` 在插件模式下通过；插件管理路由单测；端到端增加
导入、刷新、删除场景（假 OAuth 端点经 TLS 拦截代理提供）。

**状态（2026-11-12）。** 已完成，结果见
[验证记录](cpa-plugin-migration-validation.md#路线图第-5-步凭证操作改用-cpa-原生管理接口)。
Antigravity 新增账号以 CPA 记录的邮箱为身份；Antigravity 与仅有访问令牌的 Claude 凭证不能
导入，需改用授权。

### 6. 迁移模型鉴定

**做法。** 模型鉴定的探针改为 CPA 的普通 `/v1/responses` 请求，上游请求由 CPA 生成：

- Gateway 在请求账本中把该请求令牌标记为固定到某个账号的探针，选号回调只回答这个账号；
  CPA 重试时该账号已试过，回调回答不可用，探针不会换到别的账号。
- 回答取自 CPA 标准的 Responses 输出，沿用现有的输出长度、超时和错误码限制。
- 账号可用的模型列表由插件路由包装 CPA 的 `GET /v0/management/auth-files/models`。
- 与补丁镜像的差异：CPA 可能按原生规则刷新令牌并在同一账号上重试，这一点接受。

**验收。** Gateway 的模型鉴定测试在插件模式下通过；端到端增加探针场景（固定账号成功；
固定账号失败时不换号）。

**状态（2026-11-12）。** 已完成，结果见
[验证记录](cpa-plugin-migration-validation.md#路线图第-6-步模型鉴定走-cpa-的普通请求路径)。
探针以一次性身份固定账号，Gateway 的分配回调只对它回答该账号。

### 7. 账号出口不回退，补齐剩余差异

- **出口不回退（最先做）。** 插件把 Gateway 的“继承全局出口”也写成凭证上的显式
  `proxy_url`（全局出口地址），CPA 的配置与环境中不再有全局代理。这样 CPA 任何忽略账号
  代理或代理无法构建时的退路都只剩直连，而 CPA 所在的网络没有外网，直连必然失败。
  Gateway 对推送失败的出口绑定持续重试，并在启动时推送全部账号，关闭“绑定变更后后台
  刷新仍用旧出口”的窗口。
- **Provider 缓存键。** 逐个阅读 CPA 中 Claude 工具别名、Gemini 签名、提示缓存等缓存的
  键，确认都包含调用方作用域与账号。有缺口时向上游提交修复；不加入残余补丁。
- **Antigravity 模型限制。** 在 `plugin-entrypoint.sh` 生成的配置中用
  `oauth-excluded-models` 限定为五个已审模型，并加入部署静态校验。

**验收（出口部分）。** 插件单测覆盖“全局出口写成显式地址”“写入失败排除账号”；端到端
测试中给账号绑定不可用的代理，该账号的请求失败、provider 没有经其他代理收到该账号的请求，
另一个账号照常成功；部署静态校验确认配置中没有 `proxy-url`、覆盖文件清空了代理环境变量。

**状态（2026-11-12）。** 出口部分已完成，结果见
[验证记录](cpa-plugin-migration-validation.md#路线图第-7-步出口部分账号出口不回退)。
Gateway 侧的推送重试改由插件的后台对账承担（启动后到确认全部凭证前每 2 秒一轮，之后
每分钟一轮）。

### 8. 改为默认并清理

**做法。**

1. 第 4–7 步完成后，把插件镜像与 `CPA_INTEGRATION=plugin` 写入 `docker-compose.yml`；
   补丁镜像改为回退用的覆盖文件 `deploy/patch-cpa.override.yml`（参照
   `deploy/legacy-cpa.override.yml`）。
2. 真实账号验收（第 3 步）通过并稳定运行 7 天后，删除 `cliproxy-v8.0.*-gateway*.patch`
   及各阶段补丁、`Dockerfile` 中按名字断言的补丁测试清单、只为补丁镜像存在的 `/internal/*`
   兼容代码路径和 `CPA_INTEGRATION=patch`；同时删除自写的 Antigravity 旧桥接
   （`cmd/antigravity-bridge`、`internal/antigravity`），它同样违反原则 1。
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
| 被动额度的时效 | 只在账号有请求后更新 | 长时间无请求的账号显示较旧的观测，界面标明观测时间 |

## 明确不做

- 不自行实现 CPA 没有的 provider 请求（主动额度查询、身份核验端点、模型鉴定专用探针）。
- 不迁移“无显式会话标识时按对话内容推导亲和”。它需要解析请求体；Codex CLI 与 Claude Code
  都带显式会话标识。
- 不迁移 Gemini 历史校验返回 `new_session_required`。改为上游原生错误。
- 不迁移 trace 日志脱敏、WebSocket 与 Home 相关改动；对应功能在部署中本就关闭。
