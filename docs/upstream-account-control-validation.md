# 上游账号禁用与恢复验证记录

验证日期：2026-09-12。测试使用临时账号、模拟上游及隔离部署配置，没有访问真实
OAuth 账号，也没有切换生产。无需数据库迁移。

交付补丁基于 CLIProxyAPI `v7.2.150`、commit
`c77b13694318b0897f2c74104ef48aebdf8c34d6`；SHA256 为
`00633c2417755730b8abe7c5d273135a43d449d1952c3a1d489b7fbae9e32e7f`。
Compose 镜像标签为 `v7.2.150-c77b1369-00633c2417755730`。

## 自动验证

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

## 浏览器与截图

Chromium `153.0.8010.12`，桌面 `1440×1080`、移动端 `390×844`。模拟接口实际
执行禁用、密码再次验证、重新启用和刷新恢复；检查无页面异常、无移动端横向溢出。
截图仅使用合成脱敏邮箱与模拟用量。

- [桌面账号操作](screenshots/upstream-account-controls-desktop.png)
- [移动端账号操作](screenshots/upstream-account-controls-mobile.png)
- [操作成功但刷新失败](screenshots/upstream-account-refresh-failure.png)

## 环境限制及上线前检查

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
