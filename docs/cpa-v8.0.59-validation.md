# CPA v8.0.59 升级验证

本次升级把兼容层固定到 CLIProxyAPI `v8.0.59`，上游提交于 2026-11-07。

- 上游仓库：[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
- 固定 tag：`v8.0.59`
- 固定 commit：`c5251b57b3fa5dcbc689f514f4aa166daacb4481`
- Gateway 补丁：`deploy/codex-compat/cliproxy-v8.0.59-gateway.patch`
- 补丁 SHA-256：`7ab8c39000cf1d953c1d0e52236e2ceb0c75bff607786774f42f4beacd55af8f`
- 模型目录：`internal/registry/models/models.json`
- 模型目录 SHA-256：`99037d47cc67751a02847f8a5ed3463c694de88319ee458e527844821b9a9bd0`（与 v8.0.58 相同）

## 上游变更

v8.0.59 没有修改 `go.mod`、模型目录或数据库结构，主要变更为：

- Codex 与 SIWC refresh 引入 refresh journal：发送 refresh 请求前先登记 claim，成功结果保存为
  receipt；传输失败、响应不可读或无法证明 refresh token 未被轮换的状态码一律报告为
  `refresh_outcome_unknown` 且不再重试。文件存储的 journal 位于 OAuth 卷的 `.codex-refresh/`。
- Claude Responses 中途的 system 条目保持原位以保留 prompt cache；Claude OAuth 授权参数调整。
- Codex 修复 Chat Completions 嵌套自定义工具、小输出预算和 ToolSearch 延迟加载工具的缓存前缀。
- Gemini 与 Antigravity 多段 chunk 中的推理和文本不再丢失。
- 管理接口凭据列表输出 `oauth_provider`。

## 补丁重基

补丁先在 v8.0.58 上应用并提交，再三方合并到 v8.0.59。四个冲突文件均位于 refresh 链路，
合并保留了双方语义：

- `internal/auth/codex/openai_auth.go`、`siwc_auth.go`：继续使用 Gateway 的不跟随重定向请求、
  1 MiB 响应上限和脱敏的 `codexOAuthTokenError`；只有可信 token 端点明确拒绝时才是确定失败，
  其余状态包装为 `refresh_outcome_unknown`。`isNonRetryableRefreshErr` 先识别未知结果，
  避免被包装的 5xx 被当作可重试错误。
- `internal/runtime/executor/codex_executor_auth.go`：先按账号解析 Gateway 出口，再配置上游 journal。
- `sdk/cliproxy/auth/conductor_refresh.go`：错误发布前仍等待进行中的凭据保存，同时不把未知结果当作取消。

`internal/util/nocopy_invariant_test.go` 中上游已加入与补丁相同的 `bolt_store.go` 条目，删除补丁中的重复项。
三个 Gateway 安全测试和上游新增的显式路径 refresh 测试按 journal 语义调整：每个用例重置进程内
journal，脱敏状态码通过 `errors.As` 读取，测试凭据带有已验证的账号身份。

## 修复 v8.0.58 重基遗留问题

v8.0.58 的镜像构建在 CI（run 42260855725）的 `go test` 阶段失败，从未产出镜像。本次在 v8.0.59
上逐项修复，其中前五项是 Gateway 行为回归：

- Claude 工具别名状态重新按调用方、用户、provider 和稳定账号隔离（`helps.GatewayCacheIdentity`）。
  v8.0.58 重基丢失了该隔离，不同调用方可能读取同一 `previous_message_id` 的别名映射。
- Codex HTTP 与 WebSocket 路径的路由提示和模型请求头恢复使用 payload 覆盖后的 `upstreamModel`，
  WebSocket 路径恢复上游的托管图片校验；请求头仍经 `finalizeCodexOutboundHeaders` 统一收尾。
- 流式首包失败时，Gateway provider 不再被上游提前返回的 request-invalid 判断截断，
  仍由 `credentialSwitchEligible` 决定是否切换到第二个账号，最多两个账号。
- Home 选择释放会取消本次尝试的 context，账号切换判断改用请求 context，避免把释放误判为客户端取消。
- Claude 与 Antigravity OAuth 登录改走上游统一的提交流程，修复会话从未标记完成的问题，并支持定向重新登录。
  Antigravity 在提交前登记 Google subject：同一身份只对应一个凭据，定向重新登录不能改写其他身份的凭据。
- 测试替身：两个 Codex OAuth 模拟代理改为先接管连接再返回 `200`，并读取接管时缓冲的数据，
  否则高负载下 TLS ClientHello 首字节会丢失；refresh 不再重试后，该问题会让 refresh 测试随机失败
  （8 路并发压测中，原上游测试 200 次失败 55 次，修复后 0 次）。WebSocket 重新授权测试在删除临时目录前
  等待被接管的 handler 结束，避免与请求日志写入竞争（修复前约一半失败，修复后 60 次 0 失败）。

## 本地验证

本环境没有 Docker 守护进程。验证脚本从 Dockerfile 中提取 `codex-compat` 构建阶段的全部 `RUN go test`
步骤，在干净的 v8.0.59 checkout 上依次执行：tag 对应 commit 校验、`git apply --check --ignore-space-change`、
应用补丁、`git diff --check`、模型目录 SHA-256、入口脚本 `cmp`、主测试列表、race 测试、回归测试名单
检查以及 `go build ./cmd/server`。执行时清除代理变量并设置 `GOPROXY=off`，与镜像构建一致。

结果：全部 9 组 `go test` 步骤与 `go build` 通过。第一次执行暴露了上文两个测试替身问题，修复后重新生成补丁再次执行，
全部通过。其中 `TestCodexExecutorRefreshSelectiveAndDual` 在单包运行时稳定通过，只在多包并发负载下失败，
所以另用 8 路并发压测确认。

Gateway 仓库执行 `gofmt -l .`、`go vet ./...`、`go test -count=1 ./...`、
`python3 -m unittest discover -s scripts/tests -p 'test_*.py'` 和 `./scripts/validate-compose.sh`。

完整镜像构建、Trivy 扫描与运行冒烟由 GitHub Actions 完成，CI 状态以对应 main 提交的 workflow 为准。
真实 OAuth 账号验收仍须在目标 VPS 维护窗口按 [CPA v8 切换与回滚](cpa-v8-cutover.md) 执行，
重点确认 Codex refresh 成功、`refresh_outcome_unknown` 后重新登录，以及 Claude 和 Antigravity 的定向重新登录。

## 部署影响

- 无数据库迁移，无价格目录变更，Gateway 与 CPA 的选择接口契约不变。
- OAuth 卷新增 `.codex-refresh/`，权限与其他 OAuth 文件相同，现有入口校验无需修改。不要删除该目录。
- Codex refresh 遇到不确定结果时不再自动重试，未过期的 access token 仍可使用，对应账号需重新登录。
- 回滚到 v8.0.48 镜像时，旧版本不识别 `.codex-refresh/`；已出现 `refresh_outcome_unknown` 的账号仍须重新登录。
