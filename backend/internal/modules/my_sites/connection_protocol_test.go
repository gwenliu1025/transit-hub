package my_sites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"transithub/backend/internal/modules/upstream"
)

// 全部请求只访问 httptest；通过公开 Service 入口验证校验发生在远端创建之前。
type protocolFixture struct {
	service *Service
	repo    *testConnRepo
	creates int
	payload map[string]any
}

func newProtocolFixture(t *testing.T, source, admin upstream.Platform, cached string) *protocolFixture {
	t.Helper()
	f := &protocolFixture{}
	tokenName, channelName := "existing-token", "existing-channel"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"role": "admin"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/user/self":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"role": 10}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/groups":
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": 7, "name": "vip", "platform": "openai", "status": "active"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/group/":
			writeConnectionTestJSON(w, map[string]any{"data": []string{"7"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/user/self/groups":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"7": 1}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/pricing":
			writeConnectionTestJSON(w, map[string]any{"data": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/keys":
			f.creates++
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"id": 33, "key": "test-key"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/token/":
			f.creates++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			tokenName, _ = body["name"].(string)
			if body["group"] != "7" {
				t.Errorf("New API token 分组丢失: %v", body["group"])
			}
			writeConnectionTestJSON(w, map[string]any{"success": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/token/":
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": 33, "name": tokenName, "group": "7"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/token/33/key":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"key": "test-key"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts":
			f.creates++
			_ = json.NewDecoder(r.Body).Decode(&f.payload)
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"id": 44}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": 44, "name": "existing-account", "group_ids": []int{7}}}, "total": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/api/channel/":
			f.creates++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.payload, _ = body["channel"].(map[string]any)
			channelName, _ = f.payload["name"].(string)
			writeConnectionTestJSON(w, map[string]any{"success": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/channel/":
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": 44, "name": channelName}}, "total": 1})
		default:
			t.Errorf("未预期的模拟请求: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	sourceSession, adminSession := platformTestSession(source, server.URL), platformTestSession(admin, server.URL)
	stateRepo := &testStateRepo{state: &State{UserID: "user-1", AdminAccountID: "admin-1", Session: adminSession}}
	f.repo = &testConnRepo{stateRepo: stateRepo}
	lookup := testUpstreamLookup{sites: map[string]*upstream.Site{"site-1": {
		ID: "site-1", UserID: "user-1", AdminAccountID: "admin-1", Name: "source", BaseURL: server.URL, Platform: source, Session: &sourceSession,
		Metrics: upstream.Metrics{Groups: []upstream.GroupInfo{{ID: "7", Name: "vip", Platform: stringPointer(cached)}}},
	}}}
	f.service = NewService(stateRepo, upstream.NewPlatformService(upstream.NewHTTPClient(server.Client())), lookup)
	f.service.SetAdminAccountResolver(testAdminResolver{currentID: "admin-1"})
	f.service.connRepository = f.repo
	return f
}

func TestRealConnectProtocolSelection(t *testing.T) {
	for _, tc := range []struct {
		name, cached, selected, expected, errKey string
		source                                   upstream.Platform
	}{
		{"newapi-mixed-selected", "openai, anthropic", "openai", "openai", "", upstream.PlatformNewAPI},
		{"newapi-unknown-selected", "deepseek", "anthropic", "anthropic", "", upstream.PlatformNewAPI},
		{"newapi-single-overridden", "anthropic", "openai", "openai", "", upstream.PlatformNewAPI},
		{"newapi-normalized-selection", "deepseek", " GEMINI ", "gemini", "", upstream.PlatformNewAPI},
		{"newapi-single-default", "openai", "", "openai", "", upstream.PlatformNewAPI},
		{"newapi-mixed-missing", "openai, anthropic", "", "", ErrorGroupTypeRequired, upstream.PlatformNewAPI},
		{"newapi-unknown-missing", "deepseek", "", "", ErrorGroupTypeRequired, upstream.PlatformNewAPI},
		{"newapi-invalid-selected", "openai", "injected-protocol", "", ErrorGroupTypeInvalid, upstream.PlatformNewAPI},
		{"sub2api-openai", "openai", "anthropic", "openai", "", upstream.PlatformSub2API},
		{"sub2api-anthropic", "anthropic", "", "anthropic", "", upstream.PlatformSub2API},
		{"sub2api-gemini", "gemini", "", "gemini", "", upstream.PlatformSub2API},
		{"sub2api-antigravity", "antigravity", "", "antigravity", "", upstream.PlatformSub2API},
		{"sub2api-unknown-selected", "", "openai", "openai", "", upstream.PlatformSub2API},
		{"sub2api-invalid-selected", "openai", "bad", "", ErrorGroupTypeInvalid, upstream.PlatformSub2API},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProtocolFixture(t, tc.source, upstream.PlatformSub2API, tc.cached)
			noPricing := false
			result, err := f.service.RealConnect(context.Background(), "user-1", RealConnectRequest{UpstreamSiteID: "site-1", UpstreamGroupID: "7", UpstreamGroupName: "vip", GroupType: tc.selected, OwnGroupIDs: []string{"7"}, AddToPricingMapping: &noPricing})
			if tc.errKey != "" {
				if err == nil || err.Error() != tc.errKey || f.creates != 0 || f.repo.saveCalls != 0 {
					t.Fatalf("类型拒收必须零创建零持久化: err=%v creates=%d saves=%d", err, f.creates, f.repo.saveCalls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if f.creates != 2 || f.repo.saveCalls != 1 || result.Connection.GroupType != tc.expected || result.Connection.UpstreamKey != "" {
				t.Fatalf("结果不符合创建契约: %+v creates=%d saves=%d", result.Connection, f.creates, f.repo.saveCalls)
			}
			credentials, _ := f.payload["credentials"].(map[string]any)
			if f.payload["platform"] != tc.expected || f.payload["type"] != "apikey" || f.payload["priority"] != float64(1) || credentials["api_key"] != "test-key" || credentials["base_url"] == "" || !reflect.DeepEqual(f.payload["group_ids"], []any{float64(7)}) {
				t.Fatalf("账号基础载荷不符: %v", f.payload)
			}
			wantConcurrency := float64(1000)
			if tc.expected == "antigravity" {
				wantConcurrency = 10
			}
			if f.payload["concurrency"] != wantConcurrency {
				t.Fatalf("并发发生回归: %v", f.payload)
			}
			if tc.expected != "antigravity" && credentials["pool_mode"] != true {
				t.Fatal("pool_mode 丢失")
			}
			if tc.expected == "antigravity" && credentials["pool_mode"] != nil {
				t.Fatal("antigravity 不应新增 pool_mode")
			}
			if tc.expected == "gemini" && credentials["tier_id"] != "aistudio_free" {
				t.Fatal("Gemini tier 丢失")
			}
			if tc.expected == "openai" || tc.expected == "anthropic" {
				if !reflect.DeepEqual(f.payload["extra"], map[string]any{tc.expected + "_passthrough": true}) {
					t.Fatal("透传参数丢失")
				}
			}
		})
	}
}

func TestRealConnectNewAPIAdminMixedMetadataKeepsChannelSelection(t *testing.T) {
	f := newProtocolFixture(t, upstream.PlatformNewAPI, upstream.PlatformNewAPI, "openai, anthropic")
	noPricing := false
	_, err := f.service.RealConnect(context.Background(), "user-1", RealConnectRequest{UpstreamSiteID: "site-1", UpstreamGroupID: "7", UpstreamGroupName: "vip", ChannelType: 14, OwnGroupIDs: []string{"7"}, AddToPricingMapping: &noPricing})
	if err != nil {
		t.Fatal(err)
	}
	if f.creates != 2 || f.payload["type"] != float64(14) || f.payload["group"] != "7" {
		t.Fatalf("New API 管理端渠道回归: %v", f.payload)
	}
}

func TestRealBindMixedMetadataDoesNotRequireCreationProtocol(t *testing.T) {
	f := newProtocolFixture(t, upstream.PlatformNewAPI, upstream.PlatformSub2API, "openai, anthropic")
	noPricing := false
	result, err := f.service.RealBind(context.Background(), "user-1", RealBindRequest{UpstreamSiteID: "site-1", UpstreamGroupID: "7", UpstreamGroupName: "vip", UpstreamKeyID: "33", AdminGroupID: "7", AdminResourceID: "44", AddToPricingMapping: &noPricing})
	if err != nil {
		t.Fatal(err)
	}
	if f.creates != 0 || result.Connection.ProvisioningMode != ProvisioningModeExisting || result.Connection.CanDeleteRemote {
		t.Fatalf("已有绑定不得新增创建要求或接管远端: %+v", result.Connection)
	}
}

func TestConnectionCapabilitiesPublishAccountProtocolContract(t *testing.T) {
	want := []GroupTypeOption{{"openai", "OpenAI"}, {"anthropic", "Anthropic"}, {"gemini", "Gemini"}, {"antigravity", "Antigravity"}}
	if got := connectionCapabilities(upstream.PlatformSub2API); !reflect.DeepEqual(got.GroupTypes, want) || !got.RequiresGroupType {
		t.Fatalf("协议能力不符: %+v", got)
	}
	if got := connectionCapabilities(upstream.PlatformNewAPI); got.RequiresGroupType || len(got.GroupTypes) != 0 || len(got.ChannelTypes) < 4 {
		t.Fatalf("渠道能力回归: %+v", got)
	}
}

func TestWriteConnectionErrorUsesSafeKeysAndLocalStatus(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		key    string
	}{
		{"wrapped-remote-auth", fmt.Errorf("context: %w", &upstream.RequestError{MessageKey: upstream.ErrorAuth, StatusCode: 401}), 502, upstream.ErrorAuth},
		{"wrapped-remote-invalid", fmt.Errorf("context: %w", &upstream.RequestError{MessageKey: upstream.ErrorInvalidResponse}), 502, upstream.ErrorInvalidResponse},
		{"secret-remote", &upstream.RequestError{MessageKey: "secret-body-token"}, 500, ErrorUnknown},
		{"unknown-remote", &upstream.RequestError{MessageKey: upstream.ErrorUnknown}, 500, ErrorUnknown},
		{"plain-secret", errors.New("secret-body-token"), 500, ErrorUnknown},
		{"local-auth", requestError(ErrorAuthRequired), 401, ErrorAuthRequired},
		{"local-admin", requestError(ErrorAdminOnly), 403, ErrorAdminOnly},
		{"local-conflict", requestError(ErrorConnectionExists), 409, ErrorConnectionExists},
		{"local-account", requestError("admin.adminAccounts.errors.noCurrentAccount"), 409, "admin.adminAccounts.errors.noCurrentAccount"},
		{"protocol-required", requestError(ErrorGroupTypeRequired), 400, ErrorGroupTypeRequired},
		{"protocol-invalid", requestError(ErrorGroupTypeInvalid), 400, ErrorGroupTypeInvalid},
	}
	for _, key := range []string{upstream.ErrorInvalidURL, upstream.ErrorNetwork, upstream.ErrorRequest, upstream.ErrorNotFound} {
		cases = append(cases, struct {
			name   string
			err    error
			status int
			key    string
		}{key, &upstream.RequestError{MessageKey: key}, 502, key})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, tc.err)
			var payload map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || payload["message"] != tc.key {
				t.Fatalf("错误投影不符: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRealConnectSub2APIToNewAPIKeepsChannelPayload(t *testing.T) {
	f := newProtocolFixture(t, upstream.PlatformSub2API, upstream.PlatformNewAPI, "anthropic")
	noPricing := false
	result, err := f.service.RealConnect(context.Background(), "user-1", RealConnectRequest{UpstreamSiteID: "site-1", UpstreamGroupID: "7", UpstreamGroupName: "vip", ChannelType: 14, OwnGroupIDs: []string{"7"}, AddToPricingMapping: &noPricing})
	if err != nil {
		t.Fatal(err)
	}
	if f.creates != 2 || f.repo.saveCalls != 1 || f.payload["type"] != float64(14) || f.payload["group"] != "7" || f.payload["key"] != "test-key" {
		t.Fatalf("Sub2API 上游到 New API 渠道创建不符: creates=%d saves=%d payload=%v", f.creates, f.repo.saveCalls, f.payload)
	}
	if result.Connection.UpstreamPlatform != string(upstream.PlatformSub2API) || result.Connection.AdminPlatform != string(upstream.PlatformNewAPI) || result.Connection.UpstreamKey != "" {
		t.Fatalf("跨平台连接记录或凭据最小展示回归: %+v", result.Connection)
	}
}
