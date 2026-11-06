# Codespaces 开发环境

仓库的主服务使用 **Go**，这里配置的是 GitHub Codespaces / Dev Container，而非 Python `venv`。

在 GitHub 仓库页面选择 **Code → Codespaces → Create codespace**，选择包含 `.devcontainer/` 的分支（合并配置后选 `main`）。Codespaces 会构建开发容器、启动专用测试数据库并下载 Go 模块。

## 环境

- Go 1.26.10：与项目锁定的 Go 镜像一致。
- C 编译器（race detector）、Node.js（用于 `internal/server/testdata/*.cjs`）、Python 3、Git、jq。
- PostgreSQL 17.6：与 CI 一致的 digest-locked 镜像，仅在容器网络中访问，**不发布宿主机端口**。数据位于 tmpfs，容器移除后丢弃。
- 非 root 的 `vscode` 用户、独立的 Go 模块与构建缓存。

## 验证

```sh
go version
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
python3 -m unittest discover -s scripts/tests -p 'test_*.py' -v
go test -count=1 -tags=integration ./internal/store ./internal/server
```

`TEST_DATABASE_URL` 已预置为 Compose 内**专用可丢弃测试数据库**，不读取部署数据库的连接信息。

本环境只用于源码开发和测试，**不启动生产网关、Cloudflare Tunnel、CLIProxyAPI 或生产部署 Compose**，不会导入生产 `.env`、API Key 或 OAuth 凭据。不要将真实生产密钥放入 Codespace。

`deploy/cpa-panel` 是采用 Bun 1.3.14 的可选独立前端项目；此环境没有自动安装 Bun。
