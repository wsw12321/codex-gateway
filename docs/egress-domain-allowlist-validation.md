# 成员节点域名白名单验证

## 行为

- 成员出口服务器默认只接受公网 IPv4。Owner 在“出口服务器”页的“成员节点域名白名单”（默认折叠）维护允许的域名，
  每条为精确域名或 `*.` 子域名通配，不限端口，最多 256 条。
- 迁移 `0058_egress_domain_allowlist.sql` 新增 `egress_allowed_domains`。
- `GET/PUT /admin/egress-allowed-domains` 仅 Owner 可用，写入需近期验证和 Origin 检查，并写审计事件
  `egress_allowed_domains.replaced`。
- 成员的 `/admin/me/egress-servers` 不返回白名单；成员页面不显示白名单。成员保存节点时，Handler 先检查一次，
  `SaveEgressServer` 在持有出口锁的事务内再检查一次；不允许时返回 `egress_domain_not_allowed`，提示不含白名单内容。
- 修改白名单与节点写入共用 `egress_state` 行锁；仍被成员节点使用的域名不能移除（409 `egress_domain_in_use`）。

## 安全影响

成员可用域名后，域名在出口服务拨号时解析，DNS rebinding 风险由 Owner 加入白名单时的判断承担，见
[威胁模型](threat-model.md)。白名单为空时行为与之前相同。内部后缀（`localhost`、`.local`、`.internal`、
`.onion`、`.arpa`）、IP、协议、端口和路径不能加入白名单。

## 验证命令

环境：Go 1.27.4（本机，高于锁定的 1.26.11）、Node.js 24、PostgreSQL 17（一次性本地数据库）。

```sh
gofmt -l .
go vet ./...
go test -count=1 ./...
TEST_DATABASE_URL='postgres://…/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/store ./internal/server
```

- `internal/egress/domain_allowlist_test.go`：规范化、通配匹配（不含根域、不误配相似后缀）、拒绝内部后缀与 URL 形式。
- `TestEgressAllowedDomainsPostgresIntegration`：精确与通配域名在任意端口可保存，根域与未列出域名被拒，
  移除使用中的域名被拒且白名单不变。测试只清理自己的条目，可与其他包并行。
- `TestTenantProvidersHTTPPostgresIntegration`：成员读写白名单返回 403，成员列表不含白名单，
  被拒提示不泄露域名，Owner 可读取，非法条目返回 400。
- Playwright 合成数据截图：Owner 面板默认折叠、亮/暗色和 390 像素宽度布局、编辑计数与保存；成员页不显示面板。
