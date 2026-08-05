package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// availableGroupsFixture 鏄悇娴嬭瘯鍏辩敤鐨?/api/v1/groups/available 鍝嶅簲锛?
// default(id=1) 涓?vip(id=2) 浼氬湪 /groups/rates 涓懡涓笓灞炲€嶇巼锛宻table(id=3) 涓嶄細銆?
func availableGroupsFixture(w http.ResponseWriter) {
	writeJSON(w, map[string]any{
		"data": []map[string]any{
			{"id": 1, "name": "default", "platform": "openai", "rate_multiplier": 1.0},
			{"id": 2, "name": "vip", "platform": "openai", "rate_multiplier": 2.0},
			{"id": 3, "name": "stable", "platform": "claude", "rate_multiplier": 3.0},
		},
	})
}

// TestFetchSub2APIAdminGroups_DedicatedMultiplier 楠岃瘉 FetchSub2APIAdminGroups 鎸夊垎缁?ID
// 鍚堝苟 /groups/rates 涓撳睘鍊嶇巼锛氬懡涓殑鍒嗙粍浣跨敤涓撳睘鍊嶇巼瑕嗙洊锛岀己澶?ID 鐨勫垎缁勪繚鐣欓粯璁ゅ€嶇巼锛?
// rates 涓嚭鐜扮殑鏈煡 ID 涓嶄細琚柊澧炲埌缁撴灉閲屻€?
func TestFetchSub2APIAdminGroups_DedicatedMultiplier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/groups/available":
			availableGroupsFixture(w)
		case "/api/v1/groups/rates":
			writeJSON(w, map[string]any{"data": map[string]any{
				"1":   0.8,
				"2":   1.5,
				"999": 9.9,
			}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "token"}

	groups, err := service.FetchSub2APIAdminGroups(session)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups (unknown id 999 must not be added), got %d", len(groups))
	}

	byName := map[string]GroupInfo{}
	for _, g := range groups {
		byName[g.Name] = g
	}

	def := byName["default"]
	if def.Multiplier == nil || *def.Multiplier != 0.8 {
		t.Errorf("default final multiplier = %v, want 0.8", def.Multiplier)
	}
	if !def.HasDedicatedMultiplier {
		t.Errorf("default should have dedicated multiplier flag set")
	}
	if def.DefaultMultiplier == nil || *def.DefaultMultiplier != 1.0 {
		t.Errorf("default DefaultMultiplier = %v, want 1.0", def.DefaultMultiplier)
	}
	if def.DedicatedMultiplier == nil || *def.DedicatedMultiplier != 0.8 {
		t.Errorf("default DedicatedMultiplier = %v, want 0.8", def.DedicatedMultiplier)
	}

	vip := byName["vip"]
	if vip.Multiplier == nil || *vip.Multiplier != 1.5 {
		t.Errorf("vip final multiplier = %v, want 1.5", vip.Multiplier)
	}
	if !vip.HasDedicatedMultiplier {
		t.Errorf("vip should have dedicated multiplier flag set")
	}

	stable := byName["stable"]
	if stable.Multiplier == nil || *stable.Multiplier != 3.0 {
		t.Errorf("stable final multiplier = %v, want 3.0 (no rates entry, keep default)", stable.Multiplier)
	}
	if stable.HasDedicatedMultiplier {
		t.Errorf("stable should not have dedicated multiplier flag set")
	}
	if stable.DedicatedMultiplier != nil {
		t.Errorf("stable DedicatedMultiplier should be nil, got %v", *stable.DedicatedMultiplier)
	}
}

// TestFetchSub2APIAdminGroups_RatesMissingID 鍗曠嫭楠岃瘉锛?groups/rates 缂哄け鏌愪釜鍒嗙粍 ID 鏃讹紝
// 璇ュ垎缁勫繀椤讳繚鐣?/groups/available 鐨勯粯璁ゅ€嶇巼锛屼笉鍙楀叾瀹冨垎缁勮鐩栧奖鍝嶃€?
func TestFetchSub2APIAdminGroups_RatesMissingID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/groups/available":
			availableGroupsFixture(w)
		case "/api/v1/groups/rates":
			writeJSON(w, map[string]any{"data": map[string]any{"1": 0.5}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "token"}

	groups, err := service.FetchSub2APIAdminGroups(session)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byName := map[string]GroupInfo{}
	for _, g := range groups {
		byName[g.Name] = g
	}
	if vip := byName["vip"]; vip.Multiplier == nil || *vip.Multiplier != 2.0 {
		t.Errorf("vip final multiplier = %v, want 2.0 (kept default, no rates entry)", vip.Multiplier)
	}
	if stable := byName["stable"]; stable.Multiplier == nil || *stable.Multiplier != 3.0 {
		t.Errorf("stable final multiplier = %v, want 3.0 (kept default, no rates entry)", stable.Multiplier)
	}
}

// TestFetchSub2APIAdminGroups_RatesUnknownIDNotAdded 楠岃瘉 /groups/rates 涓嚭鐜?
// /groups/available 涓嶅寘鍚殑鍒嗙粍 ID 鏃朵笉浼氭柊澧炲垎缁勬潯鐩紙缂哄皯 name/platform 绛夊睍绀哄瓧娈碉級銆?
func TestFetchSub2APIAdminGroups_RatesUnknownIDNotAdded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/groups/available":
			availableGroupsFixture(w)
		case "/api/v1/groups/rates":
			writeJSON(w, map[string]any{"data": map[string]any{"42": 5.0}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "token"}

	groups, err := service.FetchSub2APIAdminGroups(session)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}
	for _, g := range groups {
		if g.ID == "42" {
			t.Fatalf("unknown rates-only id 42 must not be added as a group")
		}
	}
}

// TestFetchSub2APIAdminGroups_RatesUnavailable 楠岃瘉 /groups/rates 杩斿洖 404/闈?2xx 鏃?
// 锛堟ā鎷熸棫鐗?sub2api 灏氭湭鏀寔璇ユ帴鍙ｏ級锛孎etchSub2APIAdminGroups 浠嶅簲鎴愬姛杩斿洖 available
// 榛樿鍊嶇巼鐨勫垎缁勫垪琛紝涓嶅簲鏁翠綋澶辫触銆?
func TestFetchSub2APIAdminGroups_RatesUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/groups/available":
			availableGroupsFixture(w)
		case "/api/v1/groups/rates":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "token"}

	groups, err := service.FetchSub2APIAdminGroups(session)
	if err != nil {
		t.Fatalf("expected FetchSub2APIAdminGroups to succeed when /groups/rates is unavailable, got err: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups from available fallback, got %d", len(groups))
	}
	for _, g := range groups {
		if g.HasDedicatedMultiplier {
			t.Errorf("group %s should not have dedicated multiplier when rates endpoint is unavailable", g.Name)
		}
		if g.Multiplier == nil || g.DefaultMultiplier == nil || *g.Multiplier != *g.DefaultMultiplier {
			t.Errorf("group %s multiplier should equal default multiplier when rates endpoint is unavailable", g.Name)
		}
	}
}

// TestFetchSub2APIMetrics_UsesOverriddenMultiplier 楠岃瘉 fetchSub2APIMetrics 鐨?
// Metrics.Groups 澶嶇敤鍚屼竴濂楀悎骞堕€昏緫锛屼娇鐢ㄨ鐩栧悗鐨勬渶缁堢敓鏁堝€嶇巼銆?
func TestFetchSub2APIMetrics_UsesOverriddenMultiplier(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("鍔犺浇鍖椾含鏃堕棿鏃跺尯澶辫触: %v", err)
	}
	today := time.Now().In(shanghai).Format("2006-01-02")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			writeJSON(w, map[string]any{"data": map[string]any{"balance": 10.0, "total_recharged": 20.0}})
		case "/api/v1/usage/dashboard/stats":
			writeJSON(w, map[string]any{"data": map[string]any{"today_actual_cost": 99.0, "total_actual_cost": 30.0}})
		case "/api/v1/usage/stats":
			if got := r.URL.Query().Get("start_date"); got != today {
				t.Errorf("start_date = %q, want %q", got, today)
			}
			if got := r.URL.Query().Get("end_date"); got != today {
				t.Errorf("end_date = %q, want %q", got, today)
			}
			if got := r.URL.Query().Get("timezone"); got != "Asia/Shanghai" {
				t.Errorf("timezone = %q, want Asia/Shanghai", got)
			}
			writeJSON(w, map[string]any{"data": map[string]any{"total_actual_cost": 1.25}})
		case "/api/v1/groups/available":
			availableGroupsFixture(w)
		case "/api/v1/groups/rates":
			writeJSON(w, map[string]any{"data": map[string]any{"1": 0.8}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "token"}

	metrics, err := service.fetchSub2APIMetrics(session)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.TodayConsume.Value == nil || *metrics.TodayConsume.Value != 1.25 {
		t.Fatalf("TodayConsume = %v, want 1.25 from /api/v1/usage/stats", metrics.TodayConsume.Value)
	}
	byName := map[string]GroupInfo{}
	for _, g := range metrics.Groups {
		byName[g.Name] = g
	}
	def := byName["default"]
	if def.Multiplier == nil || *def.Multiplier != 0.8 {
		t.Errorf("Metrics.Groups default multiplier = %v, want 0.8 (overridden)", def.Multiplier)
	}
	if !def.HasDedicatedMultiplier {
		t.Errorf("Metrics.Groups default should have dedicated multiplier flag set")
	}
	stable := byName["stable"]
	if stable.Multiplier == nil || *stable.Multiplier != 3.0 {
		t.Errorf("Metrics.Groups stable multiplier = %v, want 3.0 (kept default)", stable.Multiplier)
	}
}

// TestSub2APIGroupRateOverrides 瑕嗙洊 sub2APIGroupRateOverrides helper 瀵逛笂娓稿嚑绉嶅父瑙?
// payload 褰㈡€佺殑瑙ｆ瀽锛屼互鍙婃棤鏁堟潯鐩殑瀹归敊琛屼负銆?
func TestSub2APIGroupRateOverrides(t *testing.T) {
	t.Run("object map wrapped in data", func(t *testing.T) {
		got := sub2APIGroupRateOverrides(map[string]any{"data": map[string]any{"1": 0.8, "2": 1.2}})
		if got["1"] != 0.8 || got["2"] != 1.2 {
			t.Errorf("unexpected overrides: %v", got)
		}
	})

	t.Run("array of objects wrapped in data with snake_case fields", func(t *testing.T) {
		got := sub2APIGroupRateOverrides(map[string]any{"data": []any{
			map[string]any{"group_id": 1.0, "rate_multiplier": 0.8},
		}})
		if got["1"] != 0.8 {
			t.Errorf("unexpected overrides: %v", got)
		}
	})

	t.Run("unwrapped array with camelCase string fields", func(t *testing.T) {
		got := sub2APIGroupRateOverrides([]any{
			map[string]any{"groupId": "1", "rateMultiplier": "0.8"},
		})
		if got["1"] != 0.8 {
			t.Errorf("unexpected overrides: %v", got)
		}
	})

	t.Run("invalid entries are ignored without affecting others", func(t *testing.T) {
		got := sub2APIGroupRateOverrides(map[string]any{"data": []any{
			map[string]any{"group_id": 1.0, "rate_multiplier": 0.8},
			map[string]any{"rate_multiplier": 1.0},                             // 缂?ID锛屽拷鐣?
			map[string]any{"group_id": 2.0, "rate_multiplier": "not-a-number"}, // 鏃犳晥鍊嶇巼锛屽拷鐣?
		}})
		if len(got) != 1 || got["1"] != 0.8 {
			t.Errorf("unexpected overrides: %v", got)
		}
	})
}
