# GitHub Actions 构建，服务器免登录拉取

适用于 `linux/amd64` 服务器。代码推送到 `main` 后，现有 `ci` 工作流构建
Gateway、Codex 兼容层和 Antigravity Bridge，运行测试及漏洞扫描。全部 CI
检查通过后，`publish-images` 才将同一批镜像推送到 GitHub Container Registry
（GHCR），不会重新构建，也不会连接生产服务器。

## 1. 启用发布

将这次配置提交并推送到 `wsw12321/codex-gateway` 的 `main` 分支。在仓库
**Actions → ci** 查看进度；也可以通过 **Run workflow** 选择 `main` 手动运行。
PR 和其他分支的手动运行不会发布镜像。

发布使用 GitHub 自动提供的 `GITHUB_TOKEN`，只有发布任务具有
`packages: write` 权限，不需要配置 PAT、服务器 SSH 密钥或生产环境 secret。
如果账户或组织限制了 Actions/Packages，需要在相应设置中允许该工作流发布。

三个镜像地址如下，`<完整 Git SHA>` 对应本次 CI 的提交：

```text
ghcr.io/wsw12321/codex-gateway/gateway:sha-<完整 Git SHA>
ghcr.io/wsw12321/codex-gateway/codex-compat:sha-<完整 Git SHA>
ghcr.io/wsw12321/codex-gateway/antigravity-bridge:sha-<完整 Git SHA>
```

其他 GitHub 仓库运行时，地址中的 `wsw12321/codex-gateway` 自动替换为该仓库的
小写 owner/repository。

镜像发布产物只包含构建完成的应用镜像。`.env`、`deploy/secrets/`、备份和
`upload-test*.bin` 已在 Git 和 Docker 的忽略规则中排除。流水线使用临时生成的
非生产配置，不挂载生产 OAuth volume。

## 2. 首次发布后，将三个镜像包设为 Public

GitHub 新建镜像包默认是 Private，即使代码仓库本身是公开的，也需要检查包的
可见性。首次 `publish-images` 成功后：

1. 打开 GitHub 个人主页的 **Packages**。
2. 分别打开 `codex-gateway/gateway`、`codex-gateway/codex-compat`、
   `codex-gateway/antigravity-bridge`。
3. 进入 **Package settings → Danger Zone → Change visibility → Public**。

完成后，服务器无需 `docker login` 即可下载。包会一直保持公开，后续版本不必重复
设置；代码仓库可以保持私有。公开包中的镜像可被任何人下载，GitHub 不支持再改回
私有。参见 [GitHub 镜像包可见性说明](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility)。

## 3. 下载本次发布清单

在成功运行的 **Actions → ci → Artifacts** 下载
`deployment-images-<完整 Git SHA>`，解压获得 `deployment-images.json`。
它只包含提交号、平台和三个镜像的 SHA256 digest，体积很小，可以从电脑上传：

```sh
scp deployment-images.json wsw@45.91.81.12:/tmp/deployment-images.json
```

保留这份清单。CI 中清单默认保留 90 天；中转用的 `tested-images-*` 大文件只保留
1 天，不是服务器部署所需的文件。服务器按清单中的 digest 拉取，避免同名标签
重新发布导致内容变化。

## 4. 在服务器拉取镜像

进入服务器已有项目目录，检出清单对应的提交；保留服务器自己的 `.env` 和 secret。
以下命令需要在同一个 shell 中依次执行：

```sh
revision=$(jq -er '.revision' /tmp/deployment-images.json)
git fetch origin main
git checkout --detach "$revision"

sed -i \
  -e "s/^GATEWAY_IMAGE_TAG=.*/GATEWAY_IMAGE_TAG=$revision/" \
  -e "s/^GATEWAY_VERSION=.*/GATEWAY_VERSION=$revision/" \
  -e "s/^GATEWAY_REVISION=.*/GATEWAY_REVISION=$revision/" \
  .env

./scripts/pull-ci-images.sh /tmp/deployment-images.json
```

脚本会检查提交号、版本配置、镜像仓库路径、digest 和架构，然后拉取三个镜像并
标记为现有 Compose 使用的本地名称。只有全部拉取成功后才更新本地标签，不重启
服务。若 Docker 需要 sudo，在调用脚本前加 `sudo`。

此处使用 GHCR 登录凭据与 Git 仓库登录凭据是两回事：公开镜像无需登录，但私有
代码仓库的 `git fetch` 仍需要原有 SSH key 或其他 Git 认证。

## 5. 手动更新服务

沿用 [部署与运维手册](operations.md) 和 [兼容层升级规程](compatibility-upgrades.md)
完成备份及数据库迁移准备，然后执行：

```sh
./scripts/validate-compose.sh
./scripts/compose.sh up -d --no-build --pull never \
  postgres egress-allowlist codex-compat gateway caddy cloudflared
./scripts/compose.sh ps
```

`--pull never` 让应用使用刚导入的本地镜像，避免 Compose 去 Docker Hub 查找本地
名称；上述命令要求基础设施镜像已经存在。本流程针对已有部署，首次部署仍须先按
运维手册准备基础镜像、配置和 secret。

如果已启用 Antigravity，再执行：

```sh
./scripts/compose.sh up -d --no-build --pull never antigravity-bridge
```

检查 Gateway 的 `/readyz`，按升级规程完成真实 OAuth 冒烟。需要回退时，使用上次
保存的清单、对应 Git 提交和版本配置重新拉取；涉及数据库变更时遵循其回退约束。

镜像包仍保留在 GHCR，重复部署只下载本机缺少的层。服务器到 GHCR 的速度需要以
实际拉取为准，不能用其他测速源的结果替代。
