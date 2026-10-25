# CI Go 与 Bridge 系统依赖安全更新

2026-10-25 的 [CI #125](https://github.com/wsw12321/codex-gateway/actions/runs/40149576323)
在 `go-security` 的 govulncheck 与 `containers` 的 Trivy 扫描步骤失败。
本地使用相同的固定 Trivy 镜像扫描旧 Go 1.26.9 网关镜像，复现
`CVE-2026-111943`（`crypto/x509`，HIGH）；[Go 1.26.10](https://go.dev/doc/devel/release#go1.26.10)
包含对应修复。

工具链、CI、五个 Dockerfile、镜像来源与 digest 锁、CPA 源码锁及部署校验
统一升级至 Go 1.26.10。CI 同时检查 CPA 源码锁中的 Go 版本，防止声明与实际
构建工具链脱节。

重新扫描 Bridge 时，还发现旧运行层中的以下可修复漏洞：

| 依赖 | 漏洞 | 最低安全版本 |
| --- | --- | --- |
| libexpat1 | CVE-2025-59375、[CVE-2026-77214](https://security-tracker.debian.org/tracker/CVE-2026-77214) | 2.5.0-1+deb12u5 |
| libbrotli1 | [CVE-2026-111497](https://security-tracker.debian.org/tracker/CVE-2026-111497) | 1.0.9-2+deb12u1 |
| jq、libjq1 | [CVE-2026-32316](https://security-tracker.debian.org/tracker/CVE-2026-32316) | 1.6-2.1+deb12u3 |

Debian 安全仓库已提供上述 amd64 包。Bridge 直接安装官方包，移除临时的旧
Expat 源码构建阶段及其校验清单；构建和容器运行回归均强制检查最低安全版本。
保留原有 Perl 安全版本检查、上游 AGY 下载校验及所有安全扫描门槛。

本地验证通过：

- Go 1.26.10：全量 `go test -count=1 ./...`、`go test -race -count=1 ./...`、
  `go vet ./...` 和格式检查。
- CI 固定版本 `govulncheck@v1.1.4` 与 `gosec@v2.22.8`；无可达漏洞或 gosec 问题。
- `scripts/lock-images.sh --check`，以及 CI 工具链来源一致性检查。
- 167 项部署与登录脚本测试：166 项通过、1 项按环境条件跳过。
- 独立临时副本、合成凭据下的 Compose 部署策略和语法校验。
- Gateway、CPA、Bridge 镜像构建；CPA Dockerfile 内置功能与竞态回归。
- 三个应用镜像均通过固定 Trivy 的 `--exit-code 1 --ignore-unfixed
  --severity HIGH,CRITICAL` 扫描，无可修复的 HIGH/CRITICAL 漏洞。
- CPA 运行回归、Bridge 多账号密钥环隔离与错误密码拒绝、继承新 Bridge 的
  凭据迁移镜像构建及加密密钥环往返、刷新锁回归。

本次未修改业务逻辑或数据库迁移。PostgreSQL 集成、网络路由与完整发布流程
由推送后的 CI 再次执行。
