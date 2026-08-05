package upstream

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestFetchKeyUsageToday_Sub2API_PaginatesKeysAndFiltersZeroCost 瑕嗙洊娴嬭瘯瑕佹眰 5锛?
// sub2api key 鍒楄〃蹇呴』鍒嗛〉鎷夊彇瀹屾暣锛堜笉鑳藉彧鍙栫涓€椤碉級锛屼笖鍙繚鐣欎粖鏃ユ秷璐?> 0 鐨?key銆?
func TestFetchKeyUsageToday_Sub2API_PaginatesKeysAndFiltersZeroCost(t *testing.T) {
	const totalKeys = 150 // 瓒呰繃鍗曢〉 100 鏉★紝寮哄埗瑙﹀彂绗?2 椤佃姹?
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/keys":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			const pageSize = 100
			start := (page - 1) * pageSize
			if start >= totalKeys {
				writeJSON(w, map[string]any{"data": []map[string]any{}, "total": totalKeys})
				return
			}
			end := start + pageSize
			if end > totalKeys {
				end = totalKeys
			}
			items := make([]map[string]any, 0, end-start)
			for i := start; i < end; i++ {
				items = append(items, map[string]any{
					"id":    i + 1,
					"name":  fmt.Sprintf("key-%d", i+1),
					"group": map[string]any{"name": "vip"},
				})
			}
			writeJSON(w, map[string]any{"data": items, "total": totalKeys})
		case "/api/v1/usage/stats":
			apiKeyID := r.URL.Query().Get("api_key_id")
			cost := 0.0
			switch apiKeyID {
			case "1":
				cost = 12.5
			case "150":
				cost = 3.25
			}
			writeJSON(w, map[string]any{"data": map[string]any{"total_actual_cost": cost}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "token"}

	stats, err := service.FetchKeyUsageToday(session, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 keys with nonzero cost, got %d: %+v", len(stats), stats)
	}
	byID := map[string]float64{}
	for _, s := range stats {
		byID[s.KeyID] = s.TodayAmount
		if s.GroupName != "vip" {
			t.Errorf("unexpected group name %q", s.GroupName)
		}
	}
	if byID["1"] != 12.5 {
		t.Errorf("key 1 cost = %.2f, want 12.50", byID["1"])
	}
	if byID["150"] != 3.25 {
		t.Errorf("key 150 (only reachable via page 2) cost = %.2f, want 3.25 鈥?pagination may have stopped at page 1", byID["150"])
	}
}

// TestFetchKeyUsageToday_Sub2API_UsesShanghaiDate 楠岃瘉 Sub2API 鐨勯€?key 浠婃棩缁熻
// 涓嶅彈杩涚▼鏈湴鏃跺尯褰卞搷锛屾棩鏈熷拰 timezone 鍙傛暟濮嬬粓鎸夊寳浜椂闂寸敓鎴愩€?
func TestFetchKeyUsageToday_Sub2API_UsesShanghaiDate(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("鍔犺浇鍖椾含鏃堕棿鏃跺尯澶辫触: %v", err)
	}
	now := time.Now()
	wantDate := now.In(shanghai).Format("2006-01-02")
	testLocal := time.FixedZone("UTC-12", -12*60*60)
	if now.In(testLocal).Format("2006-01-02") == wantDate {
		testLocal = time.FixedZone("UTC+14", 14*60*60)
	}
	originalLocal := time.Local
	time.Local = testLocal
	t.Cleanup(func() { time.Local = originalLocal })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/keys":
			writeJSON(w, map[string]any{
				"data":  []map[string]any{{"id": 7, "name": "beijing-key", "group": map[string]any{"name": "vip"}}},
				"total": 1,
			})
		case "/api/v1/usage/stats":
			if got := r.URL.Query().Get("start_date"); got != wantDate {
				t.Errorf("start_date = %q, want Beijing date %q", got, wantDate)
			}
			if got := r.URL.Query().Get("end_date"); got != wantDate {
				t.Errorf("end_date = %q, want Beijing date %q", got, wantDate)
			}
			if got := r.URL.Query().Get("timezone"); got != "Asia/Shanghai" {
				t.Errorf("timezone = %q, want Asia/Shanghai", got)
			}
			writeJSON(w, map[string]any{"data": map[string]any{"total_actual_cost": 2.5}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	stats, err := service.FetchKeyUsageToday(Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "token"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 1 || stats[0].TodayAmount != 2.5 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

// TestFetchKeyUsageToday_NewAPI_UsesTokenNameAndGroupFilter 瑕嗙洊娴嬭瘯瑕佹眰 6锛?
// new-api token 鍒楄〃鍒嗛〉 + token_name/group 缁熻璺緞锛氬甫鍒嗙粍鐨?token 鎸?token_name+group 鏌ヨ锛?
// 鏃犲垎缁勭殑 token 鍙寜 token_name 鏌ヨ锛堜笉鍋氬叏鍒嗙粍绌蜂妇锛夈€?
func TestFetchKeyUsageToday_NewAPI_UsesTokenNameAndGroupFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/token/":
			if r.URL.Query().Get("p") != "1" {
				writeJSON(w, map[string]any{"data": []map[string]any{}, "total": 2})
				return
			}
			writeJSON(w, map[string]any{
				"data": []map[string]any{
					{"id": 1, "name": "prod-key", "group": "vip"},
					{"id": 2, "name": "no-group-key"},
				},
				"total": 2,
			})
		case "/api/log/self/stat":
			tokenName := r.URL.Query().Get("token_name")
			group := r.URL.Query().Get("group")
			switch tokenName {
			case "prod-key":
				if group != "vip" {
					t.Fatalf("expected group=vip for prod-key, got %q", group)
				}
				writeJSON(w, map[string]any{"data": map[string]any{"quota": 250000}})
			case "no-group-key":
				if group != "" {
					t.Fatalf("expected no group param for ungrouped token, got %q", group)
				}
				writeJSON(w, map[string]any{"data": map[string]any{"quota": 0}})
			default:
				t.Fatalf("unexpected token_name: %s", tokenName)
			}
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "session=abc", UserID: "1", QuotaPerUnit: 100000}

	stats, err := service.FetchKeyUsageToday(session, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 key with nonzero cost, got %d: %+v", len(stats), stats)
	}
	if stats[0].KeyName != "prod-key" || stats[0].GroupName != "vip" {
		t.Fatalf("unexpected stat: %+v", stats[0])
	}
	if stats[0].TodayAmount != 2.5 {
		t.Errorf("todayAmount = %.4f, want 2.5000 (250000/100000 quota conversion)", stats[0].TodayAmount)
	}
}

// TestFetchKeyUsageToday_UnsupportedPlatform 楠岃瘉鏈煡骞冲彴浼氳瘽鐩存帴杩斿洖閿欒锛岃€屼笉鏄潤榛樿繑鍥炵┖缁撴灉銆?
func TestFetchKeyUsageToday_UnsupportedPlatform(t *testing.T) {
	service := newTestPlatformService(http.DefaultClient)
	_, err := service.FetchKeyUsageToday(Session{Platform: PlatformAuto}, nil)
	if err == nil {
		t.Fatal("expected error for unsupported platform, got nil")
	}
}
