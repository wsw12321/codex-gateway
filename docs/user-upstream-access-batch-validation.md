# 账号权限批量设置验证

“账号权限”页面由逐个用户编辑改为批量模式：Owner 同时勾选多个用户和多个账号，
一次启用、禁用或恢复为全部账号。规则、接口和锁顺序见
[用户上游账号权限](user-upstream-access.md)。本次无数据库迁移，CPA 选路回调不变。

## 行为与安全影响

- 新增 `GET /admin/upstream-access/users`（Owner 会话）与
  `PUT /admin/upstream-access/users`（同源、Owner、近期二次验证），中间件链与
  单用户接口及模型权限批量接口一致。单用户接口保留，页面不再调用。
- 批量写入在一个事务内完成，只写入并审计实际变化的用户 × 类型；任一用户不存在或
  待审批返回 409，未知账号返回 400，均整批回滚。审计事件沿用
  `user.upstream_access_changed`，另含 `batch_action`。
- 对“全部账号”用户禁用某账号会转换为“仅指定账号”，今后新增账号不再自动加入；
  页面预览并在提交前确认该转换及整类禁用。选路仍对每次请求按新范围失败关闭。
- 单用户保存的写入与审计提取为共享函数，行为与审计字段不变（既有回归全部通过）。

## 验证

2026-11-02 在本机使用锁定的 Go 1.26.10 镜像、Node.js v24.21.0、
一次性 PostgreSQL 17.6 及 Playwright 1.49.1（Chromium 1148）完成：

```sh
gofmt -l .                                   # 无输出
go vet ./... && go vet -tags=integration ./...
go build -o /tmp/gw ./cmd/gateway
go test -count=1 ./...
go test -race -count=1 ./...
TEST_DATABASE_URL='postgres://…' go test -count=1 -tags=integration ./internal/store ./internal/server
go run github.com/securego/gosec/v2/cmd/gosec@v2.22.8 ./...           # 无问题
go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...                  # 代码未受影响
node --test internal/server/testdata/*.cjs   # 不含 *_browser.cjs；329 通过，1 项需本机 Codex 的用例跳过
PLAYWRIGHT_MODULE=/path/to/playwright SCREENSHOT_DIR=/tmp/shots \
  node internal/server/testdata/user_upstream_access_browser.cjs
```

Go 镜像内没有 Node，`TestUserUpstreamAccessUIBehavior` 在容器中跳过，因此
Node 回归在宿主机直接运行。

### 后端

- `TestUserUpstreamAccessBatchValidationBeforeDatabase`、
  `TestNextUserUpstreamAccessAppliesBatchActions`：参数校验先于数据库；
  逐行覆盖上表三种操作在两种当前范围下的结果，不修改调用方数据。
- `TestUserUpstreamAccessBatchPostgresIntegration`：跨 Codex／Antigravity 禁用、
  启用、整类禁用、恢复全部；只审计变化项且含 `batch_action`；重复操作不产生审计；
  待审批或缺失用户、未知账号、审计失败均整批回滚；列表不含待审批用户的规则；
  选路候选与新范围一致。
- `TestUserUpstreamAccessBatchConcurrentEditsPostgresIntegration`：Owner 位于目标中时，
  批量启用、批量禁用、账号专属授权和单用户保存并发 24 次，全部成功且无死锁。
- `TestUserUpstreamAccessBatchRejectsInvalidRequests`、
  `TestUserUpstreamAccessBatchRoutesProtectSessionsAndWrites`：字段缺失、多余或重复、
  `null` 数组、大小写不符、操作与字段不匹配、超长原因、超大请求体、查询参数，
  以及会话、Owner、来源和二次验证保护。
- `TestUserUpstreamAccessBatchHTTPPostgresIntegration`：批量读取的账号与规则、
  同步失败回退本地快照后仍可写入、跨类型批量禁用后
  `/internal/upstream-accounts/eligible` 立即排除对应账号、成员越权与待审批冲突。

### 前端

`user_upstream_access_ui_test.cjs` 的 10 项回归覆盖：单次加载并排除待审批用户、
拼音搜索、预览逻辑与服务端规则逐项一致、搜索保留勾选与“全选当前搜索结果”、
矩阵与已允许人数、确认转换后提交固定载荷并忽略重复提交和保存中的勾选、
恢复全部需选择类型、无变化与缺少原因不写入、非 Owner 锁定、迟到读取不覆盖新结果、
刷新移除已不存在的勾选、身份切换中断二次验证并清空状态、迟到写入与过期 401
不影响新身份、409 提示刷新、结果不一致拒绝应用、失败保留输入。

模拟浏览器实际执行：拼音搜索勾选两位用户；启用新账号（无需确认）；再跨类型禁用
两个账号，接受转换确认后完成密码二次验证，事件顺序为写入 → 验证 → 写入；
矩阵与用户摘要随结果更新；成员身份看不到该页面。截图使用合成用户、脱敏邮箱和
模拟上游；桌面 1440×1080、移动端 390×844，移动端无横向溢出：

- [桌面账号权限](screenshots/user-upstream-access-desktop.png)
- [移动端账号权限](screenshots/user-upstream-access-mobile.png)
