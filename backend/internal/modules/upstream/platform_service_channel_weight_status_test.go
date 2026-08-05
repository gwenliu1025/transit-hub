package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUpdateNewAPIChannelWeightStatus_PutBodyIsTopLevel 楠岃瘉 PUT /api/channel/ 鐨勮姹備綋
// 瀛楁鍦?JSON 椤跺眰锛坣ew-api UpdateChannel 鐢?ShouldBindJSON(&PatchChannel) 鐩存帴缁戝畾锛夛紝
// 涓嶈兘鍍?CreateNewAPIChannel 閭ｆ牱鍖呬竴灞?"channel"锛屽惁鍒欏悗绔В鏋愪笉鍒颁换浣曞瓧娈点€?
func TestUpdateNewAPIChannelWeightStatus_PutBodyIsTopLevel(t *testing.T) {
	var putBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/channel/42":
			writeJSON(w, map[string]any{
				"data": map[string]any{
					"id": 42, "type": 1, "key": "sk-secret", "name": "my-channel",
					"base_url": "https://example.com", "models": "gpt-4o", "group": "default",
					"weight": 100, "status": 1, "priority": 0,
				},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/channel/":
			body, err := readJSONBody(r)
			if err != nil {
				t.Fatalf("failed to decode PUT body: %v", err)
			}
			putBody = body
			writeJSON(w, map[string]any{"success": true})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "session=abc", UserID: "1"}

	if err := service.UpdateNewAPIChannelWeightStatus(session, "42", 0, 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if putBody == nil {
		t.Fatalf("PUT request was never made")
	}
	// 椤跺眰蹇呴』鐩存帴鍖呭惈 id/weight/status锛屼笉鑳借鍖呭湪 "channel" 閿笅闈€?
	if _, wrapped := putBody["channel"]; wrapped {
		t.Fatalf("request body must not be wrapped in a top-level \"channel\" key: %+v", putBody)
	}
	if _, ok := putBody["id"]; !ok {
		t.Fatalf("expected top-level \"id\" field, got %+v", putBody)
	}
	weight, ok := putBody["weight"].(float64)
	if !ok || weight != 0 {
		t.Fatalf("expected top-level weight=0, got %+v", putBody["weight"])
	}
	status, ok := putBody["status"].(float64)
	if !ok || status != 2 {
		t.Fatalf("expected top-level status=2, got %+v", putBody["status"])
	}
	// 蹇呴』淇濈暀 GET 鍥炴潵鐨?key/base_url/group 绛夊瓧娈碉紝涓嶈兘琚鐩栦涪澶便€?
	if putBody["key"] != "sk-secret" || putBody["base_url"] != "https://example.com" || putBody["group"] != "default" {
		t.Fatalf("expected original channel fields (key/base_url/group) to be preserved, got %+v", putBody)
	}
}

// TestUpdateNewAPIChannelWeightStatus_GetFailurePropagates 楠岃瘉 GET 鍗曟潯 channel 澶辫触鏃?
// 鐩存帴鎶婇敊璇€忎紶缁欒皟鐢ㄦ柟锛屼笉鍙戣捣浠讳綍鐚滄祴鎬х殑 PUT 璇锋眰锛堣皟鐢ㄦ柟搴旇褰?remote_action=unsupported锛夈€?
func TestUpdateNewAPIChannelWeightStatus_GetFailurePropagates(t *testing.T) {
	putCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putCalled = true
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "session=abc", UserID: "1"}

	if err := service.UpdateNewAPIChannelWeightStatus(session, "42", 0, 2); err == nil {
		t.Fatalf("expected error when GET fails")
	}
	if putCalled {
		t.Fatalf("must not issue a PUT request when GET fails")
	}
}

func TestUpdateAdminTargetPriority_NewAPIPreservesWeightAndStatus(t *testing.T) {
	var putBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/channel/42":
			writeJSON(w, map[string]any{"data": map[string]any{
				"id": 42, "name": "channel", "type": 1, "key": "sk-secret", "base_url": "https://up",
				"models": "gpt-4o", "group": "vip", "priority": 10, "weight": 80, "status": 1,
			}})
		case r.Method == http.MethodPut && r.URL.Path == "/api/channel/":
			putBody, _ = readJSONBody(r)
			writeJSON(w, map[string]any{"success": true})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "session=abc", UserID: "1"}
	if err := service.UpdateAdminTargetPriority(session, "42", 40999); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if putBody["priority"] != float64(40999) || putBody["weight"] != float64(80) || putBody["status"] != float64(1) {
		t.Fatalf("priority update must preserve weight/status: %+v", putBody)
	}
}

func readJSONBody(r *http.Request) (map[string]any, error) {
	var body map[string]any
	err := json.NewDecoder(r.Body).Decode(&body)
	return body, err
}
