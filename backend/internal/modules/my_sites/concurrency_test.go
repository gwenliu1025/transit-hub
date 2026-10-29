package my_sites

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"transithub/backend/internal/modules/upstream"
)

// sessionOnlyStateRepo 模拟生产仓储的定向会话写入：只改会话与身份列，不触碰 mappings。
type sessionOnlyStateRepo struct {
	*testStateRepo
	sessionSaves int
}

func (r *sessionOnlyStateRepo) SaveSession(ctx context.Context, userID, adminAccountID, baseURL, email string, session upstream.Session) error {
	r.sessionSaves++
	if r.state == nil {
		r.state = &State{UserID: userID, AdminAccountID: adminAccountID, Mappings: []GroupMapping{}}
	}
	r.state.BaseURL, r.state.Email, r.state.Session = baseURL, email, session
	return nil
}

func TestValidatedStateRefreshWritesSessionOnlyAndKeepsConcurrentMappings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"access_token": "rotated-token", "refresh_token": "rotated-refresh", "expires_in": 3600}})
		case "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"role": "admin"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	expiresAt := time.Now().Add(-time.Minute).UnixMilli()
	stale := &State{UserID: "user-1", AdminAccountID: "admin-1", Mappings: []GroupMapping{}, Session: upstream.Session{
		Platform: upstream.PlatformSub2API, BaseURL: server.URL, AccessToken: "old-token", RefreshToken: "old-refresh", TokenType: "Bearer", ExpiresAt: &expiresAt,
	}}
	repo := &sessionOnlyStateRepo{testStateRepo: &testStateRepo{state: cloneState(stale)}}
	// 读取之后、刷新完成之前，另一个请求保存了新映射。
	repo.state.Mappings = []GroupMapping{{OwnGroup: "vip", UpstreamTargets: []UpstreamGroupRef{{SiteID: "site-1", GroupName: "pro"}}}}
	service := NewService(repo, upstream.NewPlatformService(upstream.NewHTTPClient(server.Client())), testUpstreamLookup{})

	validated, err := service.validatedState(context.Background(), stale)
	if err != nil {
		t.Fatalf("validatedState returned error: %v", err)
	}
	if validated.Session.AccessToken != "rotated-token" || repo.state.Session.AccessToken != "rotated-token" {
		t.Fatalf("刷新后的会话必须持久化: %+v", repo.state.Session)
	}
	if repo.sessionSaves != 1 || len(repo.saves) != 0 {
		t.Fatalf("刷新只能定向写会话，不能整行保存: sessionSaves=%d saves=%d", repo.sessionSaves, len(repo.saves))
	}
	if len(repo.state.Mappings) != 1 || repo.state.Mappings[0].OwnGroup != "vip" {
		t.Fatalf("并发保存的映射被旧数据覆盖: %+v", repo.state.Mappings)
	}
}

func TestSyncAdminSessionDoesNotRewriteMappings(t *testing.T) {
	repo := &sessionOnlyStateRepo{testStateRepo: &testStateRepo{state: &State{UserID: "user-1", AdminAccountID: "admin-1", Mappings: []GroupMapping{{OwnGroup: "vip"}}}}}
	service := NewService(repo, nil, testUpstreamLookup{})
	session := upstream.Session{Platform: upstream.PlatformSub2API, BaseURL: "https://admin.example", AccessToken: "token"}
	if err := service.SyncAdminSession(context.Background(), "user-1", "admin-1", session, "ops@example.com"); err != nil {
		t.Fatal(err)
	}
	if repo.sessionSaves != 1 || len(repo.saves) != 0 {
		t.Fatalf("登录同步只能定向写会话: sessionSaves=%d saves=%d", repo.sessionSaves, len(repo.saves))
	}
	if repo.state.Email != "ops@example.com" || repo.state.BaseURL != "https://admin.example" || len(repo.state.Mappings) != 1 {
		t.Fatalf("会话同步结果不符或映射丢失: %+v", repo.state)
	}
}

func TestUpdateAdminGroupMultiplierSerializesReadModifyWritePerAdminSite(t *testing.T) {
	var mu sync.Mutex
	inFlight, maxInFlight, puts := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/admin/groups/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			// GET 到 PUT 之间是一次读改写窗口，窗口重叠即可能丢失更新。
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"name": "g", "platform": "openai", "status": "active"}})
		case http.MethodPut:
			mu.Lock()
			inFlight--
			puts++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	defer server.Close()

	service := NewService(&testStateRepo{}, upstream.NewPlatformService(upstream.NewHTTPClient(server.Client())), testUpstreamLookup{})
	session := upstream.Session{Platform: upstream.PlatformSub2API, BaseURL: server.URL, AccessToken: "token", TokenType: "Bearer"}
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- service.UpdateAdminGroupMultiplier(session, upstream.AdminGroupInfo{ID: strconv.Itoa(i + 1), Name: "g"}, 1.5)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("更新倍率失败: %v", err)
		}
	}
	if maxInFlight != 1 || puts != 6 {
		t.Fatalf("同一管理端站点的倍率读改写必须串行: maxInFlight=%d puts=%d", maxInFlight, puts)
	}
}

func TestAdminSiteLockKeyNormalizesSameSite(t *testing.T) {
	base := upstream.Session{Platform: upstream.PlatformNewAPI, BaseURL: "https://admin.example.com"}
	for _, variant := range []string{"https://admin.example.com/", " HTTPS://Admin.Example.com ", "https://admin.example.com//"} {
		session := base
		session.BaseURL = variant
		if adminSiteLockKey(session) != adminSiteLockKey(base) {
			t.Fatalf("同一站点必须共用锁键: %q", variant)
		}
	}
	other := base
	other.BaseURL = "https://other.example.com"
	if adminSiteLockKey(other) == adminSiteLockKey(base) {
		t.Fatal("不同站点不应互相阻塞")
	}
}

func TestKeyedLocksReleaseAndReuseKeys(t *testing.T) {
	var tryLocks keyedTryLock
	release, ok := tryLocks.tryLock("target")
	if !ok {
		t.Fatal("首次占位必须成功")
	}
	if _, busy := tryLocks.tryLock("target"); busy {
		t.Fatal("持有期间同一目标必须拒绝")
	}
	if other, ok := tryLocks.tryLock("other"); !ok {
		t.Fatal("不同目标互不影响")
	} else {
		other()
	}
	release()
	if again, ok := tryLocks.tryLock("target"); !ok {
		t.Fatal("释放后必须可再次占位")
	} else {
		again()
	}

	var mutex keyedMutex
	unlock := mutex.lock("site")
	unlock()
	if len(mutex.locks) != 0 {
		t.Fatalf("空闲锁必须回收: %d", len(mutex.locks))
	}
}
