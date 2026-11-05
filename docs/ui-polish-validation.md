# 控制台界面整体优化验证

2026-11-05 在[水源喵界面](shuiyuan-ui-validation.md)基础上统一控制台的视觉层级、控件状态和响应式布局。只改动样式、两处导航按钮图标和一处概览文案；API、身份验证、权限、计费、金额格式和页面状态语义不变。样式仍为同源 `style.css`，不引入外部资源、`url()`、内联样式或新的浏览器存储。

## 改动

- **统一设计层**：新增圆角、阴影、焦点环、悬停和选中色等主题令牌，浅色与深色分别定义。字体栈补充苹方、HarmonyOS Sans、MiSans、思源黑体等常见中文字体，并在系统缺少粗体字重时允许合成粗体，避免标题失去层级。
- **控件**：输入框和下拉框统一悬停与聚焦状态（主题色边框加柔和焦点环）；下拉框使用主题色箭头；字段不再继承外层标签的粗体；复选框和进度条使用主题色。禁用按钮统一为中性灰底，不再和可用的主按钮混淆。
- **折叠区域**：所有 `details` 摘要使用同一个旋转箭头，替换原来混用的原生三角。
- **卡片和表格**：卡片使用统一圆角和轻阴影；中文表头取消大写和宽字距；表格行、列表行和成员行增加悬停反馈；提示条改为左侧色条样式。
- **反馈层**：顶部通知改为卡片样式，左侧色条区分成功和错误，并带进入动画。对话框遮罩随主题调整明暗，打开时有过渡动画。系统开启“减少动态效果”时关闭这些动画。
- **外壳**：桌面顶栏吸顶并半透明模糊；导航项更紧凑，选中项有左侧指示条，列表底部渐隐，提示还可以继续滚动；连接状态改为胶囊标签；鼠标设备上的主题切换高度收窄，触屏设备仍保持 44px 触控目标。手机菜单按钮和关闭按钮增加图标，关闭按钮移到抽屉右上角。
- **概览**：欢迎区加入水纹渐变；全员统计合并成一条带分隔线的数据带；最近请求行可悬停。没有有效订阅时，“各订阅剩余”卡片不再重复显示“当前没有有效订阅”，第二行改为“可在套餐页面购买订阅”。手机端现金余额和订阅卡占满整行，管理金额卡改为单列，长金额不再从数字中间断开。
- **上游账号**：服务商切换改为分段控件。桌面端账号摘要使用固定列宽，多张卡片的状态、并发、费用和按钮列对齐。桌面端的剩余用量保险额度表单放进浅色底框。
- **额度与订阅**：扣费状态与开关放在同一行，优先级输入与保存放在下一行。管理操作折叠卡片与批量面板使用同一种卡片样式。手机端流水调整列宽，金额不再把负号单独折到上一行。
- **其他页面**：群组列表卡片修复内容被居中网格挤向右侧的问题，选中态增加左侧色条。模型权限批量表单改为“操作原因”单独一行，按钮分组排在下面。手机端模型定价把每个档位的四个价格排成 2 × 2 网格，页面高度约缩短 30%。

## 已运行检查

环境：Go 1.26.10、Node.js 22.23.4、Chromium 153.0.8010.12，截图字体为 Noto Sans CJK。

```sh
gofmt -l .
go vet ./...
go build ./cmd/gateway
git diff --check
go test -count=1 ./...
node --test $(ls internal/server/testdata/*.cjs | grep -v _browser.cjs)
for script in internal/server/testdata/*_browser.cjs; do
  SCREENSHOT_DIR=/tmp/ui-polish/$(basename "$script" .cjs) node "$script"
done
```

浏览器脚本默认会覆盖 `docs/screenshots/` 下的历史截图，因此回归时请把 `SCREENSHOT_DIR` 指向仓库外的目录。

在云端会话中，`TestClientConfigurator` 读取到宿主注入的 `ANTHROPIC_BASE_URL` 等 Claude 环境变量，会拒绝写入并失败；改动前也一样。清除这些变量后，Go 全量测试通过。

全部 `*_browser.cjs` 浏览器脚本都使用拦截的合成数据，其中 25 个通过，覆盖 320 / 390 / 850 / 851 / 1440 px 下无横向溢出、抽屉焦点约束、浅色和深色、移动端首屏能看到首个账号摘要等原有断言。`oidc_browser.cjs` 在第 286 行的同源 Cookie 断言上失败，改动前同样失败，与本次样式无关。`shuiyuan_theme_browser.cjs` 补充了 `/site/community`、`/admin/site` 和 `/admin/model-catalog` 合成响应，现在可以完整运行。

## 合成数据截图

| 页面 | 截图 |
| --- | --- |
| 概览 | [浅色桌面](screenshots/ui-polish/overview-owner-light-desktop.png) / [深色桌面](screenshots/ui-polish/overview-owner-dark-desktop.png) / [手机](screenshots/ui-polish/overview-owner-mobile.png) |
| 上游账号 | [桌面](screenshots/ui-polish/accounts-desktop.png) |
| 额度与订阅 | [桌面](screenshots/ui-polish/billing-desktop.png) / [手机](screenshots/ui-polish/billing-mobile.png) |
| 群组额度 | [桌面](screenshots/ui-polish/groups-desktop.png) |
| 模型定价 | [手机](screenshots/ui-polish/model-pricing-mobile.png) |
| 导航 | [手机抽屉](screenshots/ui-polish/navigation-mobile.png) |
| 登录 | [浅色桌面](screenshots/ui-polish/login-light-desktop.png) / [深色手机](screenshots/ui-polish/login-dark-mobile.png) |
