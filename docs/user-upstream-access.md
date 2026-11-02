# 用户上游账号权限

Owner 在管理台“账号权限”页面同时勾选多个用户和多个账号，再批量启用、禁用或
恢复为全部账号。页面只加载一次用户和账号：用户列表复用用户名、显示名和拼音搜索，
每行显示 Codex、Antigravity、Claude 三类当前范围（如 `Codex 2/3 · Claude 已禁用`）；
账号按类型分组，显示名称、脱敏邮箱、状态及所选用户中已允许的人数。
“当前权限”矩阵列出所选用户 × 所选账号的允许／禁用（超过 2000 格时仅提示缩小范围）。
搜索不清除已有勾选，“全选当前搜索结果”只作用于匹配项。

批量操作对每个所选用户、每个涉及的类型（启用／禁用取所选账号的类型，恢复全部
由 Owner 勾选类型）计算新范围。设 `A` 为本类型所选账号：

| 操作 | 当前为“全部账号” | 当前为“仅指定账号” S |
| --- | --- | --- |
| 启用所选账号 | 不变 | `selected`，S ∪ A |
| 禁用所选账号 | `selected`，该类型全部已登记账号 − A | `selected`，S − A |
| 恢复为全部账号 | 不变 | `all`，空名单 |

禁用会把“全部账号”用户改为“仅指定账号”，今后新增账号不会自动加入；禁用某类型
全部账号会留下空名单，即禁用该类型。页面实时预览将修改的项数、其中由“全部账号”
转换和被整类禁用的项数，以及已是目标状态而跳过的项数；出现转换或整类禁用时
提交前再次确认。保存须填写原因并通过近期二次验证，整批在一个事务内完成，
只写入并审计实际变化的项；任一目标无效则整批回滚，输入保留。

| 模式 | 行为 |
| --- | --- |
| `all` | 默认值，允许全部账号，包括今后新增的账号；提交空数组。 |
| `selected` | 只允许名单内账号；不会自动加入新账号。 |
| `selected` + 空数组 | 该类型全部禁用，未知或尚未同步的账号也不能使用。 |

这些规则与账号专属授权及现有模型权限、状态、额度、并发检查取交集。
Owner 发起普通模型请求时同样受限；既有模型鉴别和内部运维自检仍沿用管理权限。
保存后，后续选路与已有会话的下一次权限检查读取新值。正在执行的请求继续完成。
零分配权重账号沿用现有会话规则，但复用时仍须通过账号权限检查。
查询失败时拒绝访问，不使用缓存名单兜底。

允许选择当前不可用的已登记账号。元数据同步或账号下线不会清除名单；
同步失败时管理页显示本地快照和提示，仍可保存权限。
保存期间锁定选择和表单并固定提交内容，重复提交、迟到响应和身份切换不会错写；
刷新会保留仍存在的勾选，并提示已移除的用户或账号。

## 批量接口

`GET /admin/upstream-access/users` 要求 Owner 会话且不能带查询参数。它先同步三类账号
元数据（失败时回退本地快照并附 `sync_warning`），再返回全部非待审批用户的显式规则；
没有规则的用户即为 `all`：

```json
{
  "providers": [
    {"provider": "codex", "accounts": [{"id": "0123456789abcdef", "display_name": "team-primary", "email_masked": "u***@example.test", "status": "available"}]},
    {"provider": "antigravity", "accounts": [], "sync_warning": "upstream_account_sync_unavailable"},
    {"provider": "anthropic", "accounts": []}
  ],
  "rules": [
    {"user_id": "00000000-0000-0000-0000-000000000001", "provider": "codex", "mode": "selected", "account_ids": []}
  ]
}
```

`PUT /admin/upstream-access/users` 要求同源、Owner 和近期二次验证。请求必须包含且仅包含：

```json
{"user_ids":["00000000-0000-0000-0000-000000000001"],"action":"disable","account_ids":["0123456789abcdef"],"providers":[],"reason":"停用团队主账号"}
```

- `action` 为 `enable`、`disable` 或 `all`。启用／禁用要求非空 `account_ids` 和空
  `providers`，类型由账号决定，可跨类型；`all` 要求非空 `providers` 和空 `account_ids`。
- `user_ids` 为 1–1000 个规范 UUID，`account_ids` 至多 1000 个；三个数组均不能为
  `null` 或含重复值。原因去除首尾空白后为 1–500 字符。
- 未知账号返回 `400 invalid_resource`；任一用户不存在或待审批返回
  `409 resource_conflict`。两者均整批回滚。

成功返回每个目标用户 × 类型写入后的范围（含未变化项）及计数：

```json
{"access":[{"user_id":"…","provider":"codex","mode":"selected","account_ids":["…"]}],"target_count":1,"changed_count":1,"converted_count":1}
```

事务先取 `upstream_accounts` 表锁，再按 ID 顺序锁定涉及类型的全部账号，最后按 ID
顺序以 `FOR NO KEY UPDATE` 锁定用户，避免与单用户保存、账号专属授权及审计外键
形成锁环。每个变化项写一条 `user.upstream_access_changed` 审计，字段与单用户保存
相同，另含 `batch_action`。

## 单用户接口

页面不再使用以下单用户接口；它们保持不变，供脚本和兼容使用。

`GET /admin/users/{user_id}/upstream-access` 要求 Owner 会话，返回：

```json
{
  "user_id": "00000000-0000-0000-0000-000000000001",
  "providers": [
    {
      "user_id": "00000000-0000-0000-0000-000000000001",
      "provider": "codex",
      "mode": "selected",
      "account_ids": ["0123456789abcdef"],
      "accounts": [
        {
          "id": "0123456789abcdef",
          "display_name": "team-primary",
          "email_masked": "u***@example.test",
          "status": "available"
        }
      ]
    },
    {
      "user_id": "00000000-0000-0000-0000-000000000001",
      "provider": "antigravity",
      "mode": "all",
      "account_ids": [],
      "accounts": [],
      "sync_warning": "upstream_account_sync_unavailable"
    }
  ]
}
```

`PUT /admin/users/{user_id}/upstream-access/{provider}` 要求同源、Owner 和近期二次验证。
`provider` 仅接受 `codex`、`antigravity`。请求必须包含且仅包含以下字段：

```json
{"mode":"selected","account_ids":["0123456789abcdef"],"reason":"分配团队账号"}
```

成功返回 `{user_id, provider, mode, account_ids}`。目标用户必须存在且非待审批；
账号 ID 必须存在、无重复且属于指定类型。原因去除首尾空白后须为 1–500 字符。
`account_ids` 必须为数组，不能为 `null`。每次写入只替换指定类型的规则和名单；
审计事件为 `user.upstream_access_changed`，包含类型、前后模式、名单数量和摘要、原因。
审计失败会回滚整个保存。

## 迁移

新增嵌入式迁移 `0030_user_upstream_access.sql`，随 `gateway migrate` 或
`gateway serve` 执行。规则以 `(user_id, provider)` 为主键，名单外键同时约束账号类型。
缺少规则等同 `all`，无需回填已有用户；空名单仍保留 `selected` 规则。
永久删除用户时级联删除两类规则和名单。

沿用项目 forward-only 迁移流程：部署前备份，迁移后回退构建也必须识别同一迁移集。
本次沿用现有内部选路接口，无需更换 sidecar 镜像。

## 验证

2026-10-06 使用 Go 1.26.8、Node.js 及一次性 PostgreSQL 16.15 完成验证：
全量单测、全量 race、`go vet`、Gateway 构建、store/server 完整集成测试均通过。
最终并发锁修复另通过受影响的 PostgreSQL race 回归；`gofmt -l .` 无输出，
`git diff --check` 通过。前端 10 项新增 Node 回归与模拟浏览器验证通过。

数据库回归覆盖默认全部、多账号、空名单、恢复全部、类型隔离、专属授权交集、
Owner 无豁免、新增及未知账号、同步保留设置、永久删除、并发替换和审计失败回滚。
并发测试复现并修复了专属名单编辑与用户范围保存之间的审计外键死锁；
用户校验改用 `FOR NO KEY UPDATE`，仍阻止冲突的状态修改和删除。
HTTP 回归覆盖两类内部选路的撤权与改选、零权重授权检查和数据库故障拒绝，
以及管理接口的权限保护、非法输入、跨类型账号和本地快照回退。
Antigravity 请求测试覆盖撤权及权限查询失败后拒绝下一次请求，同时允许已开始的请求完成。

前端回归涵盖独立保存、拼音搜索、账号过滤保留勾选、重复提交、迟到 GET/PUT/401、
身份切换、退出清理和过期表单。模拟浏览器实际操作了密码二次验证与两类保存。
以上前端记录针对当时的单用户页面；2026-11-02 起页面改为批量模式，其接口、
前端回归和截图验证见[账号权限批量设置验证](user-upstream-access-batch-validation.md)。

```sh
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build -o /tmp/user-upstream-access-gateway ./cmd/gateway
TEST_DATABASE_URL='postgres://…' go test -count=1 -tags=integration ./internal/store ./internal/server
gofmt -l .
node --test internal/server/testdata/user_upstream_access_ui_test.cjs
PLAYWRIGHT_MODULE=/path/to/playwright PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chrome \
  node internal/server/testdata/user_upstream_access_browser.cjs
```

当前截图为批量页面，使用合成用户、脱敏邮箱和模拟上游；桌面为 1440×1080、
移动端为 390×844，移动端已检查无横向溢出：

- [桌面账号权限](screenshots/user-upstream-access-desktop.png)
- [移动端账号权限](screenshots/user-upstream-access-mobile.png)
