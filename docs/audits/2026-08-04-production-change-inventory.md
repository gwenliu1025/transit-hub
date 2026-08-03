# 生产改动迁移清单

基线：官方 `origin/main@9ee10de`（包含 `v0.1.15@7ba0454`）。

## 必须迁移

| 范围 | 生产问题 | v0.1.15 状态 | 迁移方式 |
|---|---|---|---|
| Sub2API 今日成本 | `/usage/dashboard/stats` 受上游服务器日界线影响，凌晨混入前一日成本 | 仍直接读取 `today_actual_cost` | 改为显式北京时间日期的 `/usage/stats`，补时区测试 |
| 仪表盘外层成本 | 同步失败站点保留旧 `TodayConsume`，跨日继续进入今日成本和净利润 | 仍无条件汇总缓存值 | 按 `LastSyncedAt` 的北京时间日期过滤，实时与快照共用规则 |
| Key 明细日期 | per-key `/usage/stats` 使用进程本地日期 | 仍使用 `time.Now().Format` | 复用北京时间日期助手 |
| 工作区会话兜底 | Refresh Token 失效但 Access Token 仍有效时错误退出 | 仍直接返回 `ErrorAdminOnly` | refresh 失败后验证现有凭证，验证成功则继续使用 |
| 管理会话同步 | 状态校验或后台刷新成功后未同步最新会话到 `my_site_states` | 仍缺少两处同步 | 状态验证成功及后台刷新成功后调用 `SyncAdminSession` |

## 官方已吸收

| 范围 | 官方 v0.1.15 实现 |
|---|---|
| 多站点 Key 明细部分失败 | 已返回健康站点结果并携带 `KeyUsageCollectionError`，不再整体丢弃成功结果 |
| 下钻弹窗部分失败提示 | 已在响应中包含 `partial`、`failedSites`、`totalSites` |
| Sub2API Key 鉴权 | 已包含专门的 key auth 回归测试 |

因此不迁移旧版本中“完全吞掉失败错误”的实现，保留官方更完整的部分失败语义。

## 不再适用或不发布

- `backend/api.exe`：本地临时构建产物，不进入 Git。
- 生产 Compose、`.env`、数据库、Redis、Cookie、Token 和备份：只用于运行验证，不进入公开仓库。
- 旧 `v0.1.7` 完整文件：不得覆盖 `v0.1.15`，只迁移经过重新验证的行为。

## 验证边界

- 成本过滤必须排除跨日的 `error` 和 `connected` 样本。
- 当日 `syncing` 样本必须继续计入。
- 上游余额不按日期过滤。
- 所有新行为必须在官方 `v0.1.15` 结构上重新执行红绿测试。
