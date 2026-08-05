# TransitHub 安全加固验证记录

日期：2026-08-05（Asia/Shanghai）

目标分支：`codex/security-hardening-implementation`

生产约束：本次只在隔离工作树验证，没有连接生产 HTTP/HTTPS、PostgreSQL、Redis、Docker 或生产主机；生产镜像 `xiaoqian/transithub:v0.1.15-xiaoqian.2`、生产标签和生产配置未修改。

## 验证命令

后端：

- `go test ./...`：通过。
- `go test -race ./...`：通过。
- `go vet ./...`：通过。
- `go build ./...`：通过。

前端：

- `npm ci`：通过。
- `npm run typecheck`：通过。
- `npm run build`：通过。
- `npm audit --audit-level=moderate`：通过，0 个漏洞；只应用了无 `--force` 的小版本升级。

安全聚焦覆盖：

- 公网 HTTPS/DNS/重定向、userinfo、环回/私网/链路本地拒绝：`go test ./internal/security/egress` 通过。
- Ticket 来源绑定、会话撤销、Lottery 会话失效：对应 tickets/lottery 聚焦测试通过。
- 认证验证码、登录限速、JSON 请求/响应体上限、413：对应 auth/httpserver/httpjson/upstream 聚焦测试通过。
- 多租户刷新 timer：settings/upstream/httpserver 聚焦测试通过。
- 远端创建原子预留和并发幂等：my_sites/migrations 聚焦测试通过。

## 23 项状态矩阵

| # | 审计问题 | 状态 | 代码与证据 | 未证明部分 |
|---:|---|---|---|---|
| 1 | 模型发现 BaseURL SSRF | fixed | `security/egress` + model discovery 安全构造器和响应上限测试 | 未连接真实网络 |
| 2 | 真实对接 check-then-act 竞态 | fixed | `000019` 迁移、数据库行锁预留、并发只创建一次测试 | 未连接真实 PostgreSQL 多实例 |
| 3 | dashboard 登录 siteUrl SSRF | fixed | upstream 默认公网 HTTPS 客户端和 URL 规范化 | 未做部署网络动态复现 |
| 4 | 企业微信 webhook SSRF | fixed | settings 默认安全 HTTP 客户端，重定向重新校验 | 未向真实企业微信发送请求 |
| 5 | 匿名 `/api/users` 用户枚举 | fixed | 路由认证、最小用户 DTO、敏感字段测试 | 未做真实反向代理验证 |
| 6 | 更新上游站点 SSRF | fixed | `PlatformService.NormalizeURL` 与安全客户端双重控制 | 未做 DNS rebinding 动态演练 |
| 7 | 全局刷新策略跨租户影响 timer | fixed | 回调携带 user/workspace，启动加载全部策略，多租户 timer 测试 | 未连接真实数据库验证启动数据 |
| 8 | Ticket embed 身份源伪造 | fixed | token 归属 + 当前 admin BaseURL/平台绑定，伪造来源测试 | 存在校验后来源瞬时变化的理论 TOCTOU |
| 9 | 无界上游 JSON 响应 | fixed | 8 MiB `LimitReader`，超限响应测试 | 未测量生产峰值 |
| 10 | 无界 JSON 请求体 | fixed | 1 MiB 解码上限，Server 层超限返回 413，multipart 不受影响测试 | 未做压测 |
| 11 | 短上游 key 完整预览 | fixed | 所有长度统一掩码，短 key 回归测试 | 无 |
| 12 | SMTP 任意 host/port 连接 | fixed | 公网 `PublicNetworkDialer`、TLS 证书校验和目标地址测试 | 未连接真实公网 SMTP |
| 13 | 钉钉 webhook SSRF | fixed | settings 安全 HTTP 客户端 | 未向真实钉钉发送请求 |
| 14 | 真实探活 Bearer SSRF | fixed | `NewRealProbeRunner` 公网 HTTPS 客户端和响应上限 | 未做真实私网可达性测试 |
| 15 | 已保存上游同步持续 SSRF | fixed | 通用上游 client 每次请求/重定向/DNS 重新校验 | 未连接保存的生产站点 |
| 16 | 固定回显注册验证码 | mitigated | `crypto/rand`、哈希、过期、原子消费、无回显；未配置邮件投递时 fail closed；生产公开注册保持关闭 | 当前仓库没有注册前 SMTP 投递配置，因此启用公开注册仍需补注册专用邮件配置 |
| 17 | 飞书 webhook SSRF | fixed | settings 安全 HTTP 客户端 | 未向真实飞书发送请求 |
| 18 | Telegram proxy SSRF | fixed | proxy URL 公网目标校验，HTTPS Telegram endpoint，重定向校验 | 未通过真实代理首包动态验证 |
| 19 | Ticket 匿名会话 SSRF | fixed | Ticket 来源只允许公网 HTTPS，当前 admin 来源绑定 | 未做真实 DNS rebinding/重定向演练 |
| 20 | 创建上游站点 SSRF | fixed | 创建入口规范化 + 安全 client | 未连接真实上游 |
| 21 | Ticket token 轮换不撤销旧 session | fixed | 轮换删除 workspace session；每次使用重新核验 token/归属 | Redis 删除与数据库更新不是跨系统事务 |
| 22 | Lottery 撤销非原子 | mitigated | 使用 session 时重新查询当前 token/工作区/来源，数据库更新后 Redis 清理失败时 fail closed | PostgreSQL/Redis 仍不是单一事务 |
| 23 | 登录无限速 | fixed | Redis 账号+连接对端 IP 计数、指数退避、成功清零、Redis 故障 fail closed | 未连接真实 Redis 集群做集成测试 |

## 残余风险与下一步

1. 公开注册要恢复可用，必须提供注册专用公网 SMTP/TLS 投递配置并补隔离集成测试；没有该配置时接口明确返回不可用，不回显或固定验证码。
2. 需要在无外网、无宿主端口的隔离 Docker 网络中，用脱敏数据库/Redis 备份演练迁移和启动；这不是本次代码测试，不能用生产数据替代。
3. 需要在隔离网络补做 DNS rebinding、跨来源 30x、SMTP 非 SMTP 端口和 Telegram 代理首包动态验证；当前静态和 harness 测试已覆盖拒绝边界，但这些动态结果仍记为 `unproven`。
4. 本分支只允许公网 HTTPS 作为 URL 型出站目标；SMTP/TLS 和 Telegram proxy 保留各自协议，但目标仍必须是公网地址。
