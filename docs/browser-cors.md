# 浏览器直连网关的 CORS 配置

当独立网页通过浏览器直接请求 Gateway 时，需要在网关入口允许该网页的来源。
例如网页是 `https://ai.water555.com`，API 是 `https://codex.water555.com/v1`：
两者主机名不同，属于跨域请求。命令行使用 API 地址和 Key 成功，不代表浏览器
的跨域预检也能通过。

本指南适用于仓库标准 Docker Compose 部署：
`Cloudflare Tunnel → cloudflared → Caddy → gateway`。配置位置是
[`deploy/Caddyfile`](../deploy/Caddyfile)，无需重建 Gateway 镜像。
如果实际入口绕过了 Caddy，应先确认请求经过的位置；修改本文件只影响经过该
Caddy 服务的请求。

Gateway 默认不启用 CORS，也没有 `CORS_ORIGINS` 之类的环境变量开关。
`WEBAUTHN_ORIGINS` 用于管理台身份验证，不能代替 API 的 CORS 配置。
下面是可选的部署配置，仓库默认 Caddyfile 不会自动开启它。

## 1. 添加来源和接口白名单

在目标服务器的仓库根目录编辑 `deploy/Caddyfile`。将以下片段放进现有的
`http://{$GATEWAY_DOMAIN} { ... }` 站点块中、`reverse_proxy gateway:8080`
之前，保留现有配置：

```caddyfile
# 这些接口的响应随 Origin 而不同，包括未命中来源白名单的响应。
@browser_api path /v1/models /v1/responses
header @browser_api {
    +Vary Origin
    defer
}

# 仅允许指定网页读取模型 API 响应。
@ai_api {
    path /v1/models /v1/responses
    header Origin https://ai.water555.com
}
header @ai_api {
    Access-Control-Allow-Origin "https://ai.water555.com"
    Access-Control-Allow-Methods "GET, POST, OPTIONS"
    Access-Control-Allow-Headers "Authorization, Content-Type"
    defer
}

# 预检没有 API Key，在 Caddy 中直接响应。
@ai_preflight {
    method OPTIONS
    path /v1/models /v1/responses
    header Origin https://ai.water555.com
}
respond @ai_preflight 204
```

使用其他网页域名时，将片段中三处 `https://ai.water555.com` 一起替换为实际
来源。来源包含协议、主机名和非默认端口，不含路径或末尾 `/`。自定义域名与
`*.pages.dev` 预览地址是不同来源，此示例只放行一个来源。

配置覆盖 `GET /v1/models` 和 `POST /v1/responses` 所需的预检与实际响应，
包括 SSE 和 API 返回的 401、403、429 等错误。实际请求仍由 Gateway 验证
Bearer Key、权限和配额；CORS 不是身份认证，非浏览器客户端可以伪造 Origin。

网页应使用 `credentials: 'omit'` 并显式发送 `Authorization: Bearer …`。
此方式不需要 `Access-Control-Allow-Credentials`。管理台和 `/auth/*` 不在
上述路径白名单中。现有代理的 `flush_interval -1` 继续负责 SSE 即时转发。

## 2. 校验并重载 Caddy

以下命令均在服务器的仓库根目录运行。先校验修改后的配置：

```sh
./scripts/compose.sh exec -T caddy caddy validate \
  --config - --adapter caddyfile < deploy/Caddyfile
```

只有校验成功后，才执行重载：

```sh
./scripts/compose.sh exec -T caddy caddy reload \
  --config - --adapter caddyfile < deploy/Caddyfile
```

标准输入传入的是刚编辑的文件，可避免编辑器替换文件后，容器的单文件挂载
仍指向旧文件内容。文件中的 `{$GATEWAY_DOMAIN}` 由 Caddy 容器已有环境变量
展开。保留服务器上的文件修改，后续重新创建容器时也会加载它。

此步骤通过 Caddy 的管理接口重载配置，无需重建或重启 Gateway、数据库和
sidecar。若后续更新仓库，请保留这项部署配置并重新检查差异。

## 3. 验证预检和实际响应

以下检查不需要 API Key；使用其他域名时同时替换 URL 和 Origin。

先验证 Responses 的 POST 预检：

```sh
curl -i -X OPTIONS 'https://codex.water555.com/v1/responses' \
  -H 'Origin: https://ai.water555.com' \
  -H 'Access-Control-Request-Method: POST' \
  -H 'Access-Control-Request-Headers: authorization,content-type'
```

再验证模型列表的 GET 预检。GET 也会因 `Authorization` 请求头而需要预检：

```sh
curl -i -X OPTIONS 'https://codex.water555.com/v1/models' \
  -H 'Origin: https://ai.water555.com' \
  -H 'Access-Control-Request-Method: GET' \
  -H 'Access-Control-Request-Headers: authorization'
```

两者应返回 `204`，并包含以下响应头（名称大小写和顺序可能不同）：

```http
Access-Control-Allow-Origin: https://ai.water555.com
Access-Control-Allow-Methods: GET, POST, OPTIONS
Access-Control-Allow-Headers: Authorization, Content-Type
Vary: Origin
```

`Vary` 可能还包含其他值。接着验证实际错误响应也带 CORS 头：

```sh
curl -i 'https://codex.water555.com/v1/models' \
  -H 'Origin: https://ai.water555.com'
```

由于没有发送 Key，通常返回 `401`；反复尝试触发无效 Key 限流时可能返回
`429`。两者都应包含上述允许来源的响应头。这说明 API 鉴权仍然生效，网页
也能读取真实错误。预检成功并不能代替此项检查。

最后验证未允许的来源：

```sh
curl -i -X OPTIONS 'https://codex.water555.com/v1/responses' \
  -H 'Origin: https://untrusted.example' \
  -H 'Access-Control-Request-Method: POST' \
  -H 'Access-Control-Request-Headers: authorization,content-type'
```

这个响应不应包含 `Access-Control-Allow-Origin`。curl 不执行浏览器的 CORS
检查，需要查看响应头，并在网页中完成下面的实际调用验证。

## 4. 网页连接设置

| 设置 | 示例 |
| --- | --- |
| API 地址 | `https://codex.water555.com/v1` |
| API Key | 用户自己的 Gateway 设备 Key |
| 协议 | Responses |
| 模型 | `/v1/models` 返回且该 Key 有权使用的模型 |

在 `https://ai.water555.com` 刷新模型列表，再发送一条消息，确认流式回复能
持续显示。Gateway 不提供 Chat Completions；添加 CORS 不会增加这个接口。
本示例也未放行 `/v1/responses/compact` 或 Gemini 原生接口，如客户端需要
其他接口，应按实际路由单独评估并扩展路径白名单。

## 排查与回滚

- 预检仍返回 `405`：确认请求经过仓库的 Caddy 服务、已重载修改后的文件，
  且 Origin 和路径与配置完全一致。
- 预检返回 `401` 或登录跳转：检查入口是否有额外鉴权拦截 OPTIONS；浏览器
  预检不会携带 API Key。
- 预检成功但网页仍报跨域错误：检查实际 GET/POST 响应是否带允许来源的头，
  并确认实际页面来源是否变成了另一个自定义域名或预览域名。
- 出现重复的 `Access-Control-Allow-Origin`：检查其他代理或应用是否同时
  添加 CORS 头。此方案由 Caddy 统一设置。
- 能读取 JSON 错误：按 Gateway 返回的 Key、权限、模型或额度错误处理；
  这已经是实际 API 响应。

回滚时删除第 1 节新增的三个命名匹配器及对应的 `header`、`respond` 指令，
再执行第 2 节的校验和重载。网页直连恢复为不允许跨域，命令行 Bearer Key
调用和管理台继续按原配置工作。

本指南采用浏览器直连模式；[浏览器聊天开发设计](browser-chat-development.md#131-域名与路由)
中描述的 Worker 同源转发是另一种架构，不依赖这里的 CORS 配置。

参考：[MDN CORS](https://developer.mozilla.org/en-US/docs/Web/HTTP/Guides/CORS)、
[Caddy header](https://caddyserver.com/docs/caddyfile/directives/header)、
[Caddy respond](https://caddyserver.com/docs/caddyfile/directives/respond)、
[Caddy 命令行](https://caddyserver.com/docs/command-line)。
