package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MetricsDBTX 让受控修复事务复用唯一写算法，不另建一套日结 SQL。
type MetricsDBTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// MetricsRepository 负责 dashboard_daily_stats 表的持久化操作。
// 与 Redis 的 SessionStore 独立，专门用于存储每日统计快照。
type MetricsRepository struct {
	db MetricsDBTX
}

func NewMetricsRepository(db MetricsDBTX) *MetricsRepository {
	return &MetricsRepository{db: db}
}

// EnsureSchema 在服务启动时创建 dashboard_daily_stats 和 dashboard_balance_filter 表及索引，
// 并将旧数据迁移到 workspace 维度。
//
// dashboard_daily_stats: (user_id, admin_account_id, date) 唯一索引保证每工作区每天至多一行。
// dashboard_balance_filter: (user_id, admin_account_id) 唯一索引保证每工作区至多一行配置。
//
// 迁移策略：
//  1. 新增 admin_account_id 列（DEFAULT ” 兼容旧行）。
//  2. 删除旧的单维度唯一索引/约束，创建新的多维度唯一索引。
//  3. 旧数据 admin_account_id=” 保留原样，由 admin_accounts 统一归属迁移负责补值。
func (r *MetricsRepository) EnsureSchema(ctx context.Context) error {
	_, err := r.db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS dashboard_daily_stats (
			id               text PRIMARY KEY,
			user_id          text NOT NULL,
			admin_account_id text NOT NULL DEFAULT '',
			date             date NOT NULL,
			today_profit     double precision NOT NULL DEFAULT 0,
			site_balance     double precision NOT NULL DEFAULT 0,
			today_purchase   double precision NOT NULL DEFAULT 0,
			net_profit       double precision NOT NULL DEFAULT 0,
			upstream_balance double precision NOT NULL DEFAULT 0,
			created_at       timestamptz NOT NULL DEFAULT now()
		);

		-- 新增 admin_account_id 列（旧表迁移，IF NOT EXISTS 语义通过 DO NOTHING 实现）。
		DO $$ BEGIN
			ALTER TABLE dashboard_daily_stats ADD COLUMN admin_account_id text NOT NULL DEFAULT '';
		EXCEPTION WHEN duplicate_column THEN NULL;
		END $$;

		ALTER TABLE dashboard_daily_stats ADD COLUMN IF NOT EXISTS revenue_complete boolean NOT NULL DEFAULT false;
		ALTER TABLE dashboard_daily_stats ADD COLUMN IF NOT EXISTS purchase_complete boolean NOT NULL DEFAULT false;
		-- 旧行已有采样余额；新建补数行没有历史余额。重复启动不改已有标记。
		ALTER TABLE dashboard_daily_stats ADD COLUMN IF NOT EXISTS balance_recorded boolean NOT NULL DEFAULT true;
		ALTER TABLE dashboard_daily_stats ALTER COLUMN balance_recorded SET DEFAULT false;
		-- 成本是否有真实来源。首次建列时按既有证据回填：已结算或成本非零才算有记录；
		-- 未结算且成本为 0 的行（营收补数新建，或上游尚未同步时的采样）无法区分真实 0 与缺数据，
		-- 一律视为未记录：前端显示缺口而不是 ¥0，最近 7 天内的由日结自动补跑。
		DO $$ BEGIN
			ALTER TABLE dashboard_daily_stats ADD COLUMN purchase_recorded boolean NOT NULL DEFAULT false;
			UPDATE dashboard_daily_stats SET purchase_recorded = purchase_complete OR today_purchase <> 0;
		EXCEPTION WHEN duplicate_column THEN NULL;
		END $$;

		-- 删除旧的 (user_id, date) 唯一索引，避免与新索引冲突。
		DROP INDEX IF EXISTS idx_dashboard_daily_stats_user_date;

		-- 创建新的 (user_id, admin_account_id, date) 唯一索引。
		CREATE UNIQUE INDEX IF NOT EXISTS idx_dashboard_daily_stats_user_account_date
			ON dashboard_daily_stats (user_id, admin_account_id, date);
		CREATE INDEX IF NOT EXISTS idx_dashboard_daily_stats_user_date_desc
			ON dashboard_daily_stats (user_id, admin_account_id, date DESC);

		CREATE TABLE IF NOT EXISTS dashboard_balance_filter (
			user_id          text NOT NULL,
			admin_account_id text NOT NULL DEFAULT '',
			exclude_admin    boolean NOT NULL DEFAULT true,
			exclude_balances jsonb NOT NULL DEFAULT '[]',
			updated_at       timestamptz NOT NULL DEFAULT now()
		);

		-- 新增 admin_account_id 列（旧表迁移）。
		DO $$ BEGIN
			ALTER TABLE dashboard_balance_filter ADD COLUMN admin_account_id text NOT NULL DEFAULT '';
		EXCEPTION WHEN duplicate_column THEN NULL;
		END $$;

		-- 删除旧的 user_id 主键约束，改为复合唯一索引。
		-- 旧表可能用 user_id 做主键或唯一约束，需要先去除。
		ALTER TABLE dashboard_balance_filter DROP CONSTRAINT IF EXISTS dashboard_balance_filter_pkey;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_dashboard_balance_filter_user_account
			ON dashboard_balance_filter (user_id, admin_account_id);
	`)
	return err
}

// ErrSnapshotNotWritten 表示工作区已删除或实时采样已跨过北京时间日期边界。
var ErrSnapshotNotWritten = errors.New("dashboard snapshot not written")

// 成本合并规则（$14 = 日结写入）：完整日结直接覆盖；已完成的成本不再改；没有真实来源的写入不改成本；
// 日内采样以最新值为准；部分日结只能抬高已有记录（两者都是全天成本的下限），不会把成本冲回更小的值。
const mergedPurchaseSQL = `CASE
	WHEN EXCLUDED.purchase_complete THEN EXCLUDED.today_purchase
	WHEN old.purchase_complete OR NOT EXCLUDED.purchase_recorded THEN old.today_purchase
	WHEN NOT $14 OR NOT old.purchase_recorded THEN EXCLUDED.today_purchase
	ELSE GREATEST(old.today_purchase, EXCLUDED.today_purchase) END`

// 营收：日结完成值直接覆盖；日内采样（非纯成本采样）在未完成时覆盖；其余保持原值。
const mergedRevenueSQL = `CASE WHEN EXCLUDED.revenue_complete OR (NOT $14 AND NOT $16 AND NOT old.revenue_complete) THEN EXCLUDED.today_profit ELSE old.today_profit END`

// Upsert 是实时采样、成本采样与日结的唯一写入口。完整性只可提升，失败源不得清零旧值。
// 日期判定在数据库执行和冲突锁取得后重查，避免排队跨午夜的迟到采样改写历史。
func (r *MetricsRepository) Upsert(ctx context.Context, snapshot DailySnapshot) error {
	settlement := snapshot.Settlement || snapshot.RevenueComplete || snapshot.PurchaseComplete
	tag, err := r.db.Exec(ctx, `
		INSERT INTO dashboard_daily_stats AS old (id, user_id, admin_account_id, date, today_profit, site_balance, today_purchase, net_profit, upstream_balance, created_at, revenue_complete, purchase_complete, balance_recorded, purchase_recorded)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8::double precision, $9, $10, $11, $12, $13, $15
		WHERE EXISTS (SELECT 1 FROM admin_accounts WHERE user_id = $2 AND id = $3)
		AND (($14 AND $4::date < (clock_timestamp() AT TIME ZONE 'Asia/Shanghai')::date)
		  OR (NOT $14 AND $4::date = (clock_timestamp() AT TIME ZONE 'Asia/Shanghai')::date))
		ON CONFLICT (user_id, admin_account_id, date) DO UPDATE SET
			today_profit = `+mergedRevenueSQL+`,
			today_purchase = `+mergedPurchaseSQL+`,
			net_profit = (`+mergedRevenueSQL+`) - (`+mergedPurchaseSQL+`),
			site_balance = CASE WHEN NOT $14 AND EXCLUDED.balance_recorded THEN EXCLUDED.site_balance ELSE old.site_balance END,
			upstream_balance = CASE WHEN NOT $14 AND EXCLUDED.balance_recorded THEN EXCLUDED.upstream_balance ELSE old.upstream_balance END,
			revenue_complete = old.revenue_complete OR EXCLUDED.revenue_complete,
			purchase_complete = old.purchase_complete OR EXCLUDED.purchase_complete,
			balance_recorded = old.balance_recorded OR (NOT $14 AND EXCLUDED.balance_recorded),
			purchase_recorded = old.purchase_recorded OR EXCLUDED.purchase_recorded,
			created_at = EXCLUDED.created_at
		WHERE EXISTS (SELECT 1 FROM admin_accounts WHERE user_id = EXCLUDED.user_id AND id = EXCLUDED.admin_account_id)
		AND ($14 OR EXCLUDED.date = (clock_timestamp() AT TIME ZONE 'Asia/Shanghai')::date)
	`, snapshot.ID, snapshot.UserID, snapshot.AdminAccountID, snapshot.Date, snapshot.TodayProfit, snapshot.SiteBalance,
		snapshot.TodayPurchase, snapshot.TodayProfit-snapshot.TodayPurchase, snapshot.UpstreamBalance, snapshot.CreatedAt,
		snapshot.RevenueComplete, snapshot.PurchaseComplete, snapshot.BalanceRecorded, settlement,
		snapshot.PurchaseRecorded || snapshot.PurchaseComplete, snapshot.PurchaseSampleOnly)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSnapshotNotWritten
	}
	return nil
}

// ListRange 查询指定用户指定工作区最近 days 天的快照记录，按日期升序返回。
// 不包含当天（当天的数据由 LiveMetrics 实时提供），仅返回已保存的历史日期。
func (r *MetricsRepository) ListRange(ctx context.Context, userID, adminAccountID string, days int) ([]DailySnapshot, error) {
	// 与实时指标和快照写入共用北京时间日期，不依赖数据库会话的 CURRENT_DATE。
	today := time.Now().In(beijingLocation)
	rows, err := r.db.Query(ctx, `
		SELECT id, user_id, admin_account_id, date, today_profit, site_balance, today_purchase, net_profit, upstream_balance, created_at, revenue_complete, purchase_complete, balance_recorded, purchase_recorded
		FROM dashboard_daily_stats
		WHERE user_id = $1 AND admin_account_id = $2 AND date >= ($4::date - $3::int) AND date < $4::date
		ORDER BY date ASC
	`, userID, adminAccountID, days, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	snapshots := make([]DailySnapshot, 0)
	for rows.Next() {
		var s DailySnapshot
		if err := rows.Scan(&s.ID, &s.UserID, &s.AdminAccountID, &s.Date, &s.TodayProfit, &s.SiteBalance,
			&s.TodayPurchase, &s.NetProfit, &s.UpstreamBalance, &s.CreatedAt, &s.RevenueComplete, &s.PurchaseComplete, &s.BalanceRecorded, &s.PurchaseRecorded); err != nil {
			return nil, err
		}
		snapshots = append(snapshots, s)
	}
	return snapshots, rows.Err()
}

// Exists 检查指定用户指定工作区指定日期是否已有快照行。
func (r *MetricsRepository) Exists(ctx context.Context, userID, adminAccountID string, date time.Time) (bool, error) {
	var count int
	err := r.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM dashboard_daily_stats WHERE user_id = $1 AND admin_account_id = $2 AND date = $3
	`, userID, adminAccountID, date).Scan(&count)
	return count > 0, err
}

// GetBalanceFilter 读取指定用户指定工作区的余额筛选配置。
// 若用户尚未配置，返回默认值（排除 admin、不排除任何余额值）。
func (r *MetricsRepository) GetBalanceFilter(ctx context.Context, userID, adminAccountID string) (BalanceFilterConfig, error) {
	config := BalanceFilterConfig{
		UserID:          userID,
		AdminAccountID:  adminAccountID,
		ExcludeAdmin:    true,
		ExcludeBalances: []float64{},
	}
	var balancesJSON []byte
	err := r.db.QueryRow(ctx, `
		SELECT exclude_admin, exclude_balances FROM dashboard_balance_filter WHERE user_id = $1 AND admin_account_id = $2
	`, userID, adminAccountID).Scan(&config.ExcludeAdmin, &balancesJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return config, nil
		}
		return config, err
	}
	if len(balancesJSON) > 0 {
		if err := json.Unmarshal(balancesJSON, &config.ExcludeBalances); err != nil {
			return config, err
		}
	}
	return config, nil
}

// SaveBalanceFilter 保存或更新指定用户指定工作区的余额筛选配置。
// 使用 upsert 确保幂等写入，用户首次配置和后续修改都走同一路径。
func (r *MetricsRepository) SaveBalanceFilter(ctx context.Context, config BalanceFilterConfig) error {
	balancesJSON, err := json.Marshal(config.ExcludeBalances)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO dashboard_balance_filter (user_id, admin_account_id, exclude_admin, exclude_balances, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (user_id, admin_account_id) DO UPDATE SET
			exclude_admin    = EXCLUDED.exclude_admin,
			exclude_balances = EXCLUDED.exclude_balances,
			updated_at       = now()
	`, config.UserID, config.AdminAccountID, config.ExcludeAdmin, balancesJSON)
	return err
}
