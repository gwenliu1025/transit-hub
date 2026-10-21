# 2026-10-21 Fork 定制与旧草稿核对

本记录持有本次来源比较事实，不替代[当前维护导航](../FORK_MAINTENANCE.md)的采用边界，也不把历史测试视为本次测试。

## 比较锚点与版本管理现状

- 修复前本地/生产代码基线：`4c98f310ddcfd65295c6d3276e7658f73904ab5d`，本地现有 `main`。
- 本次获取的个人 Fork `origin/main`：`1f2b92ab4e13a8708cc10f6184fcfc4bcea18659`；作者 `upstream/main`：`cb3b1bb4b9c6b18d65975747a5b580549d172960`；共同祖先：`9ee10de09495114cfc01cb6fed78f44183e9c935`。
- 共同祖先→修复前 Fork：22 个提交（21 个非 merge）、103 个文件、+4143/-552 行。主要定制已纳入 Git，不是全部散落在未提交文件。
- 作者在共同祖先后的 15 个提交，以及个人远端相对本地的 1 个提交，最终差异都仅为两份 Star SVG；本轮没有需要移植的作者业务代码更新。已将本地现有 main 仅快进到上述 origin/main；未创建分支或合并作者额外 Star 提交，原本地文档和恢复包哈希保持。
- 初始未提交内容仅为 `.gitignore` 的本地资产排除、README_CN 的本地状态链接，以及未跟踪 CURRENT_STATE.md；AGENTS.md 原本被忽略。业务源码初始无未提交修改。
- 处理这些文档时保留资产排除意图，公开 README 改为指向受版本管理的维护导航，本机状态单独排除。正式源码使用用户本次指定的位置；旧路径只保留在历史证据中。

## 旧未提交成果是否保全

独立 Explore 与主线程分别核对：本机恢复包内 20 个原始文件与两个旧目录当前文件逐字节一致；两份 binary diff 也与当前 `git diff --binary` 逐字节一致。未发现本次限定范围内恢复包之外的新未提交增量。恢复包、旧目录和历史分支均未改写或删除，不新增镜像副本。

下表仅列旧文件相对 `backend/` 的路径；`src` 对应旧 transit-hub-src，`security` 对应旧 transit-hub-security-hardening。判断依据是正式实现与测试，不以文件哈希不同推断遗失。

| 旧来源 | 文件 | 在正式基线中的归位结论 |
| --- | --- | --- |
| src | `internal/modules/dashboard/metrics_service.go` | 已吸收：按北京时间同步日过滤成本，余额不随日期过滤，实时与快照共用 helper |
| src | `internal/modules/dashboard/service.go` | 已吸收并增强：验证/刷新后同步会话并处理同步错误 |
| src | `internal/modules/dashboard/service_refresh_test.go` | 被状态验证及后台刷新同步测试替代 |
| src | `internal/modules/my_sites/service.go` | 已吸收：refresh 失败后验证仍有效的原 access 会话 |
| src | `internal/modules/my_sites/service_status_test.go` | 被有效原会话及双重失败回归替代 |
| src | `internal/modules/upstream/platform_service.go` | 已吸收：显式北京时间日期的 usage/stats，不再采用 dashboard 今日成本 |
| src | `internal/modules/upstream/platform_service_sub2api_groups_test.go` | 后续测试同时覆盖日期、timezone 与统计数据来源 |
| src | `internal/modules/upstream/service.go` | 被成功项保留加 KeyUsageCollectionError 的更完整实现替代，不恢复吞错草稿 |
| src | `internal/modules/upstream/service_key_usage_today_test.go` | 被全失败、部分失败和成功项保留回归替代 |
| src | `api.exe` | 临时二进制，按既有决定不进入源码 Git；原件保全 |
| src | `internal/modules/dashboard/metrics_service_purchase_test.go` | 被日界线、同步状态、缺失指标与余额保留测试替代并增强 |
| security | `internal/modules/upstream/normalizers.go` | 由语法判断与统一 egress 公网策略分工替代 |
| security | `internal/modules/upstream/normalizers_test.go` | 下层策略覆盖部分条件；旧入口级错误映射/规范化覆盖不等量，本次 HTTP 入口回归负责相关新增覆盖 |
| security | `internal/modules/upstream/platform_service.go` | 由默认生产 validator 与包内显式测试 seam 替代；HTTP 调整由本次修复单独持有 |
| security | `internal/modules/tickets/host_guard_test.go` | 被拒绝非安全来源及 nil 默认安全客户端测试替代；成功规范化覆盖不等量 |
| security | `internal/modules/upstream/http_client_test.go` | 当前采用显式客户端注入契约与 nil 默认安全客户端，不恢复对非 nil 测试客户端强制包装的旧断言 |
| security | `internal/security/egress/policy.go` | 主要公网/请求/重定向能力被现行实现替代；下节差异仍需独立决定 |
| security | `internal/security/egress/policy_test.go` | 策略、混合 DNS、重绑定与代理测试保留；旧真实传输和默认端口同源测试并非逐项等量迁移 |
| security | `internal/security/egress/resolver.go` | 公网解析和固定 IP 拨号并入 policy.go；下节端口/地址段差异未归位 |
| security | `internal/security/egress/test_helpers_test.go` | staticResolver、sequenceResolver 与 dial seam 已由现行测试结构承接 |

## 尚未采用的旧草稿差异

这些是静态比较发现的差异，不是已动态复现的漏洞，也不自动授权回迁整个旧安全草稿。

| 差异 | 已确认事实 | 当前处理 |
| --- | --- | --- |
| 显式端口范围 | 旧 validatePort 约束 1..65535；正式基线 URL/拨号入口未持有同等显式检查 | 保持现状，作为后续边界调查项；本次只证明合法端口的 HTTP 兼容 |
| IPv6 特殊段 | 旧拒绝集合含 `2001:2::/48`，正式基线集合未含 | 保留事实，未扩展本次地址规则写面 |
| 默认端口同源 | 旧 effectivePort 将 HTTPS 缺省端口和显式 443 视为同源；正式基线比较完整 Host，表现更严格 | 保留现行严格规则，不以兼容名义静默放宽凭据传播 |
| 测试层级 | 部分旧 client.Do/TLS 重定向和入口映射测试被较低层测试替代，不能称全部测试等价迁移 | HTTP 修复补齐命中范围；Ticket 成功规范化等非目标缺口保留待处理 |

## 本轮源码与验证边界

HTTP 请求由早期 Fork 的 HTTPS-only 策略拦截。本次仅调整站点业务的 HTTP/HTTPS 能力及其直接客户端，非站点 HTTPS-only 消费者保持。代码与公开维护说明分开记录，提交历史是代码版本事实来源；本轮未推送、未发布、未改生产。

本轮源码与测试证据：

- 目标四包：297 个测试/子测试通过，0 失败、0 跳过。
- 后端全量与 race：各817个测试/子测试通过，0失败；各11项PostgreSQL测试/子测试跳过。本机Docker引擎不可用且无TEST_DATABASE_URL，未使用生产数据库，不将跳过记为集成通过。
- `go vet ./...`、`go build ./...`、前端16项日期/金额测试、typecheck、build、发布入口一致性与差异空白检查通过。前端仍有既有大于500 kB分块提示，未为此修改业务或阈值。
- 测试包含默认生产策略/工厂、真实http.Client首包与重定向处理，以及保留生产URL校验的登录→保存→同步/编辑恢复。底层响应与仓储为内存夹具，不等同于真实数据库、外网TLS链路或线上上游账号验收。
- 责任关系经独立公共契约Review核对；本记录不主张数据库集成或生产HTTP接入完成。最终本地执行状态与独立Check回执由本机CURRENT_STATE.md持有。
