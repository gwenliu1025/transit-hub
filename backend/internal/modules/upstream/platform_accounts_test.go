package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListAdminGroupAccounts_Sub2APIGroupQueryPagingAndFields 楠岃瘉 sub2api 鍒嗙粍璐﹀彿璇诲彇锛?
//   - query 鍙傛暟鏄?group=<鍒嗙粍ID>锛堜笉鏄?group_id锛夈€?
//   - 鍒嗛〉鎷夊彇鐩村埌杈惧埌 total锛堣鐩栦袱椤碉級銆?
//   - 鍩虹瀛楁涓庢帰娲荤瓥鐣ョ浉鍏冲瓧娈垫纭В鏋愩€?
//   - credentials 绛夋晱鎰熷瓧娈典笉浼氬嚭鐜板湪杩斿洖缁撴瀯閲屻€?
func TestListAdminGroupAccounts_Sub2APIGroupQueryPagingAndFields(t *testing.T) {
	var seenGroupQueries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/accounts" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		seenGroupQueries = append(seenGroupQueries, r.URL.Query().Get("group"))
		if gid := r.URL.Query().Get("group_id"); gid != "" {
			t.Fatalf("must query by group=, not group_id=; got group_id=%s", gid)
		}
		page := r.URL.Query().Get("page")
		switch page {
		case "1":
			// 绗?1 椤佃繑鍥炴弧椤碉紙100 鏉★級锛岃揩浣胯鍙栫户缁炕绗?2 椤碉紱绗竴鏉℃惡甯﹀畬鏁村瓧娈典笌鏁忔劅瀛楁銆?
			items := make([]map[string]any, 0, 100)
			items = append(items, map[string]any{
				"id": 101, "name": "acc-a", "platform": "openai", "type": "oauth", "status": "active",
				"priority": 5, "concurrency": 3, "rate_multiplier": 1.5, "load_factor": 2,
				"group_ids": []any{7.0, 8.0}, "schedulable": true,
				"credentials": map[string]any{"access_token": "SECRET-TOKEN-XYZ"},
				"api_key":     "sk-super-secret",
			})
			for i := 0; i < 99; i++ {
				items = append(items, map[string]any{"id": 1000 + i, "name": "filler", "status": "active"})
			}
			writeJSON(w, map[string]any{"data": items, "total": 101})
		case "2":
			writeJSON(w, map[string]any{
				"data": []map[string]any{
					{"id": 102, "name": "acc-b", "platform": "anthropic", "status": "disabled", "schedulable": false},
				},
				"total": 101,
			})
		default:
			t.Fatalf("unexpected page: %s", page)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "token"}

	accounts, err := service.ListAdminGroupAccounts(session, AdminGroupInfo{ID: "42", Name: "vip"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(accounts) != 101 {
		t.Fatalf("expected 101 accounts across 2 pages, got %d", len(accounts))
	}
	if len(seenGroupQueries) != 2 {
		t.Fatalf("expected 2 paged requests, got %d", len(seenGroupQueries))
	}
	for _, q := range seenGroupQueries {
		if q != "42" {
			t.Fatalf("expected group query = 42 (group ID), got %q", q)
		}
	}

	a := accounts[0]
	if a.ID != "101" || a.Name != "acc-a" || a.Platform != "openai" || a.Type != "oauth" || a.Status != "active" {
		t.Fatalf("unexpected base fields: %+v", a)
	}
	if a.Priority == nil || *a.Priority != 5 {
		t.Fatalf("priority = %v, want 5", a.Priority)
	}
	if a.Concurrency == nil || *a.Concurrency != 3 {
		t.Fatalf("concurrency = %v, want 3", a.Concurrency)
	}
	if a.RateMultiplier == nil || *a.RateMultiplier != 1.5 {
		t.Fatalf("rateMultiplier = %v, want 1.5", a.RateMultiplier)
	}
	if a.LoadFactor == nil || *a.LoadFactor != 2 {
		t.Fatalf("loadFactor = %v, want 2", a.LoadFactor)
	}
	if a.Schedulable == nil || *a.Schedulable != true {
		t.Fatalf("schedulable = %v, want true", a.Schedulable)
	}
	if a.Weight != nil {
		t.Fatalf("sub2api account must not have weight, got %v", a.Weight)
	}
	if len(a.GroupIDs) != 2 || a.GroupIDs[0] != "7" || a.GroupIDs[1] != "8" {
		t.Fatalf("groupIds = %v, want [7 8]", a.GroupIDs)
	}

	// 鏁忔劅瀛楁缁濅笉鍑虹幇鍦ㄥ簭鍒楀寲缁撴灉閲屻€?
	encoded, _ := json.Marshal(accounts)
	for _, secret := range []string{"SECRET-TOKEN-XYZ", "sk-super-secret", "credentials", "access_token", "api_key"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("sensitive field %q leaked into accounts response: %s", secret, encoded)
		}
	}
}

// TestListAdminGroupAccounts_NewAPIUsesSearchPath 楠岃瘉 new-api 浼樺厛璧?/api/channel/search?group=<鍒嗙粍鍚?锛?
// 骞舵纭В鏋?channel 瀛楁锛堝惈 weight锛夛紝key 涓嶅娉勩€?
func TestListAdminGroupAccounts_NewAPIUsesSearchPath(t *testing.T) {
	searchCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/channel/search":
			searchCalled = true
			if g := r.URL.Query().Get("group"); g != "vip" {
				t.Fatalf("expected group=vip, got %q", g)
			}
			writeJSON(w, map[string]any{
				"data": []map[string]any{
					{
						"id": 9, "type": 1, "name": "ch-a", "status": 1, "base_url": "https://up.example.com",
						"models": "gpt-4o,gpt-4o-mini", "group": "vip,default", "priority": 10, "weight": 3,
						"key": "sk-channel-secret",
					},
				},
				"total": 1,
			})
		default:
			t.Fatalf("new-api should use /api/channel/search, got path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "session=x", UserID: "1"}

	channels, err := service.ListAdminGroupAccounts(session, AdminGroupInfo{ID: "vip", Name: "vip"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !searchCalled {
		t.Fatalf("expected /api/channel/search to be used")
	}
	if len(channels) != 1 {
		t.Fatalf("expected 1 channel, got %d", len(channels))
	}
	c := channels[0]
	if c.ID != "9" || c.Name != "ch-a" || c.Type != "1" || c.Status != "1" {
		t.Fatalf("unexpected base fields: %+v", c)
	}
	if c.Weight == nil || *c.Weight != 3 {
		t.Fatalf("weight = %v, want 3", c.Weight)
	}
	if c.Priority == nil || *c.Priority != 10 {
		t.Fatalf("priority = %v, want 10", c.Priority)
	}
	if c.Models != "gpt-4o,gpt-4o-mini" {
		t.Fatalf("models = %q", c.Models)
	}

	encoded, _ := json.Marshal(channels)
	if strings.Contains(string(encoded), "sk-channel-secret") || strings.Contains(string(encoded), `"key"`) {
		t.Fatalf("channel key leaked into response: %s", encoded)
	}
}

// TestListAdminGroupAccounts_NewAPIFallsBackToLocalCommaFilter 楠岃瘉 search 鎺ュ彛澶辫触鏃讹紝
// 鍏滃簳璧?/api/channel/ 鍒嗛〉骞舵寜銆岄€楀彿鍒嗙粍绮剧‘鍖归厤銆嶆湰鍦拌繃婊わ細
//   - "vip" 鍛戒腑 group="vip,default" 涓?group="vip"銆?
//   - "vip" 涓嶈兘鍛戒腑 group="vip2"锛堢姝?substring 鍖归厤锛夈€?
func TestListAdminGroupAccounts_NewAPIFallsBackToLocalCommaFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/channel/search":
			// 妯℃嫙鏃ч儴缃叉病鏈?search 鎺ュ彛銆?
			w.WriteHeader(http.StatusNotFound)
		case "/api/channel/":
			writeJSON(w, map[string]any{
				"data": []map[string]any{
					{"id": 1, "name": "ch-vip-default", "group": "vip,default", "weight": 1},
					{"id": 2, "name": "ch-vip", "group": "vip", "weight": 2},
					{"id": 3, "name": "ch-vip2", "group": "vip2", "weight": 3},
					{"id": 4, "name": "ch-other", "group": "default,pro", "weight": 4},
				},
				"total": 4,
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "session=x", UserID: "1"}

	channels, err := service.ListAdminGroupAccounts(session, AdminGroupInfo{ID: "vip", Name: "vip"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]bool{}
	for _, c := range channels {
		got[c.Name] = true
	}
	if !got["ch-vip-default"] || !got["ch-vip"] {
		t.Fatalf("expected exact comma-group matches ch-vip-default and ch-vip, got %+v", got)
	}
	if got["ch-vip2"] {
		t.Fatalf("substring match leaked: vip must not match group vip2")
	}
	if got["ch-other"] {
		t.Fatalf("ch-other (group default,pro) must not match vip")
	}
	if len(channels) != 2 {
		t.Fatalf("expected exactly 2 matched channels, got %d", len(channels))
	}
}
