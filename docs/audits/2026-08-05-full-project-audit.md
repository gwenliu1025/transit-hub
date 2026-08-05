# TransitHub 全项目审计清单

审计基线：`60c4aafef48fd822c40ebdb6d1c36581462f3626`（官方 `v0.1.15` 与生产修复合并后的分支）。

## 审计结论

- 已完成全仓库两轮独立发现、集中静态验证和攻击路径分析。
- 当前确认需要逐项修复的候选共 23 项。它们已具备明确的 source/control/sink 证据，但尚未连接真实上游做动态 PoC。
- 未部署生产，未读取或提交生产 `.env`、数据库、Redis、备份、token 或 API key。
- 统计面板“北京时间跨日后每日成本多算约 14 至 15 元”的问题属于此前生产修复基线，不与下面的待修复清单混淆。

## 部署阶段已修复缺陷

- `000018_connection_health_strategy_mode.sql` 原先假定旧策略表已有 `priority_mode`，
  但 `v0.1.7` 运行时创建的生产表没有该列，导致升级迁移确定性失败。
- 已先增加失败回归测试，再让迁移自身在兼容回填前补齐 `priority_mode`；
  使用生产 PostgreSQL dump 和 Redis RDB 的隔离升级演练通过后，发布为
  `v0.1.15-xiaoqian.2`。

## 待修复问题

| 优先级 | Family | 问题 | 关键位置 |
| --- | --- | --- | --- |
| P0 | `connection-health-model-discovery-ssrf` | 模型发现向未经过网络策略校验的 BaseURL 发送上游 Bearer key | `backend/internal/modules/upstream/probe_credentials.go` |
| P0 | `connection-health-probe-ssrf` | 真实探活向未经过网络策略校验的 BaseURL 发送 Bearer key | `backend/internal/modules/upstream/probe_credentials.go` |
| P0 | `dashboard-login-ssrf` | dashboard 登录允许私网/回环 `siteUrl`，并向目标发送管理员凭据 | `backend/internal/modules/upstream/platform_service.go` |
| P0 | `upstream-create-ssrf` | 创建上游站点允许环回和私网 URL | `backend/internal/modules/upstream/platform_service.go` |
| P0 | `upstream-update-ssrf` | 更新上游并重新登录允许私网/回环 `siteUrl` | `backend/internal/modules/upstream/normalizers.go` |
| P0 | `upstream-sync-ssrf` | 已保存上游的同步接口持续保留 SSRF 能力 | `backend/internal/modules/upstream/service.go` |
| P0 | `tickets-embed-ssrf` | 工单匿名会话可请求 localhost 或 DNS 内网目标 | `backend/internal/modules/tickets/service.go` |
| P0 | `smtp-destination-ssrf` | SMTP 配置可触发任意主机/端口 TCP/TLS 连接 | `backend/internal/modules/settings/service.go` |
| P0 | `telegram-proxy-ssrf` | Telegram 代理 URL 可使服务端连接任意代理目标 | `backend/internal/modules/settings/service.go` |
| P0 | `dingtalk-webhook-ssrf` | 钉钉通知测试可向任意地址发起服务端 POST | `backend/internal/modules/settings/service.go` |
| P0 | `wecom-webhook-ssrf` | 企业微信通知测试可向任意地址发起服务端 POST | `backend/internal/modules/settings/service.go` |
| P0 | `feishu-webhook-ssrf` | 飞书通知测试可向任意地址发起服务端 POST | `backend/internal/modules/settings/service.go` |
| P0 | `tickets-identity-source-binding` | 工单 embed token 未绑定工作区真实上游，可由假上游伪造身份 | `backend/internal/modules/tickets/service.go` |
| P0 | `preview-credential-exposure` | 短上游 key 在预览接口中被完整返回 | `backend/internal/modules/my_sites/connection_service.go` |
| P1 | `anonymous-user-directory` | `GET /api/users` 未认证即可枚举用户目录 | `backend/internal/modules/auth/handler.go` |
| P1 | `fixed-email-verification-code` | 公开注册使用固定且直接回显的验证码 | `backend/internal/modules/auth/service.go` |
| P1 | `unbounded-request-body` | 共享 JSON 解码器无请求体大小上限 | `backend/internal/shared/httpjson/json.go` |
| P1 | `unbounded-upstream-response` | 恶意上游 JSON 响应被无界读入内存 | `backend/internal/shared/httpjson/json.go` |
| P1 | `global-refresh-scheduler-isolation` | 工作区刷新策略覆盖进程全局调度配置 | `backend/internal/httpserver/server.go` |
| P1 | `remote-action-idempotency-race` | 远程创建检查早于创建记录，竞争请求可重复创建高权限资源 | `backend/internal/modules/my_sites/connection_service.go` |
| P1 | `tickets-session-revocation` | 工单 embed token 轮换不撤销已签发 Redis session | `backend/internal/modules/tickets/service.go` |
| P1 | `lottery-session-revocation` | 抽奖会话撤销非原子，清理失败时旧会话继续生效 | `backend/internal/modules/lottery/service.go` |
| P2 | `auth-login-bruteforce` | 密码登录缺少尝试限速或账户锁定 | `backend/internal/modules/auth/service.go` |

## 修复顺序

1. 先建立统一出站 URL/DNS/重定向策略，并覆盖 upstream、dashboard、health、webhook、SMTP、Telegram 和 tickets。
2. 再修复 embed/token/session/workspace 绑定和凭据最小化。
3. 然后补齐请求体/响应体预算、验证码/登录限速、租户调度隔离和远程操作幂等。
4. 每个问题单独补充回归测试和隔离环境 PoC；修复后重新运行 `go test ./...`、`go test -race`、`go vet ./...`、`go build ./...`、`npm run typecheck`、`npm run build`。

详细证据、验证收据、攻击路径和逐项中文 write-up 位于本次扫描目录的 `report.md`、`05-findings/`、`findings/` 和 `hardening/`。
