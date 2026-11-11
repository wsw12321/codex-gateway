# CPA 插件迁移验证记录

本文记录把 `codex-compat` 从整份源码补丁迁移到 CPA 官方插件的验证过程。迁移目标、
架构与取舍见 [兼容层升级规程](compatibility-upgrades.md) 的“插件模式”一节。
默认部署仍是补丁镜像；插件模式在完成真实账号验收前不作为默认。

## 阶段 0：可行性验证

日期 2026-11-11。对象是未修改的 CLIProxyAPI `v8.0.59`
（`c5251b57b3fa5dcbc689f514f4aa166daacb4481`），以及只加了拒绝选择器的同一版本。
方法：一次性探针插件（Go，`-buildmode=c-shared`）记录每个钩子收到的内容；假上游
返回固定的 Responses SSE 或指定错误；两个明文正向代理用于观察出口。探针和假上游
不进入仓库。全部在本机回环地址完成，未使用真实账号。

| 项 | 结论 | 观察 |
|---|---|---|
| 调度请求可见入站头 | 通过 | `scheduler.pick` 的 `Options.Headers` 含全部 `X-Codex-Gateway-*` 头 |
| 调度请求可见会话元数据 | 通过 | `caller_scope`、`canonical_session_id`、`selection_id`、`request_path`；有父会话时含 `parent_session_id` |
| 候选集已排除试过的账号 | 通过 | 首个账号 429 后，第二次调度的候选不含它；处于冷却的账号也不在候选中 |
| FrontendAuth 的 Principal 进入 `caller_scope` | 通过 | 不同 Principal 得到不同 `caller_scope`；管理密钥不能访问数据面，数据面密钥不能访问管理面 |
| 钩子之间可关联 | 通过 | before-auth 与 `request.complete` 的 `RequestID` 相同；`trace_id` 贯穿拦截、终态与 usage；usage 带 `selection_id`；after-auth 元数据带 `selected_auth_id` |
| usage 插件收到每次失败 | 通过 | `usage-statistics-enabled: false` 时仍收到；失败记录含状态码与上游正文（`usage_limit_reached` 可识别） |
| 终态回调 | 通过 | 成功、失败、客户端断开（`canceled`）各一次 |
| 所有生成端点都经过调度钩子 | 通过 | `/v1/responses`（流式与非流式）、`/v1/responses/compact`、`/v1/messages`、`/v1beta/models/*`、Claude 模型走 `/v1/responses` |
| 内部头不到达上游 | 通过 | 上游只收到执行器自己构造的头；任意自定义头与 Cookie 均未转发 |
| 凭证级代理 | 通过 | 配置项的 `proxy-url` 生效；原生 `PATCH /v0/management/auth-files/fields` 能把 `proxy_url` 写入 OAuth 凭证文件 |
| 管理路由 | 通过 | 插件路由位于 `/v0/management/<path>`；无密钥 401；插件内可经回环调用原生管理接口 |
| 宿主回调 | 通过 | `host.auth.list`、`host.auth.get` 不需要回调上下文，在 `scheduler.pick` 内也可调用；`host.auth.get` 返回完整凭证 JSON |
| 插件加载 | 通过 | 文件名 `<id>-v<版本>.so`，放在插件目录或其 `<os>/<arch>` 子目录 |
| 双 Go 运行时稳定性 | 通过 | 1500 个流式请求（150 并发，每个约 400 个分片）全部 200，各钩子各调用 1500 次，进程常驻内存约 108 MB |
| 原版在插件未处理时回退轮询 | 确认存在 | 插件返回未知账号或“未处理”时，原版改用内置轮询并成功调用上游 |
| 拒绝选择器 | 通过 | `routing.strategy: "plugin-only"` 下，未知账号、未处理、插件在配置中禁用、插件文件缺失均返回 503，上游零调用 |

与计划不一致的发现：

- **插件内 panic 不会被宿主熔断，而是整个 CPA 进程退出。** 宿主的 `recover` 接不住
  另一个 Go 运行时里的 panic。因此插件必须在自己的 ABI 入口和每个后台协程里
  `recover`，并把它转成错误信封。进程退出本身是拒绝服务而非放行，但会中断所有
  进行中的请求。
- **`Reject` 的渲染不能当契约用。** 调度插件用 `Reject` 拒绝时，CPA 返回 HTTP 500，
  错误码被拼进 message（`upstream_concurrency_exceeded: ...`）。用错误信封的
  `http_status` 可以控制状态码，但响应体的 `type`/`code` 仍由 CPA 决定。这印证了
  用 Gateway 请求账本传递拒绝原因的设计。
- **原生换号规则与补丁不完全相同。** 实测 429、500、403 会换下一个账号，400 直接
  返回。403 在补丁规则里属于不重放的“其他 4xx”。可用原生的
  `oauth-request-scoped-errors` 调整，见取舍清单第 1 项。
- **CPA 启动时会联网查询客户端版本号**（与是否启用插件无关）。生产环境由出口
  白名单拦截。

本阶段没有验证的内容：真实 OAuth 凭证经凭证级代理的推理、刷新与额度查询；v8
OAuth 会话级代理参数；Provider 缓存键是否全部按 `caller_scope` 隔离（需要逐个
阅读源码）。这些留到对应阶段，并在真实账号验收中覆盖。
