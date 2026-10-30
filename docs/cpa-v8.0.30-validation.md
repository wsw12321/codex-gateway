# CPA v8.0.30 升级验证

验证日期：2026-10-16。仓库实现与本地验证已完成：Gateway、PostgreSQL 集成、部署配置、
CPA 最终补丁回放、全包普通与竞态测试、镜像构建和运行冒烟均通过。
本次没有使用真实 OAuth 账号，没有发布镜像或切换生产服务。

## 固定来源与交付范围

- 上游：[v8.0.30](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v8.0.30)，
  固定 commit `5be2cdab0869cbaa3d401891f740aebf7e15aa99`。
- 使用 Go **1.26.9** 工具链，下载包已经按官方发布的 SHA-256 验证。
- 最终补丁为 `deploy/codex-compat/cliproxy-v8.0.30-gateway.patch`；补丁和上游模型目录的
  SHA-256 记录在 [source.lock.json](../deploy/codex-compat/source.lock.json)。
  已重新核对锁文件、Compose、Dockerfile 与校验脚本中的版本和摘要。
- 模型目录来自该提交的 `internal/registry/models/models.json`，远程更新保持关闭。
  Gateway 仅开放已接入 provider、已配置价格与权限的模型，并不公开上游全部条目。
- 保留安全依赖版本 go-git/v6 `v6.0.0-alpha.5`、go-billy/v6 `v6.0.0-alpha.2`、
  compress `v1.18.7`；`golang.org/x/net v0.60.0`、`golang.org/x/crypto v0.57.0`、
  `golang.org/x/text v0.42.0` 随当前上游锁定文件。
- 本次需要执行 `0035_cpa_v8_0_30_model_catalog.sql` 并合并价格目录。
  旧 v7 回滚文件、[首次 v8 验证记录](cpa-v8-validation.md)和
  [此前 Claude 验证记录](anthropic-validation.md)保留为历史资料。

## 最终模型范围

新增公开模型仅有两项：

| 模型 | 输入上下文上限 | 最大输出 | 每百万 token 价格（美元） |
| --- | ---: | ---: | --- |
| `claude-haiku-5-5` | 1,000,000 | 128,000 | 输入 5、缓存输入 0.5、输出 25；5m/1h 缓存写入 6.25/10 |
| `gemini-4-argon-high` | 1,048,576 | 1,048,576 | 输入 3.75、缓存输入 0.375、输出 18.75；`included_in_input` |

以上为 2026-10-16 验证时的历史价格，采用当时已确认的低档模型价格乘 5 规则。Haiku 以 `claude-haiku-4-5-20251001`
为基准，Argon High 以 `gemini-3.8-flash-high` 为基准；保留既有汇率、fallback policy
和历史账单快照语义。

2026-10-17 已将模板同步为官方 API 现价，并为 Haiku 5.5 增加 100,000 输入 Token 分档；
当前价格见[原生模型与定价迁移](cpa-native-models.md)。本页保留原验证事实，不能作为新价格已完成真实账号验收的证明。

Antigravity 活动目录共五个模型：`gemini-3-flash`、`gemini-3.8-flash-high`、
`gemini-3.1-flash-lite`、`gemini-3.5-flash-lite`、`gemini-4-argon-high`。
默认模型为 `gemini-3.8-flash-high`。`gemini-pro-agent`、`gemini-3.1-pro-low`、
`gemini-3.6-flash-high`、`gemini-3.7-flash-high`、`gemini-3.7-flash` 退出活动目录和新请求路由，
历史 usage、账单及受控 legacy rollback 数据保留。

基础 `gemini-4-argon` 的上游 provider 为 `gemini`，四个 Muse Wonder 模型为 `meta`；
这些 provider 路径本次未接入，不配置其活动目录、价格、默认权限或请求路由。
上游目录中的多模态或内置工具能力也不等同于 Gateway 当前协议入口已支持全部能力。

## 已完成的验证

| 检查 | 实际结果 |
| --- | --- |
| Gateway 全量单元测试 | `go test -count=1 ./...` 通过 |
| Gateway 全量竞态测试 | `go test -race -count=1 ./...` 通过 |
| Gateway 静态分析与编译 | `go vet ./...`、Gateway 构建通过 |
| 新模型目录回归 | 新增目录测试及其定向竞态测试通过 |
| PostgreSQL 集成 | 使用临时 PostgreSQL **17.6** 容器，`internal/store`、`internal/server` 全部 integration 测试通过 |
| Python 脚本回归 | `scripts.tests.test_relay_validation` 23 项、`scripts.tests.test_pricing_catalog` 5 项，共 **28 项通过** |
| 部署配置 | 真实 Compose 渲染、Squid 三种出站模式、Caddy 的无网络配置解析通过 |
| SIWC OAuth 安全回归 | Codex OAuth 与新增 SIWC 定向普通测试、竞态测试通过；覆盖重定向阻断、响应限长和错误净化 |
| 依赖完整性与 Git 存储 | `go mod verify` 通过；保留 go-git 安全版本，仓库损坏恢复完整竞态测试通过 |
| 中断时序回归 | HTTP 取消、迟到 WebSocket 中断及 response ID 隔离连续 20 次竞态测试通过 |

数据库集成覆盖 `0035` 的五个模型退役、两个新模型的默认权限、保留既有覆盖规则、
受限 API key allowlist 与历史账本不变，以及条件回滚。已有 `0035` 快照时只应用该版本的
回滚快照，不叠加旧 `0025` 恢复；管理员后续改动和升级后新增用户的配置保留。

最终补丁生成后，28 项 Python 回归和真实部署配置校验已再次通过。

## 安全审计与最终 CPA 回归范围

手动重放审计已核对请求隔离中间件、Claude/Google 固定代理与端点限制、出站敏感头处理、
Claude 刷新身份校验、凭据文件保存、Gateway 状态文件排除及 watcher 去重接线。
v8.0.30 新增 SIWC token 请求此前未经过原有 OAuth 安全 helper；已补齐禁止 307/308
重定向、1 MiB 响应限制和错误正文净化，并通过定向普通与竞态回归。

最终 CPA 全包普通与竞态测试已通过，覆盖以下合并边界：

- 上游持久化锁、凭据版本和注册代次与 Gateway 人工开关、额度版本共同生效。
  刷新成功、401、invalid_grant、取消、重授权、禁用再启用及删除再注册的迟到结果不能覆盖新状态。
  Gateway 模式关闭新增的执行器直接读写凭据路径，继续由 Manager 检查身份、版本和注册代次后统一持久化，
  避免被拒绝的旧刷新结果经 watcher 或重启重新载入。
- Claude 工具别名缓存按调用方、用户、provider 和稳定账号隔离；缺少稳定账号时不共享状态。
  缓存丢失返回 request-scoped `thread_not_found`，未完成的流不发布工具别名。
- Antigravity 目录使用本次五模型白名单；目录发布与撤销校验凭据、注册及控制快照，
  防止迟到探测覆盖新账号目录。缺少已验证身份的凭据不能进入网络探测。
- 原生 Messages 与 count_tokens 强制使用 Claude 执行器，内部身份头不能随普通、流式、
  WebSocket 或图片请求泄漏；保留允许的普通请求头透传。
- 配置测试解析实际 entrypoint 的 YAML，并与 Dockerfile 中的测试快照比对。
  `tool-search: false`、`tls-handshake-timeout-seconds: 0`、
  `send-keepalive-before-ft: false`、`session-affinity-rebind-on: "any"` 已加入镜像冒烟显式断言。
  插件、发现、远程目录、管理面板自动更新和 response steering 保持关闭。
- 完整竞态检查发现并修复流日志收尾、异步 usage 读取已回收 Gin Context 的问题。
  HTTP 本地中断绑定当前 response ID，在完成事件发送前清除状态，取消上游后再发送中断确认。
  Dockerfile 增加这些包的完整竞态检查，防止之后升级重新引入。

## 最终 CPA 与镜像结果

| 检查 | 实际结果 |
| --- | --- |
| CPA 全量普通测试 | `go test -count=1 ./...` 通过，包含 Dockerfile 单独复制的 legacy 凭据夹具 |
| CPA 全量竞态测试 | `go test -race -p 1 -count=1 ./...` 通过；`-p 1` 限制包间并行，不跳过测试 |
| CPA 静态分析与格式 | `go vet ./...`、`gofmt -l .`、`git diff --check` 通过 |
| 注册并发回归 | 同 revision 注册及关联 E2E 连续 50 次 race 通过；测试按实际完成账号同步，清理时等待后台任务退出 |
| 干净补丁回放 | `git apply --check`、实际应用及 `git diff --cached --check` 通过，entrypoint 测试快照一致 |
| 来源与摘要 | Docker 构建验证 tag 对应完整 commit；补丁和原始模型目录 SHA-256 与锁文件一致 |
| 最终部署校验 | `scripts/validate-compose.sh` 通过；使用临时合成配置和真实 Docker，无网络运行 Squid/Caddy 配置解析，未修改站点 `.env` |
| CPA Docker 镜像 | 构建通过，含完整受影响包测试、调度/watcher 全包 race、流日志/usage/中断 race 和原有安全回归 |
| 最终镜像运行冒烟 | `scripts/test-sidecar-image.sh IMAGE` 通过：OAuth 文件权限、三个 provider capability、内部鉴权、独立管理密钥、唯一刷新锁、无 Gateway 启动和四项安全默认值 |

本地最终镜像：

```text
codex-gateway-compat:v8.0.30-5be2cdab-f10e3cfd34208d91-cpa
local image ID: sha256:38bdb87fb500012ab7290436e5a8be6522fe2f8275ba0be044bb527cdb9e1a95
runtime user: 10001:10001
```

该 ID 来自本机 `docker image inspect`；没有推送 registry，不能把它当作发布仓库的部署清单。
镜像漏洞扫描及正式发布仍由 CI 流程执行。本次一次性 PostgreSQL 容器和中间镜像标签已清理。

## 真实账号部署前验收

本次尚未运行真实账号请求。部署前仍须验证 OAuth 强制刷新并重启后的身份保持、
Haiku 5.5 Messages 普通与流式请求、Argon High 原生 Gemini 与 Antigravity Responses、
工具续轮、失败冷却、跨账号权限与账单归因，以及旧模型拒绝、历史记录可读和维护窗口回滚。
Muse 与基础 Argon 不属于本次真实账号验收范围。

本次升级不要求全量重新登录或凭据转换；必须执行 `0035` 与价格合并，并按
[v8.0.20 补丁升级流程](cpa-v8-cutover.md#已运行-v8020-的补丁升级)完成切换和条件恢复。
