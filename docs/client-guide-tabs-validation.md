# 使用指导标签页验证

2026-11-17 把管理界面“使用指导”改为「模型供应商 × 操作系统」两组标签页。本次只改控制台页面、测试和文档，Gateway 接口、配置器 `configure-client.cjs` 和数据库都没有变化。

## 行为

- **布局**：公共准备卡片位置不变，下面是两组标签页：
  - 模型供应商：OpenAI Codex、Claude、Google Antigravity；
  - 操作系统：Windows、macOS、Linux。

  排查说明和“查看使用统计 / 管理 API Keys”移到页面末尾的“排查与后续操作”卡片，三家供应商共用。
- **各供应商的客户端**：

  | 供应商 | CLI | 桌面端 | IDE 扩展 |
  | --- | --- | --- | --- |
  | OpenAI Codex | Codex CLI | Codex 桌面端，与 CLI 共用 `CODEX_HOME`；标注“Windows 已验证” | VS Code Codex 扩展，与 CLI 共用配置 |
  | Claude | Claude Code，按系统给出官方 CMD 或 Shell 安装命令 | 推荐第三方工具 CC Switch，只链接 <https://ccswitch.io>，由用户自行配置 | Claude Code 扩展，执行配置命令后勾选 `claudeCode.disableLoginPrompt` |
  | Google Antigravity | agy CLI | 不提供（管理员决定） | 不提供 |
- **系统标签**：
  - 只切换带 `data-guide-os` 的安装命令和路径说明。配置命令在三个系统上相同，不重复。
  - 初始系统按浏览器平台判断：匹配 `windows|win32|win64` 为 Windows，不用会误中 “Darwin” 的 `win`；`mac|iphone|ipad` 为 macOS；`linux|x11|cros` 为 Linux；都不匹配时用 Windows。
- **状态**：
  - 所选标签只保存在页面内存，`renderState` 刷新后保持不变。
  - 不使用 `localStorage`、`sessionStorage` 或 URL hash。
- **无障碍**：两组标签都按 WAI-ARIA tabs 实现，有 `role="tablist"`、`aria-selected` 和 roving `tabindex`。方向键循环切换，Home / End 跳到首尾，焦点移动时立即激活。
- **移动端**：两组标签各占一行；供应商标签分为三等宽列，文字可换行；页面不出现横向滚动。

## 已运行检查

环境：Go 1.26.11、Node.js 22.23.4、预装 Playwright 和 Chromium 153.0.8010.12。

```sh
gofmt -l .
go vet ./...
go build ./cmd/gateway
git diff --check
go test -count=1 ./...
node --test $(ls internal/server/testdata/*.cjs | grep -v _browser.cjs)
python3 -m unittest discover -s scripts/tests -p 'test_*.py'
node internal/server/testdata/agy_guide_browser.cjs
SCREENSHOT_DIR=<临时目录> node internal/server/testdata/anthropic_accounts_browser.cjs
node internal/server/testdata/browser_handoff_browser.cjs
```

以上全部通过。运行 Go 测试和 Node 测试前，需要清除云端会话注入的 `ANTHROPIC_BASE_URL` 和 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST`，否则配置器测试会把它们当成冲突设置。这与本次改动无关。

`shuiyuan_theme_browser.cjs` 在本次改动前后都因为缺少 `GET /admin/model-routes` 的测试数据而失败，与本次改动无关，没有处理。

新增或扩展的测试：

- `internal/server/assets_test.go`：
  - 要求页面包含：两组标签页、三个面板、新增卡片和代码块 ID，以及 CC Switch 官网、`claudeCode.disableLoginPrompt` 和 winget 命令；
  - 禁止出现 `guide-agy-desktop` 和 `guide-agy-ide`；
  - 新增 `TestGuideTabsAndCopyTargetsAreConsistent`，检查每个复制按钮指向的元素存在，`data-guide-os` 只取三个合法值，每家供应商和每个系统各有唯一的标签与面板。
- `internal/server/testdata/guide_ui_test.cjs`（由 `TestGuideDashboardUI` 调用）：用真实 `index.html` 的标签和面板属性生成模拟 DOM，检查以下内容：
  - 系统识别：Windows、macOS（包括 “Darwin” 字样）、Linux、ChromeOS、无法识别和没有 `navigator` 的情况；
  - 初始 HTML 与默认选择（Codex、Windows）一致，页面加载时不会闪动；
  - 供应商和系统切换，以及传入未知名称时不改变选择；
  - 方向键、Home、End 和无关按键；
  - `renderGuide()` 保留选择，并生成三条配置命令。
- `internal/server/testdata/agy_guide_browser.cjs`：
  - 在 Chromium 中逐一切换 3 个供应商 × 3 个系统，检查应显示和应隐藏的卡片与安装命令，并点击每个可见的复制按钮核对剪贴板内容；
  - 检查键盘导航、刷新数据后选择保持不变、浏览器存储为空；
  - 在 390 px 宽度下，三个供应商面板都不出现横向溢出；
  - 继续用原有方式校验三条配置命令。
- `internal/server/testdata/anthropic_accounts_browser.cjs`：先点击 Claude 标签再检查 Claude Code 卡片。指导页截图改为对指导区域截图，避免吸顶标题栏和跳转链接盖住内容。

另外用 Chromium 在深色主题下单独渲染了标签区域和 CC Switch 卡片，检查对比度和边框。

## 截图

- [桌面：Codex · Windows](screenshots/usage-guide-desktop.png)
- [桌面：Claude · Linux](screenshots/claude-guide-desktop.png)
- [桌面：agy CLI · Windows](screenshots/agy-guide-desktop.png)
- [手机：Codex · Windows](screenshots/usage-guide-mobile.png)
- [手机：Claude · Linux](screenshots/claude-guide-mobile.png)
- [手机：agy CLI · Linux](screenshots/agy-guide-mobile.png)

## 未验证

- **已验证**：只有 Codex 桌面端在 Windows 上改写 `config.toml` 接入，由管理员实测。
- **未用真实客户端验证**，页面内容依据各官方文档编写：
  - macOS 和 Linux 上的 Codex 桌面端；
  - Codex IDE 扩展；
  - Claude Code IDE 扩展（`claudeCode.disableLoginPrompt` 与共用 `settings.json` 的方式）；
  - CC Switch。CC Switch 是第三方工具，页面只提供官网链接和需要填写的值。
- **未在本次环境运行**：各客户端的官方安装命令和下载链接。
