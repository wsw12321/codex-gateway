# 个人资料与 SSO 自选名称验证

2026-10-05 完成。使用 Go 1.26.8、GCC 12.2.0、隔离容器内的 PostgreSQL 16.15，
以及 Node.js 18.20.4（Go 调用的前端测试）、Node.js 24.21.0 和 Chrome 154.0.8037.97
（浏览器回归）。源码以只读方式挂载到测试容器，每套完整数据库测试使用独立空库。

## 结果

以下检查全部通过：

```sh
go build -o /out/gateway ./cmd/gateway
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
gofmt -l .
git diff --check
```

设置各自一次性数据库的 `TEST_DATABASE_URL` 后，以下检查全部通过：

```sh
go test -count=1 -tags=integration ./internal/store
go test -count=1 -tags=integration ./internal/server
go test -race -count=1 -tags=integration -run 'TestProfile|TestExternal' -v ./internal/store
go test -race -count=1 -tags=integration -run 'TestProfile|TestRecoveryInvitation|TestOIDCHTTP' -v ./internal/server
```

定向集成 race 分别通过 17 个 store 和 33 个 server 顶层测试，无跳过。
完整 store/server 集成测试分别约 11 秒和 14 秒。

覆盖内容包括：

- 用户名大小写、空白、3/32/33 位、非法字符；显示名称中文、80/81 字符、空白、
  NUL、重复名称；历史 39 位 SSO 用户名单独修改显示名称。
- 两字段与审计整体提交/回滚、并发同名争用、待审和禁用账号占名、旧名释放，
  新名密码登录，以及会话、密码、Passkey、API Key、账务、SSO 和恢复码仍归原用户 ID。
- 未登录、跨站、外部 Origin、越权字段、显式 null、失效会话和过期复验拒绝。
  写入或审计等待期间超时也回滚资料和审计。
- 恢复邀请按 ID 创建；改名和旧名复用前后仍恢复原用户。兼容单独用户名调用，
  两种目标字段同时出现会被拒绝。
- SSO 缺字段和无效名称不建号，用户名占用可改名重试；每次令牌轮换保留原期限
  与验证时间，旧令牌不可重放。身份冲突及不明确的写入失败不返回重试令牌。
- SSO 并发注册/绑定无残留账号；数据库锁等待跨越原授权期限时整体回滚。
  取消可跨令牌轮换使用原 state；响应已清除事务 Cookie 时，只允许原 state 和
  刚签发的精确会话取消，不能撤销其他或后续登录会话。

## 页面与截图

新增个人资料 Node 回归 12 项，OIDC 回归扩展为 16 项，均通过并由 Go 测试调用。
覆盖草稿保留、只提交修改项、复验取消、冲突保留输入、迟到写入/状态刷新、退出切号，
以及恢复搜索编辑时清除选择和验证期间禁止按已失效选择签发邀请。

真实 Chrome 执行以下两个脚本成功，无未捕获页面错误：

```sh
PLAYWRIGHT_MODULE=/path/to/playwright \
PLAYWRIGHT_CHROMIUM_EXECUTABLE=/usr/bin/google-chrome \
  node internal/server/testdata/profile_browser.cjs
PLAYWRIGHT_MODULE=/path/to/playwright \
PLAYWRIGHT_CHROMIUM_EXECUTABLE=/usr/bin/google-chrome \
SCREENSHOT_DIR=/tmp/oidc-profile-screenshots \
  node internal/server/testdata/oidc_browser.cjs
```

个人资料页面在 320、390、850、851、1440 px 检查无横向溢出。
SSO 浏览器脚本通过本地跨站 HTTPS 服务验证 Strict Cookie、首次开通、自选名称、
名称冲突重试、原 state 取消与 SSO 再验证返回后不自动重放操作。所有账号和接口数据
均为合成测试数据；没有连接真实账号中心或部署应用。

| 页面 | 桌面 | 手机 |
| --- | --- | --- |
| 个人资料 | [Owner](screenshots/profile/profile-owner-desktop.png) | [普通会员与历史用户名](screenshots/profile/profile-member-mobile.png) |
| SSO 自选名称及冲突反馈 | [1440 px](screenshots/profile/oidc/registration-profile-1440.png) | [390 px](screenshots/profile/oidc/registration-profile-390.png) |

测试日志和构建产物保存在仓库外 `/tmp/cg-profile-20261005/out/`。临时容器、数据库
和隔离网络在验收后清理；本次不新增数据库迁移。
