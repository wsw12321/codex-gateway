# 历史第三方 Gemini 插件验证记录

本文记录已移除的旧插件实现，不代表当前 Antigravity Bridge 的验收结果。当前部署和待完成的真实账号验收见 [Antigravity 接入说明](gemini-pro.md)。

更新时间：2026-09-16。默认兼容层构建已采用 Debian slim/glibc，修复
Alpine/musl 加载 Gemini 共享库时的崩溃；Compose 双镜像构建及真实加载检查通过。

2026-09-15 已通过的接入验证：

- 固定 Go 1.24.6 镜像中的 `go test -count=1 ./...`、
  `go test -race -count=1 ./...`、`go vet ./...` 和 Gateway 构建。
- 固定 PostgreSQL 17.6 临时数据库中的完整 store 集成测试，包含 Gemini
  200000/200001 边界、思考 Token 不重复扣款及并发结算幂等。
- 登录包装的 11 项隔离测试，以及固定 Squid 镜像的配置解析。
- CLIProxyAPI 原有兼容、安全回归和新增 Gemini JSON/SSE、函数签名往返、刷新、
  429、取消、尾部用量及共享库 ABI 测试；插件全量 race 与 vet。

Alpine 3.22.1 + musl + Go 1.26.0 的实际共享库加载在 `dlopen` 中触发 SIGSEGV。
宿主增加 `-linkmode=external` 仍复现。与此一致的上游问题见
[Go #13492](https://github.com/golang/go/issues/13492) 和
[Go #54805](https://github.com/golang/go/issues/54805)。构建内真实加载检查会拒绝
生成该镜像，不能跳过检查来发布。

默认兼容层使用 `debian:bookworm-20260824-slim`，固定摘要为
`sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171`。
主程序、插件和测试二进制均用 gcc/glibc 构建；构建内 ABI 检查与最终镜像共用
`runtime-base`，包含 CA 证书和 netcat，并以 UID 10001 运行。

Dockerfile、`deploy/images.sources`、`deploy/images.lock.env`、Compose 和校验脚本
已同步使用独立的 `CLIPROXY_RUNTIME_IMAGE`；镜像标签增加 `-glibc` 后缀。
Gateway 的 `RUNTIME_IMAGE` 继续锁定原 Alpine 镜像。

2026-09-16 修复验证：

- 使用原 Dockerfile 复现 `cliproxy_dlopen` 中的 SIGSEGV 和退出码 2。
- `./scripts/compose.sh --progress plain build gateway codex-compat` 成功。
- 使用 `--no-cache-filter verify --target verify` 强制重跑真实共享库检查，
  `TestGeminiCLISharedPluginLoads`、客户端取消和无效 SSE 清理子测试全部通过。
- `./scripts/test-sidecar-image.sh` 通过：无网络、只读文件系统、UID 10001 下
  验证插件加载、OAuth 清单、危险文件权限与符号链接拒绝，以及真实启动和 TCP 探针。
- 登录包装的 11 项隔离回归全部通过，修改后的 shell 脚本语法检查通过。
- 独立临时项目使用测试域名和生成的测试 Secrets，完整 Compose/Caddy 校验通过。
  当前检出目录的 `.env` 仍含占位 `GATEWAY_DOMAIN`，直接部署校验会在该项拒绝；
  部署前需填写真实域名。
- 运行镜像配置回归通过：继承的环境变量不能覆盖锁定值；缺失独立锁、改回 Alpine
  或误用 Gateway 的运行镜像参数均被拒绝。

站点 `.env`、Secrets 与已有 OAuth 凭证未修改，也未启动或重启站点服务。

真实 Pro 账号授权及经公网 Gateway 的 JSON/SSE 用量和账务核对尚未执行。
