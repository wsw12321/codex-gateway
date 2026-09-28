# AGY 原生 Gemini 接入验证

日期：2026-09-27。本页保留旧 preview/customtools 接入的历史验证记录，
不代表当前同名模型方案的验收。当前请求示例、模型权限和 `0023` 升级要求以
[Antigravity 接入说明](gemini-pro.md) 为准；历史抓包夹具保持原始内容。
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

登录持久化修复使用加入 `libsecret-tools` 的 Bridge 镜像另行完成上述镜像测试。
AGY 版本仍为 `1.2.4`，无数据库迁移或对外 API 变化。测试用一次性 Keyring 卷和
无网络、只读根文件系统的容器执行真实 `auth-login` / `auth-verify` 凭据管理代码；
仅以假 CLI 替代 Google。合成认证文件超过 8 KiB，覆盖工具输入上限所需的分块
保存、项目/地区/订阅等完整字段，以及以下过程：

- 首个容器导入完整文件；第二个容器恢复后模拟刷新，后续用量和生成检查读取更新值。
- 第三个容器和全新 HOME 再次恢复更新后的完整文件；非认证状态没有跟随持久化。
- 文件/目录权限为 `0600` / `0700`；后验没有 TTY，`TERM=dumb`，无需输入的命令读到 EOF。
- 错误密码明确在 `keyring` 阶段拒绝；持久文件不包含令牌明文或编码后的凭据分块。
- 验证输出不含合成令牌、模型回复或 CLI stderr；测试结束删除一次性卷。

此次修复的完整 Go 单测、race、vet、两个程序构建和 49 项脚本回归均通过；
最终凭据结构校验补充后另行重跑受影响包的单测、race、vet 和构建。终端测试使用
真实 PTY，覆盖交互授权读取、前台进程组归还，以及脚本正常、失败和 HUP/INT/TERM
退出后的终端恢复。Compose 在独立临时副本和测试 secrets 下校验通过，race 仍使用
固定 Go 镜像在无外部网络的容器内执行。

测试没有进行真实 Google 授权、生产验收或服务器部署。上线时仍需重新登录并检查
脚本最终的 `Antigravity login persisted; readiness, JSON and SSE passed.` 标志。

脱敏夹具位于 `internal/antigravity/testdata/agy-1.2.4-native.json`，覆盖 AGY 默认
参数、工具 schema、调用 ID 与结果关联、无签名工具循环及标题模型请求。网关测试
另覆盖无效/停用 key、多来源凭据、模型权限、未知模型、请求大小限制；代理测试覆盖
尾部错误、不完整 SSE、非法用量、超时和取消，确认无效结果不会先输出成功响应。

## 管理界面回归

当前 `internal/server/testdata/agy_guide_browser.cjs` 使用合成用户状态验证更新后的指南：

2026-09-28 已以 Playwright `1.63.0`、Chromium `153.0.8010.12` 完成下列回归，
浏览器无脚本错误，桌面 1440×1080 与移动端 390×844 均通过并更新截图。

- Codex 地址保留 `/v1`，Gemini 示例采用当前站点 origin 和 `/v1/responses`。
- JSON 正文与 Bash/curl 请求均使用 `gemini-3.1-pro-high`，Key 经 stdin 传入 curl。
- 展示七个精确模型 ID、升级后默认禁用和旧 AGY Gemini 提供方无法直接连接的说明。
- 地址和请求命令复制内容正确；检查浏览器脚本错误与移动端横向溢出。

复现命令（Playwright 及浏览器安装在仓库外）：

```sh
PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
  node internal/server/testdata/agy_guide_browser.cjs
```

脚本生成 [桌面截图](screenshots/agy-guide-desktop.png) 和
[移动端截图](screenshots/agy-guide-mobile.png)。截图只记录指南内容，不表示真实订阅生成成功。

## 当前上线验收

在维护窗口协调切换新 Bridge 与执行 `0023` 的 Gateway。逐账号按实际可用模型进行
JSON/SSE 冒烟，管理员重新授权并重新签发受限 Key 后，用精确同名模型调用 Responses
与原生 API，核对 Standard 账单、Token、旧路径拒绝和取消后的清理。
完整备份和回滚限制见 [验收和回滚](gemini-pro.md#验收和回滚)。真实 Google 上游验收
与生产部署应在站点环境单独完成。
