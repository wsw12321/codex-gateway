# Antigravity 凭据双向迁移

`cpa-credentials-migrate` 是维护窗口内的服务端工具。它优先直接转换旧 Keyring；只有
刷新、身份、项目或模型额度验证失败的账号需要重新授权。当前工作区没有真实部署账号，
单元测试中的合成凭据不构成真实账号验收。

工具使用运行时私密文件提供的 Google OAuth 客户端强制刷新，即使旧 access token 尚未过期。
客户端必须与固定 CPA v8.0.20 的 Antigravity 客户端一致；不要换用任意自建 OAuth 客户端。
刷新成功后先把新 token 保存到 Keyring 和受保护恢复文件，再验证 Google `verified_email`
及稳定 subject、`loadCodeAssist` 项目、`fetchAvailableModels` 的原生 Gemini 模型和额度。
错误只输出固定诊断；令牌不出现在日志、命令参数或浏览器中。

若强制刷新在 Google 身份验证之前失败，工具会隔离该账号，并且**不会凭旧 access token、
掩码邮箱或账号名构造 Google subject 映射**。CPA 面板没有“任意旧账号 ID 绑定”功能，
此时不要在面板新建通用 CPA OAuth 账号来替代原账号，否则它会成为不同的 Gateway 账号。
应在仍保留的服务端旧 Registry 槽位中重新授权，再重跑前向迁移：

```sh
# 维护窗口内先排空请求并停止 CPA 和旧 bridge，禁用其自动重启。
# existing-slot 必须是原 accounts.json 的 name，不是新起的名字。
./scripts/antigravity-login.sh --credentials-only existing-slot
```

该模式显式启用 `legacy-bridge` Compose profile，拒绝仍在运行的 CPA，停止旧 bridge，
仅允许已存在的 Registry 名称进行官方 CLI 重新登录及独立凭据/模型/额度/生成检查，
不会启动 bridge HTTP 服务或恢复 API 流量。用原 Google 账号完成登录；现有槽位的稳定 ID
和 `Enabled` 禁用状态保持不变，输错新名字会在登录前拒绝。生产镜像须包含本次新增的
`auth-reauthorize` 命令，不能拿未更新的旧镜像运行这个模式。

普通 legacy 登录模式也会在任何服务/凭据变更前检查真实 CPA 容器：运行中的默认 CPA
会阻止旧登录；只有容器中唯一的 `CPA_ANTIGRAVITY_ENABLED=false` 字面配置允许普通
rollback 登录与 Codex 同时运行。检查只读取固定状态标记，不输出容器环境内容。
`--credentials-only` 更严格，仍要求 CPA 进程完全停止，不接受仅关闭 Google 的运行态。

完成后，确认所有刷新进程仍停止，使用下文维护容器的 `-direction forward -account existing-slot`
再次转换。将报告 `account_id` 与切换前 `accounts.json` 及 Gateway 账号 ID 比对，确认一致
并完成真实 Gateway 验收后才按原业务状态恢复。不要删除旧 Registry 记录、改槽位名字或
手工编辑身份映射来绕过失败；无法完成上述流程的账号继续停用。

迁移建立 `/oauth/.gateway-antigravity-identities`：

```json
{"version":1,"accounts":{"123456789":"0123456789abcdef"}}
```

Google subject 映射到旧 Gateway 16 位稳定账号 ID，与文件名、邮箱显示、刷新和重新登录
无关。重复身份或 ID 冲突会隔离，不覆盖其他账号。映射和 CPA 凭据均为 UID 10001、0600，
目录为 0700。原 Keyring 继续为 UID 10002；一次性容器以 root 运行，仅通过受限子进程
访问 UID 10002 的 Secret Service，避免放宽任一持久目录权限。

维护前先暂停新请求、账号变更和自动重启，排空流式请求与待结算预约，保存当前数据库、
配置和凭据备份，停止旧 bridge 和 CPA 进程。工具会取得两个目录的 `.gateway-refresh.lock`；
若有更新后的服务进程持有锁，工具拒绝迁移。**旧版本进程不一定实现该锁，必须先从编排层
确认它们已停止。** 整个迁移与回滚期间禁止另一个进程刷新同一凭据。

构建维护镜像时，把 `LEGACY_BRIDGE_IMAGE` 指定为已经核验的旧 bridge 镜像完整 digest，
保留它提供的固定 AGY CLI 与 Keyring 软件：

```sh
docker build -f deploy/cpa-migrate/Dockerfile \
  --build-arg LEGACY_BRIDGE_IMAGE="$LEGACY_BRIDGE_IMAGE" \
  -t codex-gateway-cpa-migrate:reviewed .
```

构建后记录维护镜像的不可变 image ID。它不属于默认服务，也不监听端口。运行时挂载旧
Keyring volume、CPA OAuth volume、原有 Keyring 密码文件及 Google OAuth 客户端文件。
从受信任的部署配置取得匹配的客户端 ID 和 secret，保存在仅所有者可读写的 0600 JSON
文件中（例如已被 Git 和 Docker 构建忽略的 `deploy/secrets/cpa-migration-google-oauth.json`）：

```json
{"client_id":"<CPA Google OAuth client ID>","client_secret":"<CPA Google OAuth client secret>"}
```

将 `GOOGLE_OAUTH_CLIENT_FILE` 设为该文件的绝对路径；不要将真实值写入源码、镜像或
命令参数。容器默认读取 `/run/secrets/cpa_migration_google_oauth`，可通过
`-google-oauth-client-file` 指定其他路径。文件缺失、权限开放或任一字段缺失时拒绝迁移，
仅输出固定诊断，不回显凭据。上述 JSON 仅展示格式，必须替换占位值后使用。

两种方向都接入保留的
`antigravity_internal` 网络，使用**已经停止的 bridge 的固定 IP `172.28.40.3`**，
其 Google 域名出口清单同时支持反向迁移的旧 CLI。普通动态 IP 不满足 Squid 源地址
规则；启动前确认该地址没有运行中容器占用。`EGRESS_NETWORK` 是此 Compose 网络的
实际名称，其他占位变量替换为部署中实际卷名：

```sh
docker run --rm --read-only --user 0:0 \
  --cap-drop ALL --cap-add SETUID --cap-add SETGID --cap-add CHOWN \
  --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add KILL \
  --security-opt no-new-privileges:true \
  --network "$EGRESS_NETWORK" --ip 172.28.40.3 \
  --mount type=volume,source="$LEGACY_KEYRING_VOLUME",target=/var/lib/antigravity/keyrings \
  --mount type=volume,source="$CPA_OAUTH_VOLUME",target=/oauth \
  --mount type=bind,source="$KEYRING_PASSWORD_FILE",target=/run/secrets/antigravity_keyring_password,readonly \
  --mount type=bind,source="$GOOGLE_OAUTH_CLIENT_FILE",target=/run/secrets/cpa_migration_google_oauth,readonly \
  --tmpfs /run/cpa-migrate:rw,noexec,nosuid,nodev,mode=0700 \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,mode=1777 \
  "$MIGRATION_IMAGE_ID" -direction forward -egress-proxy http://egress-allowlist:3128
```

密码文件必须可由原 UID 10002 读取。工具拒绝 `NO_PROXY` 绕过显式代理，拒绝重定向；
HTTP 错误响应正文不会回显。`-account name` 可单独处理一个原有账号。

导入文件初始 `disabled:true`，不会意外恢复以前被禁用的业务账号。退出状态 2 表示至少
一个账号被隔离，JSON 报告只含稳定 ID、状态、已验证模型及固定原因。成功状态为
`awaiting_gateway_verification`，只表示转换与供应商检查通过，**不表示 Gateway 生成验收
通过**。逐账号在维护窗口通过 Owner 控制面临时启用，使用只可访问该账号的真实 Gateway
Key 执行下面的请求，并在 Gateway 账单/审计中核对 request ID 的实际账号归因与非零有效
usage；随后按原业务状态决定是否恢复流量：

```sh
curl --fail-with-body --config /run/protected/gateway-smoke.curl \
  -H 'Content-Type: application/json' \
  --data '{"model":"gemini-pro-agent","input":"Reply with exactly OK.","store":false}' \
  "$GATEWAY_URL/v1/responses"
```

`gateway-smoke.curl` 为 0600 文件，通过其中的 `header` 配置提供 Authorization；Key 不应
出现在命令参数。只测试报告实际可用且已授权的模型；不可用模型保持隐藏/拒绝。还须执行
一次刷新后重启、SSE 和工具续轮验收，并记录结果，不得把其他账号成功作为本账号验收。

反向迁移在再次排空并停止 Google 请求和所有刷新进程后，以相同卷运行
`-direction reverse`。工具按稳定 subject 查找最新 CPA 文件，文件改名不会改变匹配。
它先刷新并持久化当前 CPA token，验证身份和项目，然后更新旧 Keyring 的 token 和项目，
保留其他旧 CLI 元数据。写回时强制 token 过期，要求固定旧 AGY CLI 真正刷新并完成
模型、额度与生成检查；检查失败后仍回收并保留新产生的 Keyring token。CPA 文件保持禁用。
后续还须通过实际 Gateway legacy 路径验收，失败账号保持停用并重新授权。

没有旧 Registry/Keyring 记录的新账号不会被隐式创建到旧链路；旧链路不支持的模型保持
不可用。不要恢复旧 token 快照覆盖迁移中产生的新凭据。`.gateway-migration-recovery-*`
是保存成功刷新结果的 0600 恢复文件，CPA 不加载它们；按凭据备份保护，仅用于服务端
故障恢复。7 天稳定期后清理已停止的旧镜像、恢复文件与过时配置，保留长期使用的身份映射。
