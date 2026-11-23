# CPA v8.1.1 升级验证记录

2026-11-23 将 CLIProxyAPI 从 `v8.0.77` 升级到当时最新的稳定版 `v8.1.1`。
锁定的上游提交为 `6eb8554f0ede597b2aa3e508ffb3f111d3c2b9e6`。参见
[上游发布说明](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v8.1.1)和
[两版差异](https://github.com/router-for-me/CLIProxyAPI/compare/v8.0.77...v8.1.1)。

## 构建与兼容性

- 更新 Compose、插件模块和镜像的上游版本锁定。模型目录摘要为
  `a3133d640ca23e6ed984e00670568c0478d4a06a7dce8128f208be56f973fcc6`，构建时校验。
- 现有残余补丁无 fuzz 应用，仍只改动四个上游文件。插件源码可直接用新版本的接口编译。
  上游增加了可选的拦截器终止范围和响应类型。Gateway 插件沿用整次请求的终止行为。
- 插件间接依赖 `moego` 随上游升至 `v0.1.0-alpha.5`。构建继续使用 Go 1.26.11，
  将 `klauspost/compress` 提升到安全下限 `v1.18.7`。上游的 `golang.org/x/crypto`
  与 `golang.org/x/net` 已达到锁定的下限。
- Codex 和 Claude 的模型 ID 集合一致。Antigravity 新增 `gemini-4-neon-xhigh`，已加入
  CPA 排除规则并同步测试，实际注册目录仍为五个已定价模型。新增的 Copilot 及 Vertex
  模型属于 Gateway 尚未接入的 provider。
- 升级可沿用现有数据库与 OAuth 凭证卷。

## 验证结果

- 校验上游标签、提交、模型目录与残余补丁摘要。上游插件宿主、插件 API、ABI、账号管理器及
  三项 `TestGatewayPluginOnly*` 测试通过。
- 在 WSL 用 Go 1.26.11 和 CGO 构建 CPA 与 Gateway 插件。插件格式检查、`go vet`（包括
  `e2e` 标签）和竞态测试通过。使用同一对二进制运行 17 组端到端测试，全部通过且无跳过，
  包括调度故障拒绝请求、分账号出口、Antigravity 模型目录、额度推送及会话隔离。
- 部署脚本测试通过。使用 Linux Compose 和 Node.js 运行 104 项，其中 1 项按测试设计跳过。
- Gateway 的 `go test ./...` 有一项既有出口探针测试偶发失败。
  `TestProbeCompletesTLSThroughAppliedNodeOnly` 在升级前的 `HEAD` 和升级后的工作区均可复现。

当前 Docker 引擎未启动。运行镜像构建、容器探针、`validate-compose.sh` 的镜像内检查以及
真实 OAuth 账号验收待在部署环境执行。
