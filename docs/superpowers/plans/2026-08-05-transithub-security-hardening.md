# TransitHub 安全加固实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development（本会话采用）或 superpowers:executing-plans 逐任务执行。步骤使用复选框跟踪。

**目标：** 在不触碰线上生产环境的前提下，修复审计确认的 23 项安全问题，保持公网 HTTPS 上游统计、同步、探活、Ticket、SMTP/TLS 和 Telegram proxy 能力，并把验证后的代码推送到用户的 GitHub fork。

**架构：** 统一出站策略包集中处理公网 HTTPS 和公网协议目标；认证、会话、租户、资源限制和幂等问题按各自状态边界做最小修改。每个阶段先写失败回归测试，再实现并单独提交，最终用隔离生产数据演练和全仓测试验证。

**技术栈：** Go、PostgreSQL migrations、Redis、Vue/Vite、Docker Compose、Go net/net/http、Git worktree、GitHub draft PR。

---

## Task 0：基线与验证夹具

**Files:** Create `backend/internal/security/egress/policy_test.go` and `backend/internal/security/egress/test_helpers_test.go`; update `docs/audits/2026-08-05-full-project-audit.md`.

- [ ] **Step 1：** 在 `backend` 运行 `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./...`；在 `frontend` 运行 `npm ci`、`npm run typecheck`、`npm run build`。记录命令和退出码。
- [ ] **Step 2：** 写网络测试辅助函数：可注入 DNS 解析器，构造重定向服务，捕获 Authorization 是否跨域转发；不得连接生产地址。
- [ ] **Step 3：** 运行新增测试确认基线；提交 `建立安全加固验证基线`。

## Task 1：统一公网 HTTPS 与公网协议出站策略

**Files:** Create `backend/internal/security/egress/policy.go`、`backend/internal/security/egress/resolver.go`；修改 `backend/internal/modules/upstream/http_client.go`、`normalizers.go`、`backend/internal/modules/connection_health/model_discovery.go`、`probe_runner.go`、`backend/internal/modules/tickets/host_guard.go`、`backend/internal/modules/settings/service.go`、`smtp_sender.go`、`smtp_service.go` 及其相邻测试。

- [ ] **Step 1：** 先写失败的表驱动测试：`http://`、loopback、RFC1918、链路本地、IPv6 loopback、userinfo、DNS rebinding、重定向到私网必须拒绝；合法公网 HTTPS、合法公网 SMTP/TLS、合法公网 Telegram proxy 必须保留；跨来源重定向不得带认证头。
- [ ] **Step 2：** 运行 `go test ./internal/security/egress ./internal/modules/upstream ./internal/modules/connection_health ./internal/modules/settings ./internal/modules/tickets`，确认旧代码对恶意目标测试失败。
- [ ] **Step 3：** 实现 `ValidatePublicHTTPS`、`PublicHTTPSClient`、`PublicNetworkDialer` 和可注入 resolver；在 DialContext 前校验所有解析地址，禁止未经校验的重定向，统一超时和响应上限。
- [ ] **Step 4：** 接入 upstream 通用 HTTP client、model discovery、real probe、Ticket、Webhook、SMTP sender、Telegram proxy；保持合法目标的 API 响应和错误键。
- [ ] **Step 5：** 运行 `gofmt`、上述聚焦测试和 `git diff --check`；提交 `统一限制公网出站目标`。

## Task 2：认证、身份绑定、凭据与 session

**Files:** 修改 `backend/internal/httpserver/server.go`、`backend/internal/modules/users/handler.go`、`users/repository.go`、`auth/service.go`、`auth/repository.go`、`tickets/service.go`、`tickets/repository.go`、`lottery/service.go`、`my_sites/connection_service.go`、`my_sites/types.go` 及相邻测试。

- [ ] **Step 1：** 先写失败测试：匿名 `GET /api/users` 返回 401；跨工作区 Ticket token 被拒绝；token 轮换后旧 session 被拒绝；短 key 永远掩码；验证码随机、过期、一次性原子消费；重复消费失败。
- [ ] **Step 2：** 运行 `go test ./internal/httpserver ./internal/modules/auth ./internal/modules/users ./internal/modules/tickets ./internal/modules/lottery ./internal/modules/my_sites`，确认旧行为失败。
- [ ] **Step 3：** 保护 `/api/users` 并返回最小字段；验证码使用 `crypto/rand`、哈希存储、条件更新 compare-and-consume；保持统一错误响应。
- [ ] **Step 4：** 为 Ticket/Lottery session 增加版本或撤销时间校验；token 轮换与旧 session 失效使用同一事务语义；Ticket 身份只从 token 的持久化绑定读取。
- [ ] **Step 5：** 修复 `KeyPreview` 掩码；运行 gofmt、聚焦测试和 diff 检查；提交 `修复认证身份绑定与会话撤销`。

## Task 3：资源上限、限速、租户隔离和远端幂等

**Files:** 修改 `backend/internal/shared/httpjson/json.go`、`backend/internal/modules/upstream/http_client.go`、`backend/internal/modules/auth/service.go`、`backend/internal/httpserver/server.go`、`backend/internal/modules/settings/service.go`、`backend/internal/modules/my_sites/connection_service.go`、`my_sites/repository.go`、`settings/repository.go` 及相邻测试。

- [ ] **Step 1：** 先写失败测试：超大请求体返回 413；超大上游响应被限制；连续登录失败触发退避；不同租户策略互不改变 timer；相同操作 ID 并发只产生一个远端创建意图。
- [ ] **Step 2：** 运行 `go test ./internal/shared/httpjson ./internal/modules/upstream ./internal/modules/auth ./internal/httpserver ./internal/modules/settings ./internal/modules/my_sites`，确认旧行为失败。
- [ ] **Step 3：** 在共享 JSON 解码入口使用 `http.MaxBytesReader` 或等价 LimitReader；上游响应在读取前限制，不能只截断错误预览。
- [ ] **Step 4：** 按账号、IP、设备和时间窗口使用 Redis 计数器及指数退避；成功登录清除失败计数，错误键保持一致。
- [ ] **Step 5：** 让策略回调携带 `user_id/admin_account_id`，调度器按工作区管理 timer；远端创建前原子预留服务端生成的幂等键并保留补偿审计。
- [ ] **Step 6：** 运行 gofmt、聚焦测试和 diff 检查；提交 `限制请求资源并隔离租户操作`。

## Task 4：完整验证、隔离生产演练与审计更新

**Files:** 更新 `docs/audits/2026-08-05-full-project-audit.md`；创建 `docs/audits/2026-08-05-security-hardening-validation.md`。

- [ ] **Step 1：** 在 `backend` 运行 `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./...`；在 `frontend` 运行 `npm ci`、`npm run typecheck`、`npm run build`。
- [ ] **Step 2：** 在隔离网络验证公网 HTTPS、私网/环回/IPv6、DNS rebinding、重定向、SMTP/TLS 公网目标、Telegram proxy 公网目标、认证/session、资源限制、租户和幂等行为。
- [ ] **Step 3：** 用脱敏生产 PostgreSQL dump 和 Redis RDB 在无外网、无宿主端口的 Docker 网络中演练迁移、统计、登录、同步和健康接口；不连接生产。
- [ ] **Step 4：** 更新每项 finding 的修复状态和证明缺口；运行敏感信息扫描、`git diff --check`；提交 `记录安全加固验证结果`。

## Task 5：推送 GitHub（不部署生产）

- [ ] **Step 1：** 检查 `git status --short --branch`、`git diff v0.1.15-xiaoqian.2..HEAD --stat`、`git log --oneline v0.1.15-xiaoqian.2..HEAD`，确认没有 `.env`、dump、RDB、token 或 API key。
- [ ] **Step 2：** 执行 `git push -u myfork codex/security-hardening`。
- [ ] **Step 3：** 更新或创建草稿 PR，说明 23 项修复、兼容边界、所有验证命令、隔离演练结果和未触碰生产的事实；不创建生产发布标签。

