# 内置价格目录验证记录

日期 2026-11-18。价格目录改为编译进 Gateway 镜像，服务器只在需要偏离默认价时使用本地覆盖文件。
操作说明见[用量价格目录](operations.md#用量价格目录)、[本地价格覆盖](operations.md#本地价格覆盖)和
[迁移到内置价格目录](operations.md#迁移到内置价格目录)。

## 改动

- **价格源**：`deploy/pricing-v2.example.json` 移至 `internal/config/data/pricing.json`，内容未改，经
  `go:embed` 编译进二进制，是唯一的价格副本。以下内容同时删除：
  - 两份 env 示例中的单行副本；
  - `internal/config/data/cpa-v8-model-prices.json`；
  - 离线工具 `cmd/cpa-pricing-migrate`。
- **本地覆盖**：`deploy/pricing/override.json` 可选，只读挂载为 `/etc/codex-gateway/pricing/override.json`。
  - 只能修改已有模型的价格（完整规则，结构与内置一致，所有价格大于 0）和固定汇率。
  - 文件不存在时使用内置目录；挂载目录缺失、符号链接、非普通文件、无法读取、重复键或任何校验失败，一律拒绝启动。
- **旧变量**：`GATEWAY_USAGE_PRICING_JSON` 停用。
  - `validate-egress.py` 使每次 `scripts/compose.sh` 调用都拒绝；
  - `validate-compose.sh` 和 Gateway 也会拒绝；
  - 仅 `scripts/pricing.sh export-legacy` 可在旧行仍存在时导出其值。
- **工具**：新增 `gateway pricing show [-builtin] [-model ID] | check | migrate-env`，不读取其他配置，也不连接数据库。
  运维通过 `scripts/pricing.sh` 运行。它使用独立的 Compose 项目（`scripts/pricing-tool.compose.yml`）：
  - 与 Gateway 相同的镜像、用户和只读挂载；
  - 没有网络、密钥或数据库，不与运行中的 Gateway 的固定 IP 冲突。
- **启动日志**：`pricing catalog loaded` 记录以下内容：
  - 内置目录日期和模型数；
  - 覆盖文件及被覆盖的模型、是否覆盖汇率；
  - 有效目录 sha256，数值规范化后计算，`pricing.sh check` 打印同一值；
  - 与内置价格相同、不改变任何内容的覆盖条目只告警。
- **部署校验**：`validate-compose.sh` 删除了写死 20 个模型的 jq 价格校验器，改为以下检查：
  - Gateway 只有这一项只读绑定挂载，并带固定的覆盖路径变量；
  - Gateway（uid 10001）能进入目录、读取文件；
  - 目录和文件不可被其他用户写入；
  - 文件是 `schema_version` 为 2 的 JSON 对象。

  已审核模型集合、精确价格、正价格、无退役模型和 Gemini 审核名单改由 CI 中的 Go/Python 测试对内置目录断言。

价格优先级与数据库覆盖价不变：内部零价 > “模型定价”数据库覆盖价 > 本地覆盖文件 > 内置目录。本地覆盖不改变
结构，所以已有数据库覆盖价的结构标识不受影响。本次没有数据库迁移。

## 验证

```sh
gofmt -l .                                   # 无输出
go vet ./... && go vet -tags=integration ./internal/store ./internal/server
go test -count=1 ./...                       # 全部通过（见下方环境说明）
go test -race -count=1 ./internal/config ./cmd/gateway ./internal/billing
scripts/ci-go-tool.sh github.com/securego/gosec/v2/cmd/gosec@v2.22.8 -- ./internal/config ./cmd/gateway ./internal/billing
python3 -m unittest discover -s scripts/tests -p 'test_*.py'   # 104 个通过，1 个按条件跳过
```

各项结果：

- **gosec**：0 个问题，有 2 处带理由的 `#nosec`，其中包括读取覆盖文件的 G304。
  在本环境对整个仓库运行 gosec 会因内存不足被终止，因此只扫描了改动的包；全仓库扫描由 CI 完成。
- **`TestClientConfigurator`**：在本会话中会读取运行环境注入的 `ANTHROPIC_BASE_URL` 和
  `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST`。去掉这两个变量后通过，与本改动无关。

新增和改写的测试覆盖：

- **内置目录**：
  - 可部署（v2，无退役模型，Gemini 均已审核，付费价格为正，内部模型为零价，通过 `Config.Validate`）；
  - 原 `cpa_pricing_migration_test.go` 中的价格边界与退役模型测试已迁入 `pricing_default_test.go`。
- **覆盖解析与合并**：
  - 只修改所列价格，结构标识不变，不修改内置目录；
  - 拒绝的情况：未知字段、尾随内容、`schema_version` 缺失或不符、汇率与日期不成对、未知或内部模型、`null` 规则、
    跨 schema 字段、零价或负价、缺少或多出价格字段、阈值、缓存方式、服务层或上下文档位变化、重复键、超过 4 MiB。
- **加载**：
  - 空路径、文件不存在、挂载目录不存在；
  - 目录或符号链接冒充文件；
  - 超大文件；
  - sha256 可复现，数值写法不同时仍相同；
  - 空覆盖和冗余覆盖只告警；
  - `override.example.json` 能正常应用。
- **`migrate-env`**：
  - 与内置目录相同时无输出；
  - 价格差异写入覆盖；
  - 汇率仅日期不同时不写入；
  - 报告结构变化或零价、内置目录已删除、内置目录新增的模型，以及旧目录较早的情况；
  - 拒绝 v1 和非法输入。
- **`config.Load`**：
  - 使用内置目录；
  - 拒绝旧变量，且错误中不回显其值；
  - 应用覆盖文件，并拒绝非法覆盖和缺失的挂载目录。
- **CLI**：
  - 覆盖文件损坏时，`show -builtin -model` 仍可输出起点；
  - 出错时标准输出为空；
  - 拒绝非法参数。
- **Compose 与脚本**，用真实 `docker compose config` 渲染，Docker 守护进程用桩替代：
  - `.env` 中的旧行使 `compose.sh` 和 `pricing.sh migrate-env` 失败，`export-legacy` 按 Compose 的引号规则原样导出；
  - shell 继承的同名变量被忽略；
  - `pricing.sh` 只在 `codex-gateway-pricing` 项目中运行，只接受 `deploy/pricing` 内的文件名；
  - `validate-compose.sh` 拒绝以下情况：旧变量、改动的覆盖路径、可写或多余的挂载、缺失的挂载、与 CPA 共享挂载，
    以及非对象、`schema_version` 不是 2、损坏、不可读或其他用户可写的覆盖文件；合法覆盖文件可以通过。

手工端到端检查使用本地构建的 `gateway`：

- 把上一版 `deploy/env.example` 的单行价格交给 `pricing migrate-env`：标准输出为 0 字节，报告“无需覆盖文件”。
  也就是说，与仓库模板一致的服务器迁移时只需删除这一行。
- `pricing show -builtin` 与上一版 `deploy/pricing-v2.example.json` 在 `jq -S` 规范化后完全一致。
- 用示例覆盖文件运行 `pricing check`：报告 `gpt-5.4-mini` 与汇率 `7.1`，并打印 sha256。

## 未在本环境验证

本环境没有 Docker 守护进程，因此没有构建镜像，也没有实际运行 `scripts/pricing.sh`、真实的
`validate-compose.sh`（其 Squid/Caddy 解析步骤需要 `docker run`）或带只读挂载的 Gateway 容器。这些由 CI
的 Compose 策略检查、镜像构建和部署后的以下步骤覆盖：
1. 首次部署时运行 `./scripts/pricing.sh check`；
2. 确认启动日志 `pricing catalog loaded` 的 sha256 与之一致；
3. 确认“模型定价”没有结构冲突。

PostgreSQL 集成测试只做了编译检查（`go vet -tags=integration`），未连接数据库运行。
