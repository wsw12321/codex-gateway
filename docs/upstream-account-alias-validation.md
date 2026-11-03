# 上游账号别名验证

Owner 可为 Codex、Antigravity、Claude 上游账号设置别名，便于区分账号。选择账号时
别名与原来显示的内容一起出现，格式为“别名（原名称或脱敏邮箱）”。未设置别名时
所有显示与之前一致。

## 行为与安全影响

- 迁移 `0041_upstream_account_alias.sql` 为 `upstream_accounts` 增加
  `alias TEXT NOT NULL DEFAULT ''`。现有账号别名为空，无需回填。约束限制最多
  40 个字符、无首尾空格、不含 `@` 和控制字符。禁止 `@` 是为了不把完整邮箱写进
  这张只存脱敏邮箱的表。迁移只能前进；回滚到旧版二进制前须先恢复迁移前备份。
- 别名与分配系数、并发上限一样归 Gateway 所有。CPA 元数据同步（包括模型鉴定前的
  同步）不会覆盖别名。别名只用于显示，不参与选路、权限或计费。
- 新增 `PUT /admin/{upstream,antigravity,anthropic}-accounts/{id}/alias`，
  请求体为 `{"alias": "..."}`，返回 `{"id","alias"}`。中间件链与其他账号写入一致：
  同源、Owner、5 分钟内二次验证。账号必须属于该路径对应的类型，否则返回 404。
  服务端去除首尾空白；空字符串表示清除。超长、含 `@`、换行、控制字符、
  U+2028/U+2029 或双向文本控制符时返回 `400 invalid_upstream_account_alias`。
  双向控制符可能让一个账号伪装成另一个，因此一并拒绝。请求体超过 1 KiB 返回 413。
- 每次保存写入 `upstream_account.alias_changed` 审计，元数据包含旧别名与新别名。
  审计失败时整笔回滚。
- 修改别名不需要 CPA 在线。sidecar 同步失败或 CPA 已不再列出的账号仍可修改别名，
  其他会改动 CPA 的操作仍按原规则锁定。
- 以下接口新增 `alias` 字段：`/admin/*-accounts` 列表，账号权限（单用户、批量）
  与群组账号池选项，群组账号统计，以及模型鉴定账号选项。模型鉴定选项的别名
  从 Gateway 数据库读取，读取失败时返回 500，不会误报为上游错误。

## 界面

- 上游账号卡片的标题优先显示别名，下方依次显示 CPA 名称和脱敏邮箱。
  “账号详情与设置”中新增“账号别名”输入框与“保存别名”按钮。
  未保存的草稿在列表刷新后保留。页面搜索覆盖别名，按名称排序时优先使用别名。
- 账号权限批量页（列表与矩阵表头）、用户/群组账号池选择器、专属授权对话框标题、
  群组账号统计、模型鉴定下拉框和结果标题都显示为“别名（原内容）”，
  并且都可以按别名搜索。卡片操作提示（禁用、系数、并发、额度查询）同样带上别名。
- 前端在提交前按服务端相同的规则校验，不合法的值不会发出请求。

暂未改动：使用统计和请求监控里每条请求的账号列仍只显示脱敏邮箱。

## 验证

2026-11-03 在本机使用锁定的 Go 1.26.10 镜像、Node.js v24、一次性 PostgreSQL 17.6
（`deploy/images.lock.env` 中的镜像）及 Playwright 1.49.1（Chromium 1148）完成：

```sh
gofmt -l .                                   # 无输出
go vet ./...
go build ./...
go test -count=1 ./...
go test -race -count=1 ./...
TEST_DATABASE_URL='postgres://…' go test -count=1 -tags=integration ./internal/store ./internal/server
node --test internal/server/testdata/*_test.cjs internal/server/testdata/model_identification_browser.cjs
PLAYWRIGHT_MODULE=/path/to/playwright SCREENSHOT_DIR=/tmp/shots \
  node internal/server/testdata/upstream_account_alias_browser.cjs
```

此外在同一工作树上本地复现了 CI 的 `gotest` 作业（迁移跟踪、工具链一致性、gofmt、
unit、race、vet、node-ui）和 `security` 作业（govulncheck、gosec），全部通过。

### 后端

- `TestNormalizeUpstreamAccountAlias`：去除空白（含全角空格），40 字上限按字符计数，
  ZWJ 组合 emoji 允许；`@`、制表符、换行、DEL、NUL、NEL、U+2028/U+2029、
  U+202E/U+2066 等双向控制符及非法 UTF-8 均拒绝。
- `TestUpstreamAccountAliasRejectsInvalidWritesBeforeDatabase`、
  `TestUpstreamAccountAliasMigration`：非法别名、账号 ID 或缺少操作者时不访问数据库；
  迁移包含默认值与关键约束。
- `TestUpstreamAliasRejectsInvalidRequests`：字段缺失、多余或重复，`null`、非字符串，
  大小写不符，尾随数据，不合法别名，内容类型和查询参数错误，均返回 400 且不回显输入；
  超大请求体返回 413，非法账号 ID 返回 404。
- `TestUpstreamAliasSavesNormalizedValueAndHidesDatabaseFailure`：写入去除首尾空白后的值，
  空值可清除；数据库错误返回 500，不泄露错误文本。
- `TestUpstreamStatusRouteProtectsOriginRoleAndRecentVerification` 与
  `TestAntigravityAccountRoutesRequireSession` 增加 `alias`：缺少来源、跨站、
  无会话、成员、未二次验证或验证过期均被拒绝。
- `TestUpstreamAccountAliasPostgresIntegration`：设置别名后，后续 CPA 同步不覆盖；
  列表与统计查询返回别名；其他类型的命名空间不能修改（404）；审计失败整笔回滚；
  未知账号 404；绕过 store 直接写入时，数据库约束拒绝邮箱、首尾空格、换行和超长值；
  审计条数及旧/新别名正确。
- 既有 store 与 server PostgreSQL 集成测试全部通过（账号汇总、群组账号统计、
  选路与权限查询读取了新增列）。

### 前端

- `upstream_account_ui_test.cjs`：卡片标题与副标题在有无别名、有无 CPA 名称时
  均正确；搜索覆盖别名；sidecar 同步失败或账号不可管理时仍可编辑别名，成员身份
  或已有操作进行中时锁定；不合法值不发请求；保存时去除空白、清除、40 个 emoji 边界、
  确认后刷新；提示文本正确；响应不一致时不显示已保存，草稿在刷新后保留。
- `group_upstream_access_ui_test.cjs`、`user_upstream_access_ui_test.cjs`：
  选择项与矩阵表头显示“别名（原名称）”，无障碍标签一致，可按别名搜索。
- `model_identification_browser.cjs`：下拉选项与鉴定结果标题显示“别名（脱敏邮箱）”。

模拟浏览器实际执行：本地拒绝含 `@` 的别名；保存“  备用 Pro  ”后请求体为
`{"alias":"备用 Pro"}`，刷新后卡片标题与提示正确，展开状态保留，可按别名过滤；
390 px 宽度下无横向溢出；账号权限页的账号列表与矩阵显示别名；无页面脚本错误。
截图使用合成账号和脱敏邮箱，上游均为模拟：

- [桌面上游账号](screenshots/upstream-account-alias-desktop.png)
- [移动端账号别名设置](screenshots/upstream-account-alias-mobile.png)
- [账号权限选择](screenshots/upstream-account-alias-picker-desktop.png)
