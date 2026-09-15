# Gemini 接入验证记录

验证时间：2026-09-15。网关、价格目录、登录包装及兼容补丁已实现；运行镜像选择
尚待确认，因此当前默认 Alpine 构建不能用于部署。

已通过：

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

独立候选使用 `debian:bookworm-20260824-slim`，固定摘要为
`sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171`。
该候选已通过完整镜像构建、Compose/Caddy 校验及无网络、只读文件系统、UID 10001
下的插件加载、OAuth 清单、文件权限拒绝和真实启动检查。真实共享库也通过了
OAuth 文件/登录参数识别、客户端取消及无效 SSE 后的上游连接清理测试。

候选 Dockerfile 暂存于 `/tmp/gemini-glibc-Dockerfile`，完整隔离配置在
`/tmp/gemini-compose-validation`，候选镜像为 `gemini-glibc-candidate:20260915`。
采用候选需要同步兼容层 Dockerfile、独立运行镜像锁、Compose 参数和校验脚本；
Gateway 可继续使用原 Alpine 镜像。站点 `.env`、Secrets 与已有 OAuth 凭证未修改。

真实 Pro 账号授权及经公网 Gateway 的 JSON/SSE 用量和账务核对尚未执行。
