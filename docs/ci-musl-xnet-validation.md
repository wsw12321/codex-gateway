# Alpine musl 与 golang.org/x/net 安全更新

2026-11-07，main 分支 `f92746d`（CPA v8.0.59 升级）的 [CI 任务](https://github.com/wsw12321/codex-gateway/actions/runs/42396736295)
在 Trivy 扫描阶段失败。此前所有任务都已通过，包括 CPA 镜像构建内的全部测试、sidecar 运行检查和回滚镜像。
失败的是网关镜像：它从锁定的 `alpine:3.22.6` 继承了 `musl 1.2.5-r12` 和 `musl-utils 1.2.5-r12`，命中 HIGH 漏洞
CVE-2026-107891（`regcomp` 整数溢出），修复版本为 `1.2.5-r13`。扫描在网关镜像处就退出了，其余三个镜像这次未被扫描。

为避免逐个镜像地等 CI 报错，本地用 CI 锁定的 Trivy 和当天的漏洞库扫描了全部四个镜像对应的根文件系统，又发现一个问题：
CPA 和 mihomo 依赖的 `golang.org/x/net v0.60.0` 命中 HIGH 漏洞 CVE-2026-78661（`x/net/html` 拒绝服务），
修复版本为 `v0.62.0`。若不处理，CPA 镜像和 account-egress 镜像会在下一轮扫描中失败。两个问题都与 v8.0.59 的代码改动无关，
是漏洞库更新后新报出的。

## 修改

- Alpine 运行镜像：网关（`Dockerfile`）、account-egress 和 `ss-egress`（`deploy/shadowsocks/Dockerfile`）沿用 zlib 修复的做法，
  不更换锁定的基础镜像，在运行阶段显式升级 `musl` 和 `musl-utils`，用 `apk version -t` 要求不低于 `1.2.5-r13`，否则构建失败。
- `scripts/tests/relay_image.py` 的 `require_fixed_zlib` 改为 `require_fixed_alpine_packages`，在同样的无网络、只读、无 capabilities
  容器中逐项检查 zlib、musl 和 musl-utils 的最低版本。`account_egress_image.py` 和 `shadowsocks_image.py` 改用新函数。
- CPA 补丁把 `golang.org/x/net` 升到 `v0.62.0`。Go 模块版本选择随之把 `x/crypto` 升到 `v0.58.0`、`x/sync` 到 `v0.24.0`、
  `x/sys` 到 `v0.49.0`、`x/text` 到 `v0.43.0`。补丁 SHA-256 及兼容层镜像标签同步更新。
- mihomo 依赖补丁同样升级 `golang.org/x/net` 到 `v0.62.0`，并随之升级 `x/crypto`、`x/sync`、`x/sys`、`x/term`、`x/text` 和
  `x/tools`；补丁只有这些 `golang.org/x` 依赖行发生变化。`source.lock.env` 的补丁 SHA-256 已更新，`build.sh` 写入的版本标识改为
  `v1.19.32-gateway.3`。`THIRD_PARTY_NOTICES` 同步更新依赖版本。

## 本地验证

验证环境没有 Docker 守护进程。用 Docker Registry API 按 digest 导出锁定的 `alpine:3.22.6`、`debian:bookworm-20260824-slim`
和 CI 锁定的 `aquasec/trivy:0.64.1`。对每个运行阶段，从 Dockerfile 原样提取 `RUN` 命令，在全新根文件系统中通过 `chroot`
执行，再放入镜像会携带的二进制，用 CI 相同的参数扫描：

```sh
trivy rootfs --exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL ROOTFS
```

| 镜像根文件系统 | 内容 | Trivy |
| --- | --- | --- |
| 网关（修复前） | 锁定 Alpine + 原 zlib 升级 + gateway | 退出码 1，CVE-2026-107891 |
| 网关 | 修复后的运行阶段 + gateway | 退出码 0 |
| account-egress | 修复后的运行阶段 + account-egress + mihomo | 修复 x/net 前退出码 1（CVE-2026-78661），修复后 0 |
| ss-egress | 修复后的运行阶段 | 退出码 0 |
| CPA | Debian 运行阶段 + cli-proxy-api | 修复 x/net 前退出码 1（CVE-2026-78661），修复后 0 |
| Antigravity bridge | Debian 运行阶段 + antigravity-bridge + 锁定的 agy CLI | 退出码 0 |
| 锁定 Debian 基础镜像（对照） | 未执行运行阶段 | 退出码 1，13 项（APT、perl-base、libpcre2 等） |

对照组确认 Trivy 能识别 Debian 系统包；这些发现已由 CPA 和 bridge 的 Dockerfile 显式升级，运行阶段内的 `dpkg --compare-versions`
检查均通过。三个 Alpine 运行阶段执行后 musl 和 musl-utils 均为 `1.2.5-r13`，zlib 为 `1.4-r0`，满足所有最低版本要求。

`cli-proxy-api` 由打好 v8.0.59 补丁的源码构建；mihomo 源码 `codeload.github.com` 在本环境不可达，改从 Go 模块代理获取同一
commit（`v1.19.32`）的源码，按 `build.sh` 的参数应用新依赖补丁并构建。新补丁在原始源码上 `git apply --check` 通过。
CPA 补丁更新后重新执行了 Dockerfile 构建阶段的全部测试步骤。

验证环境的出站代理会替换 TLS 证书，所以 `chroot` 中执行 `apk` 和 `apt-get` 时临时提供代理 CA，并把 Debian 软件源改为 HTTPS；
扫描前已移除这些临时文件。仓库中的 Dockerfile 不包含这些改动。完整镜像构建、容器回归和镜像扫描由 CI 完成。
