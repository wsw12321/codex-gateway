# 本地价格覆盖

Gateway 的价格目录维护在仓库的
[`internal/config/data/pricing.json`](../../internal/config/data/pricing.json)，
构建时编译进镜像。部署不需要任何价格配置：新模型和价格调整随新版本镜像自动生效。

只有服务器需要偏离仓库默认价格时，才在本目录创建 `override.json`。Compose 把本目录
只读挂载到 Gateway 的 `/etc/codex-gateway/pricing`。该文件不存在时，Gateway 只使用内置目录。
本目录中除本说明和示例外的文件都被 `.gitignore` 忽略，不会提交，也不会进入镜像构建上下文。

## 能覆盖什么

- `usd_cny_rate` 与 `fx_as_of`：固定汇率及其日期，必须同时给出。
- `models`：内置目录已有模型的价格。规则如下：
  - 每个模型都要写出完整规则；
  - 结构必须与内置规则一致，只允许改价格。结构包括缓存写入方式、最大输入、长上下文阈值、服务层和上下文档位；
  - 所有价格必须大于 0。确需零价时，由 Owner 在“模型定价”中保存，并留有审计记录。

不能新增或删除模型，不能改 `catalog_as_of` 或兜底规则，也不能改内部零价的 `codex-auto-review`。
新模型应在仓库的 `pricing.json` 中定价。`schema_version` 必须与内置目录相同（当前为 `2`）。

格式示例见 [`override.example.json`](override.example.json)。它只是示例，Gateway 不会读取。

## 操作

工具通过 `scripts/pricing.sh` 运行。它使用 `.env` 选定的 Gateway 镜像，在没有网络、密钥和数据库的
独立容器中执行，不影响正在运行的 Gateway。先以内置规则为起点写一个候选文件，检查通过后再改名生效：

```sh
./scripts/pricing.sh show -builtin -model gpt-6-sol > deploy/pricing/override.candidate.json
chmod 0644 deploy/pricing/override.candidate.json
# 编辑价格；如需改汇率，同时加入 usd_cny_rate 和 fx_as_of
./scripts/pricing.sh check override.candidate.json
mv deploy/pricing/override.candidate.json deploy/pricing/override.json
./scripts/validate-compose.sh
./scripts/compose.sh up -d --no-deps --force-recreate gateway
```

注意事项：
- 不要把 `show` 的输出直接重定向到 `override.json`。shell 会先清空该文件，工具和自动重启的 Gateway 读到的就是空文件。
- 已有覆盖文件时，可用不带 `-builtin` 的 `show -model <ID>` 取出当前有效规则再修改。
- 价格只在进程启动时读取，修改后必须重建 Gateway 容器，但不需要重新构建镜像。
- `check` 会报告实际生效的覆盖内容、提示与内置价格相同的多余条目，并打印有效价格目录的 sha256；出错时以非零状态退出。不要在检查失败时重建容器。
- 多个 Gateway 实例必须使用相同的覆盖文件。启动日志中的 `pricing catalog loaded` 记录同一 sha256。
- 仓库升级后，如果覆盖文件引用的模型已被移除或结构已变化，`check` 和 Gateway 启动都会拒绝。这时按新的内置规则重写或删除对应条目。
- 覆盖的价格会一直保留。仓库更新官方价后，请复核覆盖条目是否仍需要。

价格优先级从高到低为：
1. 内部零价；
2. Owner 在“模型定价”保存的数据库覆盖价；
3. 本地覆盖文件；
4. 内置目录。

完整流程见 [部署与运维手册](../../docs/operations.md#用量价格目录)。
