# 站点设置（交流群与充值说明）验证

Owner 在新增的“站点设置”页编辑两张卡片：登录页和概览页的交流群卡片，以及
“额度与订阅”页的充值卡片。网关不接入支付、不核验付款；用户按卡片提示付款后，
Owner 仍在“额度与订阅”为其充值。新增迁移 `0040_site_settings.sql`，CPA 及选路
回调不变。

## 行为与安全影响

- 迁移新增单行表 `site_settings`（文字字段与 `revision`）和 `site_images`
  （固定三个槽位：`community_qr`、`recharge_wechat`、`recharge_alipay`）。两张卡片
  默认关闭，现有页面在 Owner 开启前不变。两表都不引用 `users`，删除用户的流程
  不受影响。操作者记录在审计事件中。
- 路由与中间件：
  - `GET /site/community`、`GET /site/images/community_qr`：公开，仅在交流群开启时
    返回内容。
  - `GET /admin/site`、`GET /admin/site/images/{slot}`：登录会话。关闭的卡片及其
    图片对 Member 返回空或 404，Owner 可预览。收款码没有公开路由。
  - `GET /admin/site-settings`：Owner 会话。
  - `PUT /admin/site-settings`、`PUT|DELETE /admin/site-settings/images/{slot}`：同源、
    Owner、近期二次验证，与其他 Owner 写接口一致。
- 文字整体替换，必须携带读取时的 `revision`。每次写入（含图片）先锁定并递增
  该单行版本号，版本不符返回 `409 site_settings_conflict`，避免两个标签页互相覆盖。
  页面上传图片后，仅当返回的文字与表单载入时一致才采用新版本号，否则提示刷新。
- 校验（服务端与表约束双重执行）：标题 ≤40 字、说明 ≤500 字、客服 ≤80 字、
  补充说明 ≤1000 字；单行字段不含换行，所有字段拒绝控制字符；QQ 群号为 5–12 位
  且首位非 0；加群链接必须以 `https://` 开头、有主机且不含账号密码或空白。开启
  交流群时至少填写群号或链接（`site_community_contact_required`）。
- 图片：上传体 ≤5 MiB，仅 `image/png`／`image/jpeg`，且声明类型须与解码格式一致；
  宽高 ≤4096、总像素 ≤1200 万。网关解码后重新编码（PNG 最佳压缩，JPEG 质量 92），
  丢弃 EXIF（含定位）、注释及附加数据，存储前计算 SHA-256。响应固定
  `Content-Type`、`Content-Disposition: inline`、`nosniff`、`default-src 'none'` CSP 与
  ETag，并支持 304。带 EXIF 旋转标记的照片按原始像素方向显示，应上传截图或已
  转正的图片。
- 缓存：登录页接口无需会话，因此读取 30 秒快照，图片字节按 SHA-256 缓存。本实例
  写入后立即替换快照；多实例部署时其他实例最多 30 秒后同步，较慢的旧读取不会
  覆盖较新快照。
- 审计：`site.settings_updated`（版本号与两个开关）、`site.image_updated`（槽位、
  SHA-256、字节数）、`site.image_deleted`，与写入同一事务提交。
- 前端：所有文字均以 `textContent` 渲染，链接只接受 `https://` 并以
  `target=_blank rel="noopener noreferrer"` 打开，图片只接受以单个 `/` 开头的同源
  路径。充值卡片只在本人视图显示当前登录用户名，Owner 查看他人账务时隐藏。
  登出或会话失效时清空卡片与设置页。
- 回滚：迁移只新增表，但旧二进制会因未知迁移保护拒绝启动，按
  [运维手册](operations.md)的停写回退流程处理。

## 验证

2026-11-03 在本机使用锁定的 Go 1.26.10 镜像、Node.js v24.21.0、
一次性 PostgreSQL 17.6 及 Playwright 1.49.1（Chromium）完成：

```sh
gofmt -l .                                   # 无输出
go vet ./... && go vet -tags=integration ./...
go build ./cmd/gateway
go test -count=1 ./...
go test -race -count=1 ./...
TEST_DATABASE_URL='postgres://…' go test -count=1 -tags=integration ./internal/store ./internal/server
go run github.com/securego/gosec/v2/cmd/gosec@v2.22.8 ./...           # Issues: 0
go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...                  # 代码未受影响
node --test internal/server/testdata/*.cjs   # 不含 *_browser.cjs；335 通过，1 项需本机 Codex 的用例跳过
PLAYWRIGHT_MODULE=/path/to/playwright SCREENSHOT_DIR=/tmp/shots \
  node internal/server/testdata/site_settings_browser.cjs
```

Go 镜像内没有 Node，`TestSiteSettingsDashboardBehavior` 在容器中跳过，因此
Node 回归在宿主机直接运行。

### 后端

- `TestValidateSiteSettings`、`TestValidSiteImageRequiresMatchingDigestAndBounds`：
  字数、换行、控制字符与行分隔符、群号格式、`http:`／`javascript:`／大写协议／带
  账号密码／无主机／含空格链接；图片槽位、SVG、摘要不符、空数据、尺寸越界。
- `TestSiteRoutesRequireSessionOwnerAndWriteVerification`：匿名、Member、外部来源、
  未二次验证的请求均不触达存储。
- `TestSiteContentHidesDisabledSectionsAndPaymentCodesFromAnonymousVisitors`：关闭时
  公开接口返回 `null`、群二维码 404、Member 读不到收款码；开启后公开响应不含任何
  充值字段，收款码没有匿名路由。
- `TestSiteImagesAreServedAsInertCacheValidatedContent`：响应头、304、缓存命中后
  不再读库；Owner 可预览未开启槽位，未知槽位 404。
- `TestSiteSettingsUpdateNormalizesAttributesAndRefreshesCache`：去除首尾空白、
  CRLF 转 LF，记录用户、会话与请求 ID；写入立即刷新公开快照；过期版本 409、
  缺少联系方式与脚本链接 400、未知字段拒绝。
- `TestSiteImageUploadRejectsUnsafeInputAndReencodes`：SVG 415、HTML 伪装 PNG、
  PNG 声明为 JPEG、超宽 400、超限 413；PNG 末尾附加的 `<script>` 和 JPEG 的 EXIF
  APP1 段在重新编码后消失，摘要与尺寸正确；删除不存在的图片 404。
- `TestSiteSettingsPostgresLifecycle`：过期版本回滚且不递增；两个写入方持同一版本
  并发提交，恰好一个成功；图片写入递增版本、保留文字；两条审计记录；表约束拒绝
  `http:` 链接、短群号、未知槽位、SVG 类型和第二行设置；重复删除返回未找到且
  不递增版本。
- `TestEmbeddedMigrationsCoverRequiredSchema` 迁移计数更新为 40，并要求两张新表。

### 前端

`site_settings_ui_test.cjs` 的 6 项回归覆盖：交流群卡片拒绝 `javascript:` 链接和
外站、协议相对、`data:` 图片，HTML 以纯文本显示；关闭后清空。充值卡片只在本人
视图显示用户名、客服与群号，Owner 切换到他人时隐藏，重置后清空图片。迟到的内容
响应不会覆盖新登录。保存提交载入时的版本号及全部去空白字段，开启交流群但缺少
联系方式时不发请求。图片写入仅在文字未变时采用新版本号，否则提示刷新并锁定
移除按钮。上传前在本地拒绝 SVG 和超过 5 MiB 的文件，请求体为原文件并带其类型。

模拟浏览器实际执行：Owner 填写两张卡片并保存，上传三张合成的仿二维码图片（随机
噪点，非真实群码或收款码）；未登录访客在登录页看到交流群卡片；Member 在概览页
看到交流群卡片，在“额度与订阅”看到自己的用户名、固定提示、客服说明和两张收款码，
“套餐”页显示充值提示。桌面 1440×1000、移动端 390×844，均无横向溢出：

- [登录页（桌面）](screenshots/site-settings/site-community-login-desktop.png)、
  [登录页（移动）](screenshots/site-settings/site-community-login-mobile.png)
- [概览页交流群卡片](screenshots/site-settings/site-community-overview-desktop.png)
- [充值卡片（桌面）](screenshots/site-settings/site-recharge-billing-desktop.png)、
  [充值卡片（移动）](screenshots/site-settings/site-recharge-billing-mobile.png)
- [站点设置（桌面）](screenshots/site-settings/site-settings-desktop.png)、
  [站点设置（移动）](screenshots/site-settings/site-settings-mobile.png)

## 仍需目标环境验收

- 用真实 QQ 群二维码和收款码各上传一次，用手机扫描重新编码后的图片，确认可识别。
- 生产部署后确认 Cloudflare 未缓存 `/admin/site/images/*`（响应为
  `private, no-cache`）。
