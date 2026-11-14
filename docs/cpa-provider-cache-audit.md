# CPA Provider 缓存键核对

本文记录对未修改的 CLIProxyAPI `v8.0.59`
（`c5251b57b3fa5dcbc689f514f4aa166daacb4481`）中各 Provider 进程内状态的逐项核对，
对应 [CPA 插件模式路线图](cpa-plugin-roadmap.md)第 7 步的“Provider 缓存键”一项。
日期 2026-11-13。

补丁镜像把 Provider 的缓存键统一改成“调用方 + 用户 + 账号”的哈希。插件模式不改 CPA
源码，所以要确认 CPA 原生的键是否足以把两个 Gateway 调用方隔开。核对方法是阅读源码，
并对结论中依赖的性质做端到端实测。

## 结论

- **CPA 自己推导的会话身份和按会话保存的重放状态都带调用方作用域。** 两个 API Key 即使
  经同一个上游账号发送完全相同的请求，也不会共用这些状态。端到端测试
  `TestProviderSessionsAreCallerScoped` 固定了这一点。
- **键里没有账号，这是上游的设计。** CPA 让会话状态跟随会话而不是凭证，以便换号后对话
  能够延续。所以同一个调用方的同一个对话被改绑到另一个账号时，它自己的会话标识和重放内容
  会随之出现在新账号的请求里。不同调用方之间不受影响。
- **有两处可达的缓存的键不含调用方作用域**（下文缺口 1、2）。利用它们都需要先知道另一个
  调用方的上游消息 ID，得到的也不是对话内容。CPA 没有关闭这两处缓存的配置项。另有两处
  同样的缺口在本部署中不可达（缺口 3、4）。按路线图的约定，这些都不进残余补丁，准备提交
  上游。
- **客户端自带的会话标识原样发给上游。** 这不是 CPA 的缓存，但决定了上游自己的缓存与
  会话归属，与补丁镜像不同，单列在[发给上游的标识](#发给上游的标识)。

本步骤据此做了三件事：

1. 插件拒绝为没有调用方作用域头的请求选号（原来只要求用户和请求令牌）。没有这个头的
   请求共用一个固定的 principal，CPA 的全部按调用方隔离的状态对它们就不再隔离。
2. Antigravity 账号只注册五个已审模型（见[兼容层升级规程](compatibility-upgrades.md#antigravity-模型限制)）。
   缺口 4 只能经由 Antigravity 账号提供的 `claude-*` 模型到达；Gateway 本来就不为这些模型
   定价，现在 CPA 一侧也不再注册它们。
3. 增加端到端测试，CPA 升级后如果推导方式变了会在构建时失败。

上游的问题报告尚未提交，草稿见[提交上游](#提交上游)。

## 调用方作用域的来源

插件的前端认证把 `X-Codex-Gateway-Affinity` 作为 principal 交给 CPA。这个值是 Gateway 对
API Key ID 的 HMAC，每个 API Key 一个；模型鉴定的探针使用一次性的随机值。CPA 把 principal
存为请求的 `userApiKey`，并由它得到调用方作用域：

```
caller_scope = hex(sha256("cli-proxy-api:caller-scope:v1\0" + principal))
```

执行器从请求元数据的 `caller_scope` 或 `helps.APIKeyFromContext`（即 principal）取得它。
下表中“调用方”一栏为“是”，指键中含有这两者之一。

## 逐项结果

“可达”指 Gateway 能够发出的请求会走到这段代码。Gateway 的生成请求只有五种组合：Codex 模型的
`/v1/responses` 与 `/v1/responses/compact`、Claude 模型的 `/v1/messages`、Claude 模型的
`/v1/responses`、Antigravity 模型的 `/v1/responses`、Antigravity 模型的 `/v1beta/models/*`。

### 保存对话内容或其派生物的状态

| # | 状态 | 位置 | 键 | 调用方 | 账号 | 可达 |
|---|---|---|---|---|---|---|
| 1 | 推导的会话身份 | `sdk/cliproxy/session/identity.go` `DeriveID` | 调用方作用域、请求格式、开头的指令与第一条用户输入 | 是 | 否 | 是 |
| 2 | Antigravity 推理重放（思考签名、函数调用片段） | `internal/cache/antigravity_reasoning_replay_cache.go`，键由 `antigravityReasoningReplayScopeFromRequest` 生成 | 模型、`caller:` + 调用方作用域的哈希、会话键 | 是 | 否 | 是 |
| 3 | Gemini Responses 文本签名 | `internal/translator/gemini/openai/responses/trailing_signature.go` | 模型、`gemini-responses-text:` + 响应消息 ID | **否** | 否 | 是（Antigravity 模型的 `/v1/responses`） |
| 4 | Claude OAuth 工具别名（线程续接） | `internal/runtime/executor/claude_executor_tool_state.go` | `message:` + 上游消息 ID | **否** | 否 | 是（带 `thread` 字段的 `/v1/messages`） |
| 5 | 思考签名缓存 | `internal/cache/signature_cache.go` | 模型族、思考文本的 SHA-256 前 16 位 | **否** | 否 | 否 |
| 6 | Codex 推理重放 | `internal/cache/codex_reasoning_replay_cache.go`，键由 `codexReasoningReplaySessionKey` 生成 | 模型、`claude:` + 客户端会话 ID + `:agent:` + 代理 ID | **否** | 否 | 否 |
| 7 | Claude 网页搜索结果 | `internal/cache/claude_web_search_result_cache.go`，键由 `claudeWebSearchResultCacheScope` 生成 | 凭证哈希、调用方哈希、会话键、`tool_use_id` | 是 | 是 | 否（默认关闭，部署未开启） |
| 8 | Claude 兼容接口的思考重放 | `internal/cache/claude_thinking_replay_cache.go` | 凭证哈希、模型、调用方哈希、会话键 | 是 | 是 | 否（只用于 API Key 凭证） |
| 9 | Antigravity Interactions 续接 | `internal/runtime/executor/helps/antigravity_interactions_state.go` | 调用方、凭证 ID、Provider、模型、会话身份的摘要 | 是 | 是 | 否（只用于 `antigravity*` 的 Interactions 模型） |
| 10 | Claude Thread 的账号归属（v8.0.69 新增） | `sdk/cliproxy/session/claude_thread.go` | 调用方作用域的哈希、上游消息 ID 的哈希 → 凭证 ID | 是 | 值是账号 | 是（带 `thread.previous_message_id` 的 `/v1/messages`） |
| 11 | Claude Thread 的 beta 画像（v8.0.69 新增） | `internal/runtime/executor/helps/claude_thread.go` | Provider、上游 URL、凭证身份、调用方作用域的哈希，加上游消息 ID | 是 | 是 | 是（同上） |

说明：

- 第 1、2 项的键不含账号是上游有意为之，`antigravityReasoningReplayCacheKey` 的注释写明
  要与所选凭证无关，以便换号后重放还能延续。
- 第 3 项保存的是思考签名和对应文本的哈希；恢复时要求请求中同一 ID 的助手消息文本与哈希
  一致。
- 第 5 项只被 `internal/translator/antigravity/claude` 使用，即 Claude 格式的请求由
  Antigravity 账号执行。Gateway 只对 `claude-*` 模型发送 Claude 格式，而 Antigravity 账号
  现在不注册任何 `claude-*` 模型，所以走不到。
- 第 6 项只在请求格式为 Claude 时启用，即 Claude 格式的请求由 Codex 账号执行。Gateway 对
  `/v1/messages` 只放行 `claude-*` 模型，Codex 的目录里没有这类模型。
- Kimi、xAI、Devin、Meta 的缓存，Responses WebSocket 的工具输出缓存和视频任务的账号
  绑定，对应的 Provider 或端点在本部署中不存在，未列入。
- 第 10 项让同一调用方的 Claude Thread 续接请求固定到上一轮成功的账号
  （`withClaudeThreadAuth` 写入 `PinnedAuthMetadataKey`）。固定只把候选缩小到该账号，
  `pickViaPluginScheduler` 仍然调用插件，插件仍向 Gateway 核对该用户对这个账号的权限和本次
  请求的账号池；不允许时请求失败，不会换到别的账号，也不会绕过 Gateway 的分配。CPA 配置了
  KV 存储时这两项会落盘，本部署没有配置。

### 只描述账号或连接的状态

这些状态不含调用方的内容，键里有账号或出口即可。

| 状态 | 位置 | 键 | 说明 |
|---|---|---|---|
| Claude 的伪用户 ID、会话 ID、设备画像 | `helps/user_id_cache.go`、`session_id_cache.go`、`claude_device_profile.go` | 凭证 | 一个账号对上游呈现为同一台设备，所有调用方共用是设计本意 |
| Claude 续接标记（上一条消息 ID、上一个请求 ID、提示 ID、固定日期） | `helps/claude_diagnostics.go` `BeginClaudeContinuity` | 凭证身份 + 会话 ID | 只发给上游，不返回调用方。会话 ID 由 CPA 推导时带调用方作用域；由客户端提供时不带，见下节 |
| Antigravity 模型目录 | `sdk/cliproxy/antigravity_models.go` | 凭证 ID、项目、端点、代理、客户端版本 | |
| Antigravity 额度与冷却 | `internal/runtime/executor/antigravity_executor_credits.go` | 凭证 ID | |
| 传输层连接池 | `helps/proxy_helpers.go`、`helps/utls_client.go` | 代理地址（连接池另含凭证） | 连接不会跨出口复用。继承全局出口的账号代理地址相同，可能共用连接，它们的出口 IP 本来就相同 |

`helps/cache_helpers.go` 的 `codexCacheMap` 在这个版本里没有调用者。`payloadDocProbes`、
`stopChunkWithoutUsage`、`claudeFingerprintProfileWarned` 是单次请求的辅助状态或日志去重，
不保存内容。

### 插件自己的状态

会话绑定、会话根登记和并发名额的键都含调用方作用域，已由 `TestAllocation`
（两个调用方使用同一个 session id 互不影响）等测试覆盖，本次没有变化。

## 发给上游的标识

下列标识由 CPA 写进上游请求。它们决定上游把哪些请求看成同一个会话，以及上游自己的提示缓存
如何归并；CPA 进程内没有以它们为键的共享状态（上表的 Claude 续接标记除外）。

| Provider | 客户端没有提供会话标识 | 客户端提供了会话标识 |
|---|---|---|
| Codex | `prompt_cache_key` 与 `Session-Id` 取推导的会话身份，按调用方区分 | `prompt_cache_key` 原样转发；只提供了其他会话头时不设置 |
| Claude | `X-Claude-Code-Session-Id` 取推导的会话身份，按调用方区分 | 由客户端的值确定性地映射，不含调用方 |
| Antigravity | `request.sessionId` 取推导的会话身份，按调用方区分 | 取第一条用户文本的哈希，与调用方无关；转换后的请求体已有 `sessionId` 时保留该值 |

Codex 与 Claude 两行经过实测（真实 CPA 进程，同一个上游账号，两个调用方），Antigravity
一行来自源码：

- 不带任何会话标识时，同一个调用方两次请求得到相同的 `Session-Id`
  （`X-Claude-Code-Session-Id`），另一个调用方得到不同的值。
- 两个调用方都发送 `prompt_cache_key: "pck-shared"` 时，上游收到的 `Session-Id` 都是
  `pck-shared`。
- 两个调用方发送相同的 `X-Claude-Code-Session-Id` 时，上游收到的值相同。

影响：共用同一个上游账号的两个用户如果使用相同的会话标识，在上游看来是同一个会话，上游的
提示缓存也按同一个键归并。提示缓存按内容前缀命中，不会把一方的内容交给另一方。补丁镜像
把这些标识换成了含用户和账号的哈希；插件模式下要做到这一点必须改写请求内容，与“上游请求
只由 CPA 生成”的原则冲突，所以不做。Codex CLI 和 Claude Code 使用随机的会话 UUID，
正常使用不会相撞。

## 缺口

| # | 位置 | 前提 | 后果 | 本部署 |
|---|---|---|---|---|
| 1 | Gemini Responses 文本签名（上表第 3 项） | 知道另一个调用方某条响应消息的 ID 和全文 | 对方那条消息的思考签名被附加到自己发往上游的请求中 | 可达 |
| 2 | Claude OAuth 工具别名（上表第 4 项） | 知道另一个调用方的上游消息 ID，并发送不带工具定义的线程续接请求 | 对方的工具别名表被用来还原自己响应中的工具名 | 可达 |
| 3 | Codex 推理重放（上表第 6 项） | 知道另一个调用方的 Claude Code 会话 ID | 对方缓存的推理项和函数调用（含参数）被插入自己的请求 | 不可达 |
| 4 | 思考签名缓存（上表第 5 项） | 知道另一个调用方某段思考的全文 | 对方的签名被附加到自己的请求中 | 不可达 |

缺口 1、2 的标识是上游生成的随机值，只出现在创建它的调用方收到的响应里，而且得到的是
不透明的签名或工具名，不是对话内容。它们记入路线图的开放问题，在上游修复前接受。

## 提交上游

以下三份报告尚未提交，需要用维护者的 GitHub 账号在
`router-for-me/CLIProxyAPI` 提交。缺口 4 的缓存按内容寻址，是否属于缺陷由上游判断，
并入第一份报告末尾。修复合并后在这里记下链接，并从路线图的开放问题中移除。

### 1. Gemini Responses text-signature cache is not caller-scoped

> `internal/translator/gemini/openai/responses/trailing_signature.go` stores hidden
> text thought signatures with
> `cache.CacheAntigravityReasoningReplayItems(model, "gemini-responses-text:"+messageID, …)`
> and reads them back with the same key. Every other user of that cache goes through
> `antigravityReasoningReplayScopeFromRequest`, which prefixes the session key with
> `caller:<sha256(caller scope)>`. This key has no caller component, so on a
> multi-tenant instance a second API key that replays the same message ID and text
> gets the first caller's thought signature inserted into its own upstream request.
>
> Suggested fix: derive the key from the caller scope as the executor does. The
> translator has no access to request metadata today, so the scope would have to be
> passed in (for example through the context that `ConvertGeminiResponseToOpenAIResponses`
> already receives, and an equivalent parameter on the request side).
>
> Related: `internal/cache/signature_cache.go` keys signatures by model group and
> text hash only. It is content-addressed, so it needs the full thinking text, but a
> caller prefix would make it consistent with the replay cache.

### 2. Claude OAuth thread tool-alias store is keyed by message ID only

> `claudeOAuthToolAliasStore` (`internal/runtime/executor/claude_executor_tool_state.go`)
> saves the OAuth tool-name alias map under `"message:"+messageID` and loads it for a
> `thread.type == "continue"` request by `thread.previous_message_id`. The key
> contains neither the caller scope nor the credential, although
> `resolveClaudeMCPAliasOptions` already treats alias identity as belonging to the
> downstream caller. A different API key that knows a message ID can continue with
> another caller's alias map.
>
> Suggested fix: prefix the key with the caller scope (the value
> `resolveClaudeMCPAliasOptions` computes) and the credential identity.

### 3. Codex reasoning replay for Claude-format clients is not caller-scoped

> `codexReasoningReplaySessionKey` (`internal/runtime/executor/codex_executor_reasoning.go`)
> returns `claude:<session id>:agent:<agent id>` for Claude-format requests, taken from
> `X-Claude-Code-Session-Id` or `metadata.user_id`. `applyCodexReasoningReplayCacheRequiredDoc`
> uses it directly as the cache key. The other users of that function
> (`claudeThinkingReplayScopeFromRequest`, `claudeWebSearchResultCacheScope`) wrap it in
> `xaiReasoningReplayIsolateSessionKey`, which adds the caller. Without that, two API
> keys presenting the same Claude Code session ID share cached reasoning items and
> function calls, including their arguments.
>
> Suggested fix: apply the same caller isolation in `codexReasoningReplayScopeFromRequest`
> and `codexReasoningReplayScopeFromRequestDoc`, and in `ClaudeCodePromptCache`.

## CPA 升级时

升级不需要重做整份核对，按以下顺序检查：

1. 构建镜像。`TestProviderSessionsAreCallerScoped` 和 `TestAntigravityModelCatalog` 在
   构建中运行；前者失败说明会话身份的推导方式变了，需要重新阅读上表第 1 项。
2. 在新旧两个提交之间比较保存状态的文件，新增的进程级状态按本文的两栏标准补入表格：

   ```sh
   git diff --stat OLD NEW -- internal/cache internal/runtime/executor/helps \
     sdk/cliproxy/session internal/translator/gemini/openai/responses
   git grep -nE 'sync\.Map|NewBoundedLRU|= make\(map\[' NEW -- \
     internal/cache internal/runtime/executor ':!*_test.go'
   ```

3. 查看上文三份报告在上游的状态，已修复的从缺口表中移除。

### 升级记录

- **v8.0.59 → v8.0.69（2026-11-13）。** 上面第 2 步的 `git grep` 从 178 处增加到 187 处。
  新增的九处中七处是函数内的局部 map，两处是新的进程级状态，补入上表第 10、11 项，键都含
  调用方作用域。第 2、3 项的重放缓存只改了存储格式并增加只读的 `Peek*`，键不变。缺口 1、2
  涉及的 `trailing_signature.go` 与 `claude_executor_tool_state.go` 在这一版没有变化，三份
  报告仍未提交。`TestProviderSessionsAreCallerScoped` 在新版本上通过。
