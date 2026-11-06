# 出口服务器 SS2022 与连通性测试验证

“出口服务器”页面的节点新增 `2022-blake3-aes-128-gcm` 加密方式，并可对每个节点做
连通性测试。用法与结果码说明见[上游账号独立出口](account-egress.md)。

## 行为与安全影响

- 迁移 `0045_ss2022_egress_cipher.sql` 只放宽 `egress_servers.cipher` 的 CHECK，
  现有节点不变。迁移只能前进：数据库应用后，旧版 Gateway 会拒绝启动。
- 支持的加密方式与密钥格式统一定义在 `internal/egress`，Gateway 保存接口、Store、
  `account-egress` 运行时和网页四处共用同一规则。
  - `2022-blake3-aes-128-gcm` 的密码必须是一个或多个标准 Base64 密钥（带填充），
    以英文冒号分隔，每个解码后正好 16 字节。规则与锁定的 Mihomo v1.19.32
    （`sing-shadowsocks2` v0.2.8）一致。
  - 运行时也拒绝格式错误的密钥。Mihomo 会因一个无效密钥拒绝整份配置，导致所有
    节点都无法应用，因此不能依赖 Mihomo 报错。
  - 已有节点切换为 SS2022 且密码留空时，Gateway 先解密现有密码并检查格式，再以
    “节点版本未变”为条件写入。并发修改会返回 409，不会把未校验的旧密码与新加密
    方式组合起来。错误响应不回显密码。
- 全局 `.env` 出口（`SHADOWSOCKS_CIPHER`）没有改动，暂不支持 SS2022。
- 连通性测试：
  - 新接口 `POST /admin/egress-servers/{id}/test`：同源检查、Owner 会话。测试不修改
    配置，因此不要求 5 分钟内二次验证。请求体只接受 `target` 字段
    （`codex`、`antigravity`、`anthropic`，缺省为 `codex`）。
  - Gateway 先同步运行时，只测试运行时已应用的节点版本；未应用时返回 409。
  - 运行时新增控制接口 `POST /probe`，使用与 `/config` 相同的 Bearer 密钥，只在
    控制网络 `172.28.70.0/24` 上提供。目标域名必须在现有 CONNECT 白名单内，端口固定
    为 443。
  - 测试连接只经过该节点版本的本机 Mihomo 监听，不直连目标，不经过 Squid，也不
    使用账号路由凭据。证书按系统信任库校验，要求 TLS 1.2 或更高。
  - 运行时同时最多执行 4 个测试，超出时返回 `probe_busy`；每个测试最长 10 秒。
    测试期间持有该节点版本，修剪逻辑不会移除它的监听。
  - 返回内容只有固定结果码和耗时。Gateway 把未知结果码归一为 `handshake_failed`，
    并把异常耗时置 0。响应不含节点地址、密码、路由 Token 或底层错误文本，也不写
    日志或审计记录。
- 网页：
  - 每个节点卡片有“测试连通性”按钮；页面有“全部测试”按钮（同时最多 3 个）和
    测试目标选择。
  - 结果按节点 ID 与版本保存在当前页面，节点修改后显示“请重新测试”；换登录身份
    时清空。
  - 选择 SS2022 时提示 Base64 密钥格式，提交前先在本地检查格式。
  - 节点对话框的“允许 UDP”复选框改用已有的 `check-label` 样式；原来的
    `checkbox-label` 类没有任何样式，复选框会被拉伸显示。

## 验证

```sh
gofmt -l .
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./cmd/account-egress ./internal/egress ./internal/store
TEST_DATABASE_URL=… go test -count=1 -tags=integration ./internal/store ./internal/server
node --test internal/server/testdata/egress_ui_test.cjs
PLAYWRIGHT_MODULE=… SCREENSHOT_DIR=docs/screenshots node internal/server/testdata/egress_browser.cjs
python3 -m unittest discover -s scripts/tests -p 'test_*.py'
scripts/ci-go-tool.sh github.com/securego/gosec/v2/cmd/gosec@v2.22.8 -- ./...
```

以上命令在本地全部通过；集成测试使用本机一次性的 PostgreSQL 16。有一个例外：
`TestClientConfigurator` 中的两项 Claude 配置子测试，在宿主环境设置了
`CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` 和 `ANTHROPIC_BASE_URL` 时会失败。未改动
的基线提交同样失败；清除这两个变量后通过，与本次改动无关。

覆盖内容：

- `internal/egress`：四种加密方式的接受与拒绝（包括 `2022-blake3-aes-256-gcm`）；
  SS2022 单密钥与多密钥；无填充、17/32 字节、URL Base64、空段、前后空白；
  原有密码的长度、换行和 NUL 限制。
- `cmd/account-egress`：
  - SS2022 节点原样传给 Mihomo；无效密钥和不支持的加密方式在下发前被拒绝，
    已应用的配置不受影响。
  - 测试成功路径：模拟 Mihomo 先回应 CONNECT，再经过它与持有 `chatgpt.com`
    证书的本地 TLS 服务完成握手。
  - 失败路径：证书不受信任、隧道建立后立即关闭（即错误密码）、监听拒绝、
    握手超时、本地监听不存在、节点版本未应用、测试并发已满。
  - 输入校验：非白名单目标、带端口、大写、IP、未知字段、重复 JSON、错误密钥。
  - 测试结束后节点引用计数归零；结果中不含密码、节点地址或路由 Token。
- PostgreSQL 集成（`TestEgressSS2022AndConnectivityProbePostgres`）：
  - 无效 SS2022 密钥返回 400 且不回显密码；有效多密钥可以创建。
  - SS2022 节点留空密码可以保存；旧节点留空切换为 SS2022 返回
    `egress_password_required`，填写新密钥后可以切换。
  - 用过期版本保存时返回 `ErrConflict`。
  - 模拟运行时确认 Gateway 从不下发无效密钥。
  - 测试接口：未登录 401、成员 403；三个目标映射正确；运行时异常结果码被归一；
    非法目标或多余字段返回 400；节点不存在返回 404；运行时故障返回 503；
    节点修改未应用时返回 409，且不会调用运行时。
- 网页 Node 测试：
  - 请求路径与请求体正确；未知目标回落到 Codex。
  - 成功和失败文案，以及修改节点后显示“请重新测试”。
  - 换登录后迟到的结果被丢弃；“全部测试”跳过未应用节点，并发不超过 3。
  - SS2022 密钥在请求前检查；编辑时密码是否必填，以及提示文案。
- Playwright（Chromium）：新增和编辑节点；SS2022 错误密钥在对话框中报错，修正后
  保存成功；“全部测试”显示一个成功、一个失败；切换为 Anthropic 后单节点重测；
  账号绑定和解除；两个节点依次删除。还检查了 390 px 宽度下没有横向滚动。
- 镜像测试 `scripts/tests/account_egress_image.py` 增加第三个合成节点和 SS 服务
  `2022-blake3-aes-128-gcm`，回显目标为 `EXIT_C`。测试确认三种全局模式下都能使用
  该节点，配置更新和错误密码场景下它仍然可用，日志中不出现 SS2022 密钥。
  本地环境没有 Docker 守护进程，且无法下载 Mihomo 源码归档，因此该镜像测试只在
  CI 中运行。

截图（合成数据）：

- [桌面](screenshots/egress-desktop.png)
- [移动端](screenshots/egress-mobile.png)
- [SS2022 密钥校验](screenshots/egress-ss2022-dialog.png)
