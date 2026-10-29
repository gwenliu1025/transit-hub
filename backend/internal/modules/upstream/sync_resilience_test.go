package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// 定时同步遇到缓存读取失败时也必须排下一次同步，不能让该站点的自动刷新链永久中断。
func TestSyncReschedulesAfterCacheFailure(t *testing.T) {
	site := Site{ID: "one", UserID: "user", AdminAccountID: "account", Session: &Session{Platform: PlatformSub2API, AccessToken: "token"}}
	repo := &refreshSchedulerRepository{sites: []Site{site}}
	cache := &failingGetCache{fakeSiteCache: newFakeSiteCache()}
	cache.add(&site)
	s := NewService(nil, repo, nil, cache)
	s.SetWorkspaceRefreshConfig("user", "account", RefreshConfig{Enabled: true, Interval: time.Hour})
	s.mu.Lock()
	s.clearTimerLocked("one")
	s.mu.Unlock()

	cache.fail = true
	if _, err := s.sync(context.Background(), "one"); err == nil {
		t.Fatal("cache failure must be reported")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.timers["one"]; !ok {
		t.Fatal("refresh chain stopped after a transient cache failure")
	}
	s.clearAllTimersLocked()
}

type failingGetCache struct {
	*fakeSiteCache
	fail bool
}

func (c *failingGetCache) Get(ctx context.Context, id string) (*Site, error) {
	if c.fail {
		return nil, errors.New("redis unavailable")
	}
	return c.fakeSiteCache.Get(ctx, id)
}

// 续期成功但后续指标请求失败时，新令牌仍要保存；否则轮换型 refresh token 丢失后站点永久失效。
func TestSyncKeepsRefreshedSessionWhenMetricsFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/refresh" {
			fmt.Fprint(w, `{"data":{"access_token":"fresh","refresh_token":"rt-2","expires_in":3600}}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	expired := time.Now().Add(-time.Minute).UnixMilli()
	cache := newFakeSiteCache()
	cache.add(newTestSite("one", "user", "account", 1, &Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "old", RefreshToken: "rt-1", ExpiresAt: &expired}))
	s := NewService(newTestPlatformService(server.Client()), nil, nil, cache)

	if _, err := s.sync(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	stored, _ := cache.Get(context.Background(), "one")
	if stored.Status != StatusError {
		t.Fatalf("status=%s", stored.Status)
	}
	if stored.Session.AccessToken != "fresh" || stored.Session.RefreshToken != "rt-2" {
		t.Fatalf("refreshed session lost: %+v", stored.Session)
	}
}

// New API 今日消费读取失败时，同步必须失败并保留上一次的值，不能静默变成“今日消费为空”。
func TestNewAPISyncKeepsPreviousConsumeWhenStatFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/self":
			writeJSON(w, map[string]any{"success": true, "data": map[string]any{"quota": 1000, "used_quota": 10, "group": "default"}})
		case "/api/log/self/stat":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			writeJSON(w, map[string]any{"success": true, "data": map[string]any{}})
		}
	}))
	defer server.Close()
	synced := time.Now().Add(-time.Minute).UnixMilli()
	site := newTestSite("one", "user", "account", 1, &Session{Platform: PlatformNewAPI, BaseURL: server.URL, AccessToken: "token", UserID: "1"})
	site.Platform = PlatformNewAPI
	site.Metrics.TodayConsume.Value = ptrFloat(7)
	site.LastSyncedAt = &synced
	cache := newFakeSiteCache()
	cache.add(site)
	s := NewService(newTestPlatformService(server.Client()), nil, nil, cache)

	if _, err := s.sync(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	stored, _ := cache.Get(context.Background(), "one")
	if stored.Status != StatusError || stored.Metrics.TodayConsume.Value == nil || *stored.Metrics.TodayConsume.Value != 7 || *stored.LastSyncedAt != synced {
		t.Fatalf("stat failure overwrote previous consume: status=%s metrics=%+v", stored.Status, stored.Metrics.TodayConsume)
	}
}

// New API 的“今日”必须是北京时间自然日，不能跟随服务器（生产容器为 UTC）时区。
func TestNewAPITodayWindowUsesBeijingDay(t *testing.T) {
	var gotStart, gotEnd int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/log/self/stat":
			gotStart, _ = strconv.ParseInt(r.URL.Query().Get("start_timestamp"), 10, 64)
			gotEnd, _ = strconv.ParseInt(r.URL.Query().Get("end_timestamp"), 10, 64)
			writeJSON(w, map[string]any{"success": true, "data": map[string]any{"quota": 500000}})
		default:
			writeJSON(w, map[string]any{"success": true, "data": map[string]any{"quota": 0}})
		}
	}))
	defer server.Close()
	p := newTestPlatformService(server.Client())
	if _, err := p.FetchMetrics(Session{Platform: PlatformNewAPI, BaseURL: server.URL, AccessToken: "token", UserID: "1"}); err != nil {
		t.Fatal(err)
	}
	beijing := time.FixedZone("CST", 8*3600)
	start := time.Unix(gotStart, 0).In(beijing)
	if start.Hour() != 0 || start.Minute() != 0 || start.Format("2006-01-02") != time.Now().In(beijing).Format("2006-01-02") {
		t.Fatalf("start=%s", start)
	}
	if gotEnd-gotStart != 24*3600-1 {
		t.Fatalf("window=%d..%d", gotStart, gotEnd)
	}
}
