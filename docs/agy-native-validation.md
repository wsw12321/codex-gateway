# AGY 原生 Gemini 接入验证

本页记录本地协议、数据库和界面验证，历史 AGY 1.2.4 抓包夹具保持原始内容。
2026-09-28 的 CLI 指南与持久配置说明见下方“管理界面与配置命令回归”；客户端兼容
基线为官方 AGY 1.2.12。模型权限和 `0023` 升级要求以
[Antigravity 接入说明](gemini-pro.md) 为准。真实订阅账号到 Google 的验收仍待执行。

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

2026-09-28 补充执行原生别名回归：三个兼容名称在校验、授权及额度预留前归一到
`gemini-3.1-pro-high`。测试覆盖 Key 白名单和用户权限拒绝、未配置路由与价格、
标题请求占用共享配额，以及响应模型、倍率和账单使用实际模型。完整 store 与 server
数据库测试通过；本轮使用一次性 PostgreSQL `17.6`，无生产数据库读写。

同日设置 `AGY_CLI_TEST_BINARY`，运行真实官方 AGY `1.2.12` 客户端，使用独立中文及
空格路径 HOME 和合成 Gateway Key。客户端连接实际 Gateway、Bridge 协议适配器及
测试数据库，仅上游生成执行器返回固定内容。文本、标题及 `manage_task(Action=list)`
工具续轮全部通过，三个请求均完成并按 `gemini-3.1-pro-high` 结算；这一流程也通过
race 检查。测试不访问 Google，未使用真实订阅凭证。

```sh
AGY_CLI_TEST_BINARY=/path/to/official-agy-1.2.12 \
TEST_DATABASE_URL='postgres://gateway:password@127.0.0.1:5432/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/server -run TestNativeGeminiLifecyclePostgresIntegration
```

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
尾部错误、不完整 SSE、非法用量、超时和取消。JSON 及旧版单事件协议不提前输出；新版
增量协议允许已校验的文本先到达，但错误流不发送成功终止结果、工具调用或有效用量。

## 原生 Gemini 增量输出

2026-09-28 补充 Bridge `text_delta` 到 Gateway 的增量路径；非流式和旧版协商回退保留。
用真实 AGY `1.2.4` 与 `1.2.12`、合成 Gemini 服务验证了 CLI NDJSON 增量与最终文本一致。
真实 AGY `1.2.12` 连接实际 Bridge/Gateway 协议路径的回归，要求客户端在执行器获准
结束之前报告增量文本，验证无重复正文，并验证流中途错误不能成为成功回答。

测试另外覆盖长工具参数缓冲期间保活、工具及非法用量不提前释放、响应身份和文本前缀
一致性、尾部错误、超限、不完整 SSE、大整数 Token 不丢精度，以及取消后的生成进程和
工作目录清理。账号测试验证输出前可故障切换、输出后不混用账号，名额最终释放。

一次性 PostgreSQL 的原生生命周期回归使用增量执行器，包含真实客户端的文本、标题和
工具续轮；失败流验证在 `http.ErrAbortHandler` 之前完成失败记录、一次零金额结算及
并发租约释放。`httpx.Recover` 保留这一有意的 HTTP 中止，不改成正常结束的响应。

```sh
AGY_CLI_TEST_BINARY=/path/to/official-agy-1.2.12 \
  go test -count=1 ./internal/proxy -run TestNativeGeminiRealAGYIncrementalTextAndStreamError
AGY_CLI_TEST_BINARY=/path/to/official-agy-1.2.12 \
TEST_DATABASE_URL='postgres://gateway:password@127.0.0.1:5432/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/server -run TestNativeGeminiLifecyclePostgresIntegration
```

完整 `go test -count=1 ./...`、受影响四个包的 race、原生 PostgreSQL 生命周期 race、
`go vet ./...` 以及 Gateway/Bridge 构建通过。race 使用本机 Go `1.26.8` 容器，源码
只读挂载；凭据属主测试在容器临时目录中运行，数据库为一次性 PostgreSQL `17.6`。
这些验证没有调用真实 Google 推理或部署生产服务器。

## 管理界面与配置命令回归

2026-09-28 的配置器回归覆盖首次配置、重复执行、修改前备份、无关配置保留、中文和
空格路径、`CODEX_HOME`、输入取消、解析或登录失败不修改原文件，以及部分写入失败
回滚。实际新 Bash 进程在没有继承 Gateway 环境变量时成功读取保存的地址和 Key；
Zsh 验证加载文件生成和 `ZDOTDIR`，未运行真实 Zsh / macOS 终端。

启用 `GATEWAY_TEST_REAL_CODEX=1` 后，19 项 Node 测试全部通过，包括真实 Codex
`0.158.0` 在隔离目录中的合成 Key 登录、重新运行 `login status`、重复配置，以及
非法 TOML 和重复键拒绝。旧默认 `profile` 会明确报错并保持原文件，防止覆盖新的
Gateway 设置。Windows 用户环境变量及 Codex `.cmd` shim 的调用、失败回滚另有仿真测试。

```sh
GATEWAY_TEST_REAL_CODEX=1 node --test internal/server/testdata/configure_client_test.cjs
```

本轮完整 `go test -count=1 ./...`、`go test -race -count=1 ./...`、`go vet ./...`、
Gateway 构建、`gofmt -l .` 和 `git diff --check` 均通过。race 使用固定 Go `1.26.8`
镜像在隔离容器内执行，并挂载 Node 运行配置器回归；未重新执行部署镜像或 Compose
校验，上文保留的镜像验证记录属于此前任务。

2026-09-28 使用合成用户状态，以 Playwright `1.63.0`、Chromium `153.0.8010.12`
完成 `internal/server/testdata/agy_guide_browser.cjs` 回归。浏览器无脚本错误，桌面
1440×1080 与移动端 390×844 均通过，截图已更新。

- 公共准备、Codex CLI、agy CLI 三部分均包含需要的安装、配置和启动说明。
- 官方安装命令正确，Windows 明确使用 Win+R → `cmd`，Node.js 提供 LTS 安装入口。
- 两种配置命令均使用浏览器当前 origin 下载同站配置器；复制内容与对应代码块一致。
- 模拟启动器的独立临时目录、中文和空格路径、继承终端输入，以及成功、HTTP 下载失败、
  网络失败、进程启动失败、登录失败和取消后的清理与退出状态。
- 指南显示 AGY 1.2.12、三个原生别名及标题请求实际模型计费，并提示重新打开终端。
- 页面无旧下载入口，不保存 Key 或其他内容到浏览器本地存储，移动端无横向溢出。

复现命令（Playwright 及浏览器安装在仓库外）：

```sh
PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
  node internal/server/testdata/agy_guide_browser.cjs
```

完整指南截图：[桌面](screenshots/usage-guide-desktop.png)、
[移动端](screenshots/usage-guide-mobile.png)；agy 部分截图：
[桌面](screenshots/agy-guide-desktop.png)、[移动端](screenshots/agy-guide-mobile.png)。
截图只记录指南内容，不表示真实订阅生成成功。

浏览器回归在 Linux 上执行。另于 2026-09-28 使用 Windows `cmd.exe` 与官方便携版
[Node.js v24.16.0](https://nodejs.org/dist/v24.16.0/node-v24.16.0-win-x64.zip) 执行真实 CMD
启动器冒烟。Node 未安装到系统，所有文件位于独立临时目录；测试为子进程设置中文加
空格的临时 HOME、USERPROFILE、TEMP 和 TMP，未修改真实用户环境变量或客户端配置。
Windows 本地 HTTP 服务提供合成配置器，执行页面原样生成的 `node -e` 命令，覆盖：

- Codex 与 agy 两个客户端参数及带端口的站点 origin 正确传入。
- 子进程出现提示后再通过 stdin 发送合成 Key，配置器收到输入；Key 不在复制命令中。
- Windows Node 能在中文和空格临时路径下创建、执行并清理下载脚本。
- 两种客户端成功退出为 `0`；模拟登录失败保留退出码 `7`；HTTP 503 下载失败退出 `1`。
  所有情况均清理独立临时目录。

仓库中的 `internal/server/testdata/client_setup_windows.cjs` 可在 Windows 仓库目录下复现：

```bat
node internal\server\testdata\client_setup_windows.cjs
```

可用便携 Node 的完整路径代替 `node`。脚本从当前 `assets/app.js` 提取启动命令，
在系统临时目录中创建独立测试目录，结束后删除全部合成文件；非 Windows 平台明确跳过。
本轮使用便携 Windows Node 运行该仓库脚本，四种场景全部通过。

以上 Windows 冒烟使用合成配置器和管道输入，未运行真实 Codex / agy 登录流程，也
未验证真实控制台的隐藏输入或 Windows 用户环境变量写入。中文目录替代测试用户目录，
不等同于实际中文用户名账户的实机验收；该验收仍待执行。官方客户端安装器和真实 Key
登录未在本轮执行。客户端持久化文件与模拟登录回归见 `internal/server` 下的配置器测试。

## 当前上线验收

在维护窗口协调切换新 Bridge 与执行 `0023` 的 Gateway。逐账号按实际可用模型进行
JSON/SSE 冒烟，管理员重新授权并重新签发受限 Key 后，用精确同名模型调用 Responses
与原生 API，核对 Standard 账单、Token、原生别名按实际模型扣费、Responses 别名拒绝
和取消后的清理。
完整备份和回滚限制见 [验收和回滚](gemini-pro.md#验收和回滚)。真实 Google 上游验收
与生产部署应在站点环境单独完成。
