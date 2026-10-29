package dashboard

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"transithub/backend/internal/modules/upstream"
)

// 桩只记录真实写入请求，不复制数据库合并算法；原子语义由 PostgreSQL 测试验证。
type settlementStore struct {
	writes   []DailySnapshot
	rows     []DailySnapshot
	err      error
	attempts int
	onWrite  func(context.Context, DailySnapshot)
}

func (r *settlementStore) Upsert(ctx context.Context, s DailySnapshot) error {
	r.attempts++
	if r.onWrite != nil {
		r.onWrite(ctx, s)
	}
	if r.err != nil {
		return r.err
	}
	r.writes = append(r.writes, s)
	return nil
}
func (r *settlementStore) ListRange(context.Context, string, string, int) ([]DailySnapshot, error) {
	return r.rows, nil
}
func (r *settlementStore) GetBalanceFilter(context.Context, string, string) (BalanceFilterConfig, error) {
	return BalanceFilterConfig{}, nil
}
func (r *settlementStore) SaveBalanceFilter(context.Context, BalanceFilterConfig) error { return nil }

type settlementPlatform struct {
	*fakePlatformClient
	amount     float64
	err        error
	dates      []string
	identities []string
	after      func()
}

func (p *settlementPlatform) FetchAdminUsageStatsContext(ctx context.Context, s upstream.Session, start, end string) (float64, error) {
	if _, ok := ctx.Deadline(); !ok {
		return 0, errors.New("missing deadline")
	}
	p.dates = append(p.dates, start+"/"+end)
	p.identities = append(p.identities, s.AccessToken)
	if p.after != nil {
		p.after()
	}
	return p.amount, p.err
}

type settlementUpstream struct {
	*fakeUpstreamLister
	calls      []string
	err        error
	result     *upstream.DailyPurchaseResult
	inCallback bool
	sites      []upstream.Response
	staleCalls []string
}

func (u *settlementUpstream) WithDailyPurchaseForAccount(ctx context.Context, user, account, date string, consume func(context.Context, upstream.DailyPurchaseResult) error) error {
	u.calls = append(u.calls, user+"/"+account+"/"+date)
	if u.err != nil {
		return u.err
	}
	u.inCallback = true
	defer func() { u.inCallback = false }()
	result := upstream.DailyPurchaseResult{Amount: 40, Complete: true, TotalSites: 1}
	if u.result != nil {
		result = *u.result
	}
	return consume(ctx, result)
}

func (u *settlementUpstream) ListForAccount(ctx context.Context, userID, adminAccountID string) []upstream.Response {
	return u.sites
}

func (u *settlementUpstream) SyncStaleForAccount(ctx context.Context, userID, adminAccountID string, maxAge time.Duration) {
	u.staleCalls = append(u.staleCalls, userID+"/"+adminAccountID+"/"+maxAge.String())
}

// 测试时钟：北京时间 2026-10-23 00:05:01，昨日为 2026-10-22。
func newSettlementService() (*MetricsService, *settlementStore, *settlementPlatform, *settlementUpstream) {
	store := &activeSessionStore{fakeSessionStore: newFakeSessionStore(), refs: []ActiveSessionRef{{UserID: "u", AdminAccountID: "a"}}}
	store.set("u", "a", AdminSession{Session: authenticatedSession()})
	repo := &settlementStore{}
	platform := &settlementPlatform{fakePlatformClient: &fakePlatformClient{}, amount: 100}
	sites := &settlementUpstream{fakeUpstreamLister: &fakeUpstreamLister{}}
	s := NewMetricsService(store, platform, sites, repo, &fakeAdminAccounts{current: map[string]string{"u": "a"}})
	s.now = func() time.Time { return time.Date(2026, 10, 22, 16, 5, 1, 0, time.UTC) }
	return s, repo, platform, sites
}

func settlementRow(date string, mutate func(*DailySnapshot)) DailySnapshot {
	parsed, _ := time.ParseInLocation("2006-01-02", date, beijingLocation)
	row := DailySnapshot{Date: parsed}
	if mutate != nil {
		mutate(&row)
	}
	return row
}

func TestSettlementExistingSampleStillReconciles(t *testing.T) {
	s, r, p, u := newSettlementService()
	r.rows = []DailySnapshot{settlementRow("2026-10-22", func(row *DailySnapshot) {
		row.TodayProfit, row.TodayPurchase, row.BalanceRecorded, row.PurchaseRecorded = 7, 3, true, true
	})}
	if err := s.settlePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.writes) != 1 {
		t.Fatalf("writes=%v", r.writes)
	}
	got := r.writes[0]
	if got.TodayProfit != 100 || got.TodayPurchase != 40 || !got.RevenueComplete || !got.PurchaseComplete || !got.PurchaseRecorded || !got.Settlement || got.BalanceRecorded {
		t.Fatalf("snapshot=%+v", got)
	}
	if p.dates[0] != "2026-10-22/2026-10-22" || u.calls[0] != "u/a/2026-10-22" {
		t.Fatalf("dates=%v calls=%v", p.dates, u.calls)
	}
}

func TestSettlementIndependentSourcesAndPersistenceFailure(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		revenue, purchase, write bool
		writes                   int
	}{
		{"revenue fails", true, false, false, 1}, {"purchase fails", false, true, false, 1}, {"both fail", true, true, false, 0}, {"write fails", false, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, p, u := newSettlementService()
			sentinel := errors.New("injected source failure")
			if tc.revenue {
				p.err = sentinel
			}
			if tc.purchase {
				u.err = sentinel
			}
			if tc.write {
				r.err = sentinel
			}
			err := s.settlePending(context.Background())
			if !errors.Is(err, sentinel) {
				t.Fatalf("error=%v", err)
			}
			if len(r.writes) != tc.writes {
				t.Fatalf("writes=%d", len(r.writes))
			}
			if tc.writes == 1 {
				got := r.writes[0]
				if got.RevenueComplete == tc.revenue || got.PurchaseComplete == tc.purchase {
					t.Fatalf("snapshot=%+v", got)
				}
				// 成本源失败时只写营收，不能把默认 0 标成有成本记录。
				if tc.purchase && got.PurchaseRecorded {
					t.Fatalf("failed purchase marked recorded: %+v", got)
				}
			}
		})
	}
}

// 部分站点失败时：成功部分照常落账（标记有记录、未完成），并返回错误以便下个小时重试。
func TestSettlementPartialPurchaseIsPersistedAndRetried(t *testing.T) {
	s, r, _, u := newSettlementService()
	u.result = &upstream.DailyPurchaseResult{Amount: 25, Complete: false, FailedSites: 1, TotalSites: 3, Cause: errors.New("site x: auth")}
	err := s.settlePending(context.Background())
	if err == nil || !strings.Contains(err.Error(), "purchase incomplete") {
		t.Fatalf("partial purchase must be reported for retry: %v", err)
	}
	if len(r.writes) != 1 {
		t.Fatalf("writes=%v", r.writes)
	}
	got := r.writes[0]
	if got.TodayPurchase != 25 || got.PurchaseComplete || !got.PurchaseRecorded || !got.RevenueComplete || got.TodayProfit != 100 {
		t.Fatalf("snapshot=%+v", got)
	}
	// 下一轮：营收已完成、成本仍未完成，只重试成本。
	r.rows = []DailySnapshot{settlementRow("2026-10-22", func(row *DailySnapshot) {
		row.RevenueComplete, row.PurchaseRecorded, row.TodayPurchase = true, true, 25
	})}
	u.result = nil
	if err := s.settlePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := r.writes[len(r.writes)-1]
	if last.RevenueComplete || !last.PurchaseComplete || last.TodayPurchase != 40 {
		t.Fatalf("retry=%+v", last)
	}
}

func TestPendingSettlementsWindow(t *testing.T) {
	rows := []DailySnapshot{
		// 完全没有成本记录的旧日：只补成本。
		settlementRow("2026-10-20", func(row *DailySnapshot) { row.RevenueComplete = true }),
		// 已有日内采样的旧日：不改写。
		settlementRow("2026-10-19", func(row *DailySnapshot) { row.RevenueComplete, row.PurchaseRecorded = true, true }),
		// 已完成的昨日：无需处理。
		settlementRow("2026-10-22", func(row *DailySnapshot) { row.RevenueComplete, row.PurchaseComplete = true, true }),
	}
	jobs := pendingSettlements(rows, "2026-10-22")
	if len(jobs) != 1 || jobs[0] != (settlementJob{date: "2026-10-20", purchase: true}) {
		t.Fatalf("jobs=%+v", jobs)
	}
	if jobs := pendingSettlements(nil, "2026-10-22"); len(jobs) != 1 || jobs[0] != (settlementJob{date: "2026-10-22", revenue: true, purchase: true}) {
		t.Fatalf("missing yesterday=%+v", jobs)
	}
}

func TestSettlementBackfillsOlderDayWithoutCostRecord(t *testing.T) {
	s, r, p, u := newSettlementService()
	r.rows = []DailySnapshot{
		settlementRow("2026-10-18", func(row *DailySnapshot) { row.RevenueComplete = true }),
		settlementRow("2026-10-22", func(row *DailySnapshot) { row.RevenueComplete, row.PurchaseComplete = true, true }),
	}
	if err := s.settlePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.dates) != 0 || len(u.calls) != 1 || u.calls[0] != "u/a/2026-10-18" {
		t.Fatalf("older backfill must be purchase only: dates=%v calls=%v", p.dates, u.calls)
	}
	got := r.writes[0]
	if got.RevenueComplete || !got.PurchaseComplete || got.TodayPurchase != 40 || !got.Settlement {
		t.Fatalf("snapshot=%+v", got)
	}
	// 超出补跑窗口的日期与非昨日营收都必须拒绝。
	if err := s.reconcileDate(context.Background(), "u", "a", "2026-10-10", false, true); err == nil {
		t.Fatal("accepted purchase outside retry window")
	}
	if err := s.reconcileDate(context.Background(), "u", "a", "2026-10-20", true, false); err == nil {
		t.Fatal("accepted revenue for non-yesterday date")
	}
}

func TestExplicitRepairNeverUsesCurrentCost(t *testing.T) {
	s, r, p, u := newSettlementService()
	if err := s.ReconcileRevenue(context.Background(), "u", "a", "2026-10-22"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyVerifiedRevenue(context.Background(), "u", "a", "2026-08-01", 125); err != nil {
		t.Fatal(err)
	}
	if len(u.calls) != 0 || len(r.writes) != 2 || len(p.dates) != 1 {
		t.Fatalf("writes=%v calls=%v dates=%v", r.writes, u.calls, p.dates)
	}
	for _, snap := range r.writes {
		if !snap.RevenueComplete || snap.PurchaseComplete || snap.PurchaseRecorded || snap.BalanceRecorded {
			t.Fatalf("snapshot=%+v", snap)
		}
	}
	for _, date := range []string{"2026-10-23", "2026-10-24", "2026-02-30", "bad"} {
		if s.ReconcileRevenue(context.Background(), "u", "a", date) == nil {
			t.Fatalf("accepted %s", date)
		}
	}
}

func TestSettlementWorkspaceIsolationAndCancellation(t *testing.T) {
	s, r, p, u := newSettlementService()
	store := s.store.(*activeSessionStore)
	store.refs = append(store.refs, ActiveSessionRef{UserID: "other", AdminAccountID: "b"})
	session := authenticatedSession()
	session.AccessToken = "other-token"
	store.set("other", "b", AdminSession{Session: session})
	if err := s.settlePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.writes) != 2 || r.writes[1].UserID != "other" || r.writes[1].AdminAccountID != "b" || p.identities[1] != "other-token" || u.calls[1] != "other/b/2026-10-22" {
		t.Fatalf("writes=%v identities=%v calls=%v", r.writes, p.identities, u.calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ReconcileRevenue(ctx, "u", "a", "2026-10-22"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if len(r.writes) != 2 {
		t.Fatal("cancelled operation wrote")
	}
}
func TestLiveRevenueFailureAndMidnightDoNotWrite(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(map[bool]string{false: "source failure", true: "cross midnight"}[cross], func(t *testing.T) {
			s, r, p, _ := newSettlementService()
			if cross {
				p.after = func() { s.now = func() time.Time { return time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC) } }
			} else {
				p.err = errors.New("source failed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, err := s.LiveMetrics(ctx, "u")
			if err == nil || len(r.writes) != 0 {
				t.Fatalf("error=%v writes=%v", err, r.writes)
			}
		})
	}
}
func TestTrendsPreserveCompletenessContract(t *testing.T) {
	s, r, _, _ := newSettlementService()
	r.rows = []DailySnapshot{
		{RevenueComplete: true, PurchaseComplete: false, BalanceRecorded: true, PurchaseRecorded: true},
		{RevenueComplete: true},
	}
	got, err := s.Trends(context.Background(), "u", 30)
	if err != nil || len(got.Points) != 2 || !got.Points[0].RevenueComplete || got.Points[0].PurchaseComplete || !got.Points[0].BalanceRecorded || !got.Points[0].PurchaseRecorded {
		t.Fatalf("got=%+v error=%v", got, err)
	}
	if got.Points[1].PurchaseRecorded {
		t.Fatalf("row without cost record must stay unknown: %+v", got.Points[1])
	}
}
func TestReconciliationDoesNotLogSavedOnWriteFailure(t *testing.T) {
	s, r, _, _ := newSettlementService()
	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(original)
	r.err = errors.New("write rejected")
	err := s.settlePending(context.Background())
	if err == nil || !strings.Contains(err.Error(), "persist") || strings.Contains(logs.String(), "saved") {
		t.Fatalf("error=%v", err)
	}
}

func TestSettlementWatermarkWindow(t *testing.T) {
	s, r, _, _ := newSettlementService()
	for _, minute := range []int{0, 2, 4} {
		s.now = func() time.Time { return time.Date(2026, 10, 22, 16, minute, 0, 0, time.UTC) }
		if err := s.settlePending(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s.ReconcileRevenue(context.Background(), "u", "a", "2026-10-22") == nil {
			t.Fatal("accepted before watermark")
		}
	}
	if len(r.writes) != 0 {
		t.Fatal("settled before watermark")
	}
	now := time.Date(2026, 10, 22, 16, 2, 0, 0, time.UTC)
	if got := nextSettlementTime(now); got.Sub(now) != 3*time.Minute {
		t.Fatalf("next=%s", got)
	}
	now = now.Add(3 * time.Minute)
	// 之后每小时重试一次未完成的日结。
	if got := nextSettlementTime(now); got.Sub(now) != time.Hour {
		t.Fatalf("next=%s", got)
	}
	s.now = func() time.Time { return now }
	if err := s.settlePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.writes) != 1 {
		t.Fatal("did not settle after watermark")
	}
	if s.ReconcileRevenue(context.Background(), "u", "a", "2026-08-01") == nil {
		t.Fatal("accepted expired-log historical lookup")
	}
}

func TestSettlementCommitInsidePurchaseCallback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "database failure"}[fail], func(t *testing.T) {
			s, r, p, u := newSettlementService()
			sentinel := errors.New("database acceptance unknown")
			if fail {
				r.err = sentinel
			}
			r.onWrite = func(ctx context.Context, snapshot DailySnapshot) {
				if !u.inCallback {
					t.Error("settlement persisted after configuration owner released its guard")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("commit has no deadline")
				}
				if len(p.dates) != 1 || !snapshot.RevenueComplete || !snapshot.PurchaseComplete {
					t.Errorf("not one combined settlement: %+v", snapshot)
				}
			}
			err := s.settlePending(context.Background())
			if fail && !errors.Is(err, sentinel) {
				t.Fatalf("error=%v", err)
			}
			if !fail && err != nil {
				t.Fatal(err)
			}
			if r.attempts != 1 {
				t.Fatalf("database attempts=%d, want exactly one including failed/unknown commit", r.attempts)
			}
			if u.inCallback {
				t.Fatal("source callback did not exit")
			}
		})
	}
}

// 后台成本采样只写当日成本，不改营收与余额；当天没有任何计费站点数据时不写。
func TestPurchaseSampleWritesCostOnly(t *testing.T) {
	s, r, p, u := newSettlementService()
	s.now = func() time.Time { return time.Date(2026, 10, 23, 15, 51, 0, 0, time.UTC) } // 北京 23:51
	synced := time.Date(2026, 10, 23, 15, 45, 0, 0, time.UTC).UnixMilli()
	consume := 12.5
	u.sites = []upstream.Response{{ID: "x", RechargeRate: 2, LastSyncedAt: &synced, Metrics: upstream.Metrics{TodayConsume: upstream.MetricValue{Value: &consume}}}}
	s.samplePurchaseAll(context.Background())
	if len(r.writes) != 1 || len(p.dates) != 0 {
		t.Fatalf("writes=%v revenue calls=%v", r.writes, p.dates)
	}
	got := r.writes[0]
	if got.TodayPurchase != 25 || !got.PurchaseRecorded || !got.PurchaseSampleOnly || got.Settlement || got.RevenueComplete || got.BalanceRecorded || got.Date.Format("2006-01-02") != "2026-10-23" {
		t.Fatalf("sample=%+v", got)
	}
	if len(u.staleCalls) != 1 || u.staleCalls[0] != "u/a/"+endOfDayStaleSiteAge.String() {
		t.Fatalf("stale sync calls=%v", u.staleCalls)
	}
	stale := time.Date(2026, 10, 22, 15, 45, 0, 0, time.UTC).UnixMilli()
	u.sites[0].LastSyncedAt = &stale
	s.samplePurchaseAll(context.Background())
	if len(r.writes) != 1 {
		t.Fatalf("stale data must not be sampled: %v", r.writes)
	}
}

func TestNextPurchaseSampleTime(t *testing.T) {
	for _, tc := range []struct{ now, want string }{
		{"10:03", "10:10"}, {"23:50", "23:58"}, {"23:51", "23:58"}, {"23:58", "00:00"}, {"23:59", "00:00"},
	} {
		now, _ := time.ParseInLocation("2006-01-02 15:04", "2026-10-23 "+tc.now, beijingLocation)
		if got := nextPurchaseSampleTime(now).In(beijingLocation).Format("15:04"); got != tc.want {
			t.Fatalf("now=%s next=%s want=%s", tc.now, got, tc.want)
		}
	}
}
