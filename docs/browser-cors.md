# 网页工作台一键接入与 CORS

网关 `https://codex.water555.com` 的“概览”和“使用指导”提供“一键使用”，
跳转到 `https://ai.water555.com`，自动配置 Responses 连接并检查模型权限。
该功能由 Gateway 的 Go 服务提供 CORS，不需要 Caddy 添加响应头。

## 发布顺序

1. **先发布工作台**，确认它支持 `#handoff_version=1&code=…`，并在构建时设置
   `VITE_GATEWAY_URL=https://codex.water555.com`（这也是默认值）。工作台只向这个
   可信网关兑换连接码；构建变量修改后需重新构建、发布。
2. 发布包含接入接口的 Gateway 镜像。先保持 `.env` 中
   `GATEWAY_BROWSER_CLIENT_URL=` 为空，管理台入口显示尚未启用。
3. 如果线上曾按旧指南在 Caddy 设置 `@browser_api`、`@ai_api`、
   `@ai_preflight` 及对应 `header`、`respond` 指令，删除这些自定义 CORS
   块；保留原来的 `reverse_proxy gateway:8080` 和 `flush_interval -1`。
   其他代理也不能重复添加 `Access-Control-Allow-Origin`。
4. 在 `.env` 设置 `GATEWAY_BROWSER_CLIENT_URL=https://ai.water555.com`，
   按正常发布流程重新创建 Gateway 容器，使新环境变量生效。该配置同时启用入口、
   固定跳转目标与该网站 Origin 的 API CORS。不要把工作台域名添加到
   `WEBAUTHN_ORIGINS`；后者仍只用于管理台身份验证。
5. 按下面的检查验证预检、错误响应以及一次真实的一键接入。连接成功只检查模型
   列表，不会自动发送收费的推理请求。

Caddy 如有修改，先校验再重载（在服务器仓库根目录执行）：

```sh
./scripts/compose.sh exec -T caddy caddy validate \
  --config - --adapter caddyfile < deploy/Caddyfile
./scripts/compose.sh exec -T caddy caddy reload \
  --config - --adapter caddyfile < deploy/Caddyfile
```

`GATEWAY_BROWSER_CLIENT_URL` 必须是 HTTPS 绝对地址，可以包含部署路径，
不能包含用户名、密码、查询参数或片段。它为空时不允许兑换，也不开放浏览器
API CORS。只有本地设置 `GATEWAY_DEV_INSECURE_HTTP=true` 时才接受 HTTP。

## 接入与恢复

用户可以选择有效、关联设备正常且能取回完整值的 Key；没有可用 Key 时，
弹窗默认创建“网页工作台”（90 天，继承账户模型权限），并选择最新的有效设备。
没有设备则创建“网页工作台设备”。创建 Key 与接入都复用密码或 Passkey
二次验证。创建结果不明时刷新列表供用户选择，不自动重复创建。

“在这台设备记住密钥”默认关闭，未勾选时 Key 仅保存在当前工作台页面内存，
刷新后需要重新接入。勾选后按工作台设置保存于该浏览器的本地存储；备份始终
剔除密钥。已有不同连接时工作台先确认切换，取消、兑换失败或模型检查失败都
保留原连接。确认成功后保留文件及历史并进入新对话。

工作台优先选 `gpt-6.1-sol`；无权限时，按 ID 排序依次选其他可见 GPT 或
网关已支持的 Gemini 模型，排除 `codex-auto-review`。降级后显示实际模型，
Gemini 提示当前仅支持文本。模型检查失败可以重试，且不重复兑换连接码。

## 接口与安全边界

| 接口 | 授权与结果 |
| --- | --- |
| `POST /admin/browser-handoffs` | 管理台同源、登录且最近二次验证；接收 `api_key_id`、`remember_key`，返回 `launch_url`、`expires_at`。 |
| `POST /browser-handoffs/exchange` | 仅固定工作台 Origin；接收 `code`，返回 `base_url`、`api_key`、`api_key_id`、`remember_key`、`protocol: "responses"`。 |

跳转片段只有协议版本和连接码，不含 API Key。连接码使用 `cgb_v1_` 前缀与
256 位随机数，120 秒有效，单次原子消费。服务内存只保存 SHA-256 摘要、
用户／会话／Key 引用、记住选项和到期时间，最多 4096 项；到上限返回可重试
错误。签发和兑换均重新检查账户、会话、设备、Key、归属及加密数据完整性。
接入过程不记录连接码或明文 Key，也不新增持久存储；既有 Key 仍按原有机制
加密保存在数据库中。两个接入接口的所有响应禁止缓存。

沿用**单实例** Gateway 部署，无数据库迁移。重启会使待兑换连接码失效，
用户返回网关重新接入即可；不要把此内存存储直接部署到多个独立副本。
会话注销、账号／设备／Key 禁用及 Key 过期都会使待兑换连接失败。网络中断
导致兑换结果不明时，用户需返回网关重新签发；不能重放旧码。

Go 层只开放以下跨域路由，精确匹配配置 URL 的 Origin（协议、主机和端口）：

| 路径 | 预检允许的方法 | 允许的请求头 |
| --- | --- | --- |
| `/browser-handoffs/exchange` | `POST` | `Content-Type` |
| `/v1/models` | `GET` | `Authorization, Content-Type` |
| `/v1/responses` | `POST` | `Authorization, Content-Type` |

预检直接返回 204，无需 API Key；实际请求仍验证 Key 和权限。错误响应、流式
响应均带允许来源头与 `Vary: Origin`。不启用 `Access-Control-Allow-Credentials`，
客户端使用 `credentials: 'omit'`。管理、登录、`/v1/responses/compact` 和
Gemini 原生接口不在白名单中。命令行不带 Origin 的模型请求继续使用 Bearer Key。
CORS 不是身份认证，非浏览器客户端可以伪造 Origin。

## 验证

```sh
curl -i -X OPTIONS 'https://codex.water555.com/browser-handoffs/exchange' \
  -H 'Origin: https://ai.water555.com' \
  -H 'Access-Control-Request-Method: POST' \
  -H 'Access-Control-Request-Headers: content-type'
curl -i -X OPTIONS 'https://codex.water555.com/v1/models' \
  -H 'Origin: https://ai.water555.com' \
  -H 'Access-Control-Request-Method: GET' \
  -H 'Access-Control-Request-Headers: authorization'
curl -i -X OPTIONS 'https://codex.water555.com/v1/responses' \
  -H 'Origin: https://ai.water555.com' \
  -H 'Access-Control-Request-Method: POST' \
  -H 'Access-Control-Request-Headers: authorization,content-type'
```

三者应返回 204，`Access-Control-Allow-Origin` 只能有一个且等于工作台
Origin，`Access-Control-Allow-Methods` 等于该接口的方法。兑换接口另有
`Cache-Control: no-store`。用未授权来源重试应返回 403 且没有允许来源头。

```sh
curl -i 'https://codex.water555.com/v1/models' \
  -H 'Origin: https://ai.water555.com'
```

无 Key 请求应返回 401（反复尝试可能限流为 429），仍包含允许来源头。
最后在桌面与手机浏览器验证两个入口、验证取消、重复点击、创建资源、
重新签发、连接切换确认／取消、刷新持久化和备份不含密钥。

本地跨域回归可以用测试夹具提供真实 Go 接口（仅回环地址，不发送推理）：

```sh
GATEWAY_BROWSER_FIXTURE_LISTEN=127.0.0.1:4180 \
GATEWAY_BROWSER_FIXTURE_ORIGIN=http://127.0.0.1:4174 \
go test -v -run '^TestBrowserHandoffBrowserFixture$' -timeout 20m ./internal/server
```

浏览器开发服务使用 `VITE_GATEWAY_URL=http://127.0.0.1:4180` 和端口 4174。
夹具 `POST /test/setup` 接收 `models`、`model_failures`、`remember_key`，返回
真实签发的 `launch_url`；`GET /test/stats` 检查兑换／模型／推理次数，
`POST /test/stop` 结束夹具。此夹具只编译进测试二进制。

## 回滚

清空 `GATEWAY_BROWSER_CLIENT_URL` 并重新创建 Gateway 容器即可停用入口、兑换
和跨域 API。已保存的 Key 不会因此被撤销；需要撤销访问时禁用对应 Key。
不要恢复旧的 Caddy CORS 块，除非已经回滚到没有 Go CORS 的旧 Gateway 镜像，
并明确需要继续支持旧版网页直连。
