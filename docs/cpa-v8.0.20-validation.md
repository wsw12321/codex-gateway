# CPA v8.0.20 升级验证

验证日期：2026-10-08。本次交付为仓库源码、补丁、构建配置及验证记录；没有构建、
更新或运行应用镜像，没有部署服务，也没有使用站点凭据或真实 OAuth 账号。
Go 保持 1.26.8，本机源码测试使用 `CGO_ENABLED=0`。

## 固定来源与兼容范围

- 上游：[v8.0.20](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v8.0.20)，
  已核对 tag 的 peeled commit 为 `0f96f568e4dbf6f84ad7399a74b78344c5eac7e6`。
- 补丁：`deploy/codex-compat/cliproxy-v8.0.20-gateway.patch`；摘要以
  [source.lock.json](../deploy/codex-compat/source.lock.json) 为准，并同步到 Compose 与验证脚本。
  本次 SHA-256：`e731b9a15e9c6f2fa2d3409b79615811e5fc3e2d34cb2a97c6d5152e5bf63d9f`。
- 模型目录直接使用该提交的 `internal/registry/models/models.json`，SHA-256 为
  `3a97eea65c1df3ea8ad4edac838b37f7714868d1e784b3723d0650b6e848aa9a`。
  上游已包含 Codex plus/pro/team 的三份 `gpt-6.1-sol`，因此删除对应本地增补。
- 保留原补丁的安全依赖版本：go-git/v6 `v6.0.0-alpha.5`、go-billy/v6
  `v6.0.0-alpha.2`、x/crypto `v0.56.0`、x/text `v0.41.0`、compress `v1.18.7`。
- Gateway 公共 API、内部 capability 协议、数据库迁移、公开模型授权和价格配置不变；
  原生 Messages 和 count_tokens 继续强制进入 Claude 执行器。远程目录更新保持关闭。
- v7 回滚定义、[首次 v8 验证记录](cpa-v8-validation.md)、
  [此前 Claude 验证记录](anthropic-validation.md)及其历史镜像摘要保持原样，
  不作为本次版本的构建或运行证明。

## 合并后的安全回归

上游持久化锁、凭据版本和注册代次与 Gateway 人工开关、额度版本同时生效。
新增组合测试覆盖刷新成功、401、invalid_grant、取消与重授权、禁用后启用、删除后重注册
的竞争，以及保存期间人工禁用、旧刷新错误等待新授权保存、迟到成功/401/429 和旧 401
触发刷新。正常请求、count、home、流式和 duplex 结果都保留两组版本。

Claude 工具别名缓存的读写复用现有作用域算法，按调用方、用户、provider 和稳定账号隔离。
调度层向执行器传递可信 caller scope，并清除请求自带的身份元数据；缺少稳定账号时不读写
共享状态。测试覆盖跨用户/调用方/账号/provider 的同消息 ID、正常原生及 Responses 工具续聊、
缓存丢失、断流和取消。缓存丢失返回 request-scoped `thread_not_found`，不轮换账号；
未完成的流不发布工具别名状态。

Antigravity 目录保留固定生产端点、固定代理、拒绝重定向及八个公开模型白名单。
新凭据或注册代次不能复用旧目录，过期目录在探测失败时保持拒绝。
Gateway 目录发布使用凭据/注册/控制版本检查和模型注册代次 CAS，防止迟到探测结果
覆盖或撤销新账号目录；独立后台目录刷新不得绕过 Gateway 的发布策略。
成功且仍有效的目录可以跨人工开关或额度变化发布，但不修改控制状态，避免首次探测期间
禁用再启用后一直没有模型。缺少已验证身份的凭据在网络探测前被拒绝；撤销目录也必须
校验当前快照，不能撤销已重新授权的账号目录。

配置测试从实际 entrypoint 的 YAML heredoc 生成合成配置，通过上游 `LoadConfig` 解析，
并验证旧 YAML 兼容键归一化到 v8 布局后语义一致。Dockerfile 比较实际入口和补丁内测试
快照，防止测试配置与部署配置分离。

完整执行器测试中的五个上游用例原本要求透传内部身份头，与既有 Gateway 安全契约冲突：
`TestCodexExecutorDirectOpenAIImageGenerationUsesImagesEndpoint`、
`TestApplyCodexWebsocketHeadersPassesThroughClientIdentityHeadersWhenCloakingDisabled`、
`TestApplyCodexWebsocketHeadersNativeSessionCombinations`、
`TestApplyCodexHeadersPassesThroughClientIdentityHeaders`、`TestCustomMagicHeaders_Codex`。
这些断言改为检查内部标识被移除，并保留允许的普通请求头透传检查；没有放宽传输限制。

## 本地验证

| 检查 | 结果 |
| --- | --- |
| 干净上游回放 | 在独立目录检出固定提交，`git apply --check`、实际应用及 `git diff --check` 通过，无需忽略空白或三方合并 |
| CPA 受影响包 | 最终补丁回放后 57 个含测试包通过，另 13 个包无测试；包括完整 executor、translator、thinking、认证调度、持久化、配置、registry、watcher、API、session |
| CPA 静态检查与编译 | 受影响包 `go vet` 通过；`go build -trimpath` 成功，输出仅为临时源码验证二进制 |
| Gateway | `go test -count=1 ./...` 和 `go vet ./...` 通过，含 Node.js 驱动的 dashboard 回归 |
| 运维脚本 | 非 Compose 的 135 项测试完成，134 项通过，1 项 Caddy 镜像运行测试按 opt-in 条件跳过 |
| Compose 合成配置 | 23 项策略回归通过，使用临时目录、合成 secret 和真实 Compose CLI 渲染；容器运行部分使用测试替身，没有访问 daemon 或启动容器 |
| 构建回归清单 | Dockerfile 的 45 个测试存在性断言与实际编译后的测试列表一致，原有 Codex 安全回归全部保留 |
| 格式及锁 | Go 格式检查、36 个 shell 文件语法、补丁/模型目录 SHA、版本引用、基础镜像 manifest 只读校验及面板资源摘要校验通过 |

CPA 验证使用干净上游目录，并先应用补丁、复制 Dockerfile 同样使用的旧凭据测试夹具：

```sh
# 在固定 v8.0.20 源码目录执行，GATEWAY_ROOT 指向本仓库。
git apply --check "$GATEWAY_ROOT/deploy/codex-compat/cliproxy-v8.0.20-gateway.patch"
git apply "$GATEWAY_ROOT/deploy/codex-compat/cliproxy-v8.0.20-gateway.patch"
git diff --check
cp "$GATEWAY_ROOT/deploy/codex-compat/legacy-credentials.test.txt" \
  internal/watcher/synthesizer/gateway_legacy_credentials_test.go
cmp "$GATEWAY_ROOT/deploy/codex-compat/entrypoint.sh" \
  internal/config/testdata/gateway-entrypoint.sh

CGO_ENABLED=0 go test -count=1 \
  ./internal/config ./internal/cache ./internal/api ./internal/api/middleware \
  ./internal/api/handlers/management ./internal/auth/... \
  ./internal/runtime/executor/... ./internal/thinking/... ./internal/translator/... \
  ./internal/registry ./internal/logging ./internal/util ./internal/watcher/... \
  ./sdk/api/handlers ./sdk/api/handlers/openai ./sdk/api/handlers/claude \
  ./sdk/auth ./sdk/cliproxy/auth ./sdk/cliproxy/session ./sdk/cliproxy
CGO_ENABLED=0 go vet \
  ./internal/config ./internal/cache ./internal/api/... ./internal/auth/... \
  ./internal/runtime/executor/... ./internal/thinking/... ./internal/translator/... \
  ./internal/registry ./internal/logging ./internal/util ./internal/watcher/... \
  ./sdk/api/handlers/... ./sdk/auth ./sdk/cliproxy/...
CGO_ENABLED=0 go build -trimpath \
  -ldflags='-X main.Version=v8.0.20 -X main.Commit=0f96f568e4dbf6f84ad7399a74b78344c5eac7e6' \
  -o /tmp/cpa-v8.0.20-source-check ./cmd/server
```

Compose 回归通过 `python3 -m unittest discover -s scripts/tests -p 'test_relay_validation.py'`
运行，调用临时副本中的 `validate-compose.sh`。其真实 Caddy/Squid 容器解析步骤被替换为
测试替身，因此不能将这项通过记录为实际镜像运行通过。其他 Python 回归在同一测试目录
执行，并排除上述已单独执行的模块。网络与本地 socket 测试在允许监听的环境中运行，
最初的沙箱 socket 拒绝不作为功能失败，也没有通过跳过相关测试来获得通过结果。

## CI 与真实账号待验收

本机没有 C 编译器，未运行 race/CGO 检查。Dockerfile 已纳入配置、凭据并发、目录发布、
Claude 缓存及原有 Codex 安全回归；CI 仍须执行 Gateway race、CPA race、CGO 编译、
应用镜像构建、安全扫描和无网络运行冒烟。Caddy/Squid 的实际镜像配置解析也留待 CI。
本次未运行 PostgreSQL 集成测试；既有 CI 数据库集成任务仍保留。

真实账号仍须完成强制刷新后重启、通过 Gateway Key 生成、工具续聊、
跨账号权限/额度/账单归因检查，以及维护窗口切换与回滚验收。
重新授权功能可用受控账号验收；本次 v8.0.4 → v8.0.20 升级不要求全量重新登录、
凭据转换或新增数据库迁移。操作范围见 [补丁升级步骤](cpa-v8-cutover.md#已运行-v804-的补丁升级)。
本次没有新的应用镜像摘要；不得以历史摘要或源码二进制校验代替镜像验收。
