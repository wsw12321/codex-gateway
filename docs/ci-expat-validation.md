# Antigravity 镜像 Expat 安全更新

2026-10-01 的 [CI 任务](https://github.com/wsw12321/codex-gateway/actions/runs/36811061781/job/110206000124)
在 Trivy 扫描阶段失败。网关和 CPA 镜像扫描通过；Antigravity 镜像安装的
`libexpat1 2.5.0-1+deb12u3` 命中 6 个 HIGH 漏洞：CVE-2024-28757、
CVE-2025-59375、CVE-2026-25210、CVE-2026-45186、CVE-2026-66046、CVE-2026-93990。

Debian 当天发布的 [DLA-4807-1](https://www.debian.org/lts/security/2026/dla-4807)
将修复版本列为 `2.5.0-1+deb12u4`。修复时官方 Bookworm 安全仓库已有源码与部分
架构的二进制包，但 amd64 尚未发布，`apt-get update` 后候选版本仍为 `deb12u3`。

Bridge Dockerfile 在独立 Bookworm 阶段构建官方 Debian 源码包，三个下载输入的
SHA256 固定在 `deploy/antigravity-bridge/expat.sha256`。`dpkg-source` 应用官方
安全补丁，`dpkg-buildpackage` 生成 Debian 包，再执行上游解析器测试。
运行镜像安装生成的 `libexpat1` 包并检查版本，保留 Debian 的包数据库
和许可证文件。现有密钥环容器回归同时要求 Expat 版本至少为 `deb12u4`。

验证命令：

```sh
docker build -f deploy/antigravity-bridge/Dockerfile \
  -t codex-gateway-antigravity:ci-expat-validation .
./scripts/test-antigravity-image.sh codex-gateway-antigravity:ci-expat-validation
set -a
. deploy/images.lock.env
set +a
docker run --rm --volume /var/run/docker.sock:/var/run/docker.sock \
  "$TRIVY_IMAGE" image --exit-code 1 --ignore-unfixed \
  --severity HIGH,CRITICAL codex-gateway-antigravity:ci-expat-validation
```

本地验证通过：三个应用镜像的锁定 Trivy 扫描均无可修复的 HIGH/CRITICAL 漏洞；
修复镜像的密钥环、多账号隔离、错误密码拒绝以及继承该镜像的凭据迁移和回滚
回归通过。旧 `deb12u3` 镜像被新增版本检查拒绝。另通过 67 项部署/登录脚本
回归、`go test -count=1 ./...`、`go vet ./...`、格式与差异检查，以及使用
临时非生产配置的 Compose 部署策略与语法校验。

官方 amd64 修复包发布后，可将源码构建替换为安装该安全包；应保留版本检查并重新
执行上游依赖的运行验证与 Trivy 扫描。
