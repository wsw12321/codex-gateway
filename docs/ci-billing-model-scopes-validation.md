# 计费模型范围 CI 回归修复

2026-10-16 的 [CI 任务](https://github.com/wsw12321/codex-gateway/actions/runs/38935468038)
只有 PostgreSQL 集成测试失败；Go 检查、安全扫描、供应链检查和容器构建通过。
使用 CI 锁定的 PostgreSQL 17.6 镜像在临时数据库复现后，确认三个原因：

- 分组与现金模型范围审计新增 `[]string` 元数据，校验器只接受 `[]any`，
  返回 `unsupported metadata value []string`，使写入事务回滚。
- 计费测试只执行迁移，没有按网关启动顺序同步模型目录并初始化计费范围，
  因而现金或分组资金按预期被拒绝使用。
- 历史迁移测试在旧结构上调用当前分组读写方法，缺少 migration 0036 的字段。

修复精确支持字符串数组审计值，继续执行嵌套深度、敏感字段名和 32 KiB 大小检查。
计费测试显式声明部署模型目录并调用一次性初始化；历史迁移测试用旧结构构造数据，
在调用当前读取方法前补齐所需结构。

新增 PostgreSQL 回归验证空目录拒绝、旧范围初始化、现金与分组范围修改、
幂等回放、目录增加不自动扩展已保存范围、范围修改后的在途结算，以及订阅范围独立性。
原有资金、权限、结算断言及 CI 检查继续执行。

验证命令（Go 1.26.9，数据库必须为一次性测试库）：

```sh
go build ./cmd/gateway
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
TEST_DATABASE_URL='postgres://gateway:integration-only-password@127.0.0.1:55436/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/store ./internal/server
gofmt -l .
git diff --check
```
