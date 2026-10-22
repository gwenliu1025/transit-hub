# Fork 定制与上游更新维护

本页是当前 Fork 定制的维护导航；行为由对应源码和测试持有，不复制算法或历史安全矩阵。历史核对事实见[定制核对记录](audits/2026-10-21-fork-customization-audit.md)，本地执行状态由被 Git 排除的 `CURRENT_STATE.md` 持有；生产实例状态由各站点的运维 owner 持有。

## 当前定制与回归入口

下列定制已在历史提交中保存；更新作者代码时逐项回查直接实现和测试，而不是用上游整文件覆盖。表中后端模块简称以 `backend/internal/modules/` 为前缀。

| 维护责任 | 主要实现 owner | 直接回归入口 |
| --- | --- | --- |
| 北京时间今日成本、同步日过滤与 Key 明细日期 | `upstream/platform_service.go`；`dashboard/metrics_service.go` | `upstream/platform_service_key_usage_today_test.go`；`dashboard/metrics_service_purchase_test.go` |
| 管理员会话有效性与 my-sites 会话同步 | `my_sites/service.go`；`dashboard/service.go` | `my_sites/service_status_test.go`；`dashboard/service_refresh_test.go` |
| 旧库 priority_mode 升级兼容 | `backend/internal/database/migrations/000018_connection_health_strategy_mode.sql` | 同目录 `runner_test.go` |
| 公网目标、DNS 重验、重定向及站点协议 | `backend/internal/security/egress/policy.go`；upstream 与 connection_health 默认客户端 | `egress/http_policy_test.go`（位于 security 下）、`upstream/http_compat_test.go`、`connection_health/http_compat_test.go`、`backend/internal/httpserver/upstream_http_test.go` |
| 认证、验证码、登录限速与凭据最小展示 | `auth/`、`users/types.go`、`my_sites/connection_service.go` | `auth/security_test.go`、`users/types_test.go`、`my_sites/credential_preview_test.go` |
| Ticket/Lottery 嵌入身份绑定与撤销 | `tickets/`、`lottery/service.go` | 同模块身份、来源与会话撤销回归 |
| 请求/响应预算与工作区调度隔离 | `backend/internal/shared/httpjson/`、`backend/internal/httpserver/server.go`、`settings/`、`upstream/service.go` | 请求/响应上限、`settings/strategy_workspace_test.go`、`upstream/refresh_scheduler_test.go` |
| 真实对接幂等预留与补偿边界 | `my_sites/connection_service.go`、`repository.go`；迁移 `000019` | `my_sites/connection_idempotency_test.go`、`backend/internal/database/migrations/idempotency_migration_test.go` |
| New API 对接协议选择与错误边界 | `my_sites/connection_protocol.go`、`connection_service.go`、`handler.go`；前端 `GroupRatesView.vue` 与 `utils/connectionOptions.ts` | `my_sites/connection_protocol_test.go`；`frontend/tests/connections.test.ts`；[修复与验证记录](audits/2026-10-21-newapi-group-connection-fix.md) |
| 自然周/月、完整日结、昨日对比与跨日缓存 | `frontend/src/modules/admin/utils/dashboardMetrics.ts`；`dashboard/metrics_service.go`、`metrics_repository.go`；`upstream/daily_usage.go` | `frontend/tests/dashboardMetrics.test.ts`；dashboard日结/真实PG测试；`upstream/daily_usage_test.go`；[修复与恢复边界](audits/2026-10-22-dashboard-daily-settlement.md) |
| Fork 部署来源与发布版本一致性 | `backend/internal/config/config.go`、`scripts/verify-release.mjs`、`.github/workflows/release.yml` | `node scripts/verify-release.mjs` 与发布工作流 |

早期成本/会话变更见[生产改动迁移清单](audits/2026-08-04-production-change-inventory.md)，安全加固的历史范围与未证明项见[当时的安全验证](audits/2026-08-05-security-hardening-validation.md)，自然周期正式发布见[v0.1.16](releases/v0.1.16.md)。历史日期记录不作为本轮验证通过证明。

## HTTP 站点兼容调整

本次用户要求恢复可用的 HTTP 上游，并维护定制与后续升级记录。这是对 Fork 早期 HTTPS-only 站点限制的明确调整，不是移除公网访问边界。

- 站点业务使用统一的公网 HTTP/HTTPS 策略：URL 规范化、登录、管理员与上游指标读取、已保存站点同步、模型发现和探活共用该策略；不填写协议时仍默认 HTTPS。公网 IP 与合法非默认端口不因格式被拒绝。
- 公网目标校验仍在入口、实际请求、每跳重定向及拨号前执行；私网、环回、链路本地、特殊用途地址和混合 DNS 不因此开放。显式 HTTP 入口不授权 HTTPS 重定向降级；跨来源敏感 Header 与带请求体的重放由统一策略控制。
- 通知和匿名嵌入等非站点业务保留原 HTTPS-only 客户端；不因上游支持 HTTP 而跟随放宽。测试必须同时证明新站点入口可用与原 HTTPS-only 拒绝行为仍有效。
- 早先创建失败且没有 Session 的站点，需在编辑界面重新提交该站点认证信息并保存，成功建立会话后再同步；单纯刷新不会重新构造缺失凭据。凭据不写入维护记录或测试证据。
- 当前是源码修复，不自动修改发布版本号或生产实例。新的发布、安装与真实站点验收分别完成后才记为生效；不得复用旧 v0.1.16 的测试结果证明新代码。

## 更新作者版本时的最小流程

1. 核对当前工作树和现有分支，先辨明每个未提交文件的来源。个人 Fork 为 `origin`，作者为只读 `upstream`；只获取已有目标引用，记录当前 Fork、作者和共同祖先，不为更新擅自创建分支。
2. 做三方比较：共同祖先→Fork 定制、共同祖先→作者新增、当前工作树→HEAD。仅图表/文档更新与业务更新分开判断；对上表每个受影响 owner 检查作者是否已吸收、实现是否等价、测试是否仍覆盖。旧草稿不直接覆盖正式实现。
3. 在获准的现有分支完成最小整合；旧定制只有在需求明确被替代或已被作者等价吸收且验证成立时退出。冲突或尚未采用的旧草稿差异保留为待处理证据，不静默选边。
4. 执行后端 `go test -count=1 -timeout=3m ./...`、`go test -race -count=1 -timeout=3m ./...`、`go vet ./...`、`go build ./...`；前端 `npm run test:dashboard`、`npm run test:connections`、`npm run typecheck`、`npm run build`；根目录 `node scripts/verify-release.mjs` 与差异空白检查。真实 PostgreSQL 使用独立 `TEST_DATABASE_URL`，跳过不算集成通过；环境缺口不借生产依赖补测。
5. 独立检查同一最终来源和全部直接消费者后，按闭包显式选择文件形成可审查提交，记录测试及未验证边界。`.local/`、`_skill-cache/`、本机状态、凭据、临时二进制与恢复包不进入公开提交；不要使用整树盲目提交。
6. 发布继续以 `defaultAppVersion` 和现有发布一致性脚本/工作流为准，不套用其他项目的版本规则。提交、推送、发布、生产切换是不同动作；缺少对应授权时停在当前已完成层。撤销本次修复使用本次提交的反向补丁，不硬重置带有既有修改的工作树，不恢复旧数据库来撤回纯代码变更。
