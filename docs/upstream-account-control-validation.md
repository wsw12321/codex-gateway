# 上游账号分配系数与控制验证记录

## 2026-09-20 分配系数页面验证

本次兼容层基于 CLIProxyAPI `v7.2.150`、commit
`c77b13694318b0897f2c74104ef48aebdf8c34d6`。最终补丁 SHA256：
`dc0a889cc8e6b505d162e60182347544e09eb629cd38c7de11916ce50e7ce535`；镜像标签：
`v7.2.150-c77b1369-dc0a889cc8e6b505-codex-only`。Gateway 配套应用
`0009_upstream_allocation.sql`。本次只构建、验证，没有部署或访问真实 OAuth。

账号页新增分配系数、近 24 小时已结算费用、实际费用占比及参考目标占比。
费用时间窗独立于历史筛选，并显示窗口起止时间；实际占比按所有已归因账号
费用计算，参考目标按当前启用账号系数计算。页面说明实际选路会按模型及
实时可用账号重新计算。系数为 0 时显示“停止接收新对话”，明确已有有效
绑定继续使用。

保存系数沿用 Owner、近期验证及统一操作互斥；输入仅接受 `0..2147483647`
的整数。成功必须取得匹配账号 ID 和整数系数的确认响应。保存成功后的
统计刷新失败会保留已确认值和排空状态，并分别展示保存成功与刷新失败。
重新启用系数为 0 的账号不会误报已恢复新对话分配。

验证结果：

- Gateway `go test -count=1 ./...`、`go test -race -count=1 ./...`、
  `go vet ./...` 及 `go build ./cmd/gateway`：全部通过。
- PostgreSQL 全部 store 集成测试：使用隔离测试数据库通过，包括系数持久化、
  同步保留权重、滚动费用口径及边界、未结算记录排除、新账号默认值和审计事务。
- `node internal/server/testdata/upstream_account_ui_test.cjs`：20 项通过，覆盖
  状态控制及系数保存、权限、整数边界、重复提交、近期验证／取消、响应校验、
  会话失效、刷新失败与零系数排空。
- `node --check internal/server/assets/app.js`：通过。
- `go test -count=1 ./internal/server -run 'TestUpstreamAccountsDashboard|TestUpstreamAccountDashboardBehavior'`：通过。
- Chromium `153.0.8010.12`：实际页面与模拟 API 联动验证输入错误、密码二次
  验证后保存、零系数排空、历史筛选独立性、成员权限、保存失败、保存成功后
  刷新失败及恢复；6 次配置请求，页面异常为 0。
- 桌面 `1440×1080`、移动端 `390×844`：截图已保存；移动端没有横向溢出。
- `./scripts/compose.sh build gateway codex-compat`：两个镜像构建通过；兼容层
  构建内完整 9 个包、命名执行器回归和新增分配竞态测试全部通过。
- 兼容层 `go test -race -count=1 ./sdk/cliproxy/auth`：整个账号包通过；
  `go vet ./sdk/cliproxy/auth ./sdk/cliproxy`：通过。
- 分配回归覆盖已有绑定及 0 系数排空、单候选／无显式会话／派生及 LCP 会话、
  并发首次绑定、失效与重启、固定 URL／无代理／禁止重定向、取消、畸形回调、
  回调期间禁用账号、插件／Home 绕过拒绝、回调失败 503、两账号切换及最终归因。
- `./scripts/test-sidecar-image.sh`：新镜像在无网络、合成凭据环境通过 OAuth
  清单、权限拒绝及无 Gateway 时独立启动检查。
- `python3 scripts/tests/test_oauth_login.py`：11 项通过，包含 Gateway 就绪顺序
  及启动失败后停止未验证 sidecar；修改的 Shell 脚本语法检查通过。
- Compose 校验：当前本地 `.env` 的占位域名按预期被拒绝；另建独立非生产配置
  及合成 secrets 后，完整静态策略和固定镜像的 Caddy 配置校验通过。Caddy
  校验容器使用无网络模式，不分配项目固定 IP、不挂载项目 secrets 或持久卷。
- 当前 Caddy 配置的隔离 HTTP 验证：`GET /internal`、
  `GET /internal/upstream-accounts/select` 和同路径 `POST` 均返回 404。

截图使用合成脱敏邮箱、模拟已结算费用与模拟上游，无真实 OAuth 凭据：

- [桌面系数与费用占比](screenshots/upstream-allocation-desktop.png)
- [移动端系数与费用占比](screenshots/upstream-allocation-mobile.png)
- [保存成功后统计刷新失败](screenshots/upstream-allocation-refresh-failure.png)

浏览器验证脚本位于
[`internal/server/testdata/upstream_allocation_browser.cjs`](../internal/server/testdata/upstream_allocation_browser.cjs)。
在应用目录外安装 Playwright 和 Chromium 后运行：

```sh
PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
  node internal/server/testdata/upstream_allocation_browser.cjs
```

脚本拦截所有请求，不连接实际服务。Linux 无中文字体时需提供可用的
Fontconfig 中文字体配置；本次使用临时字体配置。受限沙箱无法启动 Chromium，
本次获准在沙箱外运行上述模拟浏览器验证。

## 2026-09-12 禁用与恢复验证（历史记录）

验证日期：2026-09-12。测试使用临时账号、模拟上游及隔离部署配置，没有访问真实
OAuth 账号，也没有切换生产。无需数据库迁移。

交付补丁基于 CLIProxyAPI `v7.2.150`、commit
`c77b13694318b0897f2c74104ef48aebdf8c34d6`；SHA256 为
`00633c2417755730b8abe7c5d273135a43d449d1952c3a1d489b7fbae9e32e7f`。
Compose 镜像标签为 `v7.2.150-c77b1369-00633c2417755730`。

### 自动验证

| 检查 | 结果 |
| --- | --- |
| Gateway `go test -count=1 ./...` | 通过，锁定 Go 1.24.6 |
| Gateway `go test -race -count=1 ./...` | 通过，锁定 Go 1.24.6 |
| Gateway `go vet ./...`、`go build ./cmd/gateway` | 通过，构建产物写入临时目录 |
| 格式、变更空白、Shell 与 JavaScript 语法 | 通过 |
| 页面行为测试 | 10 项 Node 测试通过，由 Go 资源测试调用 |
| 锁定源码上 `git apply --check --ignore-space-change` 及应用补丁 | 通过，72 个变更文件逐字节一致 |
| sidecar Dockerfile 中整组补丁回归与指定执行器回归 | 通过，Go 1.26.0，全新源码目录应用最终补丁后执行 |
| sidecar 受影响包 `go vet` 和 `cmd/server` 编译 | 通过；源码归档验证构建关闭自动 VCS 标记，显式注入锁定版本与 commit |
| sidecar API、文件监听、文件加载、账号调度、服务生命周期竞态测试 | 通过 |
| 实际 Codex 执行器与 Manager 联动竞态测试 | 8 项场景通过 |
| `./scripts/compose.sh config --quiet` | 通过，临时非生产配置 |
| `./scripts/validate-compose.sh` | 静态策略全部通过；最终 Caddy 容器检查因缺少 Docker daemon 未完成 |
| `./scripts/compose.sh build gateway codex-compat` | 已执行，因缺少 Docker daemon 失败；另提示未安装 buildx |

兼容层整组包包括 `internal/api`、`internal/auth/codex`、`internal/logging`、
`internal/watcher`、`internal/watcher/synthesizer`、`sdk/api/handlers`、`sdk/auth`、
`sdk/cliproxy/auth` 和 `sdk/cliproxy`；执行器命名测试由 Dockerfile 固定并验证存在。

回归覆盖手动禁用、所有模型冷却清除、普通 429 到期恢复、明确耗尽锁定及重新
启用后再次耗尽；覆盖重复账号文件、文件替换、OAuth 刷新、状态重载、会话亲和、
账号消失、全部账号不可用、并发操作、旧请求结果、状态损坏及持久化失败。
普通响应、compact、SSE 首字节前后错误使用实际 Codex 执行器验证；禁用前已开始
的普通请求和 SSE 完成后不会解除禁用，重新启用前的旧耗尽不会重新锁定。

接口测试覆盖 Owner、同源、近期验证、内部 Bearer、严格布尔请求、重复／未知
字段、非法 ID、确认响应、错误脱敏、审计与列表同步顺序。页面测试覆盖重复提交、
旧列表响应、验证取消、账号消失、确认结果异常、同步失败及成功后的刷新失败。

### 浏览器与截图

Chromium `153.0.8010.12`，桌面 `1440×1080`、移动端 `390×844`。模拟接口实际
执行禁用、密码再次验证、重新启用和刷新恢复；检查无页面异常、无移动端横向溢出。
截图仅使用合成脱敏邮箱与模拟用量。

- [桌面账号操作](screenshots/upstream-account-controls-desktop.png)
- [移动端账号操作](screenshots/upstream-account-controls-mobile.png)
- [操作成功但刷新失败](screenshots/upstream-account-refresh-failure.png)

### 环境限制及上线前检查

本机不存在 `/var/run/docker.sock`，实际错误为
`Cannot connect to the Docker daemon at unix:///var/run/docker.sock`。
已在临时目录准备 Docker 29.1.3、Compose 2.40.3 和非生产 secrets，确认此错误
来自缺少 daemon。没有修改工作区 `.env` 或 `deploy/secrets`。

上线前仍需在具备 Docker daemon／buildx 的环境完成整个 Compose 校验和两镜像
构建，再按[升级规程](compatibility-upgrades.md)执行真实 OAuth 冒烟与单 sidecar
切换。PostgreSQL 集成测试未执行，本次沿用现有表与状态统计结构。

磁盘无法写入时，自动额度锁定仍阻止当前进程分流，但无法保证新增锁定重启后
存在；固定日志和恢复流程见[运维文档](operations.md)。启用操作持久化失败时
不会报告成功，并继续阻止分流。
