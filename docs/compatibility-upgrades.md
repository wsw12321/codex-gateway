# CLIProxyAPI 兼容层升级规程

生产固定在 CLIProxyAPI `v7.2.127`、commit
`ecc9aa72b32f34b680d03b0724b531a21ae74472`。版本号和 commit 必须作为一组
更新，Docker 构建会验证 tag 指向该 commit。

升级候选必须先在隔离环境覆盖以下契约：

- 认证后的 `GET /v1/responses` 返回一次
  `426 responses_websocket_unsupported` 后客户端立即改用 HTTPS/SSE，探测不进入
  usage、配额、计费、并发租约或 sidecar；
- 非流式和 SSE `POST /v1/responses`；
- `POST /v1/responses/compact`；
- 模型列表；
- 安全清理后的 401、429、5xx；
- usage 分散在多个 SSE chunk 时的 input、cached input、output、reasoning；
- 客户端断开取消、首 token 超时和总超时；
- OAuth refresh 成功、refresh 失效和 reauthentication required；
- sidecar 不记录请求/响应正文或 token，管理 API 无法从兼容层网络远程访问。

切换生产前运行加密数据库备份，但不要复制 OAuth volume。持有设备登录锁，
停止旧 sidecar，确认其容器状态不是 running，才允许候选版本挂载现有 token。
完成模型列表和最小 Responses 人工 Pro 冒烟后才能恢复 Gateway 流量。

失败回滚时先停止候选实例，再启动旧实例。任何时刻都不允许两个 sidecar
共享同一个 refresh token。若 token 已因候选版本失效，保持服务关闭并重新
执行 `scripts/codex-device-login.sh`。
