package dashboard

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMetricsRepositoryListRange_BeijingDatesAndWorkspace(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("设置 TEST_DATABASE_URL 后运行 PostgreSQL 历史日期边界测试")
	}

	for _, timezone := range []string{"Etc/GMT+12", "Pacific/Kiritimati"} {
		t.Run(timezone, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			config, err := pgxpool.ParseConfig(databaseURL)
			if err != nil {
				t.Fatal(err)
			}
			// 单连接临时表覆盖同名正式表，关闭连接即清理，不写业务数据。
			config.MaxConns = 1
			config.ConnConfig.RuntimeParams["timezone"] = timezone
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			_, err = pool.Exec(ctx, `CREATE TEMP TABLE dashboard_daily_stats (
				id text, user_id text, admin_account_id text, date date,
				today_profit double precision DEFAULT 1, site_balance double precision DEFAULT 2,
				today_purchase double precision DEFAULT 3, net_profit double precision DEFAULT 4,
				upstream_balance double precision DEFAULT 5, created_at timestamptz DEFAULT now()
			)`)
			if err != nil {
				t.Fatal(err)
			}
			today := time.Now().In(beijingLocation)
			for _, offset := range []int{-31, -30, -1, 0, 1} {
				_, err = pool.Exec(ctx, `INSERT INTO dashboard_daily_stats (id, user_id, admin_account_id, date)
					VALUES ($1, 'user-a', 'workspace-a', $2), ($1, 'user-b', 'workspace-a', $2), ($1, 'user-a', 'workspace-b', $2)`,
					fmt.Sprint(offset), today.AddDate(0, 0, offset))
				if err != nil {
					t.Fatal(err)
				}
			}
			repository := NewMetricsRepository(pool)
			for _, days := range []int{7, 30} {
				rows, err := repository.ListRange(ctx, "user-a", "workspace-a", days)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{today.AddDate(0, 0, -1).Format("2006-01-02")}
				if days == 30 {
					want = append([]string{today.AddDate(0, 0, -30).Format("2006-01-02")}, want...)
				}
				if len(rows) != len(want) {
					t.Fatalf("days=%d: got %d rows, want %v", days, len(rows), want)
				}
				for i, row := range rows {
					if row.UserID != "user-a" || row.AdminAccountID != "workspace-a" || row.Date.Format("2006-01-02") != want[i] {
						t.Fatalf("days=%d row[%d]=%+v, want date %s in user-a/workspace-a", days, i, row, want[i])
					}
				}
			}
		})
	}
}
