package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"transithub/backend/internal/modules/dashboard"
)

// 隔离schema只存在于显式TEST_DATABASE_URL，不接生产，也不复用历史测试通过结论。
func postgresFixture(t *testing.T) (postgresDatabase, repairInput, context.Context) {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("设置独立TEST_DATABASE_URL后验证营收CLI事务与锁")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	root, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "reconcile_cli_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := root.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		root.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 4
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["timezone"] = "Pacific/Kiritimati"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, err := root.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		root.Close()
	})
	if _, err := pool.Exec(ctx, `CREATE TABLE admin_accounts(id text PRIMARY KEY,user_id text NOT NULL); INSERT INTO admin_accounts VALUES ('a','u'),('other','u')`); err != nil {
		t.Fatal(err)
	}
	if err := dashboard.NewMetricsRepository(pool).EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	input := repairInput{UserID: "u", AdminAccountID: "a"}
	for i := 0; i < 2; i++ {
		date := time.Now().In(beijing).AddDate(0, 0, i-3)
		input.Rows = append(input.Rows, revenueRow{Date: date.Format(time.DateOnly), Revenue: 100 + float64(i)})
		_, err := pool.Exec(ctx, `INSERT INTO dashboard_daily_stats(id,user_id,admin_account_id,date,today_profit,today_purchase,net_profit,site_balance,upstream_balance,revenue_complete,purchase_complete,balance_recorded)
		 VALUES ($1,'u','a',$2,5,3,2,30,40,false,$3,$4)`, input.Rows[i].Date, date, i == 1, i == 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	return postgresDatabase{pool: pool}, input, ctx
}

func TestPostgresRepairReadOnlyDryRunAndAtomicApply(t *testing.T) {
	db, input, ctx := postgresFixture(t)
	before, err := db.Read(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runRepair(ctx, db, input, false, "")
	if err != nil || plan.Status != "dry_run" {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	unchanged, err := db.Read(ctx, input)
	if err != nil || !reflect.DeepEqual(before, unchanged) {
		t.Fatalf("dry-run changed rows: %v", err)
	}
	result, err := runRepair(ctx, db, input, true, plan.PlanHash)
	if err != nil || result.Status != "committed_verified" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	after, err := db.Read(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRows(input, before, after); err != nil {
		t.Fatal(err)
	}
	if after[0].CreatedAt == before[0].CreatedAt {
		t.Fatal("write did not use existing settlement owner")
	}
	if result, err := runRepair(ctx, db, input, true, plan.PlanHash); err == nil || result.Status != "rolled_back" {
		t.Fatalf("accepted stale plan: result=%+v err=%v", result, err)
	}
	// 明确读只读事务标志，避免未来实现将预演悄悄变成写事务。
	tx, err := db.Begin(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var readOnly string
	if err := tx.(postgresTransaction).tx.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil || readOnly != "on" {
		t.Fatalf("readOnly=%s err=%v", readOnly, err)
	}
}

func TestPostgresRepairRollbackOnLaterWriteFailure(t *testing.T) {
	db, input, ctx := postgresFixture(t)
	before, err := db.Read(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runRepair(ctx, db, input, false, "")
	if err != nil {
		t.Fatal(err)
	}
	// 第二行失败必须撤回第一行，触发器仅在测试schema内。
	_, err = db.pool.Exec(ctx, `CREATE FUNCTION reject_second_repair() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
	 IF OLD.purchase_complete THEN RAISE EXCEPTION 'test rejection'; END IF; RETURN NEW; END $$;
	 CREATE TRIGGER reject_second BEFORE UPDATE ON dashboard_daily_stats FOR EACH ROW EXECUTE FUNCTION reject_second_repair()`)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runRepair(ctx, db, input, true, plan.PlanHash)
	if err == nil || result.Status != "rolled_back" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	after, err := db.Read(ctx, input)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("partial writes survived: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestPostgresRepairMissingRowAndDriftReject(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "created_at drift", true: "missing row"}[missing], func(t *testing.T) {
			db, input, ctx := postgresFixture(t)
			plan, err := runRepair(ctx, db, input, false, "")
			if err != nil {
				t.Fatal(err)
			}
			query := `UPDATE dashboard_daily_stats SET created_at=created_at+interval '1 second' WHERE id=$1`
			if missing {
				query = `DELETE FROM dashboard_daily_stats WHERE id=$1`
			}
			if _, err := db.pool.Exec(ctx, query, input.Rows[1].Date); err != nil {
				t.Fatal(err)
			}
			before, err := db.Read(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			result, err := runRepair(ctx, db, input, true, plan.PlanHash)
			if err == nil || result.Status != "rolled_back" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			after, err := db.Read(ctx, input)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected plan changed rows: %v", err)
			}
		})
	}
}

func TestPostgresRepairLocksWorkspaceAndAllRows(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		t.Run(map[bool]string{false: "last target row", true: "workspace"}[workspace], func(t *testing.T) {
			db, input, ctx := postgresFixture(t)
			plan, err := runRepair(ctx, db, input, false, "")
			if err != nil {
				t.Fatal(err)
			}
			before, err := db.Read(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(ctx)
			query := `SELECT id FROM dashboard_daily_stats WHERE id=$1 FOR UPDATE`
			id := input.Rows[1].Date
			if workspace {
				query = `SELECT id FROM admin_accounts WHERE id=$1 FOR UPDATE`
				id = input.AdminAccountID
			}
			var locked string
			if err := blocker.QueryRow(ctx, query, id).Scan(&locked); err != nil {
				t.Fatal(err)
			}
			bounded, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer cancel()
			started := time.Now()
			result, err := runRepair(bounded, db, input, true, plan.PlanHash)
			// 驱动取消可能断开连接，因此区分已确认回滚与待回查；两者都不得提交。
			if err == nil || (result.Status != "rolled_back" && result.Status != "rollback_unconfirmed") || time.Since(started) < 100*time.Millisecond {
				t.Fatalf("did not respect lock: result=%+v err=%v", result, err)
			}
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			after, err := db.Read(ctx, input)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("wrote before all locks obtained: %v", err)
			}
		})
	}
}

func TestPostgresDryRunNeverInitializesSchema(t *testing.T) {
	db, input, ctx := postgresFixture(t)
	if _, err := db.pool.Exec(ctx, "DROP TABLE dashboard_daily_stats"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRepair(ctx, db, input, false, ""); err == nil {
		t.Fatal("missing schema accepted")
	}
	var exists bool
	if err := db.pool.QueryRow(ctx, "SELECT to_regclass('dashboard_daily_stats') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("dry-run initialized schema: exists=%v err=%v", exists, err)
	}
}
