# Alpine OpenSSL 安全更新

2026-11-26，main 分支 `524e177` 的 [CI 任务](https://github.com/wsw12321/ai-hub/actions/runs/45740220898/job/139643093883)
在 `containers` 的 Trivy 扫描阶段失败。此前所有步骤都已通过，包括三个应用镜像的构建、CPA 镜像构建内的全部测试，以及
relay、Shadowsocks、account-egress 和 CPA 的容器回归。
失败的是网关镜像：它从锁定的 `alpine:3.22.6` 继承了 `libcrypto3 3.5.8-r0` 和 `libssl3 3.5.8-r0`，命中 HIGH 漏洞
CVE-2026-124709（QUIC 流处理内存泄漏导致拒绝服务），修复版本为 `3.5.9-r2`。扫描在网关镜像处就退出了，CPA 和
account-egress 两个镜像这次未被扫描。这一项与 `524e177` 的代码改动无关，是漏洞库更新后新报出的。

## 修改

- Alpine 运行镜像：网关（`Dockerfile`）、account-egress 和 `ss-egress`（`deploy/shadowsocks/Dockerfile`）沿用 zlib 和 musl
  修复的做法，不更换锁定的基础镜像，在运行阶段显式升级 `libcrypto3` 和 `libssl3`，用 `apk version -t` 要求不低于
  `3.5.9-r2`，否则构建失败。修复时 Alpine v3.22 的 main 仓库已发布 `3.5.9-r2`。
- `scripts/tests/relay_image.py` 的 `ALPINE_SECURITY_FIXES` 增加 `libcrypto3` 和 `libssl3`。`account_egress_image.py` 和
  `shadowsocks_image.py` 通过 `require_fixed_alpine_packages` 在无网络、只读、无 capabilities 的容器中检查这两个包的最低版本。
- 与 musl 修复时一样，镜像标签保持不变。CPA 镜像使用 Debian bookworm 的 OpenSSL 3.0 分支，不受影响，未改动。

## 本地验证

本地启动了 Docker 守护进程，用 CI 锁定的 `aquasec/trivy:0.64.1@sha256:a8ca2907…88e9e` 和 CI 相同的参数扫描镜像：

```sh
trivy image --exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL IMAGE
```

漏洞库取自 `mirror.gcr.io/aquasec/trivy-db:2`，`UpdatedAt` 为 2026-11-26T01:07:25Z，早于 CI 任务的开始时间（02:06:47Z）。

待扫描的镜像这样构建：基础镜像按 `deploy/images.lock.env` 的 digest 拉取。从各 Dockerfile 的运行阶段原样提取 `RUN` 命令，
修改前的版本取自 `git show HEAD:<Dockerfile>`。命令在锁定的基础镜像中执行，再放入镜像会携带的二进制。

二进制均用 Go 1.26.11 构建：

- `gateway`、`account-egress` 和 `ss-egress-helper` 按 Dockerfile 的参数构建。
- mihomo：本环境访问 `codeload.github.com` 返回 403，从 Go 模块代理获取同一 commit 的源码（`v1.19.32`），其 `LICENSE`
  与仓库副本一致。按 `build.sh` 的参数应用依赖补丁并构建。
- `cli-proxy-api` 和插件：从 Go 模块代理获取 CLIProxyAPI `v8.1.1`（`Origin.Hash` 为 `6eb8554f…c9e6`，与
  `plugin.lock.json` 一致）。按 `plugin.Dockerfile` 应用残留补丁，校验模型目录 SHA-256，执行安全依赖下限和 `go mod verify` 后构建。

| 镜像 | libcrypto3 / libssl3 | Trivy |
| --- | --- | --- |
| 网关（修复前） | 3.5.8-r0 | 退出码 1，CVE-2026-124709（HIGH，两项），与 CI 日志一致 |
| 网关 | 3.5.9-r2 | 退出码 0（alpine 0，gobinary 0） |
| account-egress（修复前） | 3.5.8-r0 | 退出码 1，CVE-2026-124709（HIGH，两项） |
| account-egress | 3.5.9-r2 | 退出码 0（alpine 0，`account-egress` 0，`mihomo` 0） |
| ss-egress | 3.5.9-r2 | 退出码 0（alpine 0，`mihomo` 0，`ss-egress-helper` 0） |
| CPA（未改动） | `libssl3 3.0.22-1~deb12u2` | 退出码 0（debian 0，`cli-proxy-api` 0） |

三个 Alpine 镜像中 zlib、musl 和 musl-utils 仍满足原有的最低版本。修复前后的镜像中 `/etc/ssl/certs/ca-certificates.crt`
与锁定基础镜像逐字节一致，构建时临时注入的文件均已删除。

`require_fixed_alpine_packages` 拒绝修复前的网关和 account-egress 镜像，报错为
`lacks the libcrypto3 3.5.9-r2 fix for CVE-2026-124709`，并接受三个修复后的镜像。

验证环境的出站代理会替换 TLS 证书，构建时只做了以下临时改动：

- Alpine 构建临时把代理 CA 追加到系统证书束。
- Debian 构建临时把 APT 软件源改为 HTTPS，并为 APT 指定代理和代理 CA。

这些改动都在同一 `RUN` 中还原或删除，仓库中的 Dockerfile 不包含它们。

`python3 -m unittest discover -s scripts/tests -p 'test_*.py'` 中 `test_lock_images` 的 28 个子测试在本环境失败，修改前后
结果相同。原因是本环境的 `rm` 是一个会拒绝执行的会话包装，测试临时文件无法删除。CI 的同一步骤在 `524e177` 上已通过。
其余测试通过。完整镜像构建、容器回归和镜像扫描由 CI 完成。
