# TransitHub 安全加固设计

**日期：** 2026-08-05  
**基线：** `7c28bde`（生产修复分支，不修改生产标签）  
**目标分支：** `codex/security-hardening`

## 目标

在不改变当前生产部署、不提交生产凭据、不破坏现有公网 HTTPS 上游统计能力的前提下，修复全项目审计发现的 23 项安全问题，并把验证结果提交到用户的 GitHub fork。所有行为变化先在隔离工作树、单元测试、集成测试和脱敏生产数据演练中确认；本次工作不执行生产发布。

## 已确认的兼容边界

- 当前生产 24 个上游地址全部为 `https://`，没有明文 HTTP、其他协议或字面环回/私网地址。
- URL 型 HTTP 出站仅允许公网 HTTPS。
- SMTP/TLS 和 Telegram proxy 不是 HTTPS 协议，因此保留其能力，但只允许解析到公网地址；不引入人工白名单配置。
- 不删除统计、同步、探活、Webhook、邮件、Ticket 或抽奖功能；不安全的目标返回稳定的参数错误，不静默改写目标。
- 生产数据库、Redis、Caddy、Sub2API 和现有应用容器不在本次开发范围内。

## 设计原则

1. **统一控制而非散落判断。** 所有出站连接经过统一的目标解析、DNS 解析、IP 分类、重定向和凭据发送策略。
2. **先测试后实现。** 每个修复先添加能在旧代码上失败的回归测试，再写最小实现；保留合法公网 HTTPS、合法 SMTP/TLS 和合法 Telegram proxy 的正向测试。
3. **拒绝而不是降级。** 目标不满足策略时返回可识别的安全错误；不把 HTTPS 降级为 HTTP，不把失败目标替换成另一个目标。
4. **数据与会话原子性。** 身份绑定、session 撤销、幂等预留和租户调度上下文必须在边界处不可绕过。
5. **可回滚。** 每个阶段单独提交；不改写 `v0.1.15-xiaoqian.2`，只推送新的开发分支和草稿 PR。

## 统一出站策略

新增小型安全包，提供两类能力：

- `PublicHTTPSClient`：用于上游、Dashboard、模型发现、真实探活、Ticket、钉钉/企微/飞书。要求 scheme 为 `https`、无 URL userinfo；解析主机的所有地址并拒绝 loopback、RFC1918、链路本地、未指定地址、组播、云元数据和其他非公网地址；禁止未经校验的重定向，跨来源重定向不转发认证凭据；统一连接超时、请求体和响应体上限。
- `PublicNetworkDialer`：用于 SMTP/TLS 和 Telegram proxy。保留各自协议和现有 API，只在连接前对主机解析结果执行公网地址策略，使用连接超时和并发预算。

目标校验必须在实际 `DialContext` 前执行，不能只检查字符串或第一次 DNS 结果。解析器、拨号器、重定向检查器和时钟/网络依赖可注入，以便在隔离测试中覆盖 DNS rebinding、IPv4/IPv6、重定向和超时路径。

## 身份、凭据与会话

- 将 `/api/users` 纳入认证路由；响应只返回当前授权范围所需的最小字段。
- 邮箱验证码改为密码学随机值，数据库只存哈希；验证码请求返回统一成功信息，注册时使用带过期时间的原子 compare-and-consume。
- Ticket embed token 解析后只从服务端持久化记录得到 `user_id/admin_account_id/upstream_id`，不信任请求体身份；token 轮换递增 session 版本并使旧 session 失效。
- Lottery session 撤销与 Redis/数据库清理在同一事务语义下完成；清理失败不得继续接受旧 session。
- `KeyPreview` 永远掩码，包括短 key；普通列表接口不提供完整凭据。

## 资源、租户与幂等

- 共享 JSON 解码器在解码前使用统一请求体上限；上游响应在读取前使用统一响应体上限。
- 登录失败按账号、来源 IP、设备标识和时间窗口实施服务端限速与指数退避；成功登录清理对应失败计数。
- 远端 key/token/channel/account 创建前先以服务端生成的幂等键原子预留本地记录；重复请求返回已有操作结果或明确冲突，补偿删除保留并记录未知结果。
- 刷新策略回调显式传递 `user_id/admin_account_id`，调度器按工作区保存和重建 timer，不再读取全局第一条策略覆盖所有租户。

## 测试与验证

每个阶段必须包含：

- 旧代码失败、新代码通过的聚焦回归测试。
- 同一真实接口的合法公网/合法协议正向测试。
- 私网、环回、链路本地、IPv6、DNS rebinding、重定向、超大请求/响应、并发重复请求等负向测试。
- `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./...`。
- `frontend` 目录执行 `npm ci` 后运行 `npm run typecheck`、`npm run build`。
- 使用脱敏生产 PostgreSQL dump 和 Redis RDB 的隔离迁移/启动演练；不连接生产网络，不使用生产端口。
- 重新运行安全扫描或等价 source/control/sink 追踪，确认 23 项路径已关闭或明确记录剩余证明缺口。

## 发布与回滚

- 代码仅在 `E:/gwenliu/XiaoQianAPI/zhongzhuan/transit-hub-security-hardening` 工作树修改。
- 每个阶段形成独立提交，最终推送到 `gwenliu1025/transit-hub` 的 `codex/security-hardening` 分支并更新草稿 PR。
- 不修改生产 `.env`、数据库、Redis、Docker Compose、现有生产镜像或正式标签。
- 若任一阶段破坏合法公网 HTTPS、SMTP/TLS、Telegram proxy、统计、同步或 Ticket 流程，停止推进该阶段并回滚该阶段提交。

