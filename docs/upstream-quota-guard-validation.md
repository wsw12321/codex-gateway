# 上游账号剩余用量保险额度验证

Owner 设置一个全局“剩余用量保险额度”（默认 5%）。Codex 或 Claude 账号的 5 小时额度
或周额度剩余不超过该比例时，Gateway 将账号标记为“达到限额”，记录对应的额度刷新时间，
并停止向它分配请求；刷新时间过后 5 分钟自动恢复为“有剩余额度”。该状态由 Gateway
自己保存，独立于 CPA 的状态，在账号卡片上排在所有状态之前。Owner 可以手动切换状态。
Antigravity 没有 5 小时和周窗口，不受影响。

## 行为与安全影响

- 迁移 `0044_upstream_quota_guard.sql` 新增两张表，现有账号无需回填，没有记录即为
  “有剩余额度”。迁移只能前进。
  - `upstream_quota_guard_settings`：单行表，保存 `safety_percent`，取值 0–50 的整数，
    默认 5。
  - `upstream_quota_guard_states`：每个账号一行，保存最近一次 5 小时与周窗口读数、
    状态及其来源（自动或手动）、受限窗口、刷新时间 `reset_at`、恢复时间
    `restore_at`（刷新时间 + 5 分钟），以及自动检查计划。CPA 元数据同步不写这张表。
- 判定规则：某个窗口的剩余比例（100 − 已用）不超过保险额度时计为触发。刷新时间已过的
  窗口不计。多个窗口同时触发时，取最晚的刷新时间，保证所有触发窗口都已刷新才恢复。
  触发窗口缺少刷新时间时，先保持 1 小时，之后随请求重新查询。
- 恢复只看时间：`restore_at` 一过，查询即视为“有剩余额度”，无需定时任务，重启后依然有效。
- 选路：`/internal/{upstream,anthropic}-accounts/eligible` 与 `select` 排除处于“达到限额”
  的账号。CPA 每次选号（包括已绑定的对话和重试）都会先调用 eligible，并清除被排除
  账号的绑定，所以限额账号上的会话会换到其他账号；已开始的请求不受影响。
  选路接口、CPA 补丁和兼容镜像均无改动。所有账号都受限时，请求按现有逻辑失败，
  不会回退到付费 API。管理页“近 24 小时目标占比”不再计入受限账号。
- 手动切换只改变当前状态，下一次自动查询仍按实际额度判断。
  - 手动设为“达到限额”时，以最近读数中最早的未来刷新时间 + 5 分钟作为恢复时间；
    没有可用读数时一直保持，直到 Owner 手动恢复。
  - 手动恢复后，下一次自动检查至少等到上次自动检查 10 分钟之后。
  - Owner 或群组成员的额度查询结果会更新读数与检查计划，但不覆盖手动状态。
- 修改保险额度后，立即按每个自动状态账号的最近读数重新判断，手动状态不变。
  读数只会高估剩余额度，因此这一步不会提前限额。仍为“有剩余额度”的账号在下一次
  请求时重新查询，同样受 10 分钟下限约束。
- 新接口的中间件链与其他账号写入一致：同源、Owner、5 分钟内二次验证。
  - `PUT /admin/{upstream,anthropic}-accounts/{id}/quota-guard`：请求体只能是
    `{"limited": true|false}`。
  - `PUT /admin/upstream-quota-guard`：请求体只能是 `{"safety_percent": 0..50}`。
  - 账号列表新增每个账号的 `quota_guard` 与顶层 `quota_guard_safety_percent`；
    Antigravity 账号的 `quota_guard` 为 `null`，响应中也没有比例字段。
- 审计事件：
  - 自动查询 ChatGPT 额度：`upstream_account.quota_queried`，`via=quota_guard`，无执行人。
  - 状态变化：`upstream_quota_guard.limited`／`restored`，无执行人。
  - 手动切换：`upstream_quota_guard.manual_set`。
  - 修改比例：`upstream_quota_guard.settings_updated`。
- 日志只记录 provider 与固定错误码，不记录上游响应正文。Codex 额度查询沿用现有
  CPA 路径（`ResolveGatewayEgress`），走账号绑定的出口；Claude 只读取 CPA 已观测的
  快照，不额外请求 Claude。

## 查询频率

只有账号开始处理请求时（Gateway 从 CPA 响应头得知服务账号）才可能触发自动查询；
没有请求的账号不查询。

- **10 分钟下限**：每次自动查询前，先在数据库里原子登记 `auto_checked_at`，登记成功
  才查询。查询失败、重启或多实例运行时也同样有效。
- **自动查询并发**：同一 Gateway 进程最多同时进行 4 个；进程内缓存计划时间，
  未到期的请求不访问数据库。
- **间隔选择**：每次查询后，按各窗口的最小余量（剩余 − 保险额度，百分点）选间隔：

| 最小余量 | 下次查询间隔 |
| --- | --- |
| ≤ 10 | 10 分钟 |
| ≤ 20 | 15 分钟 |
| ≤ 35 | 30 分钟 |
| > 35 | 60 分钟 |

- **用量增长修正**：同一窗口周期内用量上涨时，按上涨速度估算余量耗尽的时间，间隔
  不超过该时间的一半。
- **取值范围**：最终间隔限制在 10–60 分钟。
- **受限账号**：下次检查时间设为恢复时间，恢复后随第一个请求查询。

## 界面

- **卡片状态行**：Codex 与 Claude 账号卡片的状态区第一个徽章是“有剩余额度”（绿色）
  或“达到限额”（红色），“最终分流”排在其后；展开详情后同样排在最前。
- **状态行下方说明**：
  - 受限时显示“5 小时额度 …刷新 · …自动恢复”，或“需手动恢复”。
  - 有余量时显示“5 小时额度剩余 X% · 周额度剩余 Y% · …查询”。
  - 手动状态带“手动设置”前缀。
- **切换按钮**：说明旁的按钮“设为达到限额”或“恢复有剩余额度”可切换状态。它不依赖
  CPA 同步，操作进行中锁定；服务端确认后再刷新列表。
- **保险额度设置**：工具栏下方的“剩余用量保险额度”在本地校验 0–50 的整数后保存。
  列表刷新不覆盖未保存的输入；Antigravity 页隐藏该设置。
- **筛选**：账号状态筛选新增“达到限额”。

## 验证

2026-11-04 在本机使用锁定的 Go 1.26.10 镜像、Node.js v24、一次性 PostgreSQL 17.6
（`deploy/images.lock.env` 中的镜像）及 Playwright 1.49.1（Chromium 1148）完成：

```sh
gofmt -l .                                   # 无输出
go vet ./...
go vet -tags=integration ./internal/store ./internal/server
go build ./...
go test -count=1 ./...
go test -race -count=1 ./...
TEST_DATABASE_URL='postgres://…' go test -count=1 -p 1 -tags=integration ./internal/store ./internal/server
scripts/ci-go-tool.sh github.com/securego/gosec/v2/cmd/gosec@v2.22.8 -- ./...   # 0 issues
node --test internal/server/testdata/*_test.cjs internal/server/testdata/model_identification_browser.cjs
PLAYWRIGHT_MODULE=/path/to/playwright SCREENSHOT_DIR=/tmp/shots \
  node internal/server/testdata/upstream_quota_guard_browser.cjs
```

`upstream_allocation_browser.cjs`、`upstream_account_alias_browser.cjs`、
`anthropic_accounts_browser.cjs` 与 `antigravity_accounts_browser.cjs` 也重新运行，均通过。

### 后端

- 判定规则测试 `TestEvaluateUpstreamQuotaGuard*`：
  - 剩余恰好等于保险额度即触发；0% 保险额度只在用尽时触发；两个窗口都触发时取较晚
    的刷新时间。
  - 已刷新的窗口不计；缺少刷新时间时保持 1 小时，但已知更晚的刷新时间优先。
  - 余量分档间隔正确；用量快速上涨时缩短间隔，但不短于 10 分钟；不同周期的读数
    不参与速度估算。
- `TestUpstreamQuotaGuardStateReadsExpiredLimitAsAvailable`、
  `TestUpstreamQuotaGuardNextAutomaticCheckHonorsFloor`、
  `TestSoonestUpstreamQuotaResetSkipsPastWindows`、`TestUpstreamQuotaGuardMigration`：
  覆盖读取时状态、下次检查时间、手动受限的刷新时间以及迁移约束。
- `TestRunUpstreamQuotaCheckFollowsClaimReadAndRecord`：
  - 登记失败、非 Codex/Claude 账号、读取失败时都等待 10 分钟，且不写入状态；
  - 未到期时沿用数据库计划；
  - 登记成功时按 provider 读取，并以自动读数写入。
- `TestNoteUpstreamQuotaUseTouchesDatabaseOnlyWhenDue`：首个请求只登记一次，之后未到期的
  请求和非法账号 ID 都不访问数据库。
- `TestUpstreamQuotaGuardSchedulerBoundsConcurrentChecks`：并发上限、计划时间、
  运行中不被覆盖，以及修改比例后清空缓存。
- `TestRecordUpstreamQuotaReadingIsNotAutomatic`、
  `TestUpstreamQuotaObservationKeepsReportedWindows`：Owner 和成员的查询结果以非自动读数写入，
  只保留带百分比的窗口。
- `TestUpstreamQuotaGuardHandlersRejectInvalidRequests`：两个写接口拒绝非法 ID、错误内容类型、
  非布尔值、多余字段、`null`、超出范围或含小数的百分比，以及查询参数。
- `TestUpstreamStatusRouteProtectsOriginRoleAndRecentVerification` 新增两个路由：缺少来源、
  跨站、无会话、成员、未二次验证或验证过期均被拒绝。
- `TestUpstreamAccountsFallsBackToDurableLocalSummary`：sidecar 不可用时，列表仍返回本地
  的 `quota_guard` 与保险额度。
- `TestUpstreamQuotaGuardPostgresIntegration`：
  - **登记**：10 分钟内重复登记被拒绝；Antigravity 账号不登记。
  - **选路**：自动限额后 eligible 和 select 都排除账号、目标占比为 0、恢复时间后重新可选，
    并写入审计。
  - **手动状态**：手动恢复的下次检查时间正确；非自动读数不覆盖手动状态，下一次自动读数会
    覆盖；没有读数时手动限额一直保持；有读数时按最早的刷新时间恢复。
  - **provider 隔离**：provider 不匹配时返回 404，Antigravity 被拒绝。
  - **修改比例**：只重新判断自动状态，不能超过 50%。
- 既有 store 与 server PostgreSQL 集成测试全部通过，包括选路、权限和群组账号池相关测试。

### 前端

`upstream_account_ui_test.cjs` 新增 6 个用例，全部 46 个用例通过：

- **状态与说明**：受限、有余量、手动无恢复时间、尚未查询的说明文本正确；徽章在
  摘要和详情中都排在最前；分配状态显示“达到限额 · 暂停新对话分配”；成员身份下
  按钮锁定。
- **Antigravity**：不显示状态与设置。
- **切换按钮**：sidecar 同步失败时仍可切换；请求进行中按钮显示“切换中…”，且其他
  写操作锁定；请求体为 `{"limited":true}`；确认后刷新列表。
- **Claude 路径与失败确认**：Claude 使用 `/admin/anthropic-accounts/…/quota-guard`；
  服务端确认不一致时不报告成功。
- **筛选**：“达到限额”筛选只显示受限账号。
- **保险额度设置**：从列表加载；刷新不覆盖未保存的输入；`51` 与 `4.5` 在本地拒绝，
  不发请求；保存一次，退出登录后清空。

模拟浏览器（`upstream_quota_guard_browser.cjs`）中：

- 首个徽章为“达到限额”，并显示刷新与恢复时间；
- 来回切换得到正确的请求体，状态随之更新；
- 本地拒绝 60%，保存 10%；
- “达到限额”筛选生效；
- 320、390、850、1100 px 宽度下无横向溢出，页面无脚本错误。

截图使用合成账号与模拟上游：

- [桌面上游账号](screenshots/upstream-quota-guard-desktop.png)
- [移动端](screenshots/upstream-quota-guard-mobile.png)
