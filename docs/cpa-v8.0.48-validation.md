# CPA v8.0.48 升级验证

验证日期：2026-11-01。仓库实现与本地验证已完成：CPA 补丁在真实 Go 工具链下重基、全包普通与竞态
测试、干净回放、镜像构建、运行冒烟、Trivy，以及 Gateway 单元、竞态、PostgreSQL 集成、安全扫描和
部署配置校验均已在本地按 CI 步骤通过。本次没有使用真实 OAuth 账号，没有推送提交、发布镜像或切换生产服务。

## 此前升级提交未通过 CI 的原因

`946d1c5` 之后的 30 个提交（`7b4ef3e` … `1132029`）全部未通过或被取消，失败都在 `containers` 作业。
本次从 `946d1c5` 重新开始，不复用那些分阶段补丁，并逐项避开以下问题：

| 此前的问题 | 本次做法 |
| --- | --- |
| 没有可用的 Go 工具链，补丁从未编译，靠 CI 逐次暴露编译、测试和竞态错误 | 在锁定 digest 的 `golang:1.26.10-bookworm` 容器中编译、测试、竞态并构建镜像 |
| 只拼接部分分阶段补丁，约 100 个 Gateway 文件（额度、诊断探针、Claude 工具别名、内部接口测试等）丢失 | 以 v8.0.30 完整补丁为输入做三方合并重基，194 个文件，功能与回归测试全部保留 |
| 为让 CI 变绿删除 `validate-compose.sh` 断言、替换 Dockerfile 回归清单、把 entrypoint 快照比对改为语法检查 | 未删除或弱化任何断言；回归清单名称全部保留；`cmp` 快照比对保留 |
| 丢失 `klauspost/compress v1.18.7`，Trivy 报 CVE-2026-63209；之后又手工修改补丁导致 hunk 损坏 | 保留 v1.18.7；补丁只由 `git diff` 生成，从不手改 |
| 补丁 SHA 分多个提交逐处更新，镜像标签中的摘要与补丁不一致 | SHA 只计算一次，所有锁定位置在同一提交更新；镜像标签前 16 位与补丁 SHA 一致 |
| 编辑脚本丢失可执行位 | 每次提交前检查 `git diff --summary`，无模式变化 |

## 固定来源与交付范围

- 上游：[v8.0.48](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v8.0.48)，固定 commit
  `dbb706215a3afd2f0ea3c96482d4233856466caa`；Go **1.26.10**，与上游 `go.mod` 要求一致。
- 补丁：`deploy/codex-compat/cliproxy-v8.0.48-gateway.patch`，摘要记录在
  [source.lock.json](../deploy/codex-compat/source.lock.json)，镜像标签为
  `codex-gateway-compat:v8.0.48-dbb70621-dfa7fe36e765f172-cpa`。v8.0.30 补丁保留作为回滚输入。
- 依赖：上游已自带 go-git/v6 `v6.0.0-alpha.5`、go-billy/v6 `v6.0.0-alpha.2`、`golang.org/x/crypto v0.57.0`、
  `x/net v0.60.0`、`x/text v0.42.0`；补丁只把 `klauspost/compress` 从上游的 `v1.17.4` 升到 `v1.18.7`。
- 模型目录：`internal/registry/models/models.json` SHA-256
  `32f663de911083339cf5347be74b0b4cbf1471bc1968362897cc94ebf021c306`。Gateway 只退役上游删除的 `gpt-5.5`
  （`0038_cpa_v8_0_48_model_catalog.sql`），不新增模型；`gpt-6.1-terra`、Kimi、xAI 仍不可用。
  新增 CPA 测试 `TestGatewayV8048ModelCatalogPolicy` 确认 Gateway 定价的模型仍在对应 provider 目录中，
  且 `gpt-5.5` 不会重新出现。

## 重基中的合并决定

v8.0.30 补丁对 v8.0.48 三方合并产生 42 个冲突文件。除逐一合并外，以下决定改变或确认了行为：

- **内部响应不压缩**：上游新增默认开启的 `response-compression`。Gateway 的 Go 传输会自动发送
  `Accept-Encoding: gzip`，因此入口固定 `response-compression.enabled: false`，由 entrypoint 快照测试、
  镜像冒烟和 `validate-compose.sh` 三处断言。`web-search-result-cache`、上游请求压缩和
  `save-cooldown-status` 保持关闭，并在快照测试中断言默认值。
- **`X-Codex-Gateway-Groups`**：Gateway 自多分组功能起发送该头，旧补丁未剥离。现在 CPA 入口直接消费，
  并加入全部上游传输的内部头剥离；新增测试证明此前可经模型 override header 泄漏到上游。
- **迟到 401**：上游在 `tryRefreshAfterUnauthorized` 中新增未受保护的拒绝标记；改为只作用于产生该 401 的
  凭据快照，`TestGatewayLateUnauthorizedDoesNotRefreshReplacement` 恢复通过。
- **刷新失败与凭据保存**：保留“刷新失败发布等待进行中的凭据保存”的顺序保证；上游新测试改为并发执行
  刷新，断言不变。
- **OAuth 会话**：上游新增 `claimOAuthSessionCommit` 后，Antigravity 身份登记失败须调用
  `FailOAuthSessionCommit`，否则会话永久挂起。
- **SIWC 与凭据分区**：头保护使用上游的请求级 SIWC 判定（官方 Codex 客户端仍用外层 token），
  Alpha Search 使用外层 OAuth 分区；auth 文件的 `header:Authorization` 不能覆盖 OAuth 凭据。
- **刷新错误分类**：可信 token 端点的规范化错误码取补丁与上游的并集，不读取未经规范化的上游正文。
- **本地中断**：沿用上游的 ownership 设计，增加 `response_id` 绑定，只拒绝显式指向其它响应的中断。
- **插件拦截器**：只有正文实际被改写时才丢弃派生会话身份，原样返回不影响上游新增的派生会话。
- **重复 Codex 账号**：上游新增的文件索引会把去重标记覆盖为空，改为写入最小身份标记。
- **guardian 子代理继承**：新增 `TestGatewayGuardianInheritanceIsCallerScopedAndEligible`，确认只在相同调用方
  作用域内、且只在 Gateway 允许的候选账号中继承父会话账号。
- **zstd**：compress v1.18 默认不再为该夹具生成单段帧。实测生产解码上限仍拒绝 64 MiB 单段帧，测试显式
  使用 `WithSingleSegment(true)`，生产代码未改。
- **上游计时测试**：`TestServiceHeartbeatFailoverDoesNotDrainBlockingScope` 在原版 v8.0.48 竞态模式下同样约
  1–2/10 失败（重连退避最长 1 秒而等待只有 1 秒）；等待改为覆盖首轮退避，20 次竞态运行全部通过。
- **上游 Codex keepalive 上传测试**：`TestCodexWebsockets_KeepalivePingDuringUpload_*` 三个测试的假上游在收到
  `response.create` 之前就发送 `response.completed` 并关闭连接，客户端读循环随即因 EOF 关闭 socket；若被钩子挂起的
  写入此时尚未执行，就以 `use of closed network connection` 失败（`41c94d1` 仅改文档，CI 镜像构建仍因此失败）。
  上游 `84e83441` 只修复了 xAI 的同名测试，这里按同一做法等请求到达再应答，并用 `sync.Once` 保护钩子通道。
  在钩子后注入 200 ms 延迟可稳定复现原失败；修复后注入延迟仍通过，三个测试 `-race -count=200` 全部通过。

因 Gateway 安全策略而调整断言的上游测试：SIWC 绑定资格（OAuth 分类在注册时固定）、续期恢复流程
（夹具 `id_token` 与 `account_id` 保持一致，矛盾身份会被隔离）、Antigravity 会话代理（配置持久身份登记）、
Alpha Search 凭据范围、Codex 客户端忽略 SIWC（账号身份种子）、陈旧刷新失败（并发刷新）。

## CPA 验证结果

| 检查 | 实际结果 |
| --- | --- |
| 格式与静态分析 | `gofmt -l .` 仅列出上游自带、v8.0.48 原版即未格式化的 `claude_thinking_replay_test.go`；`go vet ./...` 通过 |
| 全量测试 | `go test -count=1 ./...` 通过 |
| 全量竞态 | `go test -race -p 1 -count=1 ./...` 通过，102 个包，无 data race |
| 干净回放 | 在全新的 `dbb7062` checkout 上 `git apply --check --ignore-space-change`、应用和 `git diff --check` 通过；模型目录 SHA 一致；entrypoint 快照与部署文件一致；回放树与测试树逐字节相同 |
| 镜像构建 | Dockerfile 内全部回归（含凭据、目录、Codex、Claude 清单和三组竞态测试）通过 |
| 运行冒烟 | `scripts/test-sidecar-image.sh` 通过，包括新增的响应压缩关闭断言 |
| 漏洞扫描 | 锁定 Trivy 镜像扫描 gateway、compat、antigravity 三个镜像：HIGH/CRITICAL 均为 0（含 `cli-proxy-api` Go 二进制） |

## Gateway 与部署验证结果

按 `.github/workflows/ci.yml` 在干净 worktree 中逐步复现，分别验证每个提交：

| 作业 | CPA 补丁提交 | gpt-5.5 退役提交 |
| --- | --- | --- |
| containers：Shell 语法、三种出口及 OIDC 的 Compose 策略、Python 回归（167 项，1 项跳过）、relay/Shadowsocks、镜像构建、sidecar、Antigravity、legacy 回滚、迁移工具、Trivy | 通过 | 通过 |
| supply-chain：许可证与上游声明 | 通过 | 通过 |
| go-test：迁移跟踪、工具链一致、gofmt、单元、竞态、vet、面板 Node 测试 | 通过 | 通过 |
| PostgreSQL 17.6 集成 | 通过 | 通过（含新增 `0038` 迁移、回滚优先级测试） |
| go-security：govulncheck、gosec | 通过 | 通过 |

gpt-5.5 退役提交的 containers、supply-chain 和 go-security 作业在修订前运行；修订只调整了
`cpa_v848_migration_integration_test.go`（回滚会刷新未变倍率行的 `updated_at`，改为逐键比较并单独核对倍率值），
修订后的提交重新通过 go-test 与 PostgreSQL 集成作业。

面板 Node 测试在本地须清除会话继承的 `ANTHROPIC_BASE_URL`、`ANTHROPIC_AUTH_TOKEN`，否则客户端配置测试
会按设计拒绝“冲突的 Claude 设置”；`946d1c5` 在同一环境下表现相同，CI 运行器没有这些变量。
`lock-images.sh --check` 需要 skopeo，本地未执行；本次未修改 `images.lock.env` 或 `images.sources`。

## 真实账号部署前验收（未执行）

部署前仍须按 [CPA v8 切换与回滚](cpa-v8-cutover.md) 的 v8.0.30 升级步骤，在维护窗口完成：价格合并移除
`gpt-5.5`、`0038` 迁移；Codex、Claude、Antigravity 的 JSON/SSE、工具续聊、compact、强制刷新后重启、
跨账号权限和账单归因；模型列表不再包含 `gpt-5.5` 且历史记录可读；以及回滚演练。
