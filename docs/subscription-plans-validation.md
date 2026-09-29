# 套餐销售验证

本次实现增加套餐目录、购买并重开、绑定续费及 Owner 套餐管理。迁移为
`0024_subscription_plans.sql`；操作与升级说明见[运维手册](operations.md#套餐销售购买重开与绑定续费)。

## 已覆盖行为

- 购买覆盖管理员订阅、同套餐或其他同周期套餐；从第 1 期重新计时，补满新周期额度，
  不结转旧权益。管理员重开清除绑定，旧购买确认和续费确认均失效。
- 续费保留当前周期、剩余额度及原绑定，只延长原到期时间；自然跨期按原规则重置额度。
  每次遵守最低购买数及 99 期上限，累计 101 期可以展示和继续续费。
- 修改名称、价格、周期、额度、最低购买数及下架均使旧绑定立即失效；恢复旧配置或
  重新上架不恢复绑定。无实际变化的保存保留版本与续费资格。
- 余额不足、审计写入失败、金额／日期／累计整数溢出全部回滚，无扣款、订阅变更或
  孤立操作记录；FIFO 现金 lot 与账户余额一致。
- 并发重复请求只扣款一次；套餐编辑与购买、续费分别测试两种数据库锁等待顺序。
  成交后改价、续费、管理员重开均不改变原操作重试的完整响应。
- 已受理请求仍按原周期结算，购买不改变本人扣费来源设置。历史清理保留当前套餐目录，
  已清理操作 ID 被墓碑阻止重新执行；管理引用包含套餐编辑者。
- 从旧 schema 升级保留人工订阅额度、余额、周期起止、到期时间和历史操作响应；
  原订阅没有套餐绑定。
- 接口覆盖登录、同源、近期验证、Owner 权限、仅会话身份、十进制字符串、数量和版本，
  拒绝目标用户、客户端总价、未知字段、大小写变体、重复字段及无效 JSON。
- 前端覆盖精确金额、覆盖提示、续费预览、版本冲突后重新确认、响应丢失后原请求重试、
  迟到的身份验证响应、Owner 查看他人只读，以及累计超过 99 期时管理员重开输入不截断。

## 验证环境与命令

Go 1.26.8、Node.js 24.16.0、PostgreSQL 17.6、Chromium 153。数据库使用临时容器与
一次性数据库，未连接业务数据库；浏览器全部拦截为合成数据，不发起真实扣款或上游请求。

```sh
go build -o /tmp/codex-gateway-plans-gateway ./cmd/gateway
go test -count=1 ./...
go vet ./...
gofmt -l .
git diff --check

# TEST_DATABASE_URL 必须指向新建的一次性数据库。
go test -count=1 -tags=integration ./internal/store ./internal/server

# 需要 C 编译器；本次在现有 golang:1.26.8-bookworm 容器内运行。
CGO_ENABLED=1 go test -race -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 -tags=integration \
  ./internal/store ./internal/server -run '^TestBillingPlan'

node internal/server/testdata/billing_plans.cjs
node internal/server/testdata/billing_plans_browser.cjs
```

Node 单元回归同时由 Go 的 `TestBillingPlansUI` 调用。Playwright 装在仓库外时可用
`PLAYWRIGHT_MODULE` 指定模块路径，非默认 Chromium 路径用 `PLAYWRIGHT_BROWSERS_PATH`。
浏览器脚本默认将截图写入 `docs/screenshots/subscription-plans`，也可用 `SCREENSHOT_DIR`
覆盖输出路径。

以上构建、全量单元测试、静态分析、全量 Go 竞态检查、全量 PostgreSQL 集成测试、
套餐集成竞态检查及浏览器脚本均已通过；新增 Node 回归共 18 项。

## 界面检查

桌面 1440×1080、手机 390×844（完整目录截图为 390×1280），检查套餐目录、覆盖购买确认、续费确认和 Owner 编辑。
未出现页面横向溢出或未捕获脚本错误。模拟购买响应丢失后，重试携带完全相同的请求正文
及操作 ID；Owner 切换到他人账务后购买与续费入口不可见。

| 页面 | 截图 |
| --- | --- |
| 桌面订阅与套餐目录 | [桌面目录](screenshots/subscription-plans/catalog-desktop.png) |
| 手机套餐目录 | [手机目录](screenshots/subscription-plans/catalog-mobile.png) |
| 手机购买与覆盖提示 | [购买确认](screenshots/subscription-plans/purchase-mobile.png) |
| 续费与到期时间预览 | [续费确认](screenshots/subscription-plans/renewal-desktop.png) |
| Owner 编辑与解绑提示 | [编辑套餐](screenshots/subscription-plans/owner-editor-desktop.png) |

待确认操作只保存在当前页面内存中；整页刷新或退出后须先核对账务流水，再创建新操作。
