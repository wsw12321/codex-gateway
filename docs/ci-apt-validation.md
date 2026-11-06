# Debian APT 安全更新

2026-11-06，main 分支 `1f2187f` 的 [CI 任务](https://github.com/wsw12321/codex-gateway/actions/runs/42145418165) 在 Trivy 扫描阶段失败。网关镜像扫描通过；CPA 镜像（`codex-compat`）从锁定的 `debian:bookworm-20260824-slim` 继承了 `apt` 和 `libapt-pkg6.0` 2.6.1，命中 HIGH 漏洞 CVE-2026-109518，修复版本为 2.6.2。由于扫描在 CPA 镜像处退出，Antigravity bridge 和 account-egress 两个镜像这次未被扫描。这次失败与当次的代码改动无关，是漏洞库更新后对未变化的基础镜像新报出的问题。

修复时，Debian `bookworm-security` 已发布 amd64 的 `apt` / `libapt-pkg6.0` 2.6.2，`bookworm` 主仓库仍为 2.6.1。做法与 perl-base（DLA-4821-1）的修复相同：不更换锁定的基础镜像，在运行镜像的 `apt-get install` 中显式升级这两个包，并用 `dpkg --compare-versions` 要求版本不低于 `2.6.2`。

- 修改了三个 Debian 运行镜像：CPA（`deploy/codex-compat/Dockerfile`）、CPA 回滚镜像（`legacy.Dockerfile`）以及 Antigravity bridge。凭据迁移镜像继承 bridge 镜像，因此一并修复。
- `scripts/test-sidecar-image.sh`、`scripts/test-legacy-sidecar-image.sh` 和 `scripts/tests/antigravity-image-probe.sh` 增加了同样的版本检查，带有旧 APT 的镜像会被容器回归拒绝。
- 网关和 account-egress 使用 Alpine 运行镜像，没有改动。

## 本地验证

在本地 Docker 29.9.2 上分别构建修复前（`1f2187f`）和修复后的三个运行阶段，并用 CI 锁定的 `aquasec/trivy:0.64.1` 以相同参数扫描：

```sh
docker run --rm --volume /var/run/docker.sock:/var/run/docker.sock \
  "$TRIVY_IMAGE" image --exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL IMAGE
```

| 镜像 | apt / libapt-pkg6.0 | Trivy |
| --- | --- | --- |
| CPA 运行阶段（修复前） | 2.6.1 | 退出码 1，CVE-2026-109518 ×2 |
| CPA 运行阶段（修复后） | 2.6.2 | 退出码 0 |
| CPA 回滚镜像运行阶段 | 2.6.2 | 退出码 0 |
| Antigravity bridge 运行阶段 | 2.6.2 | 退出码 0 |

验证环境的出站代理只支持 HTTPS，所以本地构建时使用了 Dockerfile 的临时副本，副本只把 Debian 软件源切换为 HTTPS 并信任代理证书；仓库中的 Dockerfile 不包含这些改动。同一环境禁止下载 CLIProxyAPI 和 mihomo 的 GitHub 源码，因此完整镜像的构建、容器回归和 account-egress 的扫描交由 CI 完成。

新增的版本检查片段在修复前的镜像中输出 `apt lacks the CVE-2026-109518 security fix` 并退出，在修复后的镜像中通过。另外，CI 的 Shell 语法检查、`python3 -m unittest discover -s scripts/tests -p 'test_*.py'`（170 项，1 项跳过）和 `git diff --check` 均通过。
