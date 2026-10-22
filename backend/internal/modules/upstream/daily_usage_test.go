package upstream

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestDatedUsagePlatformBoundaries(t *testing.T) {
	for _, platform := range []Platform{PlatformSub2API, PlatformNewAPI} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%v", platform, admin), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					q := r.URL.Query()
					if platform == PlatformSub2API {
						path := "/api/v1/usage/stats"
						if admin {
							path = "/api/v1/admin/usage/stats"
						}
						if r.URL.Path != path || q.Get("start_date") != "2026-10-22" || q.Get("end_date") != "2026-10-22" || q.Get("timezone") != "Asia/Shanghai" {
							t.Errorf("unexpected request %s", r.URL)
						}
						fmt.Fprint(w, `{"data":{"total_actual_cost":12.5,"total_cost":99}}`)
					} else {
						// 独立 UTC oracle：北京时间 10/22 自然日为 UTC 10/21 16:00 至 10/22 15:59:59。
						start := time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC).Unix()
						if r.URL.Path != "/api/log/self/stat" || q.Get("type") != "2" || q.Get("start_timestamp") != fmt.Sprint(start) || q.Get("end_timestamp") != fmt.Sprint(start+86399) {
							t.Errorf("unexpected request %s", r.URL)
						}
						fmt.Fprint(w, `{"success":true,"data":{"quota":1250}}`)
					}
				}))
				defer server.Close()
				s := NewPlatformService(NewHTTPClient(server.Client()))
				session := Session{Platform: platform, BaseURL: server.URL, AccessToken: "token", UserID: "1", QuotaPerUnit: 100}
				got, err := s.fetchDatedUsage(context.Background(), session, "2026-10-22", "2026-10-22", admin)
				if err != nil || got != 12.5 {
					t.Fatalf("got %v, %v", got, err)
				}
			})
		}
	}
}

func TestDatedUsageRejectsInvalidAmounts(t *testing.T) {
	for _, platform := range []Platform{PlatformSub2API, PlatformNewAPI} {
		for _, field := range []string{`null`, `-1`, `"NaN"`, `"+Inf"`, `"oops"`, `true`, `missing`, `nominal`} {
			t.Run(string(platform)+"/"+field, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					key := "total_actual_cost"
					if platform == PlatformNewAPI {
						key = "quota"
					}
					if field == "missing" {
						fmt.Fprint(w, `{"data":{}}`)
						return
					}
					if field == "nominal" {
						fmt.Fprint(w, `{"data":{"total_cost":9}}`)
						return
					}
					fmt.Fprintf(w, `{"data":{%q:%s}}`, key, field)
				}))
				defer server.Close()
				s := NewPlatformService(NewHTTPClient(server.Client()))
				session := Session{Platform: platform, BaseURL: server.URL, AccessToken: "token", UserID: "1"}
				if value, err := s.FetchAdminUsageStatsContext(context.Background(), session, "2026-10-22", "2026-10-22"); err == nil || value != 0 {
					t.Fatalf("accepted invalid amount: %v %v", value, err)
				}
			})
		}
	}
}

func TestDatedUsageDateValidation(t *testing.T) {
	for _, dates := range [][2]string{{"2026-2-01", "2026-2-01"}, {"2026-02-30", "2026-02-30"}, {"2026-10-22", "2026-10-21"}, {"2026-10-22&timezone=UTC", "2026-10-22"}} {
		if _, _, err := dailyUsageBounds(dates[0], dates[1]); err == nil {
			t.Fatalf("accepted %v", dates)
		}
	}
	start, end, err := dailyUsageBounds("2026-12-31", "2027-01-01")
	if err != nil || end.Sub(start) != 48*time.Hour-time.Second {
		t.Fatalf("year boundary: %v %v %v", start, end, err)
	}
}

func dailyTestService(t *testing.T, handler http.HandlerFunc) (*Service, *fakeSiteCache) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cache := newFakeSiteCache()
	cache.add(newTestSite("one", "user", "account", 0.5, &Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "token"}))
	return NewService(NewPlatformService(NewHTTPClient(server.Client())), &dailyTestRepository{cache: cache}, nil, cache), cache
}

func TestDailyPurchaseWorkspaceAndDatedSource(t *testing.T) {
	s, cache := dailyTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start_date") != "2026-10-22" {
			t.Errorf("wrong date %s", r.URL)
		}
		fmt.Fprint(w, `{"data":{"total_actual_cost":18}}`)
	})
	cache.sites["one"].Metrics.TodayConsume.Value = ptrFloat(999)
	cache.add(newTestSite("other-account", "user", "other", 2, nil))
	cache.add(newTestSite("other-user", "other", "account", 2, nil))
	got, err := dailyPurchaseForTest(s, context.Background(), "user", "account", "2026-10-22")
	if err != nil || got != 9 {
		t.Fatalf("got %v %v", got, err)
	}
	got, err = dailyPurchaseForTest(s, context.Background(), "user", "empty", "2026-10-22")
	if err != nil || got != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
}

func TestDailyPurchaseFailureIsNotZero(t *testing.T) {
	for _, mode := range []string{"missing-session", "partial-failure", "invalid-rate", "zero-rate", "empty"} {
		t.Run(mode, func(t *testing.T) {
			s, cache := dailyTestService(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"total_actual_cost":18}}`) })
			switch mode {
			case "missing-session":
				cache.sites["one"].Session = nil
			case "partial-failure":
				cache.add(newTestSite("bad", "user", "account", 2, nil))
			case "invalid-rate":
				cache.sites["one"].RechargeRate = math.NaN()
			case "zero-rate":
				cache.sites["one"].RechargeRate = 0
				cache.sites["one"].Session = nil
			case "empty":
				delete(cache.sites, "one")
			}
			value, err := dailyPurchaseForTest(s, context.Background(), "user", "account", "2026-10-22")
			wantErr := mode != "zero-rate" && mode != "empty"
			if (err != nil) != wantErr || value != 0 {
				t.Fatalf("value=%v err=%v", value, err)
			}
		})
	}
}

func TestDailyPurchaseConfigurationChange(t *testing.T) {
	for _, mode := range []string{"rate", "aba", "identity", "add-remove-aba", "metrics-only", "other-workspace"} {
		t.Run(mode, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			s, cache := dailyTestService(t, func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				fmt.Fprint(w, `{"data":{"total_actual_cost":18}}`)
			})
			result := make(chan error, 1)
			go func() {
				_, err := dailyPurchaseForTest(s, context.Background(), "user", "account", "2026-10-22")
				result <- err
			}()
			<-started
			site, _ := cache.Get(context.Background(), "one")
			switch mode {
			case "rate", "aba":
				site.RechargeRate = 2
				if err := s.setCachedSite(context.Background(), site); err != nil {
					t.Fatal(err)
				}
				if mode == "aba" {
					site.RechargeRate = 0.5
					if err := s.setCachedSite(context.Background(), site); err != nil {
						t.Fatal(err)
					}
				}
			case "identity":
				site.Account = "changed"
				if err := s.setCachedSite(context.Background(), site); err != nil {
					t.Fatal(err)
				}
			case "add-remove-aba":
				added := newTestSite("added", "user", "account", 1, nil)
				if err := s.setCachedSite(context.Background(), added); err != nil {
					t.Fatal(err)
				}
				if err := s.deleteCachedSite(context.Background(), "added", "user"); err != nil {
					t.Fatal(err)
				}
			case "metrics-only":
				site.Metrics.TodayConsume.Value = ptrFloat(200)
				if err := s.setCachedSite(context.Background(), site); err != nil {
					t.Fatal(err)
				}
			case "other-workspace":
				if err := s.setCachedSite(context.Background(), newTestSite("other", "user", "other", 1, nil)); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			err := <-result
			wantErr := mode != "metrics-only" && mode != "other-workspace"
			if (err != nil) != wantErr {
				t.Fatalf("error=%v", err)
			}
			if wantErr && !strings.Contains(err.Error(), "configuration") {
				t.Fatal(err)
			}
		})
	}
}

func TestDailyPurchaseCancellationAndConcurrency(t *testing.T) {
	var active, peak atomic.Int32
	entered := make(chan struct{}, 8)
	s, cache := dailyTestService(t, func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		<-r.Context().Done()
	})
	for i := 0; i < 8; i++ {
		site := *cache.sites["one"]
		site.ID = fmt.Sprint(i)
		cache.add(&site)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := dailyPurchaseForTest(s, ctx, "user", "account", "2026-10-22"); result <- err }()
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("requests did not start")
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not release workers")
	}
	if peak.Load() > 4 {
		t.Fatalf("unbounded concurrency %d", peak.Load())
	}
}

func ptrFloat(v float64) *float64 { return &v }

// 测试统一经受保护的消费者读取金额，生产不再保留裸金额采集入口。
func dailyPurchaseForTest(s *Service, ctx context.Context, userID, accountID, date string) (float64, error) {
	var value float64
	err := s.WithDailyPurchaseForAccount(ctx, userID, accountID, date, func(_ context.Context, amount float64) error { value = amount; return nil })
	return value, err
}

type dailyTestRepository struct {
	cache *fakeSiteCache
	sites []Site
	err   error
}

func (r *dailyTestRepository) ListSites(ctx context.Context) ([]Site, error) {
	return r.ListSitesForUser(ctx, "user")
}
func (r *dailyTestRepository) ListSitesForUser(ctx context.Context, userID string) ([]Site, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.cache == nil {
		return r.sites, nil
	}
	list, err := r.cache.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	sites := make([]Site, 0, len(list))
	for _, site := range list {
		sites = append(sites, *site)
	}
	return sites, nil
}
func (r *dailyTestRepository) SaveSite(ctx context.Context, site Site) error           { return nil }
func (r *dailyTestRepository) DeleteSite(ctx context.Context, userID, id string) error { return nil }

func TestDailyPurchaseAuthoritativeMembership(t *testing.T) {
	for _, mode := range []string{"missing-cache-member", "missing-all-cache", "repository-failure", "repository-unavailable", "configuration-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			s, cache := dailyTestService(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"total_actual_cost":18}}`) })
			configured := *cache.sites["one"]
			repo := &dailyTestRepository{sites: []Site{configured}}
			s.repository = repo
			switch mode {
			case "missing-cache-member":
				other := configured
				other.ID = "missing"
				repo.sites = append(repo.sites, other)
			case "missing-all-cache":
				delete(cache.sites, "one")
			case "repository-failure":
				repo.err = errors.New("repository unavailable")
			case "repository-unavailable":
				s.repository = nil
			case "configuration-mismatch":
				cache.sites["one"].RechargeRate = 2
			}
			called := false
			err := s.WithDailyPurchaseForAccount(context.Background(), "user", "account", "2026-10-22", func(context.Context, float64) error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("err=%v consumer=%v", err, called)
			}
		})
	}
}

func TestDailyPurchaseConsumerHoldsConfigurationStable(t *testing.T) {
	s, cache := dailyTestService(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"total_actual_cost":18}}`) })
	site, _ := cache.Get(context.Background(), "one")
	entered, release, updated := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	result := make(chan error, 1)
	go func() {
		result <- s.WithDailyPurchaseForAccount(context.Background(), "user", "account", "2026-10-22", func(ctx context.Context, amount float64) error {
			if amount != 9 {
				t.Errorf("amount=%v", amount)
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > dailyUsageTimeout {
				t.Error("consumer missing shared deadline")
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	go func() { site.RechargeRate = 2; updated <- s.setCachedSite(context.Background(), site) }()
	select {
	case err := <-updated:
		t.Fatalf("configuration changed before commit returned: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-updated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("configuration lock not released")
	}
}

func TestDailyPurchaseConsumerFailureAndCancellation(t *testing.T) {
	s, _ := dailyTestService(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"total_actual_cost":18}}`) })
	expected := errors.New("commit rejected")
	err := s.WithDailyPurchaseForAccount(context.Background(), "user", "account", "2026-10-22", func(context.Context, float64) error { return expected })
	if !errors.Is(err, expected) {
		t.Fatalf("commit error lost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = s.WithDailyPurchaseForAccount(ctx, "user", "account", "2026-10-22", func(consumerCtx context.Context, _ float64) error {
		cancel()
		<-consumerCtx.Done()
		return consumerCtx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error lost: %v", err)
	}
}

// 使用真实 RedisSiteCache 的列表解码路径，只在 Redis 命令传输处注入悬空集合成员。
type missingMemberRedisHook struct{}

func (missingMemberRedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (missingMemberRedisHook) ProcessHook(_ redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		if members, ok := cmd.(*redis.StringSliceCmd); ok {
			members.SetVal([]string{"one"})
			return nil
		}
		return errors.New("unexpected redis command")
	}
}
func (missingMemberRedisHook) ProcessPipelineHook(_ redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(_ context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			cmd.SetErr(redis.Nil)
		}
		return redis.Nil
	}
}

func TestDailyPurchaseRealCacheMissingMember(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "unused:6379"})
	t.Cleanup(func() { _ = client.Close() })
	client.AddHook(missingMemberRedisHook{})
	cache := NewRedisSiteCache(client)
	items, err := cache.ListByUser(context.Background(), "user")
	if err != nil || len(items) != 0 {
		t.Fatalf("fixture must reproduce omitted member: %v %v", items, err)
	}
	repo := &dailyTestRepository{sites: []Site{{ID: "one", UserID: "user", AdminAccountID: "account", RechargeRate: 1}}}
	s := NewService(nil, repo, nil, cache)
	called := false
	err = s.WithDailyPurchaseForAccount(context.Background(), "user", "account", "2026-10-22", func(context.Context, float64) error { called = true; return nil })
	if err == nil || called || !strings.Contains(err.Error(), "cache member missing") {
		t.Fatalf("missing member accepted: %v consumer=%v", err, called)
	}
}

// 采集起点和结束复核的缓存失败均必须阻断，不得变成空站点零成本。
type failingDailyCache struct {
	*fakeSiteCache
	calls  int
	failAt int
}

func (c *failingDailyCache) ListByUser(ctx context.Context, userID string) ([]*Site, error) {
	c.calls++
	if c.calls == c.failAt {
		return nil, errors.New("cache unavailable")
	}
	return c.fakeSiteCache.ListByUser(ctx, userID)
}

func TestDailyPurchaseCacheFailure(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			s, cache := dailyTestService(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"total_actual_cost":18}}`) })
			s.cache = &failingDailyCache{fakeSiteCache: cache, failAt: failAt}
			if value, err := dailyPurchaseForTest(s, context.Background(), "user", "account", "2026-10-22"); err == nil || value != 0 {
				t.Fatalf("value=%v err=%v", value, err)
			}
		})
	}
}

func TestDailyPurchaseWorkspaceCleanupReleasesGeneration(t *testing.T) {
	s, cache := dailyTestService(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"total_actual_cost":18}}`) })
	site, _ := cache.Get(context.Background(), "one")
	site.RechargeRate = 2
	if err := s.setCachedSite(context.Background(), site); err != nil {
		t.Fatal(err)
	}
	workspace := refreshWorkspace{userID: "user", adminAccountID: "account"}
	if s.configGenerations[workspace] == 0 {
		t.Fatal("configuration generation not recorded")
	}
	if err := s.CleanupDeletedWorkspaceSites(context.Background(), "user", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	if _, exists := s.configGenerations[workspace]; exists {
		t.Fatal("deleted workspace generation retained")
	}
	if s.cacheEpoch == 0 {
		t.Fatal("cleanup did not invalidate collectors")
	}
}

func TestDatedUsagePublicEntryPoints(t *testing.T) {
	for _, platform := range []Platform{PlatformSub2API, PlatformNewAPI} {
		t.Run(string(platform), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if platform == PlatformSub2API {
					if r.URL.Query().Get("timezone") != "Asia/Shanghai" {
						t.Errorf("missing timezone %s", r.URL)
					}
					fmt.Fprint(w, `{"data":{"total_actual_cost":0}}`)
				} else {
					fmt.Fprint(w, `{"data":{"quota":0}}`)
				}
			}))
			defer server.Close()
			p := NewPlatformService(NewHTTPClient(server.Client()))
			session := Session{Platform: platform, BaseURL: server.URL, AccessToken: "token", UserID: "1"}
			if value, err := p.FetchAdminUsageStats(session, "2026-10-22", "2026-10-22"); err != nil || value != 0 {
				t.Fatalf("old API %v %v", value, err)
			}
			if platform == PlatformSub2API {
				if value, err := p.FetchSub2APIAdminUsageStats(session, "2026-10-22", "2026-10-22"); err != nil || value != 0 {
					t.Fatalf("sub2api API %v %v", value, err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := p.FetchAdminUsageStatsContext(ctx, session, "2026-10-22", "2026-10-22"); !errors.Is(err, context.Canceled) {
				t.Fatalf("context API %v", err)
			}
		})
	}
}
