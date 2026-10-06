# Debian 运行镜像 Perl 安全更新

2026-10-06 的 [CI 任务](https://github.com/wsw12321/codex-gateway/actions/runs/37462043209/job/112263747021)
通过镜像构建和运行回归后，在 Trivy 扫描步骤失败。本地使用相同锁定镜像和
扫描参数复现：Gateway 通过，CPA 和 Antigravity Bridge 的
`perl-base 5.36.0-7+deb12u3` 各命中 3 个 CRITICAL 和 4 个 HIGH 漏洞。

CRITICAL：CVE-2026-13221、CVE-2026-42496、CVE-2026-8376。
HIGH：CVE-2026-42497、CVE-2026-48962、CVE-2026-57432、CVE-2026-57433。

Debian 官方 [DLA-4821-1](https://www.debian.org/lts/security/2026/dla-4821)
将 Bookworm 修复版本列为 `5.36.0-7+deb12u4`。
锁定的 Debian 基础镜像包含旧版 `perl-base`；仅运行 `apt-get update` 和安装
原有依赖列表不会升级这个已安装包。

当前 CPA、回滚 CPA 和 Bridge 的运行阶段显式安装 `perl-base`，从 Debian
软件源更新该包，并通过 `dpkg --compare-versions` 要求版本至少为
`5.36.0-7+deb12u4`。对应容器运行回归也检查最低版本，拒绝旧的脆弱镜像。
凭据迁移镜像继承修复后的 Bridge。基础镜像 digest、上游应用锁定版本、
Expat 修复和 Trivy 的 HIGH/CRITICAL 阻断规则保持不变。

构建修复镜像后，执行现有运行回归和锁定的 Trivy 扫描：

```sh
./scripts/test-sidecar-image.sh "$compat_image"
./scripts/test-legacy-sidecar-image.sh "$legacy_image"
./scripts/test-antigravity-image.sh "$bridge_image"
CPA_MIGRATION_TEST_IMAGE="$migration_image" ./scripts/test-cpa-migration-image.sh
set -a
. deploy/images.lock.env
set +a
for image in "$gateway_image" "$compat_image" "$bridge_image"; do
    docker run --rm --volume /var/run/docker.sock:/var/run/docker.sock \
        "$TRIVY_IMAGE" image --exit-code 1 --ignore-unfixed \
        --severity HIGH,CRITICAL "$image"
done
python3 -m unittest discover -s scripts/tests -p 'test_*.py' -v
```

其中镜像变量应指向本次构建的测试镜像。容器运行回归使用合成凭据和隔离网络，
不挂载生产 OAuth 数据或站点 secret。

本地验证结果（2026-10-06）：旧 CPA 和 Bridge 镜像被新增版本检查拒绝；
修复后的 CPA、回滚 CPA、Bridge 及继承 Bridge 的凭据迁移镜像构建和运行
回归通过。Gateway、修复后的 CPA 和 Bridge 使用同一份 Trivy 数据库扫描，
均无可修复的 HIGH/CRITICAL 漏洞及 secret 告警。115 项脚本回归完成，
默认不启用的 Caddy Docker 测试跳过 1 项，其余通过。
