package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"transithub/backend/internal/modules/dashboard"
)

type beforeRow struct {
	Exists           bool    `json:"exists"`
	ID               string  `json:"id"`
	Date             string  `json:"date"`
	Revenue          float64 `json:"revenue"`
	Purchase         float64 `json:"purchase"`
	Net              float64 `json:"net"`
	SiteBalance      float64 `json:"siteBalance"`
	UpstreamBalance  float64 `json:"upstreamBalance"`
	RevenueComplete  bool    `json:"revenueComplete"`
	PurchaseComplete bool    `json:"purchaseComplete"`
	BalanceRecorded  bool    `json:"balanceRecorded"`
	CreatedAt        string  `json:"createdAt"`
}

type report struct {
	Status        string       `json:"status"`
	WorkspaceHash string       `json:"workspaceHash"`
	StartDate     string       `json:"startDate"`
	EndDate       string       `json:"endDate"`
	RowCount      int          `json:"rowCount"`
	ChangedRows   int          `json:"changedRows"`
	BeforeTotal   float64      `json:"beforeRevenueTotal"`
	TargetTotal   float64      `json:"targetRevenueTotal"`
	PlanHash      string       `json:"planHash"`
	Rows          []revenueRow `json:"rows"`
	Error         string       `json:"error,omitempty"`
}

// 该接口只隔离事务生命周期以测试接受状态，不拥有日结计算或第二套写算法。
type repairTransaction interface {
	Read(context.Context, repairInput, bool) ([]beforeRow, error)
	Apply(context.Context, repairInput, revenueRow) error
	Commit(context.Context) error
	Rollback(context.Context) error
}

type repairDatabase interface {
	Begin(context.Context, bool) (repairTransaction, error)
	Read(context.Context, repairInput) ([]beforeRow, error)
}

func makePlan(input repairInput, rows []beforeRow) (report, error) {
	if len(rows) != len(input.Rows) {
		return report{}, errors.New("目标历史行缺失，拒绝创建新行")
	}
	r := report{Status: "dry_run", RowCount: len(rows), StartDate: input.Rows[0].Date, EndDate: input.Rows[len(input.Rows)-1].Date, Rows: input.Rows}
	identity, _ := json.Marshal([]string{input.UserID, input.AdminAccountID})
	workspace := sha256.Sum256(identity)
	r.WorkspaceHash = hex.EncodeToString(workspace[:])
	for i, old := range rows {
		if !old.Exists || old.Date != input.Rows[i].Date || old.ID == "" || old.CreatedAt == "" {
			return report{}, errors.New("目标历史行缺失或身份不一致")
		}
		for _, value := range []float64{old.Revenue, old.Purchase, old.Net, old.SiteBalance, old.UpstreamBalance, input.Rows[i].Revenue - old.Purchase} {
			if !finite(value) {
				return report{}, errors.New("历史行或派生值包含非有限金额")
			}
		}
		if old.Revenue != input.Rows[i].Revenue || !old.RevenueComplete || old.Net != input.Rows[i].Revenue-old.Purchase {
			r.ChangedRows++
		}
		r.BeforeTotal += old.Revenue
		r.TargetTotal += input.Rows[i].Revenue
	}
	if !finite(r.BeforeTotal) || !finite(r.TargetTotal) {
		return report{}, errors.New("计划合计溢出")
	}
	canonical, err := json.Marshal(struct {
		Version string      `json:"version"`
		Input   repairInput `json:"input"`
		Before  []beforeRow `json:"before"`
	}{"dashboard-revenue-plan-v1", input, rows})
	if err != nil {
		return report{}, errors.New("计划编码失败")
	}
	hash := sha256.Sum256(canonical)
	r.PlanHash = hex.EncodeToString(hash[:])
	return r, nil
}

func verifyRows(input repairInput, before, after []beforeRow) error {
	if len(before) != len(input.Rows) || len(after) != len(before) {
		return errors.New("回读行数不一致")
	}
	for i, row := range after {
		old, target := before[i], input.Rows[i]
		if !row.Exists || row.ID != old.ID || row.Date != target.Date || row.Revenue != target.Revenue || !row.RevenueComplete || row.Purchase != old.Purchase || row.PurchaseComplete != old.PurchaseComplete || row.SiteBalance != old.SiteBalance || row.UpstreamBalance != old.UpstreamBalance || row.BalanceRecorded != old.BalanceRecorded || row.Net != target.Revenue-old.Purchase {
			return fmt.Errorf("日期%s的营收或保全字段回读不一致", target.Date)
		}
	}
	return nil
}

func runRepair(ctx context.Context, db repairDatabase, input repairInput, apply bool, expected string) (result report, failure error) {
	result.Status = "not_started"
	if apply {
		decoded, err := hex.DecodeString(expected)
		if err != nil || len(decoded) != sha256.Size || expected != hex.EncodeToString(decoded) {
			return result, errors.New("apply要求有效的小写expected-plan-sha256")
		}
	} else if expected != "" {
		return result, errors.New("expected-plan-sha256仅用于apply")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tx, err := db.Begin(ctx, apply)
	if err != nil {
		return result, errors.New("建立数据库事务失败")
	}
	commitAttempted := false
	defer func() {
		// 提交已发出后即使返回错误也不宣称回滚；连接清理不改变接受状态。
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		err := tx.Rollback(cleanup)
		if failure != nil && !commitAttempted && apply {
			result.Status = "rolled_back"
			if err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				result.Status = "rollback_unconfirmed"
			}
		}
	}()
	before, err := tx.Read(ctx, input, apply)
	if err != nil {
		return result, errors.New("读取或锁定工作区与目标历史行失败")
	}
	result, err = makePlan(input, before)
	if err != nil {
		return result, err
	}
	if !apply {
		return result, nil
	}
	if result.PlanHash != expected {
		return result, errors.New("计划已漂移，必须重新dry-run并审阅")
	}
	for _, row := range input.Rows {
		if err := tx.Apply(ctx, input, row); err != nil {
			return result, fmt.Errorf("日期%s写入失败，事务未提交", row.Date)
		}
	}
	after, err := tx.Read(ctx, input, false)
	if err != nil {
		return result, errors.New("事务内回读失败，事务未提交")
	}
	if err := verifyRows(input, before, after); err != nil {
		return result, err
	}
	commitAttempted = true
	if err := tx.Commit(ctx); err != nil {
		result.Status = "commit_unknown"
		return result, errors.New("提交结果未知；禁止自动重试，须重新只读核对")
	}
	result.Status = "committed_readback_failed"
	// 只有提交后独立回读成立才报告成功；失败也不会伪装成已回滚。
	after, err = db.Read(ctx, input)
	if err != nil {
		return result, errors.New("事务已提交，但事务外回读失败")
	}
	if err := verifyRows(input, before, after); err != nil {
		return result, err
	}
	result.Status = "committed_verified"
	return result, nil
}

type postgresDatabase struct{ pool *pgxpool.Pool }
type postgresTransaction struct{ tx pgx.Tx }

func (db postgresDatabase) Begin(ctx context.Context, apply bool) (repairTransaction, error) {
	options := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	if apply {
		options.AccessMode = pgx.ReadWrite
	}
	tx, err := db.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	if apply {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'; SET LOCAL statement_timeout = '60s'"); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
	}
	return postgresTransaction{tx: tx}, nil
}

func (db postgresDatabase) Read(ctx context.Context, input repairInput) ([]beforeRow, error) {
	return readRows(ctx, db.pool, input, false)
}

func (tx postgresTransaction) Read(ctx context.Context, input repairInput, lock bool) ([]beforeRow, error) {
	return readRows(ctx, tx.tx, input, lock)
}

func (tx postgresTransaction) Apply(ctx context.Context, input repairInput, row revenueRow) error {
	repository := dashboard.NewMetricsRepository(tx.tx)
	service := dashboard.NewMetricsService(nil, nil, nil, repository, nil)
	return service.ApplyVerifiedRevenue(ctx, input.UserID, input.AdminAccountID, row.Date, row.Revenue)
}

func (tx postgresTransaction) Commit(ctx context.Context) error   { return tx.tx.Commit(ctx) }
func (tx postgresTransaction) Rollback(ctx context.Context) error { return tx.tx.Rollback(ctx) }

func readRows(ctx context.Context, db dashboard.MetricsDBTX, input repairInput, lock bool) ([]beforeRow, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	var id string
	if err := db.QueryRow(ctx, "SELECT id FROM admin_accounts WHERE user_id=$1 AND id=$2"+suffix, input.UserID, input.AdminAccountID).Scan(&id); err != nil {
		return nil, err
	}
	start, _ := time.Parse(time.DateOnly, input.Rows[0].Date)
	end, _ := time.Parse(time.DateOnly, input.Rows[len(input.Rows)-1].Date)
	rows, err := db.Query(ctx, `SELECT id,date,today_profit,today_purchase,net_profit,site_balance,upstream_balance,
	 revenue_complete,purchase_complete,balance_recorded,created_at FROM dashboard_daily_stats
	 WHERE user_id=$1 AND admin_account_id=$2 AND date BETWEEN $3::date AND $4::date ORDER BY date`+suffix,
		input.UserID, input.AdminAccountID, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]beforeRow, 0, len(input.Rows))
	for rows.Next() {
		var row beforeRow
		var date, created time.Time
		if err := rows.Scan(&row.ID, &date, &row.Revenue, &row.Purchase, &row.Net, &row.SiteBalance, &row.UpstreamBalance, &row.RevenueComplete, &row.PurchaseComplete, &row.BalanceRecorded, &created); err != nil {
			return nil, err
		}
		row.Exists, row.Date, row.CreatedAt = true, date.Format(time.DateOnly), created.UTC().Format(time.RFC3339Nano)
		result = append(result, row)
	}
	return result, rows.Err()
}
