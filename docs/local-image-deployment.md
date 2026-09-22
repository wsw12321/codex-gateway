# 本地构建并传送到服务器

可以按 [部署与运维手册](operations.md#4-构建和首次启动) 在服务器构建，或使用
[GitHub Actions 构建并从 GHCR 拉取](ci-image-deployment.md)，减少电脑上传大文件。
以下流程适用于确认过 Git revision、目标架构和镜像标签的直接镜像交付；示例假设服务器为 `linux/amd64`。

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
  "codex-gateway-compat:v7.3.12-2eb8dd11-64ac7f9db31f94df-codex-only" \
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

近 24 小时费用分配功能必须同时导入并升级本次 Gateway 和兼容层，应用
`0009_upstream_allocation.sql`。兼容层启动不等待 Gateway；等 Gateway
`/readyz` 返回 200 后再运行 `./scripts/smoke-sidecar.sh`。回调失败或全部候选
系数为 0 时，新分配返回 503。上线前按升级规程完成备份与真实 OAuth 冒烟。

完成本次配套升级后，后续仅更新 Gateway 时执行：

```sh
./scripts/compose.sh up -d --no-build --no-deps --force-recreate gateway
```
