# 多订阅、资金优先级与统一模型选择器验证

实现与升级步骤见[运维手册](operations.md#升级到多订阅0034)。迁移为
`internal/store/migrations/0034_multi_subscriptions.sql`。

## 行为覆盖

- 同周期多订阅、重复购买新增、按 ID 重开／停用、跨用户拒绝；账务响应为有效订阅数组，
  保留用户关闭扣费的有效订阅。
- 日／周／月与 1–3650 天自定义周期、管理员无限期、单次购买／续费上限、日期和累计
  期数溢出。闲置后直接定位当前段和周期，周期额度不结转。
- 编辑套餐保持绑定、下架仍可续费、逻辑删除解绑。多次续费买入最新配置，依次追加
  不同周期／额度／模型的权益段；旧段和当前余额不变。
- 整数优先级 1–100、订阅与现金穿插、同优先级订阅优先、周期天数降序、精确金额降序、
  ID 稳定排序。模型不匹配不能使用订阅，订阅不改变调用权限。
- 请求受理时固定顺序、订阅周期和现金批次；受理后禁用、重开、改变模型、充值或跨期，
  结算仍使用旧快照。并发结算、幂等重试、审计失败回滚及旧资金规则保留。
- 保留旧订阅与周期 ID、余额和历史流水，旧套餐一次性清空并解绑。旧模型范围在当前
  目录同步后一次性初始化，重复启动不扩大范围。历史清理保留在途引用与有效权益。
- 同源、近期验证、Owner／本人权限、未知字段、重复字段、无效数量和报价冲突。
  历史请求重放在模型退出当前目录后仍返回原成交快照。
- 模型选择器使用目录分组，支持展开、全选／部分选中、清空、搜索、单选和键盘操作；
  API Key 限制模式空选拒绝；切换用户／退出后的迟到响应失效。
- 用量筛选保留仅存在于长期流水中的历史模型，并按实际模型归属和用户所有权筛选。
  重试时间覆盖未来模型权益段、耗尽后的下一周期及最终到期边界；迁移前资金规则
  版本 2 的在途请求仍按群组、日、周、现金原顺序结算并保持幂等。

## 验证环境

Go 1.26.8、Node.js 24.21.0、PostgreSQL 16.15、Google Chrome 155。
PostgreSQL 使用临时容器和一次性数据库，浏览器请求全部使用合成数据。
没有访问生产数据库或执行部署。

以下检查已通过：

```sh
go build -o /tmp/codex-multi-subscriptions-gateway ./cmd/gateway
go test -count=1 ./...
go vet ./...
CGO_ENABLED=1 go test -race -count=1 ./...
TEST_DATABASE_URL='<fresh-disposable-database>' \
  go test -count=1 -tags=integration ./internal/store ./internal/server
TEST_DATABASE_URL='<disposable-database>' CGO_ENABLED=1 \
  go test -race -count=1 -tags=integration ./internal/store \
  -run '^TestBilling(Entitlements|Plan|Postgres|Source)'
node internal/server/testdata/model_picker.cjs
node internal/server/testdata/billing_plans_browser.cjs
gofmt -l .
git diff --check
```

Go 单元测试同时运行 Node 界面回归。浏览器脚本可用 `PLAYWRIGHT_MODULE` 指向仓库外
Playwright 模块，用 `PLAYWRIGHT_CHROMIUM_EXECUTABLE` 指定浏览器。完整集成测试应使用
新数据库，部分旧用例会创建固定用户名及唯一 Owner。

## 界面截图

桌面 1440×1080、手机 390×844；检查页面没有横向溢出或未捕获脚本错误。

| 场景 | 截图 |
| --- | --- |
| 独立订阅与本人消费偏好 | [桌面](screenshots/multi-subscriptions/subscriptions-desktop.png) |
| 手机余额与消费偏好 | [手机](screenshots/multi-subscriptions/subscriptions-mobile.png) |
| 手机独立订阅卡片 | [订阅卡](screenshots/multi-subscriptions/subscription-cards-mobile.png) |
| 独立套餐页面 | [套餐目录](screenshots/multi-subscriptions/plans-desktop.png) |
| 自定义周期与模型范围 | [套餐编辑](screenshots/multi-subscriptions/plan-models-desktop.png) |
| 统一模型选择器 | [手机选择器](screenshots/multi-subscriptions/model-picker-mobile.png) |
| 追加续费与实际生效时间 | [续费确认](screenshots/multi-subscriptions/renewal-entitlements-desktop.png) |
| 管理员重开与被替换权益 | [重开确认](screenshots/multi-subscriptions/admin-reopen-entitlements-desktop.png) |
| 模型权限统一选择器 | [桌面](screenshots/multi-subscriptions/model-access-desktop.png)、[手机](screenshots/multi-subscriptions/model-access-mobile.png) |
