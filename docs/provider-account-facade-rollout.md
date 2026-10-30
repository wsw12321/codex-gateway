# Provider account facade 发布与回滚清单

本清单用于发布 `internal/proxy` 的 provider adapter registry 与 account facade
改动。它只改变 Gateway 到各 CPA provider 的路由/管理边界，**不改变 CPA 版本、镜像
锁、价格目录或数据库 schema**。发布前请确认目标 revision 确实没有同时包含迁移；
若包含迁移，以 [CPA v8 切换与回滚](cpa-v8-cutover.md) 的 forward-only 约束为准。

## 发布前冻结与检查

1. 记录当前 Gateway revision、三个应用镜像 digest、`.env`、`deploy/images.lock.env`
   和数据库备份。确认维护窗口内没有待结算请求、账号变更或 OAuth 刷新。
2. 在待发布 revision 上运行最小回归；完整发布仍需遵循 [CI 镜像部署流程](ci-image-deployment.md)：

   ```sh
   go test -count=1 ./internal/proxy/...
   go test -race -count=1 ./internal/proxy/...
   go vet ./internal/proxy/...
   ./scripts/validate-compose.sh
   ```

   失败时不要替换生产镜像。\`validate-compose.sh\` 通过只说明 Compose/锁文件未漂移，
   不能代替真实 provider 账号验收。
3. 预先保留上一个可用的 Gateway 镜像清单。这个变更无数据库迁移，回滚优先使用该
   清单恢复 Gateway 镜像，不要用旧全库备份覆盖当前数据库。

## 小流量验收

保持维护页或仅允许一名 Owner 访问，按实际启用的 provider 逐项检查：

- `GET /admin/cpa/api/codex/accounts`
- `GET /admin/cpa/api/antigravity/accounts`
- `GET /admin/cpa/api/anthropic/accounts`
- 对一个已知账号读取 quota；只有在专用测试账号上才执行启停。

上述请求必须使用 Owner 会话及近期身份验证。每个 provider 的 sidecar 能力探测必须
返回精确协议标记 `upstream_account_access_v1`；Claude 还必须返回
`anthropic_messages_v1`。能力探测失败时应返回不可用错误并停止后续管理请求，不能
把请求转发到另一个 provider。

从 sidecar 日志或受控测试桩核对路径隔离：Codex 使用
`/internal/upstream-accounts`，Antigravity 使用 `/internal/antigravity-accounts`，
Claude 使用 `/internal/anthropic-accounts`。未知 provider 应得到
`provider_not_configured`，而不是使用 Codex 作为隐式 fallback。账号列表中的 ID、重复
记录和 quota schema 错误也应 fail closed；不要把凭据或完整 quota 响应写入日志。

完成管理验收后，再用真实 Gateway API Key 做每个已启用 provider 的 JSON 与 SSE 生成，
核对响应中的账号归因、账单与用量；`/readyz` 或容器 healthy 只能证明 Gateway 自身可用。

## 失败处理与回滚

1. 发现能力探测、provider 路径、账号归因或真实生成异常时，立即停止新流量并等待活动
   请求排空；不要反复点击启停或删除账号。
2. 先恢复上一份 `.env`、镜像清单和配置备份，执行校验并仅重建 Gateway（不执行
   `down -v`，不删除 PostgreSQL/OAuth 卷）：

   ```sh
   ./scripts/validate-compose.sh
   ./scripts/compose.sh up -d --no-build --pull never \
     postgres egress-allowlist codex-compat gateway caddy cloudflared
   ./scripts/compose.sh ps
   ```

   如上一 revision 需要从 GHCR 重新拉取，使用保留的 deployment manifest 和
   `scripts/pull-ci-images.sh`；不要用可变 `latest` 标签。
3. 回滚后先访问 `/readyz`，再重复上面的 provider 列表、quota 和真实生成验收。
   旧 CPA 若不支持 `upstream_account_access_v1`，管理接口必须明确失败并保持无写入；
   这属于安全的 fail-closed 状态，不能通过改名 provider、绕过能力探测或切换到别的
   sidecar 来“恢复”。
4. 记录失败 provider、精确 revision/digest、能力探测结果和 sidecar HTTP 状态。保留
   当前与上一 revision 至少一个稳定期，确认无待处理请求后再清理临时镜像或备份。

该回滚只针对 provider facade/adapter 发布；CPA 版本升级、模型目录/价格迁移、凭据
双向迁移和数据库迁移必须分别按其专用回滚文档执行。
