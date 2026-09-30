# CPA v8 本地验证记录

验证日期：2026-09-30。范围是当前工作区、合成凭据、HTTP stub、一次性 PostgreSQL 和
本地 Docker 镜像，未连接生产实例，未执行真实 Google/OpenAI 账号验收或生产切换。

CPA 的固定源码、补丁、Go 版本和 Sol 模型目录来源见
[`source.lock.json`](../deploy/codex-compat/source.lock.json)。面板固定 v1.25.0 的来源和
受限衍生范围见 [`UPSTREAM.json`](../deploy/cpa-panel/UPSTREAM.json)。实际构建的本地
OCI 摘要记录在 [`cpa-v8-validation.lock.json`](../deploy/cpa-v8-validation.lock.json)。
这些镜像尚未发布到远程仓库；锁文件不是 CI 的生产部署 manifest。发布后应使用 CI
生成的 registry digest，并按 [切换流程](cpa-v8-cutover.md) 在实际服务器验收。

| 检查 | 结果与实际覆盖 |
| --- | --- |
| Gateway `go test -count=1 ./...` | 通过，包括原有 Codex、legacy bridge、管理鉴权、原生协议及计费单元回归 |
| Gateway `go test -race -count=1 ./...`、`go vet ./...` | 固定 Go 1.26.8 容器通过；最后增加的回滚管理隔离又执行了 server race 和全包 vet |
| 一次性 PostgreSQL store/server 集成测试 | 通过；store 36.258 秒，server 4.297 秒，按顺序运行，未使用生产数据 |
| 原生 Gateway 生成与结算 | 数据库集成验证 JSON、SSE、Responses、provider/稳定账号归因、退役别名拒绝、缺失 usage 失败和额度释放 |
| usage 回归 | Gemini 输出包含生成与思考、缓存输入不重复收费、SSE 累计快照不相加、Responses 思考不再加到输出 |
| 一次性模型迁移与回滚 | 九模型默认/现有用户权限和倍率、受限 Key 原样保留、其他模型及历史记录保留、条件回滚与重启不重跑 |
| CPA v8 镜像 | 正常 Dockerfile 构建通过完整受影响模块、race、指定 Codex 安全回归、测试存在性及补丁摘要检查 |
| CPA v8 无网络运行 | 版本/提交、OAuth 文件权限、两类账号能力、独立管理密钥、受限接口及刷新锁拒绝第二进程均通过 |
| CPA v7 兼容回滚镜像 | 独立旧入口和测试夹具；源码/补丁检查、完整原有回归、race、构建和无网络运行通过 |
| 最终 legacy bridge 镜像 | 源码构建与单元测试通过；最终 OCI 构建及新代码的容器 Keyring 探针被 Docker 存储故障阻断，尚未通过 |
| 凭据维护镜像 | 正常构建；合成加密 Keyring 跨 UID/独立 D-Bus 会话往返、受限 tmpfs 和活动刷新锁拒绝通过 |
| 面板 | 固定源码/资源摘要、构建、Owner/近期验证/同源/一次性 OAuth、桌面/手机和 CSP 检查通过 |
| Compose | 临时非生产配置通过网络、secret、默认 profile、版本/补丁/价格检查；固定 Squid/Caddy 镜像无网络实际解析通过 |
| 运维脚本 | 最终 67 项 Python 回归通过，包括原账号维护重授权、默认 CPA 拒绝旧刷新器、profile 和源锁检查 |

PostgreSQL 第一次重用测试库时，原有 `integration-owner` 固定 fixture 产生重复用户名；
新建一次性数据库后 store/server 均通过，没有通过放宽约束规避失败。

主机没有 C 编译器，race 在已固定的 Go Debian 镜像执行。容器使用只读源码挂载、
非 root UID 和 `--network none`；PostgreSQL 仅开放本地测试端口。Compose 校验使用
临时目录和合成 secret，未修改站点 `.env` 或挂载真实凭据。所有截图使用合成账号。

## Docker 存储故障与待补检查

主要镜像和容器测试完成后，最终 bridge 构建依次返回
`/var/lib/docker/buildkit/containerd-overlayfs/metadata_v2.db: read-only file system`
和 `/var/lib/desktop-containerd/daemon/io.containerd.metadata.v1.bolt/meta.db: read-only file system`。
随后只读 `docker info` 也超时。未重启 Docker、清理全局缓存或修改其他容器。
测试 PostgreSQL 已成功停止；因 daemon 失去响应，其容器删除未确认。

最终 bridge 二进制已在主机成功构建，SHA-256 为
`d769f07ab8e01bc6971d08f4b28708d4c0ba45a9ff1d0be948abec6b78b2f740`。
相关 Go 单元测试、vet 和 shell 语法检查通过；最后新增的旧账号维护重授权路径尚需在 Docker
恢复后补做 race、bridge 镜像构建和 `scripts/test-antigravity-image.sh <镜像>`。
不得把先前基于旧 bridge 镜像成功的维护工具探针当作最终 bridge 镜像的验证。
CI 已加入独立旧 CPA 回滚构建和维护工具探针；上线前必须补齐上述检查。

真实上线仍必须逐账号完成 OAuth/迁移、强制刷新后重启、通过真实 Gateway Key 生成、
工具续轮、额度与账单核对；新模型以各账号实际可用目录为准。导入工具的
`awaiting_gateway_verification` 不代表生成验收成功。维护窗口排空、生产备份、实际切换
和携带最新 token 的反向迁移尚未执行。
