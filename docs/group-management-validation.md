# 群组管理验证

2026-11-05 为“群组额度”页面新增三项功能：已归档群组默认隐藏、按名称搜索群组，以及只修改名称的重命名操作。额度、周期、计费、成员和账号池的语义均不变，也没有新增数据库迁移。

## 行为

- **已归档群组默认隐藏**：群组列表默认只显示未归档的群组，列表上方的状态行会注明隐藏了几个，例如“显示 3 / 5 个群组 · 已隐藏 2 个已归档”。勾选“显示已归档群组”后，已归档群组以虚线卡片显示，并且也能被搜索到。取消勾选时，如果当前选中的是已归档群组，会自动切换到筛选结果中的第一个群组；没有可选群组时，详情区会隐藏。全部群组都已归档时，列表提示勾选复选框查看。
- **刷新和归档后的选中项**：刷新或写操作完成后，若原先选中的群组仍在筛选范围内，则保持选中；否则改选筛选结果中的第一个群组。因此在隐藏已归档群组时，刚归档的群组会被替换为下一个未归档群组。
- **按名称搜索**：搜索框按群组名称、全拼或首字母过滤，空格分隔的每个词都必须命中。拼音由服务端在返回 `GET /admin/groups` 时根据名称计算（`pinyin_full`、`pinyin_initials`、`search_index`），不写入数据库，改名后立即生效。搜索只影响列表，不改变当前选中的群组。没有匹配结果时，会注明是在未归档群组还是全部群组中搜索的。
- **重命名**：在群组详情中点击“重命名”，填写新名称（会去掉首尾空格，最多 120 个字符）和操作原因。前端会拦截空名称、与当前名称相同和缺少原因的提交。只调用 `PUT /admin/groups/{id}/name`，请求体只包含 `name`、`reason` 和 `operation_id`，不会把额度、周期等其他字段一起提交，因此不会用过时的值覆盖其他修改。已归档群组的“重命名”按钮不可用，服务端也会返回 409。
- **筛选状态**：搜索词和“显示已归档群组”的勾选状态只保存在当前页面内存中，退出登录或切换身份后清空，不写入浏览器存储。

## 接口与安全

`PUT /admin/groups/{id}/name` 与其他群组写操作使用相同的保护：仅限 Owner、校验同源 Origin、需要 5 分钟内的近期身份验证，并要求填写操作 ID 和原因。名称在处理器和存储层各校验一次，范围为 1–120 个字符。

写入沿用群组操作的幂等机制：用同一个操作 ID 重放时，返回原来的响应，不会重复写入，也不会覆盖之后的改名；同一个操作 ID 用于不同名称时返回冲突。审计事件 `group.rename` 记录 `previous_name`、`name`、原因和操作 ID。数据库中的名称约束（1–120 字符、允许重名）保持不变。

## 已运行检查

环境：Go 1.26.10、Node.js 22.23.4、Chromium 153.0.8010.12、PostgreSQL 16。

```sh
gofmt -l .
go vet ./...
go test -count=1 ./...
TEST_DATABASE_URL='postgres://gateway@127.0.0.1:55432/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/store ./internal/server
node --test $(ls internal/server/testdata/*.cjs | grep -v _browser.cjs)
SCREENSHOT_DIR=/tmp/group-list node internal/server/testdata/group_list_browser.cjs
SCREENSHOT_DIR=/tmp/groups node internal/server/testdata/groups_access_browser.cjs
```

在云端会话中运行 Go 测试前，需要清除宿主注入的 `ANTHROPIC_BASE_URL` 等 Claude 环境变量，否则 `TestClientConfigurator` 会失败；这与本次改动无关。

覆盖范围：

- 处理器测试：重命名接口与其他群组写接口一起检查 Owner 会话、Origin、跨站请求、近期验证、操作 ID 和原因；名称缺失、`null`、空白、非字符串或超过 120 个字符时不会调用存储层；正好 120 个字符的名称可以保存；已归档冲突映射为 409；列表返回派生的拼音字段，已归档群组仍在接口结果中。
- 集成测试：`TestGroupRenamePostgresIntegration` 确认改名只修改名称，期 ID 和期号不变；名称去掉首尾空格；审计记录包含旧名称；重放不会覆盖之后的改名；同一个操作 ID 用于不同名称时冲突；名称无效、群组不存在或已归档时被拒绝。store 和 server 两个包的集成测试全部通过。
- `group_list_ui_test.cjs`：覆盖默认隐藏与勾选显示、名称／全拼／首字母和多词搜索、空状态和状态行、隐藏已归档时自动改选、刷新时不再选中已归档群组、重命名的请求内容和刷新后的结果、无效名称拦截、已归档群组不能改名，以及身份重置后清空筛选状态。
- `group_list_browser.cjs`：在 Chromium 中完成同样的流程，并检查 320 / 390 / 1440 px 下没有横向溢出。原有的群组、群组账号用量、邀请、控制台布局和主题浏览器回归继续通过。

## 合成数据截图

| 场景 | 截图 |
| --- | --- |
| 默认隐藏已归档 | [桌面](screenshots/group-management/group-list-desktop.png) / [手机](screenshots/group-management/group-list-mobile.png) |
| 显示已归档并搜索首字母 | [桌面](screenshots/group-management/group-list-archived-desktop.png) |
| 重命名 | [桌面](screenshots/group-management/group-rename-desktop.png) |
