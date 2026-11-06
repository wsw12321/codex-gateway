# Alpine zlib 安全更新

2026-11-06，main 分支 `315f9e5` 的 [CI 任务](https://github.com/wsw12321/codex-gateway/actions/runs/42176544025) 在 Trivy 扫描阶段失败。失败的是网关镜像：它从锁定的 `alpine:3.22.6` 继承了 `zlib 1.3.2-r0`，命中 HIGH 漏洞 CVE-2026-85091（`gz_vacate()` 堆溢出），修复版本为 `1.3.2-r1`。扫描在网关镜像处就退出了，因此 CPA、Antigravity bridge 和 account-egress 三个镜像这次未被扫描。

这次失败与当次的代码改动无关。基础镜像没有变化，是漏洞库更新后新报出的问题；上一轮 CI（`8cc5a72`）的扫描是通过的。当次的其他任务都已通过，其中包括新增 SS2022 节点的账号出口镜像测试。由于扫描失败，后续的镜像导出和发布被跳过，这次提交没有产出 CI 镜像。

修复时，Alpine v3.22 的 main 仓库已发布 `zlib 1.3.2-r1`。做法与 Debian 的 perl-base、APT 修复相同：不更换锁定的基础镜像，在运行阶段显式升级这个包，并用 `apk version -t` 要求版本不低于 `1.3.2-r1`。仓库源或版本不满足时，构建直接失败。

- 修改了三个 Alpine 运行镜像：网关（`Dockerfile`）、account-egress 和全局 SS 模式的 `ss-egress`（`deploy/shadowsocks/Dockerfile`）。CI 的 Trivy 不扫描 `ss-egress`，但它使用同一个基础镜像。
- `scripts/tests/relay_image.py` 新增 `require_fixed_zlib`。`account_egress_image.py` 和 `shadowsocks_image.py` 在启动沙箱前用它检查刚构建的镜像：容器无网络、只读、丢弃全部 capabilities，以镜像默认的非 root 用户运行。带旧 zlib 的镜像会被拒绝。
- CPA 和 Antigravity bridge 使用 Debian 运行镜像，未受影响，没有改动。用锁定的 Trivy 扫描 `debian:bookworm-20260824-slim` 基础镜像，没有 zlib 相关的发现；其余发现（APT、perl-base、libpcre2）已由这两个 Dockerfile 显式升级，上一轮 CI 的扫描也已通过。

## 本地验证

验证环境没有 Docker 守护进程，验证步骤如下：

1. 用 `crane v0.20.6` 按 digest 导出锁定的 `alpine:3.22.6` 根文件系统和 CI 锁定的 `aquasec/trivy:0.64.1`。
2. 对三个 Dockerfile，从运行阶段原样提取 `RUN` 命令，在各自全新的根文件系统中通过 `chroot` 以 root 执行。
3. 用 Trivy 以 CI 相同的参数扫描执行后的根文件系统：

```sh
trivy rootfs --exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL --scanners vuln ROOTFS
```

| 运行阶段 | zlib | `require_fixed_zlib` 检查 | Trivy |
| --- | --- | --- | --- |
| 锁定基础镜像（修复前） | 1.3.2-r0 | 拒绝 | 退出码 1，CVE-2026-85091 |
| 网关 | 1.3.2-r1 | 通过 | 退出码 0 |
| account-egress | 1.3.2-r1 | 通过 | 退出码 0 |
| ss-egress | 1.3.2-r1 | 通过 | 退出码 0 |

验证环境的出站代理会替换 TLS 证书，所以本地 `chroot` 中执行 `apk` 时设置了 `SSL_CERT_FILE`，指向临时追加了代理证书的 CA 文件。扫描前已恢复原始 CA 文件；仓库中的 Dockerfile 不包含这些改动。同一环境无法下载 mihomo 的 GitHub 源码，因此完整镜像的构建、容器回归和镜像扫描交由 CI 完成。另外，`python3 -m unittest discover -s scripts/tests -p 'test_*.py'`（170 项，1 项跳过）和 `git diff --check` 均通过。
