# Antigravity 账号管理界面验证

管理员新增 `#antigravity-accounts` 页面，与“上游账号”共用账号卡片、筛选、敏感操作验证和权限编辑流程。切换页面会清除旧列表与实时并发采样，并使尚未完成的请求失效，避免两个提供方的账号和操作混用。

## 功能对应

| 功能 | Antigravity 页面 |
| --- | --- |
| 新增或刷新登录 | 页面说明 `./scripts/antigravity-login.sh <账号名称>`；多个名称对应多个账号；无参数继续维护默认账号 |
| 账号身份 | 脱敏邮箱、登录名称、套餐与最近同步时间；不显示凭据 |
| 状态控制 | 启用、禁用、上游就绪状态、手动状态、限流冷却与最终分流状态 |
| 轮换分配 | 分配系数、近 24 小时已结算费用、费用占比与参考目标；系数 0 停止接收新请求 |
| 并发 | 每账号请求并发上限，当前执行请求数每 5 秒采样；过期或失败样本显示暂不可用 |
| 使用权限 | 共享、指定授权用户、搜索和批量选择；写操作要求近期身份验证 |
| 历史用量 | 本月、7 天、30 天、自定义、全部历史；请求、错误、各类 Token 与 API 等价成本 |
| 官方额度 | 不显示查询按钮；说明没有精确额度百分比或重置时间 |

AGY 请求没有稳定的对话标识，因此界面明确采用请求并发。429 导致约 60 秒自动冷却；重新启用只恢复手动开关，不会绕过冷却或修复失效凭据。Codex 页面继续使用 root 对话并发及原有额度查询。

## 已执行验证

2026-09-28，Node 24.16.0、Chromium 153.0.8010.12，所有浏览器请求均使用合成数据和本地拦截，没有访问真实上游账号。

- `node --test internal/server/testdata/upstream_account_ui_test.cjs`：原有 Codex 控制与新增 Antigravity API 命名空间、请求并发文案、额度操作隐藏、登录名称、冷却与手动开关区别，以及验证期间失效操作的回归覆盖。
- `node internal/server/testdata/antigravity_accounts_browser.cjs`：7 次成功写操作，覆盖系数、并发上限、启停、专属授权用户与密码二次验证；全部历史筛选；同步故障下禁止操作；写入成功但刷新失败的独立提示；页面切换时旧列表响应丢弃；权限验证中切换提供方取消写入；Member 无权进入页面。
- `node internal/server/testdata/upstream_allocation_browser.cjs`：原有 Codex 分配系数、验证、失败恢复与响应式布局回归通过。
- `node internal/server/testdata/groups_access_browser.cjs`：原有群组和账号使用权限回归通过。

浏览器检查无 JavaScript 错误。桌面 1440×1080、移动端 390×844 均无横向溢出。截图：[桌面](screenshots/antigravity-accounts-desktop.png)、[移动端](screenshots/antigravity-accounts-mobile.png)。

复现浏览器检查时，Playwright 和 Chromium 可安装在仓库外：

```sh
PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
  node internal/server/testdata/antigravity_accounts_browser.cjs
```

界面测试只验证前端行为；账号轮换、持久化、权限边界与请求转发由对应 Go 单元和集成测试验证。
