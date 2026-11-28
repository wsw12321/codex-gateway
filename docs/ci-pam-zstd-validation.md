# Debian PAM / zstd 安全更新与 gosec G104

2026-11-27，PR #19（`c484421`）和 PR #20 的 CI 失败。

- **containers（两个 PR 都有）**：Trivy 扫描 CPA 镜像（`codex-compat`）时，发现从锁定的 `debian:bookworm-20260824-slim` 继承的 `libpam0g`、`libpam-modules`、`libpam-modules-bin`、`libpam-runtime` 1.5.2-6+deb12u2 命中 HIGH 漏洞 CVE-2025-6020，修复版本为 1.5.2-6+deb12u3。这与 PR 的代码改动无关，是漏洞库更新后对未变化的基础镜像新报出的问题。
- **go-security（仅 PR #19）**：gosec G104 报告 `internal/store/egress_domains.go` 中扫描出错时调用的 `rows.Close()` 未处理返回值。

2026-11-28 在本地用当天的漏洞库复现时，Trivy 还报出 `libzstd1` 1.5.4+dfsg2-5 命中 HIGH 漏洞 CVE-2026-124851，修复版本为 1.5.4+dfsg2-5+deb12u1。

## 修复

- 两个修复版本都只在 `bookworm-security` 中发布，`bookworm` 主仓库仍是旧版本。做法与 perl-base、APT 的修复相同：不更换锁定的基础镜像，在 `deploy/codex-compat/plugin.Dockerfile` 运行阶段的 `apt-get install` 中显式升级这 5 个包，并用 `dpkg --compare-versions` 检查版本不低于修复版本。如果软件源或版本不满足要求，构建会直接失败。
- gosec：改为 `_ = rows.Close()`。返回给调用方的仍是扫描错误。
- 网关和 account-egress 使用 Alpine 运行镜像，这次没有改动。

## 本地验证

在 macOS（arm64）上用 Colima（VZ + Rosetta）运行 Docker 29.5.2，所有镜像按 CI 的 `linux/amd64` 构建。Trivy 使用 `deploy/images.lock.env` 中锁定摘要的 `aquasec/trivy:0.64.1`，参数与 CI 相同（`--exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL`）。

| 检查 | 结果 |
| --- | --- |
| CPA 运行阶段（修复前，`--target runtime-base`） | libpam 1.5.2-6+deb12u2，Trivy 退出码 1，5 项 HIGH（CVE-2025-6020 ×4、CVE-2026-124851 ×1） |
| CPA 运行阶段（修复后） | libpam 1.5.2-6+deb12u3、libzstd1 1.5.4+dfsg2-5+deb12u1，Trivy 退出码 0 |
| `./scripts/compose.sh build gateway codex-compat account-egress`（按 CI 方式准备 `.env`） | 成功，包括 CPA 自身测试和插件 e2e 套件 |
| `./scripts/test-cpa-plugin-image.sh` | 通过 |
| Trivy：gateway / codex-compat / account-egress 完整镜像 | 退出码均为 0 |
| gosec v2.22.8（Go 1.26.11，`-exclude-dir=deploy/codex-compat/plugin ./...`） | Issues: 0 |
| `gofmt -l .`、`go vet ./internal/store`、`go test ./internal/store`、`python3 -m unittest scripts.tests.test_cpa_plugin_deploy` | 通过 |

注意：使用 Go 1.27 编译 gosec v2.22.8 时会报 `internal error: package "context" without types`，无法得到扫描结果。在本地运行时需要设置 `GOTOOLCHAIN=go1.26.11`。
