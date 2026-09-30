# 批量邀请码与群组邀请

Owner 从“邀请管理”创建注册邀请码，或在群组详情选择“创建邀请码”。创建时设置到期时间、
人数上限和是否审核；省略配置时保持 24 小时、1 人、免审核。群组邀请码绑定一个群组，
只用于已有账号申请入群。配置保存后不能修改，需要变更时撤销并重新创建。

邀请码和完整链接只在创建成功时显示，数据库仅保存令牌摘要。链接令牌位于 URL fragment，
页面读入后立即清理地址栏，并只在内存中保留；刷新页面需要重新打开原链接。

## 申请与审核

- 提交成功的待审和已通过申请都占用名额；失败不占用，拒绝立即释放。
- 注册需要审核时，账号保存原密码或 Passkey 以及恢复码，不创建会话，不能登录或使用业务功能。
  保存只显示一次的恢复码后，页面提示等待审核；批准后使用原凭证登录。
- 群组邀请先要求密码或 Passkey 登录，再显示当前账号和目标群组，确认后才申请。
  免审邀请立即入群，需审邀请等待 Owner 批准。已在其他群组的用户必须先移出原群组。
- 同一用户重复提交已有申请不会重复占额或重新执行；已通过后被移出的成员，重放原申请不会重新入群。
- 过期和撤销只阻止新申请，已有申请仍可审核。群组归档后不能批准入群，但可以拒绝申请。
- Owner 在详情中逐条或勾选审批，同一码的一次审批最多 100 条，在一个事务中全部成功或回滚。
  列表默认每页 50 条，接口最多每页 100 条。

拒绝注册申请会在事务中删除待审账号、登录凭证、恢复码、默认权限、空账本及申请，释放用户名。
拒绝入群申请只删除申请，保留原账号。系统不保存拒绝账号或专门的拒绝历史；普通审计仅记录
邀请码及操作数量，待审注册审计不关联待删账号、不记录申请人资料。重新申请会生成新的申请 ID，
旧拒绝请求不会影响新申请。重复批准已通过申请为空操作，批准已删除申请返回冲突。

待审注册账号只出现在审批名单，不出现在普通用户管理列表，也不能被充值、配置订阅、手动加组、
修改用户权限、签发恢复邀请或通过普通数据维护删除。已通过申请的账号被普通维护删除时，
保留必要申请快照和占用名额；其未处理入群申请被清理。

## 接口

管理读取要求 Owner 会话；管理写入还要求同源请求和 5 分钟内的身份验证。

| 接口 | 请求或返回 |
| --- | --- |
| `POST /admin/invitations` | `kind`、`expires_at`（RFC3339）、`max_uses`、`requires_approval`、`group_id`；成功返回一次性 `token`、`link` |
| `GET /admin/invitations` | `kind`、`group_id`、`limit`、`offset` 筛选；返回 `invitations` |
| `GET /admin/invitations/{id}/applications` | `limit`、`offset`；返回申请 `id`、用户名、显示名称、注册时间、申请时间、审批状态 |
| `POST /admin/invitations/{id}/review` | `application_ids`、`decision: approve/reject`；返回 `updated_count` |
| `POST /admin/invitations/{id}/revoke` | 撤销后返回 `ok` |
| `POST /auth/invitations/inspect` | 请求体 `invitation_token`；返回类型、群组展示信息、有效期、名额和状态，不返回令牌 |
| `POST /auth/invitations/join` | 已登录会话及请求体 `invitation_token`；返回 `status` 与申请 |

密码与 Passkey 注册完成响应均包含 `status: pending/approved`、`requires_approval` 和一次性
`recovery_codes`。待审响应不设置登录 Cookie。账号恢复仍使用 `kind: recovery` 与
`target_username`，Owner 初始化仍由终端签发，两者保持单次、最长 24 小时且免审核。

## 迁移与验证

迁移 `internal/store/migrations/0026_batch_invitations.sql` 扩展邀请码和用户状态，新增
`invitation_applications`，把旧已使用成员邀请码回填成已通过申请，保持不可再次使用。
升级沿用数据库备份流程；旧程序不理解待审状态和新的占用规则，不应连接迁移后的数据库。

名额在邀请码行锁内检查和写入，审批不新增占用量，拒绝删除申请自然释放名额。
涉及已有账号的事务按账务账户、用户、邀请码、申请、群组的顺序加锁，同类对象按 ID 排序。

Go 1.26.8 验证命令：

```sh
gofmt -l .
go build ./cmd/gateway
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
TEST_DATABASE_URL='postgres://user@127.0.0.1:5432/disposable_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/store ./internal/server
node internal/server/testdata/invitations_browser.cjs
```

Race 检查需要 C 编译器和 `CGO_ENABLED=1`。数据库测试只使用一次性数据库；浏览器测试通过
`PLAYWRIGHT_MODULE` 指定 Playwright，`PLAYWRIGHT_BROWSERS_PATH` 指定可选浏览器目录。
浏览器请求使用合成测试数据，覆盖桌面、移动端、两种登录方式、入群确认及退出后的迟到响应。

页面截图：[邀请管理桌面](screenshots/invitations-desktop.png)、
[邀请管理移动端](screenshots/invitations-mobile.png)、[群组邀请确认](screenshots/group-invitation-mobile.png)。

2026-09-30 验证使用 Go 1.26.8 和独立临时 PostgreSQL 16：构建、格式、全量单元测试、
全量 race、`go vet` 均通过；store/server 全量集成测试同时启用 race 并通过。
邀请浏览器回归、已有群组浏览器回归及 11 项邀请 Node 回归通过，页面截图使用合成数据。
