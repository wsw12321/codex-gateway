# CPA v8 切换、演练与回滚

本次固定 CPA **v8.0.4 / d33f63f8e3d98428440ebca5a5b6a981a61ff71e**，
管理面板 **v1.25.0 / b87b9487f63e08ad97b1fb4e7c17b4adb811b922**。
CPA 源码、补丁 SHA-256 和构建回归由 Dockerfile 与 Compose 校验；面板固定源码组件、
依赖锁和同源资源摘要见 `deploy/cpa-panel/`。面板是去除配置、代理、权重、插件、任意请求
与导出能力的受限构建，业务管理继续使用 Gateway 页面。所有自动更新关闭。

默认链路：客户端 → Gateway → 同一 CPA → Codex / Antigravity。
Gateway 管身份、模型和账号权限、权重、并发、限额、价格、账单及审计；CPA 管 OAuth、
供应商协议、凭据状态和刷新。`antigravity-bridge` 只在 `legacy-bridge` profile 中运行。

## 构建与上线前演练

先保留当前部署的镜像 **digest**、配置和受保护凭据。采用 Go 1.26.8：

```sh
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
./scripts/validate-cpa-panel.sh
./scripts/validate-compose.sh
./scripts/compose.sh build gateway codex-compat antigravity-bridge
```

race 需要 C 编译器；前端回归需要 Node。面板重建使用 Bun 1.3.14 和
`scripts/build-cpa-panel.sh`，不能从 CPA 公网自动更新。数据库测试必须使用一次性库：

```sh
TEST_DATABASE_URL="$DISPOSABLE_DATABASE_URL" \
  go test -count=1 -p 1 -tags=integration ./internal/store ./internal/server
```

本地记录见 [验证记录](cpa-v8-validation.md)。本地合成凭据、HTTP stub 和测试数据库的成功，
不能代替真实账号的刷新及 Gateway 生成验收。CI 发布后必须记录应用镜像的 registry digest；
Docker 本地 image ID 是本地内容标识，不应冒充 registry manifest digest。

提前构建兼容回滚组合：**本次 Gateway** 加 `deploy/legacy-bridge.override.yml`；
若需回退 CPA 本身，再加 `deploy/legacy-cpa.override.yml`。旧 Gateway 二进制不能读取新增
迁移，禁止回退它。保留当前 Go 构建意味着新 schema、稳定映射和历史价格快照仍可处理。
回滚 profile 只路由旧 CLI 与新目录共有的 Flash-high ID，新增 Pro/lite/Sol 的实际能力
须逐项检查；旧 CPA 缺少的模型会隐藏或拒绝，绝不替换成其他模型。

```sh
legacy_image=codex-gateway-compat:v7.3.15-673131f5-2dc404c8b22496a6-rollback
docker build -f deploy/codex-compat/legacy.Dockerfile -t "$legacy_image" .
./scripts/test-legacy-sidecar-image.sh "$legacy_image"
./scripts/test-sidecar-image.sh
./scripts/test-antigravity-image.sh
```

另外按凭据迁移文档构建维护镜像，并运行 `scripts/test-cpa-migration-image.sh` 的合成探针。
当前本地最终 bridge 镜像检查受 Docker 存储故障阻断，必须先补齐
[验证记录中的待补检查](cpa-v8-validation.md#docker-存储故障与待补检查)，才能进入维护窗口。

## 维护窗口

1. 在实际部署服务器暂停新推理请求、账号变更、登录脚本与自动重启。关闭新流量后等待
   Gateway 的活动请求、CPA 账号活动请求归零，确认不存在待结算预约。备份数据库、当前
   部署配置、OAuth 和 Keyring；备份加密并记录此次修改前值。不要导出凭据到浏览器。
2. 使用 `scripts/bootstrap-secrets.sh` 增补 `cpa_management_key`，保持 0640 和部署 secret GID。
   它只能挂载到 Gateway 与 CPA，必须与执行用 Sidecar Key 不同。没有公网管理端口。
3. 停止 bridge 与 CPA，确认唯一刷新进程退出。按照
   [凭据双向迁移](cpa-credentials-migration.md) 在服务端 tmpfs 中运行正向转换。
   工具强制刷新并验证真实 Google subject、项目、原生模型和额度；每个失败账号单独隔离，
   仅这些账号重新登录。保留刷新产生的最新凭据，禁止旧快照覆盖它们。
4. 按 [价格合并步骤](cpa-native-models.md) 保存 `pricing.before.json`、`pricing.applied.json`，
   合并九个模型后写回完整配置。其余 Codex 模型和汇率不能用示例值覆盖。
   启动新 Gateway 时 `0025` 执行一次，目标用户/默认权限允许、倍率为 1，受限 Key 不变。
5. 先启动 CPA 和 Gateway，保持对外维护状态。检查凭据目录 0700、文件 0600、UID 10001，
   独立 subject→稳定账号 ID 映射、`.gateway-account-state`、旧专属名单、权重和费用历史。
   不增加可任意编辑的 YAML 卷；运行配置仍由入口脚本生成。
6. Owner 通过 `/admin/cpa/` 管理账号。凭据写操作需近期验证和同源校验；OAuth 绑定当前
   Owner 会话、5 分钟有效且单次消费。启停复用 Gateway 控制状态，删除先禁用并排空。
   新转换文件默认停用，逐个临时启用、核对原状态，验收后才恢复原来可用的账号。
7. 使用真实 Gateway Key 逐账号验证 `GET /v1/models`、Codex/Sol 和实际可用 Gemini 模型的
   JSON、SSE、函数调用/返回、签名透传、取消、429。检查账单、账户归因和非零有效 usage；
   确认共享/专属权限、撤权后旧会话、权重、禁用和会话并发不会被面板或上传绕过。
   不可用模型隐藏或拒绝。旧 CLI 会话不能续接时应明确提示新建会话。
8. 强制刷新后重启，再做两类供应商请求；重新授权后稳定 ID 不变。确认不同用户重复使用
   相同会话/工具 ID 不串账号或签名缓存。完成本次演练记录后恢复流量。

## 回滚

Antigravity 故障时优先保留 CPA v8 的 Codex 服务。再次暂停 Google 流量和账号变更，排空后
停止 CPA 与 bridge；用最新 CPA 文件执行反向转换，写回 Keyring 并验证旧 CLI 确实刷新。
失败账号保持停用、重新授权，不能恢复过期 token 快照。新账号没有旧 Keyring 槽位时也保持停用。

启用 bridge override 会设置 `CPA_ANTIGRAVITY_ENABLED=false`：CPA 的 Google 账号加载、管理
与后台刷新被禁止，只有旧 bridge 拥有 Google 刷新职责。之后以原项目 env 和 image locks
渲染以下组合并核查，不要同时启动两套 Google 刷新器：

```sh
docker compose --project-name codex-gateway --project-directory "$REPOSITORY" \
  --env-file "$REPOSITORY/.env" --env-file "$REPOSITORY/deploy/images.lock.env" \
  -f "$REPOSITORY/docker-compose.yml" \
  -f "$REPOSITORY/deploy/legacy-bridge.override.yml" \
  --profile legacy-bridge config --quiet
```

确认后将最后的 `config --quiet` 改为 `up -d gateway codex-compat antigravity-bridge`。
若故障在 CPA 本身，再加入 `-f "$REPOSITORY/deploy/legacy-cpa.override.yml"` 使用预先核验的旧 CPA。
沿用本次 Gateway 和同一数据库；旧 CPA 不支持新增受限管理 API，需使用保留的服务端登录流程。
操作前清除覆盖 Compose env-file 的同名 shell 环境变量，原则与 `scripts/compose.sh` 一致。

按 [模型价格回滚](cpa-native-models.md) 执行条件恢复：只有仍等于切换时 applied 快照的
当前配置才能恢复，保留后续 Owner 修改、新用户和业务记录。**不得用旧全库备份覆盖当前库**，
也不删除迁移记录、历史账单、余额和预约快照。回滚后通过 Gateway 再验收并核对账户归因。

## 稳定期

默认保留 7 天已停止的旧镜像、受保护凭据备份、维护镜像及回滚配置。结束后清理旧 bridge
运行依赖、CLI 下载域名和旧网络出口权限，保留仍在使用的稳定身份映射和控制状态。
清理前确认不再需要回滚。三项 Flash-high 价格在 **2026-12-31** 到期，安排到期前复核并
人工更新价格配置；此次没有自动调价系统。
