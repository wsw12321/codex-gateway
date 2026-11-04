# 群组账号用量查看验证

Owner 可以为单个群组开启“允许成员查看用量”。开启后，群组成员能看到该群组
“指定账号”池中 Codex、Claude 账号的 5 小时与每周用量。设置默认不勾选。

## 生效规则

成员能看到某个账号，当且仅当存在一个群组同时满足：

- 成员属于该群组（以 `group_memberships` 为准）；
- 群组已开启“允许成员查看用量”，且未归档、未到期（`expires_at` 为空或晚于当前时间）；
- 群组在该服务商的账号池为“仅指定账号”，且名单包含该账号；
- 服务商为 Codex 或 Claude。

例如 Codex、Antigravity 为“全部账号”，Claude 指定了两个账号，则成员只能看到
这两个 Claude 账号。“全部账号”池按配置方式判断而不按数量比较：即使“指定账号”
选中了全部现有账号也会展示，之后新增的账号不会自动可见；“全部账号”池永远不
暴露该服务商的账号清单。空名单不展示任何账号。Antigravity 只有按模型的剩余比例、
没有 5 小时/周窗口，因此即使选择了指定账号也不展示。成员属于多个群组时按群组
优先级分组显示，同一账号在每个授予它的群组下各出现一次，但每个账号只读取一次。
可见性只取决于群组账号池，不叠加用户级“账号权限”。

## 行为与安全影响

- 迁移 `0042_group_account_usage_visibility.sql` 为 `user_groups` 增加
  `account_usage_visible BOOLEAN NOT NULL DEFAULT false`，现有群组全部保持关闭，
  无需回填。迁移只能前进；回滚到旧版二进制前须先恢复迁移前备份。
- 新增 `PUT /admin/groups/{id}/account-usage-visibility`，请求体为
  `{"operation_id","reason","enabled"}`，返回完整群组。中间件链与其他群组写入
  一致：同源、Owner、5 分钟内二次验证。`enabled` 必须是布尔值；`operation_id`
  幂等，用同一 ID 提交相反的值返回 409。已归档群组返回 409。每次写入记录
  `group.account_usage.enable` 或 `group.account_usage.disable` 审计。
- `GET /admin/groups`、`GET /admin/groups/{id}` 及个人账务中的群组新增
  `account_usage_visible` 字段。
- 新增 `GET /admin/billing/me/account-usage`，任何登录用户可调用，只返回本人
  可见的账号；不接受查询参数，账号集合完全由服务端根据会话用户推出。响应形如：

  ```json
  {"groups":[{"group_id":"…","group_name":"研究组","accounts":[
    {"provider":"anthropic","alias":"Claude 主力","source":"observed",
     "updated_at":"…","cooldown_until":"…","limited":false,
     "five_hour":{"used_percent":42,"reset_at":"…"},
     "seven_day":{"used_percent":18,"reset_at":"…"}}]}]}
  ```

  响应不含账号 ID、CPA 名称、脱敏邮箱、套餐、权重或成本。单个账号读取失败只在
  该账号上返回固定错误码：`usage_rate_limited`、`usage_timeout`、
  `account_disabled`、`usage_unavailable`，不透出上游原文。
- 用量来源：
  - Codex：复用 Owner 额度查询的实时 ChatGPT 请求，只取默认 `codex` 额度桶，
    窗口 300 分钟记为 5 小时、10080 分钟记为每周，其他额外额度桶不展示。
    服务端按账号缓存 1 分钟，并发请求合并为一次；与 Owner 额度查询共用每账号
    5 秒限流。只有真正请求上游时写 `upstream_account.quota_queried` 审计，
    元数据带 `"via":"group_account_usage"`，操作者为发起读取的成员。Owner 手动
    查询额度成功后也会刷新这份缓存。
  - Claude：读取 Owner 账号页使用的同一个 CPA 观测快照，不向 Claude 发请求，
    带观测时间与冷却时间，缓存 15 秒。CPA 管理接口未配置时显示为暂不可用。
  - 失败结果缓存 10 秒，避免刷新放大对异常上游的请求。单次请求最多等待 20 秒，
    超时的读取在后台继续完成并写入缓存。
- 不改动 CPA 补丁，也不改变选号、权限或计费。

## 界面

- Owner：群组详情 → “群组账号池”区块顶部新增“允许成员查看用量”开关、
  操作原因和“保存设置”。下方状态行按已保存的账号池实时说明效果，例如
  “已开启，成员可查看：Claude（指定 2 个账号）；不展示：Codex（全部账号）、
  Antigravity（不支持）。”保存账号池后状态行同步更新。同一群组刷新时保留未保存的
  勾选，切换群组时重置；已归档群组或其他群组操作进行中时开关只读。
- 成员：“额度与订阅”页每张群组卡片下方新增“账号用量”小节，仅在该群组有可见
  账号时出现。每个账号显示别名（未设置时为“Claude 账号 1”这样的编号）、状态
  （正常、已达限额、冷却至某时、查询失败原因）、5 小时与每周的已用比例、进度条和
  重置时间，以及查询或观测时间。“刷新用量”强制重新读取；30 秒内重新渲染页面
  （例如翻页流水）复用上次结果。Owner 查看其他用户的账务时不加载。

截图：

- `docs/screenshots/group-account-usage-owner-desktop.png`、`group-account-usage-owner-mobile.png`
- `docs/screenshots/group-account-usage-member-desktop.png`、`group-account-usage-member-mobile.png`
- `groups-desktop.png`、`groups-mobile.png`、`groups-upstream-pool-*.png` 已按新布局重新生成。

## 验证

2026-11-04 在本机使用锁定的 Go 1.26.10 镜像、Node.js v24、一次性 PostgreSQL
（`deploy/images.lock.env` 中的镜像）及 Playwright 1.49.1（Chromium 1148）完成：

```sh
gofmt -l .                                   # 无输出
go vet ./...
go build ./...
go test -count=1 ./...
go test -race -count=1 ./...
TEST_DATABASE_URL='postgres://…' go test -count=1 -tags=integration ./internal/store ./internal/server
node --test internal/server/testdata/group_account_usage_ui_test.cjs   # 以及 *_test.go 引用的全部 testdata/*.cjs
PLAYWRIGHT_MODULE=/path/to/playwright SCREENSHOT_DIR=/tmp/shots \
  node internal/server/testdata/group_account_usage_browser.cjs
PLAYWRIGHT_MODULE=/path/to/playwright SCREENSHOT_DIR=/tmp/shots \
  node internal/server/testdata/groups_access_browser.cjs
```

覆盖内容：

- `internal/store/group_account_usage_integration_test.go`：默认关闭；需求示例
  （Codex/AGY 全部账号、Claude 指定两个）只返回两个 Claude 账号；非成员、空名单、
  选中的 Antigravity 不返回；多群组按优先级排序；到期、归档后不再返回；归档群组
  写入 409；幂等重放与相反值重放 409；审计条数。
- `internal/server/group_account_usage_integration_test.go`：HTTP 全流程，成员调用
  开关返回 403，响应不含账号 ID 与邮箱，“全部账号”的 Codex 不被读取，非成员
  Owner 得到空列表，关闭后成员不再可见。
- `internal/server/group_account_usage_test.go`：会话要求与查询参数拒绝、按群组分组、
  去标识、同一账号只读一次、失败结果缓存、缓存并发合并与过期、Codex/Claude 窗口
  换算与校验、Codex 读取审计与错误码映射、Owner 额度查询刷新缓存、`enabled` 类型校验。
  `groups_test.go` 的路由策略、幂等、操作原因、审计归属与错误映射表格测试覆盖新接口。
- `group_account_usage_ui_test.cjs`：开关默认值与状态文案、保存载荷与校验、草稿保留、
  只读状态、成员卡片的别名/编号/状态/进度、结果复用与强制刷新、Owner 查看他人不加载、
  失败提示、身份切换后丢弃迟到响应。

此外在同一提交上本地复现了 CI 的 `gotest` 作业（迁移跟踪、工具链一致性、gofmt、
unit、race、vet、node-ui）和 `security` 作业（govulncheck、gosec）。
