package group_rate_campaigns

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const postgresTestTimeout = 15 * time.Second

// TestCampaignStatusSQLGuards 锁定状态落定语句的条件（无需数据库）：FinishStart 只能写 running 中的活动，
// FinishEnd 只能写 ending 中的活动，调度扫描必须有界并遵守退避，避免并发开启/结束互相覆盖或无限重试。
func TestCampaignStatusSQLGuards(t *testing.T) {
	if !strings.Contains(finishStartSQL, "status = 'running'") {
		t.Fatalf("FinishStart must only update campaigns still running: %s", finishStartSQL)
	}
	if !strings.Contains(finishEndSQL, "status = 'ending'") || !strings.Contains(finishEndSQL, "next_end_retry_at = $3") {
		t.Fatalf("FinishEnd must only update ending campaigns and record the retry time: %s", finishEndSQL)
	}
	if !strings.Contains(listDueRunningSQL, "end_attempts < $2") || !strings.Contains(listDueRunningSQL, "next_end_retry_at <= $1") {
		t.Fatalf("due running scan must bound attempts and honour backoff: %s", listDueRunningSQL)
	}
	if !strings.Contains(claimForEndingSQL, "end_attempts = end_attempts + 1") || !strings.Contains(claimStuckEndingSQL, "updated_at <= $2") {
		t.Fatalf("ending claims must count attempts and only take over stale ending campaigns")
	}
}

// TestRepositoryEndRetryAndStatusGuardsWithPostgres 在真实 PostgreSQL 上验证有界重试、退避、
// 状态守卫和卡住 ending 的接管语句。
func TestRepositoryEndRetryAndStatusGuardsWithPostgres(t *testing.T) {
	pool := openPostgresTestPool(t)
	repo := NewRepository(pool)
	ctx, cancel := context.WithTimeout(context.Background(), postgresTestTimeout)
	defer cancel()
	if err := repo.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if err := repo.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema must be idempotent: %v", err)
	}

	now := time.Now()
	past := now.Add(-time.Hour)
	insert := func(id string, status string) {
		t.Helper()
		if err := repo.Insert(ctx, Campaign{
			ID: id, UserID: "user1", AdminAccountID: "ws1", Name: id, Status: status,
			StartMode: StartScheduled, StartAt: &past, EndMode: EndScheduled, EndAt: &past,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	isDue := func(id string) bool {
		t.Helper()
		ids, err := repo.ListDueRunning(ctx, now)
		if err != nil {
			t.Fatalf("list due running: %v", err)
		}
		return slices.Contains(ids, id)
	}
	statusOf := func(id string) string {
		t.Helper()
		campaign, err := repo.GetByID(ctx, id)
		if err != nil || campaign == nil {
			t.Fatalf("get %s: campaign=%v err=%v", id, campaign, err)
		}
		return campaign.Status
	}

	insert("due", StatusRunning)
	if !isDue("due") {
		t.Fatalf("expected running campaign past end_at to be due")
	}
	for attempt := 1; attempt <= maxEndAttempts; attempt++ {
		got, claimed, err := repo.ClaimForEnding(ctx, "due")
		if err != nil || !claimed || got != attempt {
			t.Fatalf("claim attempt %d: got=%d claimed=%v err=%v", attempt, got, claimed, err)
		}
		retryAt := now.Add(time.Hour)
		if err := repo.FinishEnd(ctx, "due", StatusPartial, &retryAt); err != nil {
			t.Fatalf("finish end: %v", err)
		}
		if isDue("due") {
			t.Fatalf("partial campaign must wait for its retry time after attempt %d", attempt)
		}
		if _, err := pool.Exec(ctx, `UPDATE group_rate_campaigns SET next_end_retry_at = $2 WHERE id = $1`, "due", now.Add(-time.Minute)); err != nil {
			t.Fatalf("expire retry time: %v", err)
		}
		if want := attempt < maxEndAttempts; isDue("due") != want {
			t.Fatalf("after attempt %d due=%v, want %v", attempt, !want, want)
		}
	}
	if err := repo.FinishEnd(ctx, "due", StatusEnded, nil); err != nil {
		t.Fatalf("finish end on non-ending campaign: %v", err)
	}
	if got := statusOf("due"); got != StatusPartial {
		t.Fatalf("FinishEnd must not touch a campaign that is not ending, got %q", got)
	}

	insert("racing", StatusScheduled)
	if claimed, err := repo.ClaimForRunning(ctx, "racing"); err != nil || !claimed {
		t.Fatalf("claim running: claimed=%v err=%v", claimed, err)
	}
	if _, claimed, err := repo.ClaimForEnding(ctx, "racing"); err != nil || !claimed {
		t.Fatalf("claim ending during start: claimed=%v err=%v", claimed, err)
	}
	if finished, err := repo.FinishStart(ctx, "racing", StatusRunning); err != nil || finished {
		t.Fatalf("FinishStart must not overwrite a concurrent end: finished=%v err=%v", finished, err)
	}
	if got := statusOf("racing"); got != StatusEnding {
		t.Fatalf("expected racing campaign to stay ending, got %q", got)
	}

	insert("normal", StatusScheduled)
	if claimed, err := repo.ClaimForRunning(ctx, "normal"); err != nil || !claimed {
		t.Fatalf("claim running: claimed=%v err=%v", claimed, err)
	}
	if finished, err := repo.FinishStart(ctx, "normal", StatusPartial); err != nil || !finished {
		t.Fatalf("FinishStart should finish a running campaign: finished=%v err=%v", finished, err)
	}

	stuck, err := repo.ListStuckEnding(ctx, now.Add(-time.Hour))
	if err != nil || slices.Contains(stuck, "racing") {
		t.Fatalf("recently claimed ending campaign must not be stuck: ids=%v err=%v", stuck, err)
	}
	if _, claimed, err := repo.ClaimStuckEnding(ctx, "racing", now.Add(-time.Hour)); err != nil || claimed {
		t.Fatalf("must not take over a fresh ending campaign: claimed=%v err=%v", claimed, err)
	}
	stuck, err = repo.ListStuckEnding(ctx, now.Add(time.Hour))
	if err != nil || !slices.Contains(stuck, "racing") {
		t.Fatalf("expected stale ending campaign to be listed: ids=%v err=%v", stuck, err)
	}
	if attempt, claimed, err := repo.ClaimStuckEnding(ctx, "racing", now.Add(time.Hour)); err != nil || !claimed || attempt != 2 {
		t.Fatalf("take over stale ending: attempt=%d claimed=%v err=%v", attempt, claimed, err)
	}
}

func openPostgresTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL repository tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), postgresTestTimeout)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	schema := fmt.Sprintf("group_rate_campaigns_test_%d", time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		adminPool.Close()
		t.Fatalf("create test schema: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		adminPool.Close()
		t.Fatalf("parse PostgreSQL config: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		adminPool.Close()
		t.Fatalf("connect test schema: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		adminPool.Close()
		t.Fatalf("ping test schema: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), postgresTestTimeout)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
		adminPool.Close()
	})
	return pool
}
