# 出口服务器 SOCKS5 验证

“出口服务器”页面的节点新增 SOCKS5 协议，可与 Shadowsocks 节点一样按账号绑定。
用法见[上游账号独立出口](account-egress.md#socks5-节点)。全局 `EGRESS_MODE` 不变。

## 行为与安全影响

- 迁移 `0048_socks5_egress.sql` 给 `egress_servers` 增加 `protocol`（默认 `ss`）和
  `username`，`password_ciphertext` 改为可空，并用一条 CHECK 约束每种协议的合法组合：
  SS 必须有受支持的 cipher、无用户名、有密文；SOCKS5 无 cipher，用户名为空时密文必须
  为空，否则必须有密文。CHECK 用 `COALESCE` 处理空密文，避免 NULL 让约束失效。
  现有节点不变。迁移只能前进：数据库应用后，旧版 Gateway 会拒绝启动。
- 协议常量与 SOCKS5 凭据规则（RFC 1929：各不超过 255 字节、单行 UTF-8，用户名和密码
  同时为空或同时非空）定义在 `internal/egress`，Gateway 保存接口、Store 与
  `account-egress` 运行时使用同一份规则，非法节点在到达 Mihomo 前被拒绝。
- 密码沿用现有 AES-GCM 加密与节点 ID 绑定；用户名明文保存，只出现在 Owner 的列表与
  编辑表单中，不进入审计元数据或日志。浏览器响应不含密码或密文。
- 留空密码只在协议不变且已有密码时保留；切换协议或启用 SOCKS5 认证须重新填写
  （`egress_password_required`），保留时带 revision 乐观校验。清空用户名会删除密码。
- 运行时为 SOCKS5 节点生成 Mihomo `type: socks5` 出站（无 TLS），仍只经本机 loopback
  HTTP 监听接入，`MATCH,REJECT` 与无直连回退不变。CPA 插件、`resolve-egress` 与
  路由凭据格式不变。
- SS 节点下发给运行时的 JSON 不含 `protocol`/`username` 字段，只有 SS 节点时与旧运行时
  完全兼容；新运行时把缺省协议视为 SS。含 SOCKS5 节点的快照需要新版 `account-egress`。
- SOCKS5 不加密：凭据与目标域名在运行时到 SOCKS5 服务器之间可见，模型流量本身仍是
  TLS。文档提示只在可信链路上使用。

## 验证命令与结果

```sh
gofmt -l .                                   # 无输出
go vet ./...                                 # 通过
go test -count=1 ./...                       # 通过
go test -race -count=1 ./cmd/account-egress ./internal/egress ./internal/store   # 通过
node --test internal/server/testdata/egress_ui_test.cjs                         # 16/16 通过
TEST_DATABASE_URL=postgres://…/gateway_test go test -count=1 -tags=integration ./internal/store ./internal/server   # 通过（本地 PostgreSQL 16）
python3 -m unittest discover -s scripts/tests -p 'test_*.py'                    # 100 个通过，1 个跳过
SCREENSHOT_DIR=docs/screenshots node internal/server/testdata/egress_browser.cjs # 通过
```

新增覆盖：

- `internal/egress/socks5_test.go`：协议取值与 SOCKS5 凭据边界。
- `cmd/account-egress/runtime_test.go`：SOCKS5 有/无认证的 Mihomo 配置形状、非法字段
  （带 cipher、只有用户名或密码、超长、换行、未知协议）不会到达 Mihomo 且不影响已应用
  配置；缺省协议仍渲染为 SS，SS 节点的线上格式不变。
- `internal/server/egress_test.go`：快照携带 SOCKS5 节点、无认证节点不解密、
  DTO 不泄露密码。
- `internal/store/egress_integration_test.go`：SOCKS5 生命周期（无认证、启用认证、
  保留与清除密码、切换到 SS）、数据库 CHECK 拒绝非法组合、审计不含凭据。
- `internal/server/egress_integration_test.go`：HTTP 层的创建、校验错误码、
  `egress_password_required`、协议来回切换与运行时快照内容。
- `internal/server/testdata/egress_ui_test.cjs` 与 `egress_browser.cjs`：协议切换时字段显隐、
  密码要求、请求载荷与卡片文案。

`scripts/tests/account_egress_image.py` 增加了两个合成 SOCKS5 出口（Mihomo `socks` 入站，
一个要求用户名/密码，一个无认证），检查三种全局模式下的独立出口、错误密码不回退直连、
日志不含凭据。本次开发环境没有可用的 Docker 守护进程，该镜像测试未在本地运行，
需由 CI（`./scripts/test-account-egress-image.sh`）验证。

## 截图

- `docs/screenshots/egress-socks5-dialog.png`：新增 SOCKS5 节点对话框。
- `docs/screenshots/egress-desktop.png`、`egress-mobile.png`：含 SOCKS5 节点的列表。
