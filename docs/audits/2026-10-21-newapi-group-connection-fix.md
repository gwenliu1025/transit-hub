# New API 分组对接修复与验证

本记录保存本次修复范围与验收，不取代源码和测试的行为定义。当前定制入口为 [Fork 维护说明](../FORK_MAINTENANCE.md)。

## 范围与基线

- 用户要求：修复 New API 分组对接与错误提示，保留原 Sub2API 对接，留存改动、测试和版本记录，先不动生产。
- 修改基线：`7817dcd05a3f3e0cb98bdabedcc0dfb9a2d6e7bf`，在既有 `main` 上完成；此前 HTTP 兼容代码和文档提交完整保留。
- 作者上游的15个 Star 图片提交按用户决定暂不合入。本次没有数据库迁移、新依赖、版本号修改、推送、发布或生产操作。

## 已实施变更

| 责任 | 实现与直接消费者 | 变更边界 |
| --- | --- | --- |
| 账号创建协议 | `backend/internal/modules/my_sites/connection_protocol.go`，由 `connection_service.go` 和 `service.go` 消费 | 原厂商描述保持；后端同一协议定义持有允许选项、校验和载荷参数。New API 合法显式选择优先，混合/未知缺选择与非法选择在远端创建前返回固定错误；Sub2API 已知合法缓存仍优先。 |
| 对接选择界面 | `frontend/src/modules/admin/views/GroupRatesView.vue`、`utils/connectionOptions.ts`、两端 `my_sites` 类型声明 | UI 消费后端能力，不再整串匹配混合厂商；New API 可选择/覆盖实际协议，清空后禁止提交，切换后剔除不匹配自有分组。管理端 New API 显示完整合法渠道，建议只作默认；已有绑定不增加创建协议要求。 |
| 安全错误展示 | `backend/internal/modules/my_sites/handler.go`、前端错误投影及中英文案 | 仅透传已归类的固定上游错误键；远端认证失败不伪装为本站401。未知正文、内部异常与凭据不直接展示。映射、凭据和资源读取区分加载/失败/空结果，支持手动重读，旧响应不回填新弹窗。 |
| 写后读回 | `GroupRatesView.vue` 的映射读取与 `refreshAfterMutation` | 读取返回成功/失败/失效，写入成功后刷新失败在页面显示；清除旧映射，保留已经成功的写入，不自动重试写操作，不因报错更换 operationId。 |
| 持续回归 | `frontend/package.json`、`.github/workflows/release.yml` | 新增 `test:connections`，接入既有前端CI验证；保留原仪表盘测试、构建及后端验证。 |

厂商元数据归一化、数据库、远端凭据创建顺序、幂等预留、补偿流程和原连接记录不做重构。创建协议相关的 Sub2API 四类载荷分别验证并发、pool、透传和 tier 参数；New API 两端的渠道/令牌适配继续消费原平台接口。

## 最终来源验证

以下结果来自返工后的同一最终源码，不复用前一轮851项/12项的通过数字。独立 Review 曾发现写后映射读取失败被吞掉，已在同一闭包修复并加入创建、绑定两种回归。

| 验证入口 | 结果 |
| --- | --- |
| `go test -json -count=1 -timeout=3m ./internal/modules/my_sites ./internal/modules/upstream` | 178个测试/子测试通过，无失败或跳过 |
| `go test -json -count=1 -timeout=3m ./...` | 852通过，11项PostgreSQL测试/子测试跳过 |
| `go test -race -json -count=1 -timeout=3m ./...` | 852通过，11项PostgreSQL测试/子测试跳过 |
| `go vet ./...`、`go build ./...` | 通过 |
| `npm run test:connections` | 13项通过；编译并执行实际SFC setup和API客户端，包含切换、清空、旧响应失效、安全错误、读取失败恢复及写后刷新失败 |
| `npm run test:dashboard` | 原16项经营统计回归通过 |
| `npm run typecheck`、`npm run build` | 通过；保留既有大于500 kB分块提示，不改构建阈值 |
| 后台真实构建页面 | 11个场景通过；API为隔离合成夹具，包含协议选择、Sub2API四协议、New API渠道、已有绑定、加载错误与恢复、两种写后刷新失败；390×844视口内可滚动到提交按钮，关窗恢复页面滚动 |
| `node scripts/verify-release.mjs`、差异空白检查 | 通过；应用版本保持原值 |
| 文本与既有资产 | 严格UTF-8，无新增BOM；旧文件换行保留，新文件LF；既有恢复包SHA256与修改前一致 |

代码测试入口：`backend/internal/modules/my_sites/connection_protocol_test.go`、`connection_service_test.go`、`connection_idempotency_test.go`、`credential_preview_test.go`、`frontend/tests/connections.test.ts`。四种平台组合均有公开 `Service.RealConnect` 主流程覆盖；已有资源绑定单独覆盖，不用底层helper通过替代主入口。

本机受控证据位于 `_skill-cache/evidence/NewAPI分组对接/`：`validation.json`及原始测试日志、`browser-check.json`、可重跑的验证脚本与桌面/移动端截图。该目录不进入公开Git或镜像；正式持续回归由上述仓库测试及CI持有。

## 验收边界与回滚

- 本机未配置独立 `TEST_DATABASE_URL`，本轮数据库跳过不算集成通过。未借用生产数据库、真实站点账号或令牌；本地模拟适配和浏览器夹具不代表用户真实站点已对接成功。
- 源码和本地提交与生产更新分开。本次只完成本地修复、文档和版本管理；实际发布或真实站点验收需后续单独进行。
- 代码回滚使用本次修复提交的 `git revert`；不硬重置工作树、不回滚数据库、不撤掉此前HTTP兼容提交。提交身份由Git历史持有，本机当前交付状态由 `CURRENT_STATE.md` 持有。
