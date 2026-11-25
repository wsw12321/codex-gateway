# 聚合网关分支的 gosec 与 mihomo dhcp 修复

2026-11-25，`aggregation-gateway` 分支的两次 CI（手动触发的 [45557861500](https://github.com/wsw12321/codex-gateway/actions/runs/45557861500)，
提交 `11aadea`；PR 触发的 [45564438077](https://github.com/wsw12321/codex-gateway/actions/runs/45564438077)，提交 `e011403`）
都有两个任务失败，其余任务（Go 测试与竞态检测、PostgreSQL 集成测试、供应链检查、容器构建与回归）通过：

- `go-security` 的 gosec 步骤报 6 项：
  - `internal/server/tenant_providers.go` 的 `credentialOwnershipProtocol` 报 G101（疑似硬编码凭据）。
  - `internal/store` 中拼接 `accountScopeSQL(...)` 的 5 处查询报 G202（SQL 字符串拼接）：`ListUpstreamAccounts` 以及
    `model_suppliers.go` 的 `ModelSuppliers`、`ReplaceModelSupplierKey`、`SetModelSupplierEnabled`、`DeleteModelSupplier`。

  这 6 处都是本分支新增的代码。
- `containers` 的 Trivy 步骤在 account-egress 镜像中发现 HIGH 漏洞：mihomo 依赖的 `github.com/insomniacslk/dhcp`
  `v0.0.0-20250109001534-8abf58130905` 命中 `GHSA-jp3c-6v3g-mpxv`（畸形 IPv4 载荷长度导致拒绝服务），修复版本为
  `v0.0.0-20260719225207-c76316d4aa82`。网关和 CPA 镜像扫描结果为 0。这一项与本分支的代码无关，是漏洞库更新后新报出的；
  `main` 下次构建同样会失败。

## 修改

- gosec：
  - `credentialOwnershipProtocol` 的值是与插件约定的协议名 `credential_ownership_v1`，不是凭据。
  - `accountScopeSQL` 只拼接固定的列名、别名和占位符名，所有者及其它值都以绑定参数传入。
  - 按仓库约定，在这 6 处语句前加有针对性并写明理由的 `#nosec G101` / `#nosec G202`，SQL 和行为不变。
  - 同一片段用在 `QueryRowContext` 的语句未被 gosec v2.22.8 报出，没有加注释。
- mihomo 依赖补丁 `deploy/shadowsocks/upstream/mihomo-v1.19.32-dependencies.patch`：
  - 在原有升级之外，把 `insomniacslk/dhcp` 升到修复版本 `c76316d4aa82`（即 Trivy 给出的最低修复版本）。
  - 该版本的 `go.mod` 只要求 `x/net v0.38.0`、`x/sys v0.31.0`，低于补丁已有的版本，所以 Go 模块版本选择没有带动其它依赖：
    `go.mod` 只改这一行，`go.sum` 只新增这一版本的两行校验和。
  - 补丁仍用 `diff -u` 生成，不手工修改。先用同样方式从上游原文件和旧补丁结果重新生成旧补丁，与仓库中的文件逐字节一致，再生成新补丁。
  - `source.lock.env` 的补丁 SHA-256 更新为 `d28d1bfc…7498a`，`build.sh` 写入的版本标识改为 `v1.19.32-gateway.4`。
  - `THIRD_PARTY_NOTICES` 和 `docs/operations.md` 同步更新。
  - 镜像标签 `1.19.32-v2` 与上次依赖升级（`gateway.3`）时一样保持不变。

## 本地验证

gosec 使用 CI 锁定的 `v2.22.8` 和相同参数 `-exclude-dir=deploy/codex-compat/plugin ./...`：

- 修改前复现 CI 的 6 项，退出码 1。
- 修改后 `Issues: 0`，退出码 0（`Nosec` 从 31 增至 37）。

mihomo：

- 本环境访问 `codeload.github.com` 返回 403，因此从 Go 模块代理获取同一 commit 的源码（`v1.19.32`，`Origin.Hash` 为
  `88dcbf7f1614a67c3b36b848ee3592dfa92ada36`），其 `LICENSE` 与仓库副本一致。
- 新补丁在原始源码上 `git apply --check` 通过。
- 按 `build.sh` 的参数（Go 1.26.11、`GOTOOLCHAIN=local`、`CGO_ENABLED=0 GOAMD64=v1`、`-mod=readonly -tags with_gvisor -trimpath`）
  分别构建旧补丁和新补丁的二进制。`go version -m` 显示新二进制的 dhcp 为修复版本，`mihomo -v` 输出 `v1.19.32-gateway.4`。
- 唯一使用 dhcp 的 `component/dhcp` 包 `go vet` 通过（该包没有测试文件）。

Trivy 使用 CI 锁定的 `aquasec/trivy:0.64.1@sha256:a8ca2907…88e9e`。镜像经 Docker Registry API 按 digest 拉取，
4 个层的 digest 与 CI 日志一致。漏洞库取自当天的 `mirror.gcr.io/aquasec/trivy-db:2`，参数与 CI 相同：

```sh
trivy rootfs --exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL ROOTFS   # ROOTFS 只含 usr/local/bin/mihomo
```

| 二进制 | dhcp 版本 | Trivy |
| --- | --- | --- |
| 旧补丁（`gateway.3`） | `v0.0.0-20250109001534-8abf58130905` | 退出码 1，`GHSA-jp3c-6v3g-mpxv`（HIGH） |
| 新补丁（`gateway.4`） | `v0.0.0-20260719225207-c76316d4aa82` | 退出码 0 |

account-egress 镜像其余部分（Alpine 运行阶段、`account-egress`）在 CI 中扫描结果已为 0，本次未改动。

Gateway 检查（Go 1.26.11，未设置 `ANTHROPIC_BASE_URL` 与 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST`）：

```sh
gofmt -l .                                              # 无输出
go vet ./...
go vet -tags=integration ./internal/store ./internal/server
go build ./cmd/gateway
go test -count=1 ./...                                  # 含 Node 仪表盘测试
git diff --check
```

全部通过。验证环境没有 Docker 守护进程，完整镜像构建、容器回归和镜像扫描由 CI 完成。
