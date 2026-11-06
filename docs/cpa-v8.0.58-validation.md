# CPA v8.0.58 升级验证

本次升级把兼容层固定到 CLIProxyAPI `v8.0.58`，上游发布于 2026-11-06。

- 上游仓库：[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
- 固定 tag：`v8.0.58`
- 固定 commit：`e5e6ad1e1b91fc20ab9964e6fb22c0c269b9d882`
- Gateway 补丁：`deploy/codex-compat/cliproxy-v8.0.58-gateway.patch`
- 补丁 SHA-256：`2f30514cb673c3361370120130a1e18ad295a1c6db94f932b0dc88abbbf51e3d`
- 模型目录：`internal/registry/models/models.json`
- 模型目录 SHA-256：`99037d47cc67751a02847f8a5ed3463c694de88319ee458e527844821b9a9bd0`

补丁从 v8.0.48 的 Gateway 变更重新进行三方合并，保留 Gateway 的账号、路由、
权限、审计和 egress 隔离行为，同时吸收 v8.0.58 的上游变更。干净的
v8.0.58 checkout 上执行 `git apply --check --ignore-space-change` 和
`git diff --check` 均通过。

v8.0.58 的上游模型目录在既有目录基础上新增 `gpt-6-chat-thinking`。Gateway
仍要求模型先进入本地价格目录和用户权限策略后才会公开；升级本身不会绕过价格或授权检查。
本次上游发布没有新增数据库迁移，已有的 v8.0.48 目录迁移仍作为历史升级前置保留。

完整 Go 测试、镜像构建和真实账号验收由 GitHub Actions 与目标 VPS 的维护窗口完成；
CI 状态以对应 main 提交的 workflow 为准。
