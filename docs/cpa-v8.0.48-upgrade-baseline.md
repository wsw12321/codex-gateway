# CPA v8.0.48 升级准备基线（仅准备，不是上线）

> 状态：`preparation_only`。本文件及配套 JSON 只记录候选版本和重基工作量，**没有升级部署锁**，也没有执行数据库迁移、OAuth 迁移或生产切换。

## 候选与现行版本

- 候选上游：CLIProxyAPI `v8.0.48`，commit `dbb706215a3afd2f0ea3c96482d4233856466caa`；来源和发布说明见 [GitHub release](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v8.0.48)。
- 现行部署：CPA `v8.0.30`，commit `5be2cdab0869cbaa3d401891f740aebf7e15aa99`，Gateway 补丁 `deploy/codex-compat/cliproxy-v8.0.30-gateway.patch`，SHA-256 `f10e3cfd34208d912a216a414807bcc6262f14389d4738d71141d7db40a3f876`。
- v8.0.48 的上游变化涉及 structured usage、AI Studio SSE 分片重组、Responses WebSocket 断开和延迟日志、相对 auth-dir 冷却状态、未完成流的冷却判定、Claude tool-result 语义及 Codex compaction capsule；这些变化必须在重基后重新审查，不能以旧版测试结果代替。

可审计的机器可读记录在 [`deploy/codex-compat/cpa-v8.0.48-upgrade-baseline.json`](../deploy/codex-compat/cpa-v8.0.48-upgrade-baseline.json)。`registry_digest` 留空是有意的：只有 CI 发布并记录 registry manifest digest 后才能填写。

## 阶段 1 门槛

1. 从干净的 v8.0.48 源码树开始，在临时目录运行旧补丁的 `git apply --reject`，保存 `.rej` 清单；禁止直接把 Compose 或 `source.lock.json` 改成 v8.0.48。
2. 当前 patch 对 v8.0.48 的探测产生 47 个冲突文件，主要集中于依赖锁、Codex/Claude executor、`sdk/cliproxy/auth` conductor、WebSocket handler、watcher 和 usage。完整路径与 hunk 数量见 JSON；任何冲突都必须由人工按语义重基并配套测试。
3. 重基应拆为可独立审阅的提交：
   - 依赖和上游版本锁（不改变运行配置）；
   - Gateway 身份、账号选择、并发、持久状态；
   - Codex/Claude/Antigravity executor 与传输边界；
   - watcher、usage、模型目录和测试；
   - 最后才更新 Dockerfile、Compose、校验脚本、CI 和文档。
4. 每个提交保留上一提交可构建的回滚点。完成前不得删除旧 patch、v8.0.30 镜像锁、`deploy/legacy-cpa.override.yml` 或 v8.0.30 模型回滚迁移记录。

## 回滚资料（先备份再切换）

- 现行 CPA 镜像标签前缀：`codex-gateway-compat:v8.0.30-5be2cdab-f10e3cfd34208d91-cpa`；实际 registry digest 必须在维护窗口前记录在发布清单中。
- 兼容回滚镜像：`codex-gateway-compat:v7.3.15-673131f5-2dc404c8b22496a6-rollback`。
- 回滚沿用 [`docs/cpa-v8-cutover.md`](cpa-v8-cutover.md) 的停写、排空、单一 refresh 进程和条件模型目录恢复规则；不得 `down -v`、不得用旧全库备份覆盖当前数据库、不得同时挂载同一 refresh token 到两个 CPA。
- 候选版本若改变数据库 schema，必须先证明旧 Gateway 可读；否则只允许在隔离数据库演练，生产流量保持停用。

## 通过条件（阶段 1 不执行）

后续阶段须在干净 checkout 复核：`git apply --check` 无冲突、`go test -count=1 ./...`、`go test -race -count=1 ./...`、`go vet ./...`、CPA patch 聚焦回归、Compose/镜像构建、sidecar/legacy/migration 探针和 Trivy；再进行 staging 真实 Gateway Key 的 JSON/SSE/工具续聊/usage/账单冒烟。阶段 1 只建立候选基线，不能把这些条件标记为已通过。
