# 群组管理验证

2026-11-05 为“群组额度”页面新增两项功能：已归档群组默认隐藏，以及按名称搜索群组。改名直接复用原有的群组编辑弹窗和 `PUT /admin/groups/{id}` 接口，只把入口改名为“编辑群组”。额度、周期、计费、成员和账号池的语义均不变，没有新增写接口，也没有新增数据库迁移。

## 行为

- **已归档群组默认隐藏**：群组列表默认只显示未归档的群组，列表上方的状态行会注明隐藏了几个，例如“显示 3 / 5 个群组 · 已隐藏 2 个已归档”。
  - 勾选“显示已归档群组”后，已归档群组以虚线卡片显示，也可以被搜索到。
  - 取消勾选时，如果当前选中的是已归档群组，会自动切换到筛选结果中的第一个群组；没有可选群组时隐藏详情区。
  - 刷新或保存后，若原先选中的群组仍在筛选范围内，则保持选中，否则改选筛选结果中的第一个群组。因此，在隐藏已归档群组时归档一个群组，选中项会切换到下一个未归档群组。
  - 全部群组都已归档时，列表会提示勾选复选框查看。
- **按名称搜索**：搜索框可按群组名称、全拼或首字母过滤，空格分隔的每个词都必须命中。
  - 匹配直接复用用户选择器的 `userMatchesSearch`，只比较名称和拼音字段，不比较群组 ID，避免短的搜索词命中所有 UUID。
  - 拼音由服务端在返回 `GET /admin/groups` 时根据名称计算（`pinyin_full`、`pinyin_initials`、`search_index`），复用用户搜索的 `pinyinForText` / `pinyinInitialsForText`，不写入数据库，因此改名后刷新列表即可按新名称搜索。
  - 搜索只影响列表，不改变当前选中的群组。没有匹配结果时，会注明是在未归档群组还是全部群组中搜索的。
- **改名**：在群组详情中点击“编辑群组”（原“编辑额度”），在原有弹窗里修改“群组名称”并填写操作原因。弹窗打开时预填当前设置。只修改名称时不会提交周期起点，周期和用量都保留；模型选择器未加载完成时不会提交 `model_ids`，原有模型范围保持不变。弹窗说明已改为“仅修改名称、额度或成员上限保留当前周期和累计”。已归档群组的“编辑群组”按钮不可用，服务端也会拒绝修改。
- **筛选状态**：搜索词和“显示已归档群组”的勾选状态只保存在当前页面内存中，退出登录或切换身份后清空，不写入浏览器存储。

## 已运行检查

环境：Go 1.26.10、Node.js 22.23.4、Chromium 153.0.8010.12、PostgreSQL 16。

```sh
gofmt -l .
go vet ./...
go test -count=1 ./...
TEST_DATABASE_URL='postgres://gateway@127.0.0.1:55432/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/store ./internal/server
node --test $(ls internal/server/testdata/*.cjs | grep -v _browser.cjs)
SCREENSHOT_DIR=/tmp/group-list node internal/server/testdata/group_list_browser.cjs
SCREENSHOT_DIR=/tmp/groups node internal/server/testdata/groups_access_browser.cjs
```

在云端会话中运行 Go 测试前，需要清除宿主注入的 `ANTHROPIC_BASE_URL` 等 Claude 环境变量，否则 `TestClientConfigurator` 会失败；这与本次改动无关。

覆盖范围：

- `TestGroupListIncludesDerivedPinyinSearchIndex`：列表返回由名称派生的拼音字段，已归档群组仍包含在接口结果中，由页面决定是否显示。
- `group_list_ui_test.cjs`：默认隐藏与勾选显示；名称、全拼、首字母和多词搜索；搜索不匹配群组 ID；空状态和状态行；隐藏已归档群组时自动改选；刷新时不再选中被隐藏的已归档群组；通过编辑弹窗改名时请求沿用原有额度、周期和期数，不包含 `starts_at` 和 `model_ids`；保存后列表刷新；已归档群组不能编辑；身份重置后清空筛选状态。
- `group_list_browser.cjs`：在 Chromium 中完成同样的流程，并检查 320 / 390 / 1440 px 下没有横向溢出。原有的群组、群组账号用量、邀请、控制台布局和主题浏览器回归继续通过。

## 合成数据截图

| 场景 | 截图 |
| --- | --- |
| 默认隐藏已归档 | [桌面](screenshots/group-management/group-list-desktop.png) / [手机](screenshots/group-management/group-list-mobile.png) |
| 显示已归档并搜索首字母 | [桌面](screenshots/group-management/group-list-archived-desktop.png) |
| 在“编辑群组”中改名 | [桌面](screenshots/group-management/group-rename-desktop.png) |
