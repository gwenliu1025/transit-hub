package dashboard

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 独立 schema 允许两个真实连接竞争同一行，退出后仅删除本测试生成的命名空间。
func settlementPostgres(t *testing.T) (*MetricsRepository, context.Context) {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("设置独立 TEST_DATABASE_URL 后验证日结数据库合并与并发")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	id, err := metricsRandomID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "dashboard_settlement_" + id
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = pool.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 4
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["timezone"] = "Pacific/Kiritimati"
	scoped, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		scoped.Close()
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := pool.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		pool.Close()
	})
	_, err = scoped.Exec(ctx, `CREATE TABLE admin_accounts(id text PRIMARY KEY,user_id text NOT NULL);
 INSERT INTO admin_accounts VALUES ('a','u'),('b','u'),('c','other');
 CREATE TABLE dashboard_daily_stats(id text PRIMARY KEY,user_id text NOT NULL,admin_account_id text NOT NULL,date date NOT NULL,
 today_profit double precision NOT NULL DEFAULT 0,site_balance double precision NOT NULL DEFAULT 0,today_purchase double precision NOT NULL DEFAULT 0,
 net_profit double precision NOT NULL DEFAULT 0,upstream_balance double precision NOT NULL DEFAULT 0,created_at timestamptz NOT NULL DEFAULT now());
 INSERT INTO dashboard_daily_stats(id,user_id,admin_account_id,date,today_profit,today_purchase,site_balance,upstream_balance)
 VALUES ('old','u','a',(statement_timestamp() AT TIME ZONE 'Asia/Shanghai')::date-1,7,3,500,800)`)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMetricsRepository(scoped)
	if err := repo.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return repo, ctx
}

func TestMetricsRepositorySettlementAtomicMerge(t *testing.T) {
	r, ctx := settlementPostgres(t)
	date := time.Now().In(beijingLocation).AddDate(0, 0, -1)
	snap := DailySnapshot{ID: "repair", UserID: "u", AdminAccountID: "a", Date: date, TodayProfit: 100, RevenueComplete: true, CreatedAt: time.Now()}
	read := func() DailySnapshot {
		t.Helper()
		rows, err := r.ListRange(ctx, "u", "a", 7)
		if err != nil || len(rows) != 1 {
			t.Fatalf("rows=%v err=%v", rows, err)
		}
		return rows[0]
	}
	old := read()
	if old.RevenueComplete || old.PurchaseComplete || !old.BalanceRecorded || !old.PurchaseRecorded {
		t.Fatalf("migration=%+v", old)
	}
	if err := r.Upsert(ctx, snap); err != nil {
		t.Fatal(err)
	}
	got := read()
	if got.TodayProfit != 100 || got.TodayPurchase != 3 || got.NetProfit != 97 || !got.RevenueComplete || got.PurchaseComplete || got.SiteBalance != 500 || got.UpstreamBalance != 800 || !got.BalanceRecorded {
		t.Fatalf("partial=%+v", got)
	}
	snap.RevenueComplete = false
	snap.TodayProfit = 0
	snap.PurchaseComplete = true
	snap.TodayPurchase = 40
	if err := r.Upsert(ctx, snap); err != nil {
		t.Fatal(err)
	}
	got = read()
	if got.TodayProfit != 100 || got.TodayPurchase != 40 || got.NetProfit != 60 || !got.RevenueComplete || !got.PurchaseComplete {
		t.Fatalf("completed=%+v", got)
	}
	// 并发旧日采样必须在数据库日期门拒收；独立营收修正仍保留已结算成本和历史余额。
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	live := snap
	live.PurchaseComplete = false
	live.TodayProfit = 1
	live.TodayPurchase = 1
	live.BalanceRecorded = true
	live.SiteBalance = 999
	repair := snap
	repair.RevenueComplete = true
	repair.PurchaseComplete = false
	repair.TodayProfit = 125
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := r.Upsert(ctx, live)
		if !errors.Is(err, ErrSnapshotNotWritten) {
			errs <- err
		}
	}()
	go func() {
		defer wg.Done()
		if err := r.Upsert(ctx, repair); err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent outcome=%v", err)
	}
	got = read()
	if got.TodayProfit != 125 || got.TodayPurchase != 40 || got.NetProfit != 85 || got.SiteBalance != 500 || !got.RevenueComplete || !got.PurchaseComplete {
		t.Fatalf("after concurrent writes=%+v", got)
	}
	if err := r.Upsert(ctx, repair); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if after := read(); after != got {
		t.Fatalf("idempotency changed row: before=%+v after=%+v", got, after)
	}
	repair.ID = "missing"
	repair.AdminAccountID = "b"
	if err := r.Upsert(ctx, repair); err != nil {
		t.Fatal(err)
	}
	rows, err := r.ListRange(ctx, "u", "b", 7)
	if err != nil || len(rows) != 1 {
		t.Fatalf("new history: rows=%v err=%v", rows, err)
	}
	if rows[0].BalanceRecorded || rows[0].SiteBalance != 0 || rows[0].PurchaseComplete || rows[0].PurchaseRecorded {
		t.Fatalf("new history invented fields=%+v", rows[0])
	}
	repair.AdminAccountID = "c"
	if err := r.Upsert(ctx, repair); !errors.Is(err, ErrSnapshotNotWritten) {
		t.Fatalf("foreign workspace err=%v", err)
	}
	if rows, err := r.ListRange(ctx, "other", "c", 7); err != nil || len(rows) != 0 {
		t.Fatalf("foreign rows=%v err=%v", rows, err)
	}
}

func TestMetricsRepositoryLiveTodayDoesNotFinalize(t *testing.T) {
	r, ctx := settlementPostgres(t)
	snap := DailySnapshot{ID: "today", UserID: "u", AdminAccountID: "a", Date: time.Now().In(beijingLocation), TodayProfit: 23, TodayPurchase: 10, SiteBalance: 500, BalanceRecorded: true, CreatedAt: time.Now()}
	if err := r.Upsert(ctx, snap); err != nil {
		t.Fatal(err)
	}
	var revenue, purchase, balance bool
	var net float64
	if err := r.db.QueryRow(ctx, `SELECT revenue_complete,purchase_complete,balance_recorded,net_profit FROM dashboard_daily_stats WHERE id='today'`).Scan(&revenue, &purchase, &balance, &net); err != nil {
		t.Fatal(err)
	}
	if revenue || purchase || !balance || net != 13 {
		t.Fatalf("flags=%v/%v/%v net=%v", revenue, purchase, balance, net)
	}
	snap.RevenueComplete = true
	if err := r.Upsert(ctx, snap); !errors.Is(err, ErrSnapshotNotWritten) {
		t.Fatalf("accepted today's settlement: %v", err)
	}
}

// 成本合并：没有真实来源的写入不改成本；纯成本采样不碰营收；部分日结只抬高已有记录；完整日结覆盖并锁定。
func TestMetricsRepositoryPurchaseMerge(t *testing.T) {
	r, ctx := settlementPostgres(t)
	today := time.Now().In(beijingLocation)
	read := func(account string, offset int) DailySnapshot {
		t.Helper()
		rows, err := r.ListRange(ctx, "u", account, 7)
		if err != nil {
			t.Fatal(err)
		}
		want := today.AddDate(0, 0, offset).Format("2006-01-02")
		for _, row := range rows {
			if row.Date.Format("2006-01-02") == want {
				return row
			}
		}
		// 今日行不在 ListRange 范围内，直接查询。
		var row DailySnapshot
		if err := r.db.QueryRow(ctx, `SELECT today_profit,today_purchase,net_profit,revenue_complete,purchase_complete,purchase_recorded FROM dashboard_daily_stats WHERE user_id='u' AND admin_account_id=$1 AND date=$2`, account, want).
			Scan(&row.TodayProfit, &row.TodayPurchase, &row.NetProfit, &row.RevenueComplete, &row.PurchaseComplete, &row.PurchaseRecorded); err != nil {
			t.Fatal(err)
		}
		return row
	}
	write := func(s DailySnapshot) {
		t.Helper()
		if err := r.Upsert(ctx, s); err != nil {
			t.Fatalf("upsert %+v: %v", s, err)
		}
	}

	// 今日：仪表盘采样（营收 50、成本 20）→ 无数据采样不改成本 → 纯成本采样只改成本。
	live := DailySnapshot{ID: "l1", UserID: "u", AdminAccountID: "b", Date: today, TodayProfit: 50, TodayPurchase: 20, PurchaseRecorded: true, BalanceRecorded: true, CreatedAt: time.Now()}
	write(live)
	live.ID, live.TodayProfit, live.TodayPurchase, live.PurchaseRecorded = "l2", 60, 0, false
	write(live)
	if got := read("b", 0); got.TodayProfit != 60 || got.TodayPurchase != 20 || got.NetProfit != 40 || !got.PurchaseRecorded {
		t.Fatalf("unrecorded sample overwrote cost: %+v", got)
	}
	write(DailySnapshot{ID: "s1", UserID: "u", AdminAccountID: "b", Date: today, TodayPurchase: 35, PurchaseRecorded: true, PurchaseSampleOnly: true, CreatedAt: time.Now()})
	if got := read("b", 0); got.TodayProfit != 60 || got.TodayPurchase != 35 || got.NetProfit != 25 {
		t.Fatalf("cost-only sample touched revenue: %+v", got)
	}

	// 昨日（账户 b 无旧行）：仅营收日结建行时成本未知。
	yesterday := today.AddDate(0, 0, -1)
	write(DailySnapshot{ID: "y1", UserID: "u", AdminAccountID: "b", Date: yesterday, TodayProfit: 100, RevenueComplete: true, Settlement: true, CreatedAt: time.Now()})
	if got := read("b", -1); got.PurchaseRecorded || got.TodayPurchase != 0 {
		t.Fatalf("revenue-only settlement invented cost: %+v", got)
	}
	partial := DailySnapshot{ID: "y2", UserID: "u", AdminAccountID: "b", Date: yesterday, TodayPurchase: 30, PurchaseRecorded: true, Settlement: true, CreatedAt: time.Now()}
	write(partial)
	partial.TodayPurchase = 20
	write(partial)
	if got := read("b", -1); got.TodayPurchase != 30 || got.NetProfit != 70 || got.PurchaseComplete {
		t.Fatalf("partial settlement lowered cost: %+v", got)
	}
	write(DailySnapshot{ID: "y3", UserID: "u", AdminAccountID: "b", Date: yesterday, TodayPurchase: 28, PurchaseComplete: true, Settlement: true, CreatedAt: time.Now()})
	partial.TodayPurchase = 60
	write(partial)
	if got := read("b", -1); got.TodayPurchase != 28 || got.NetProfit != 72 || !got.PurchaseComplete || !got.RevenueComplete || got.TodayProfit != 100 {
		t.Fatalf("complete settlement not authoritative: %+v", got)
	}
}

// 首次建列时按既有证据回填 purchase_recorded：只由营收补数新建、从未采样成本的行保持未知。
func TestMetricsRepositoryPurchaseRecordedBackfill(t *testing.T) {
	r, ctx := settlementPostgres(t)
	if _, err := r.db.Exec(ctx, `ALTER TABLE dashboard_daily_stats DROP COLUMN purchase_recorded;
		INSERT INTO dashboard_daily_stats(id,user_id,admin_account_id,date,today_profit,today_purchase,revenue_complete,purchase_complete,balance_recorded)
		VALUES ('revenue-only','u','b',(statement_timestamp() AT TIME ZONE 'Asia/Shanghai')::date-2,90,0,true,false,false),
		       ('nonzero','u','b',(statement_timestamp() AT TIME ZONE 'Asia/Shanghai')::date-3,90,12,true,false,false),
		       ('stale-sample','u','b',(statement_timestamp() AT TIME ZONE 'Asia/Shanghai')::date-4,90,0,false,false,true),
		       ('settled-zero','u','b',(statement_timestamp() AT TIME ZONE 'Asia/Shanghai')::date-5,90,0,true,true,false)`); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	rows, err := r.db.Query(ctx, `SELECT id, purchase_recorded FROM dashboard_daily_stats`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var recorded bool
		if err := rows.Scan(&id, &recorded); err != nil {
			t.Fatal(err)
		}
		got[id] = recorded
	}
	// 上游未同步时的 0 成本采样（余额已记录）同样无法证明当日成本为 0；已结算的 0 是真实值。
	if !got["old"] || got["revenue-only"] || !got["nonzero"] || got["stale-sample"] || !got["settled-zero"] {
		t.Fatalf("backfill=%v", got)
	}
}
