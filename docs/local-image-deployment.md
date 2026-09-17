# 本地构建并传送到服务器

正式生产部署仍应按 [部署与运维手册](operations.md#4-构建和首次启动) 在目标服务器现场构建。以下流程适用于确认过 Git revision、目标架构和镜像标签的直接镜像交付；示例假设服务器为 `linux/amd64`。

## 1. 本地构建

将本地 `.env` 中的 `GATEWAY_IMAGE_TAG`、`GATEWAY_VERSION` 和 `GATEWAY_REVISION` 设置为审阅过的版本及完整 Git SHA，然后执行：

```sh
git status --short
git rev-parse HEAD
DOCKER_DEFAULT_PLATFORM=linux/amd64 \
  ./scripts/compose.sh build gateway codex-compat antigravity-bridge
```

不使用 Antigravity 时可省略 `antigravity-bridge`。

## 2. 传送镜像

`gateway_tag` 必须与本地 `.env` 中的 `GATEWAY_IMAGE_TAG` 完全一致：

```sh
gateway_tag=<完整Git-SHA或正式版本>
docker save \
  "codex-gateway-gateway:${gateway_tag}" \
  "codex-gateway-compat:v7.2.150-c77b1369-00633c2417755730-codex-only" \
  "codex-gateway-antigravity:agy1.2.4-${gateway_tag}" \
  | gzip -1 \
  | ssh deploy@server 'gzip -dc | docker load'
```

远端 Docker 需要 root 权限时，将最后一段改为 `gzip -dc | sudo docker load`。

## 3. 服务器启动

服务器必须检出与镜像相同的 Git revision，并保留服务器自己的 `.env` 和 `deploy/secrets/`：

```sh
cd /opt/codex-gateway
git fetch --tags --prune
git checkout --detach <与本地相同的完整Git-SHA>
./scripts/validate-compose.sh
./scripts/compose.sh up -d --no-build \
  postgres egress-allowlist codex-compat gateway caddy cloudflared
./scripts/compose.sh ps
```

服务器 `.env` 中的三个版本字段必须与已导入镜像一致。域名、价格、并发限制和模型路由等运行配置以服务器 `.env` 为准；数据库口令、加密密钥和 Tunnel Token 以服务器 `deploy/secrets/` 为准。本地 `.env` 不会进入镜像，只用于本地构建的标签和版本参数。

仅更新 Gateway 时执行：

```sh
./scripts/compose.sh up -d --no-build --no-deps --force-recreate gateway
```
