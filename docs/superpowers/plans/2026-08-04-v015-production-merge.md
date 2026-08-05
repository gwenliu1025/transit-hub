# TransitHub v0.1.15 Production Merge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在官方最新 `v0.1.15` 基线上重建生产修复，完成全量审计，并向公开 fork 发布可复现分支与官方标签。

**Architecture:** 以官方 `origin/main` 的隔离 worktree 为唯一实现环境；旧工作区仅作为行为差异来源。已知修复采用测试先行逐项移植，审计结果写入独立中文报告，GitHub 发布使用 fork 分支和草稿 PR，不改写官方历史。

**Tech Stack:** Go 1.25、Vue 3、TypeScript、Vite、PostgreSQL、Redis、Docker、GitHub CLI

---

### Task 1: 固化基线与旧改动清单

**Files:**
- Create: `docs/audits/2026-08-04-production-change-inventory.md`

- [ ] 对比旧工作区未提交差异与官方 `origin/main` 对应实现。
- [ ] 将每项旧改动分类为“必须移植”“官方已吸收”“不再适用”“仅测试或运维”。
- [ ] 确认清单不包含 `backend/api.exe`、生产凭证或运行数据。
- [ ] 提交基线设计、计划和清单。

### Task 2: 重建北京时间成本数据源

**Files:**
- Modify: `backend/internal/modules/upstream/platform_service.go`
- Modify: `backend/internal/modules/upstream/platform_service_sub2api_groups_test.go`
- Modify: `backend/internal/modules/upstream/platform_service_key_usage_today_test.go`

- [ ] 先增加测试：dashboard 汇总返回旧日成本时，`TodayConsume` 必须取显式 `Asia/Shanghai` 当日 `/api/v1/usage/stats`。
- [ ] 运行定向测试并确认因仍读取 dashboard 值而失败。
- [ ] 增加单一北京时间日期助手，并让 Sub2API 指标与 key 明细使用相同日期。
- [ ] 运行 upstream 定向测试并确认通过。
- [ ] 提交该项修复。

### Task 3: 重建跨日陈旧成本过滤

**Files:**
- Modify: `backend/internal/modules/dashboard/metrics_service.go`
- Create: `backend/internal/modules/dashboard/metrics_service_purchase_test.go`

- [ ] 先增加测试：过期 `error` 和过期 `connected` 成本均排除，当日 `syncing` 成本保留。
- [ ] 运行定向测试并确认辅助行为尚不存在。
- [ ] 实现按目标北京时间日期过滤的纯函数。
- [ ] 实时卡片传入北京时间今天，午夜快照传入正在保存的昨天。
- [ ] 断言上游余额仍按原规则求和。
- [ ] 运行 dashboard 定向测试和竞态测试并确认通过。
- [ ] 提交该项修复。

### Task 4: 重建容错与会话同步修复

**Files:**
- Modify: `backend/internal/modules/upstream/service.go`
- Modify: `backend/internal/modules/upstream/service_key_usage_today_test.go`
- Modify: `backend/internal/modules/dashboard/service.go`
- Modify: `backend/internal/modules/dashboard/service_refresh_test.go`
- Modify: `backend/internal/modules/my_sites/service.go`
- Modify: `backend/internal/modules/my_sites/service_status_test.go`

- [ ] 对照官方新版本确认每项旧修复是否仍缺失。
- [ ] 对仍缺失行为先增加失败测试：单站点失败保留健康明细、有效 access token 可兜底、验证成功会话同步到工作区状态。
- [ ] 逐项实现最小修复并分别运行定向测试。
- [ ] 不迁移已被官方等价解决的旧代码。
- [ ] 提交该项修复。

### Task 5: 全量验证与缺陷审计

**Files:**
- Create: `docs/audits/2026-08-04-full-project-audit.md`

- [ ] 执行 `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./...`。
- [ ] 执行 `npm ci`、`npm run typecheck`、`npm run build`。
- [ ] 验证 Dockerfile 与 Compose 配置可解析。
- [ ] 对安全、计费/时区、并发/缓存、数据库迁移、前端和部署分别进行独立审计。
- [ ] 只将有代码证据或稳定复现的事项列为 bug，并合并重复发现。
- [ ] 提交中文审计报告。

### Task 6: 标签同步与公开发布

**Files:**
- Modify: `README_CN.md`（仅在需要说明 fork 差异与同步方式时）

- [ ] 检查提交范围和敏感信息扫描结果。
- [ ] 将官方全部 `v*` 标签原样推送到 `myfork`。
- [ ] 推送 `codex/v0.1.15-production-fixes` 到 `gwenliu1025/transit-hub`。
- [ ] 创建中文草稿 PR，正文包含基线、生产修复、验证结果和审计报告入口。
- [ ] 验证 fork 标签集合与 upstream 一致，并记录分支、提交和 PR 地址。
