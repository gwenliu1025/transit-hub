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
	inCallback bool
}

func (u *settlementUpstream) WithDailyPurchaseForAccount(ctx context.Context, user, account, date string, consume func(context.Context, float64) error) error {
	u.calls = append(u.calls, user+"/"+account+"/"+date)
	if u.err != nil {
		return u.err
	}
	u.inCallback = true
	defer func() { u.inCallback = false }()
	return consume(ctx, 40)
}

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
func TestSettlementExistingSampleStillReconciles(t *testing.T) {
	s, r, p, u := newSettlementService()
	r.rows = []DailySnapshot{{TodayProfit: 7, TodayPurchase: 3, BalanceRecorded: true}}
	if err := s.reconcileAll(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(r.writes) != 1 {
		t.Fatalf("writes=%v", r.writes)
	}
	got := r.writes[0]
	if got.TodayProfit != 100 || got.TodayPurchase != 40 || !got.RevenueComplete || !got.PurchaseComplete || got.BalanceRecorded {
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
			err := s.reconcileAll(context.Background(), true)
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
			}
		})
	}
}
func TestStartupAndExplicitRepairNeverUseCurrentCost(t *testing.T) {
	s, r, p, u := newSettlementService()
	if err := s.reconcileAll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyVerifiedRevenue(context.Background(), "u", "a", "2026-08-01", 125); err != nil {
		t.Fatal(err)
	}
	if len(u.calls) != 0 || len(r.writes) != 2 || len(p.dates) != 1 {
		t.Fatalf("writes=%v calls=%v dates=%v", r.writes, u.calls, p.dates)
	}
	for _, snap := range r.writes {
		if !snap.RevenueComplete || snap.PurchaseComplete || snap.BalanceRecorded {
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
	s, r, p, _ := newSettlementService()
	store := s.store.(*activeSessionStore)
	store.refs = append(store.refs, ActiveSessionRef{UserID: "other", AdminAccountID: "b"})
	session := authenticatedSession()
	session.AccessToken = "other-token"
	store.set("other", "b", AdminSession{Session: session})
	if err := s.reconcileAll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(r.writes) != 2 || r.writes[1].UserID != "other" || r.writes[1].AdminAccountID != "b" || p.identities[1] != "other-token" {
		t.Fatalf("writes=%v identities=%v", r.writes, p.identities)
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
	r.rows = []DailySnapshot{{RevenueComplete: true, PurchaseComplete: false, BalanceRecorded: true}}
	got, err := s.Trends(context.Background(), "u", 30)
	if err != nil || len(got.Points) != 1 || !got.Points[0].RevenueComplete || got.Points[0].PurchaseComplete || !got.Points[0].BalanceRecorded {
		t.Fatalf("got=%+v error=%v", got, err)
	}
}
func TestReconciliationDoesNotLogSavedOnWriteFailure(t *testing.T) {
	s, r, _, _ := newSettlementService()
	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(original)
	r.err = errors.New("write rejected")
	err := s.reconcileAll(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "persist") || strings.Contains(logs.String(), "saved") {
		t.Fatalf("error=%v", err)
	}
}

func TestSettlementWatermarkWindow(t *testing.T) {
	s, r, _, _ := newSettlementService()
	for _, minute := range []int{0, 2, 4} {
		s.now = func() time.Time { return time.Date(2026, 10, 22, 16, minute, 0, 0, time.UTC) }
		if err := s.reconcileAll(context.Background(), true); err != nil {
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
	if got := nextSettlementTime(now); got.Sub(now) != 24*time.Hour {
		t.Fatalf("next=%s", got)
	}
	s.now = func() time.Time { return now }
	if err := s.reconcileAll(context.Background(), true); err != nil {
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
			err := s.reconcileAll(context.Background(), true)
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
