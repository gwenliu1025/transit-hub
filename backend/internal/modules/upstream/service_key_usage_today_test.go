package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeSiteCache 鏄?SiteCache 鐨勫唴瀛樺疄鐜帮紝浠呬緵娴嬭瘯浣跨敤銆?
type fakeSiteCache struct {
	sites map[string]*Site
}

func newFakeSiteCache() *fakeSiteCache {
	return &fakeSiteCache{sites: map[string]*Site{}}
}

func (f *fakeSiteCache) add(site *Site) {
	stored := *site
	f.sites[site.ID] = &stored
}

func (f *fakeSiteCache) Get(ctx context.Context, id string) (*Site, error) {
	site, ok := f.sites[id]
	if !ok {
		return nil, nil
	}
	stored := *site
	return &stored, nil
}

func (f *fakeSiteCache) Set(ctx context.Context, site *Site) error {
	stored := *site
	f.sites[site.ID] = &stored
	return nil
}

func (f *fakeSiteCache) Delete(ctx context.Context, id string, userID string) error {
	delete(f.sites, id)
	return nil
}

func (f *fakeSiteCache) ListByUser(ctx context.Context, userID string) ([]*Site, error) {
	result := make([]*Site, 0)
	for _, site := range f.sites {
		if site.UserID == userID {
			stored := *site
			result = append(result, &stored)
		}
	}
	return result, nil
}

func (f *fakeSiteCache) Flush(ctx context.Context) error { return nil }

// fakeAccountResolver 鏄?AdminAccountResolver 鐨勫唴瀛樺疄鐜帮紝鎸?userID 杩斿洖鍥哄畾鐨勫綋鍓嶅伐浣滃尯銆?
type fakeAccountResolver struct {
	current map[string]string
}

func (f *fakeAccountResolver) RequireCurrentID(ctx context.Context, userID string) (string, error) {
	id, ok := f.current[userID]
	if !ok {
		return "", newRequestError("admin.adminAccounts.errors.noCurrentAccount", "")
	}
	return id, nil
}

// sub2APIKeyServer 鍚姩涓€涓渶灏?sub2api httptest server锛氬崟椤?key 鍒楄〃 + 鍥哄畾浠婃棩娑堣垂銆?
func sub2APIKeyServer(t *testing.T, keyID string, keyName string, groupName string, todayCost float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/keys":
			writeJSON(w, map[string]any{"data": []map[string]any{
				{"id": keyID, "name": keyName, "group": map[string]any{"name": groupName}},
			}})
		case "/api/v1/usage/stats":
			writeJSON(w, map[string]any{"data": map[string]any{"total_actual_cost": todayCost}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
}

func newTestSite(id, userID, adminAccountID string, rechargeRate float64, session *Session) *Site {
	return &Site{
		ID:             id,
		UserID:         userID,
		AdminAccountID: adminAccountID,
		Name:           "site-" + id,
		Platform:       PlatformSub2API,
		RechargeRate:   rechargeRate,
		Status:         StatusConnected,
		Session:        session,
	}
}

// TestServiceKeyUsageToday_WorkspaceIsolation 瑕嗙洊娴嬭瘯瑕佹眰 1锛氬彧杩斿洖褰撳墠宸ヤ綔鍖虹珯鐐圭殑鏁版嵁锛?
// 鍏朵粬宸ヤ綔鍖猴紙鍗充娇鍚屼竴鐢ㄦ埛鍚嶄笅锛夌殑绔欑偣涓嶅緱娣峰叆缁撴灉銆?
func TestServiceKeyUsageToday_WorkspaceIsolation(t *testing.T) {
	serverA := sub2APIKeyServer(t, "1", "key-a", "vip", 10)
	defer serverA.Close()
	serverB := sub2APIKeyServer(t, "2", "key-b", "vip", 20)
	defer serverB.Close()

	cache := newFakeSiteCache()
	cache.add(newTestSite("site-a", "user-1", "acc-1", 2, &Session{Platform: PlatformSub2API, BaseURL: serverA.URL, AccessToken: "token"}))
	cache.add(newTestSite("site-b", "user-1", "acc-2", 2, &Session{Platform: PlatformSub2API, BaseURL: serverB.URL, AccessToken: "token"}))

	svc := NewService(newTestPlatformService(http.DefaultClient), nil, nil, cache)
	svc.SetAdminAccountResolver(&fakeAccountResolver{current: map[string]string{"user-1": "acc-1"}})

	items, err := svc.KeyUsageToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item from acc-1 only, got %d: %+v", len(items), items)
	}
	if items[0].SiteID != "site-a" {
		t.Fatalf("expected site-a data, workspace isolation leaked: %+v", items[0])
	}
}

// TestServiceKeyUsageToday_SkipsRechargeRateZero 楠岃瘉 rechargeRate <= 0 鐨勭珯鐐硅鏁翠綋璺宠繃锛?
// 涓?dashboard.MetricsService.LiveMetrics() 涓?todayPurchase 鐨勫彛寰勪繚鎸佷竴鑷淬€?
func TestServiceKeyUsageToday_SkipsRechargeRateZero(t *testing.T) {
	server := sub2APIKeyServer(t, "1", "key-a", "vip", 10)
	defer server.Close()

	cache := newFakeSiteCache()
	cache.add(newTestSite("site-a", "user-1", "acc-1", 0, &Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "token"}))

	svc := NewService(newTestPlatformService(http.DefaultClient), nil, nil, cache)
	svc.SetAdminAccountResolver(&fakeAccountResolver{current: map[string]string{"user-1": "acc-1"}})

	items, err := svc.KeyUsageToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items for rechargeRate<=0 site, got %+v", items)
	}
}

// TestServiceKeyUsageToday_FiltersZeroCostAndAppliesRechargeRate 瑕嗙洊娴嬭瘯瑕佹眰 2 鍜屽瓧娈垫崲绠楋細
// 0 娑堣垂鐨?key 琚繃婊わ紱鍓╀綑 key 鐨?todayAmount = 涓婃父鍘熷閲戦 * rechargeRate銆?
func TestServiceKeyUsageToday_FiltersZeroCostAndAppliesRechargeRate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/keys":
			writeJSON(w, map[string]any{"data": []map[string]any{
				{"id": "1", "name": "zero-cost-key", "group": map[string]any{"name": "vip"}},
				{"id": "2", "name": "prod-key", "group": map[string]any{"name": "vip"}},
			}})
		case "/api/v1/usage/stats":
			apiKeyID := r.URL.Query().Get("api_key_id")
			cost := 0.0
			if apiKeyID == "2" {
				cost = 33.3
			}
			writeJSON(w, map[string]any{"data": map[string]any{"total_actual_cost": cost}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	cache := newFakeSiteCache()
	cache.add(newTestSite("site-a", "user-1", "acc-1", 2, &Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "token"}))

	svc := NewService(newTestPlatformService(http.DefaultClient), nil, nil, cache)
	svc.SetAdminAccountResolver(&fakeAccountResolver{current: map[string]string{"user-1": "acc-1"}})

	items, err := svc.KeyUsageToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item (zero-cost key filtered out), got %d: %+v", len(items), items)
	}
	if items[0].KeyName != "prod-key" {
		t.Fatalf("unexpected key survived filtering: %+v", items[0])
	}
	if items[0].RawAmount != 33.3 {
		t.Errorf("rawAmount = %.2f, want 33.30", items[0].RawAmount)
	}
	if items[0].TodayAmount != 66.6 {
		t.Errorf("todayAmount = %.2f, want 66.60 (33.3 * rechargeRate 2)", items[0].TodayAmount)
	}
}

// TestServiceKeyUsageToday_ExternalErrorFailsClosed 瑕嗙洊娴嬭瘯瑕佹眰 9锛?
// 澶栭儴骞冲彴璇锋眰澶辫触鏃舵暣涓柟娉曡繑鍥為敊璇紝涓嶈兘鎶婂け璐ョ珯鐐规倓鎮勫綋 0 澶勭悊銆?
func TestServiceKeyUsageToday_ExternalErrorFailsClosed(t *testing.T) {
	failingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failingServer.Close()

	cache := newFakeSiteCache()
	cache.add(newTestSite("site-a", "user-1", "acc-1", 2, &Session{Platform: PlatformSub2API, BaseURL: failingServer.URL, AccessToken: "token"}))

	svc := NewService(newTestPlatformService(http.DefaultClient), nil, nil, cache)
	svc.SetAdminAccountResolver(&fakeAccountResolver{current: map[string]string{"user-1": "acc-1"}})

	_, err := svc.KeyUsageToday(context.Background(), "user-1")
	if err == nil {
		t.Fatal("expected error when upstream platform request fails, got nil (silently treated as 0)")
	}
}

// TestServiceKeyUsageToday_PartialFailureKeepsSuccessfulItems 楠岃瘉澶氱珯鐐归噰闆嗘椂锛?
// 鍗曚釜绔欑偣澶辫触涓嶄細涓㈠純鍏朵粬绔欑偣宸茬粡鍙栧緱鐨勬暟鎹紝鍚屾椂杩斿洖鍙瘑鍒殑澶辫触绔欑偣璁℃暟銆?
func TestServiceKeyUsageToday_PartialFailureKeepsSuccessfulItems(t *testing.T) {
	successServer := sub2APIKeyServer(t, "1", "working-key", "vip", 10)
	defer successServer.Close()
	failingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failingServer.Close()

	cache := newFakeSiteCache()
	cache.add(newTestSite("site-success", "user-1", "acc-1", 2, &Session{Platform: PlatformSub2API, BaseURL: successServer.URL, AccessToken: "token"}))
	cache.add(newTestSite("site-failure", "user-1", "acc-1", 2, &Session{Platform: PlatformSub2API, BaseURL: failingServer.URL, AccessToken: "token"}))

	svc := NewService(newTestPlatformService(http.DefaultClient), nil, nil, cache)
	svc.SetAdminAccountResolver(&fakeAccountResolver{current: map[string]string{"user-1": "acc-1"}})

	items, err := svc.KeyUsageToday(context.Background(), "user-1")
	var collectionErr *KeyUsageCollectionError
	if !errors.As(err, &collectionErr) {
		t.Fatalf("expected KeyUsageCollectionError, got %v", err)
	}
	if collectionErr.FailedSites != 1 || collectionErr.TotalSites != 2 {
		t.Fatalf("unexpected failure counts: %+v", collectionErr)
	}
	if len(items) != 1 || items[0].SiteID != "site-success" || items[0].TodayAmount != 20 {
		t.Fatalf("successful site data should be preserved, got %+v", items)
	}
}

// TestServiceBalanceBreakdown_SortsDescendingWithUnknownBalanceLast 瑕嗙洊娴嬭瘯瑕佹眰 7銆?锛?
// 鎸?balance 闄嶅簭鎺掑簭锛屾湭鐭ヤ綑棰濓紙rechargeRate<=0锛夌珯鐐规帓鍦ㄦ渶鍚庯紱total 绛変簬宸茬煡浣欓涔嬪拰锛?
// 涓?LiveMetrics 涓?upstreamBalance 鐨勮绠楀彛寰勪竴鑷达紙rechargeRate<=0 绔欑偣涓嶈鍏?total锛夈€?
func TestServiceBalanceBreakdown_SortsDescendingWithUnknownBalanceLast(t *testing.T) {
	highBalance := 50.0
	lowBalance := 5.0

	cache := newFakeSiteCache()
	siteHigh := newTestSite("site-high", "user-1", "acc-1", 2, nil)
	siteHigh.Metrics.Balance.Value = &highBalance
	cache.add(siteHigh)

	siteLow := newTestSite("site-low", "user-1", "acc-1", 2, nil)
	siteLow.Metrics.Balance.Value = &lowBalance
	cache.add(siteLow)

	siteUnknown := newTestSite("site-unknown", "user-1", "acc-1", 0, nil) // rechargeRate<=0 => 鏈煡浣欓
	cache.add(siteUnknown)

	svc := NewService(newTestPlatformService(http.DefaultClient), nil, nil, cache)
	svc.SetAdminAccountResolver(&fakeAccountResolver{current: map[string]string{"user-1": "acc-1"}})

	items, err := svc.BalanceBreakdown(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected all 3 sites (unknown balance sites are shown, not omitted), got %d", len(items))
	}

	// Service 灞備笉鎺掑簭锛堟帓搴忕敱 dashboard.MetricsService.UpstreamBalanceBreakdown 瀹屾垚锛夛紝
	// 杩欓噷鍙牎楠屾暟鎹湰韬細宸茬煡浣欓宸叉崲绠椾负 CNY锛屾湭鐭ヤ綑棰濅负 nil銆?
	byID := map[string]*BalanceBreakdownItem{}
	for i := range items {
		byID[items[i].SiteID] = &items[i]
	}
	if byID["site-high"].Balance == nil || *byID["site-high"].Balance != 100 {
		t.Errorf("site-high balance = %v, want 100 (50 * rechargeRate 2)", byID["site-high"].Balance)
	}
	if byID["site-low"].Balance == nil || *byID["site-low"].Balance != 10 {
		t.Errorf("site-low balance = %v, want 10 (5 * rechargeRate 2)", byID["site-low"].Balance)
	}
	if byID["site-unknown"].Balance != nil {
		t.Errorf("site-unknown balance = %v, want nil (rechargeRate<=0 => unknown)", byID["site-unknown"].Balance)
	}
}
