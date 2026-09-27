# AGY 原生 Gemini 接入验证

日期：2026-09-27。客户端配置和能力边界见 [Antigravity 接入说明](gemini-pro.md)。
本记录区分本地协议、数据库和界面验证；真实订阅账号到 Google 的验收仍待执行。

## 数据库与请求生命周期

在一次性 PostgreSQL `17.6` 容器上执行完整 store integration suite，全部通过：

```sh
TEST_DATABASE_URL='postgres://gateway:password@127.0.0.1:5432/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/store
```

新迁移 `0020_gemini_endpoints.sql` 扩展请求、日汇总和月汇总 endpoint 约束。
Gemini 回归对 `gemini.generateContent`、`gemini.streamGenerateContent` 两个接口
覆盖 200,000 / 200,001 输入 Token 的短/长上下文价格、缓存作为输入子集、思考计入
输出、并发及重复结算只产生一条账单、日/月汇总保留独立 endpoint。资金已满足但
流量配额不足的请求验证 quota、usage、billing reservation 和 ledger 均不留记录；
群组额度拒绝也覆盖两个原生接口。

Gateway 集成测试 `TestNativeGeminiLifecyclePostgresIntegration` 使用实际 HTTP
处理器、实际 Bridge 协议适配器和一次性 PostgreSQL schema；仅以固定执行器替代
Google CLI 进程。验证文本、工具调用与 `role=model` 中的工具结果、同会话模型别名
切换、凭据隔离、Token 计量与一次结算、配额及资金不足拒绝回滚，以及超时和取消
后的租约清理：

```sh
TEST_DATABASE_URL='postgres://gateway:password@127.0.0.1:5432/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/server -run TestNativeGeminiLifecyclePostgresIntegration
```

上述结果不表示真实 AGY 进程、订阅凭证或 Google 上游已经验收。

## Go、Compose 与镜像

Go `1.26.8` 下完整单测、race、静态检查和两个程序构建通过：

```sh
go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 ./...
go vet ./...
go build ./cmd/gateway ./cmd/antigravity-bridge
gofmt -l .
git diff --check
```

宿主默认关闭 CGO 且没有 C 编译器，race 使用 `deploy/images.lock.env` 固定的
Go Docker 镜像执行；源码和模块缓存只读挂载，容器无外部网络。最后的请求缓冲区
并发许可调整另行通过相关 server race 和静态检查。

在独立临时检出副本中生成测试 `.env` 与测试 secrets，完成 Compose 校验及
`gateway`、`codex-compat`、`antigravity-bridge` 三个镜像构建。主工作目录没有创建
站点配置或登录凭据。Bridge 镜像的无网络、只读文件系统测试通过 Keyring 创建、
容器重启后解锁及错误密码拒绝。

```sh
./scripts/validate-compose.sh
./scripts/compose.sh --progress plain build gateway codex-compat antigravity-bridge
./scripts/test-antigravity-image.sh <built-bridge-image>
```

脱敏夹具位于 `internal/antigravity/testdata/agy-1.2.4-native.json`，覆盖 AGY 默认
参数、工具 schema、调用 ID 与结果关联、无签名工具循环及标题模型请求。网关测试
另覆盖无效/停用 key、多来源凭据、模型权限、未知模型、请求大小限制；代理测试覆盖
尾部错误、不完整 SSE、非法用量、超时和取消，确认无效结果不会先输出成功响应。

## 管理界面

使用 Playwright `1.63.0`、Chromium `153.0.8010.12` 及合成用户状态验证使用指导：

- Codex 地址保留 `/v1`，AGY 地址精确等于站点 origin。
- `modelProvider` 配置为 `gemini`；说明要求合并现有配置。
- 启动命令读取 Gateway key 到当前终端环境，模型为 `gemini-3.1-pro-high`。
- 地址和启动命令复制内容正确；浏览器没有脚本错误。
- 1440×1080 桌面和 390×844 移动端无页面横向溢出。

截图为 AGY 指导面板，截取时隐藏粘滞导航以避免遮挡：
[桌面截图](screenshots/agy-guide-desktop.png)、[移动端截图](screenshots/agy-guide-mobile.png)。

复现命令（Playwright 及浏览器安装在仓库外）：

```sh
PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
  node internal/server/testdata/agy_guide_browser.cjs
```

## 上线验收

先部署新版 Bridge，再部署执行迁移的 Gateway。使用真实订阅账号从本机 AGY
完成文本和本地工具任务，确认两种模型别名切换、Flash Lite 标题 404 不阻断任务、
真实 Token 与账单对应，以及取消后的进程和租约清理。本次验证没有部署服务或修改
登录凭证；Google 上游验收仍待完成。
