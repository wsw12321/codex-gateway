# 模型批量权限、账号专属与群组额度

## 使用规则

模型权限页面可以同时选择多个模型和多个用户，一次启用或禁用全部组合。
“全部已有用户”和“新用户默认值”同样支持多个模型；默认值仍只在注册时复制。
批量写入在一个事务内完成，任何无效目标或缺失权限记录都会使整次操作回滚。

上游账号默认为共享。Owner 可以将账号改为专属，并选择至少一位用户。
用户可使用所有共享账号以及授权给自己的专属账号；分配算法只对这些账号计算
原有权重和近 24 小时费用差额。已有会话、重试和故障切换都重新校验授权；
失去权限的绑定会被撤销，正在执行的请求继续完成。权重为零仍仅阻止新分配。

群组页面支持创建、编辑、成员批量加入／移除和空群组归档。每个用户最多属于
一个群组，转组必须先移除。群组按全部成员请求的实际美元费用累计，和个人
日／周／月订阅及余额同时生效；个人余额仍能接续订阅。群组额度为零会阻止新请求。

固定周期分别是 24 小时、7 天和 31 天；自定义周期使用正整数天。周期起点留空时
创建即生效，指定未来起点时需等待开始。周期无限循环，剩余额度不结转。
仅修改金额时保留已用金额；修改周期或起点时关闭旧期并建立新期。

群组上限控制新请求的准入，不提前预估费用，也不中断已接收请求。因此在途请求
可能使已用金额超过上限。实际成本包含个人资金无法覆盖的部分，但群组不会产生
第二次个人扣款。请求接收时固定群组和周期，跨期、移除成员、重开周期和归档
均不改变其结算归属；重试不重复累计。模型目录查询不消耗群组额度。

## 接口与持久化

所有管理写入沿用 Owner、同源请求和近期身份验证检查，并记录审计。
群组写入使用 `operation_id` 与 `reason`，同一操作的重试返回原结果，参数冲突
返回 409。个人账务接口增加 `group` 摘要，不包含其他成员明细。

| 接口 | 用途 |
| --- | --- |
| `GET /admin/model-access/users?models=a&models=b` | 查询多个模型的用户权限 |
| `PUT /admin/model-access/users` | `models × user_ids` 或全部用户批量写入 |
| `PUT /admin/model-access/defaults` | 多模型注册默认值 |
| `PUT /admin/upstream-accounts/{id}/access` | `mode`、`user_ids`、`reason` |
| `GET/POST /admin/groups` | 列表／创建群组 |
| `GET/PUT/DELETE /admin/groups/{id}` | 详情／编辑／归档空群组 |
| `PUT /admin/groups/{id}/members` | `action: add/remove`、`user_ids` |

原单模型权限接口保持兼容。迁移 `0011_upstream_account_access.sql` 和
`0012_user_groups.sql` 位于 `internal/store/migrations`；既有账号默认共享，
既有用户无群组，既有账单不补扣。迁移后保留不可变的原始请求账单及群组周期引用。

群组额度不足或尚未开始时返回 HTTP 429、错误码 `group_quota_exceeded` 和
`Retry-After`。授权查询失败、身份无效或缺少可用上游账号时拒绝转发。

## 验证

以下检查已在开发验证中通过，数据库使用独立的临时 PostgreSQL 17 容器：

- `go test -count=1 ./...`
- `go test -race -count=1 ./...`（固定 Go 1.26.8 构建镜像，启用 CGO）
- `go vet ./...`，网关二进制构建及 `gofmt` 检查
- `TEST_DATABASE_URL=... go test -count=1 -tags=integration ./internal/store`
- `node internal/server/testdata/model_access_batch_browser.cjs`
- `node internal/server/testdata/groups_access_browser.cjs`
- gateway 与 codex-compat 镜像构建；兼容服务内置回归及选路竞态测试
- `scripts/test-sidecar-image.sh` 的隔离运行校验
- `scripts/validate-compose.sh`（使用临时副本、`.invalid` 域名和非生产密钥）

浏览器脚本使用 `PLAYWRIGHT_MODULE` 指定可选测试驱动，所有 HTTP 请求均被本地
合成数据拦截，不访问真实 OAuth 账号。截图覆盖桌面和移动端：

- [模型权限桌面](screenshots/model-access-desktop.png)、[移动端](screenshots/model-access-mobile.png)
- [群组桌面](screenshots/groups-desktop.png)、[移动端](screenshots/groups-mobile.png)
- [专属账号桌面](screenshots/upstream-exclusive-desktop.png)、[移动端](screenshots/upstream-exclusive-mobile.png)

回归覆盖原子批量回滚、授权变更撤销旧绑定、身份伪造和内部头泄漏、多人并发结算、
订阅／余额接续、超限在途请求、自然换期和手动重开、换组／归档后的延迟结算、
幂等重试、零额度、刷新失败，以及身份改变后的迟到响应和旧会话 401。

## 发布

gateway 与 codex-compat 必须配套更新。网关每次 Codex 生成请求检查兼容服务的
`upstream_account_access_v1` 能力；旧镜像缺少此能力会被拒绝，不能绕过专属限制。
此版本兼容镜像为 `codex-gateway-compat:v7.2.150-c77b1369-eaefd05c4c478e73-codex-only`。

本次仅构建与验证，没有部署、启动生产服务、修改现有 `.env` 或操作真实 OAuth。
直接侧车冒烟只检查元数据、模型目录和权限协议；真实 JSON／SSE 生成验证应使用
有效用户 API Key 经 Gateway 完成。
