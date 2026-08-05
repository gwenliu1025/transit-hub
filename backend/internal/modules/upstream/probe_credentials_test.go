package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestResolveProbeCredential_NewAPIChannelKeySuccess 楠岃瘉 new-api锛?
//   - base_url 鏉ヨ嚜 channel 鍒楄〃瀛楁锛坅ccount.BaseURL锛夛紝涓嶄粠 GET /api/channel/:id 鍙栵紙閭ｉ噷娌℃湁 key锛夈€?
//   - key 閫氳繃 POST /api/channel/:id/key 涓存椂鑾峰彇锛屾垚鍔熸椂鍙瀯閫犳帰娲诲嚟鎹€?
func TestResolveProbeCredential_NewAPIChannelKeySuccess(t *testing.T) {
	keyCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/channel/100/key":
			keyCalled = true
			writeJSON(w, map[string]any{"data": map[string]any{"key": "sk-plain-secret"}})
		case r.URL.Path == "/api/channel/100":
			// GetChannel 涓嶈繑鍥?key锛坰electAll=false 鏃?DB.Omit("key")锛夈€傚鏋滆В鏋愬櫒閿欒鍦?
			// 渚濊禆杩欓噷鍙?key锛屽氨浼氭嬁涓嶅埌 key 鈥斺€?浣嗗畠涓嶅簲璇ヨ蛋杩欓噷銆?
			writeJSON(w, map[string]any{"data": map[string]any{"id": 100, "name": "ch", "base_url": "https://up"}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "s=1", UserID: "1"}
	account := AdminGroupAccountInfo{ID: "100", Name: "ch", BaseURL: "https://up.example.com", Models: "gpt-4o,gpt-4o-mini"}

	cred, err := service.ResolveProbeCredential(session, account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !keyCalled {
		t.Fatalf("expected POST /api/channel/:id/key to be used for key")
	}
	if cred.Key != "sk-plain-secret" || cred.BaseURL != "https://up.example.com" {
		t.Fatalf("unexpected credential: %+v", cred)
	}
	if len(cred.Models) != 2 {
		t.Fatalf("expected 2 models parsed, got %v", cred.Models)
	}
}

// TestResolveProbeCredential_NewAPIKeyUnauthorizedIsUnavailable 楠岃瘉 key 鎺ュ彛 401 鏃剁洰鏍囦笉鍙帰娲?
// 锛堝畨鍏ㄩ獙璇?鏍规潈闄愪笉瓒筹級锛屾槧灏勪负 secure_verification_required銆?
func TestResolveProbeCredential_NewAPIKeyUnauthorizedIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/channel/100/key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "s=1", UserID: "1"}
	account := AdminGroupAccountInfo{ID: "100", BaseURL: "https://up"}

	_, err := service.ResolveProbeCredential(session, account)
	if ProbeCredentialReason(err) != ReasonSecureVerificationRequired {
		t.Fatalf("expected secure_verification_required, got %v", err)
	}
}

// TestResolveProbeCredential_NewAPIKeyForbiddenIsSecureVerification 楠岃瘉 key 鎺ュ彛杩斿洖 403
// 锛坮oot/瀹夊叏楠岃瘉涓嶈冻鐨勫父瑙佺姸鎬佺爜锛夋椂锛屽綊绫讳负 secure_verification_required锛岃€屼笉鏄?
// credential_unavailable锛屽墠绔墠鑳芥彁绀虹敤鎴烽渶瑕?root 瀹夊叏楠岃瘉銆?
func TestResolveProbeCredential_NewAPIKeyForbiddenIsSecureVerification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/channel/100/key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "s=1", UserID: "1"}
	account := AdminGroupAccountInfo{ID: "100", BaseURL: "https://up"}

	_, err := service.ResolveProbeCredential(session, account)
	if ProbeCredentialReason(err) != ReasonSecureVerificationRequired {
		t.Fatalf("expected secure_verification_required for 403, got %v", err)
	}
}

// TestResolveProbeCredential_NewAPIMissingBaseURLIsUnavailable 楠岃瘉缂?base_url 鏃朵笉鍙帰娲伙紝
// 涓旀牴鏈笉鍘昏皟鐢ㄥ彈淇濇姢鐨?key 鎺ュ彛銆?
func TestResolveProbeCredential_NewAPIMissingBaseURLIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("must not hit any endpoint when base_url is missing, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "s=1", UserID: "1"}
	account := AdminGroupAccountInfo{ID: "100"} // 鏃?base_url

	_, err := service.ResolveProbeCredential(session, account)
	if ProbeCredentialReason(err) != ReasonBaseURLUnavailable {
		t.Fatalf("expected base_url_unavailable, got %v", err)
	}
}

// TestResolveProbeCredential_Sub2APIExportSingleAccount 楠岃瘉 sub2api锛氬鍑烘帴鍙ｅ彲鐢ㄤ笖姝ｅソ杩斿洖
// 涓€涓处鍙枫€佸惈鏄庢枃 credentials 鏃讹紝鍙瀯閫犳帰娲诲嚟鎹€?
func TestResolveProbeCredential_Sub2APIExportSingleAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/accounts/data" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("ids") != "55" || r.URL.Query().Get("include_proxies") != "false" {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		writeJSON(w, map[string]any{"data": []map[string]any{
			{"name": "acc", "credentials": map[string]any{"api_key": "sk-sub2-secret", "base_url": "https://sub2-up"}},
		}})
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "tok"}
	account := AdminGroupAccountInfo{ID: "55", Name: "acc", Models: "gpt-4o"}

	cred, err := service.ResolveProbeCredential(session, account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Key != "sk-sub2-secret" || cred.BaseURL != "https://sub2-up" {
		t.Fatalf("unexpected credential: %+v", cred)
	}
}

// TestResolveProbeCredential_Sub2APIExportAccountsShapeSuccess 楠岃瘉 sub2api 鐪熷疄瀵煎嚭鎺ュ彛缁撴瀯锛?
// data 鏄璞°€佽处鍙锋暟缁勫湪 data.accounts锛堣€屼笉鏄棫鍋囪鐨?data 鐩存帴鏄暟缁勶級鏃朵粛鑳芥纭В鏋愩€?
func TestResolveProbeCredential_Sub2APIExportAccountsShapeSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/accounts/data" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("ids") != "1443" || r.URL.Query().Get("include_proxies") != "false" {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		writeJSON(w, map[string]any{"data": map[string]any{
			"accounts": []map[string]any{
				{"name": "acc", "credentials": map[string]any{"api_key": "sk-sub2-secret", "base_url": "https://sub2-up"}},
			},
			"proxies":     []any{},
			"exported_at": "2026-07-07T00:00:00Z",
		}})
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "tok"}
	account := AdminGroupAccountInfo{ID: "1443", Name: "acc", Models: "gpt-4o"}

	cred, err := service.ResolveProbeCredential(session, account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Key != "sk-sub2-secret" || cred.BaseURL != "https://sub2-up" {
		t.Fatalf("unexpected credential: %+v", cred)
	}
}

// TestResolveProbeCredential_Sub2APIExportAccountsShapeEmptyIsUnavailable 楠岃瘉 data.accounts[]
// 缁撴瀯涓嬭处鍙锋暟閲忎负 0 鏃朵粛鏍囪 credential_unavailable锛屼笉缂栭€犲嚟鎹€?
func TestResolveProbeCredential_Sub2APIExportAccountsShapeEmptyIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{
			"accounts": []map[string]any{},
			"proxies":  []any{},
		}})
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "tok"}
	account := AdminGroupAccountInfo{ID: "1443"}

	_, err := service.ResolveProbeCredential(session, account)
	if ProbeCredentialReason(err) != ReasonCredentialUnavailable {
		t.Fatalf("expected credential_unavailable for empty data.accounts, got %v", err)
	}
}

// TestResolveProbeCredential_Sub2APIExportAccountsShapeNotExactlyOne 楠岃瘉 data.accounts[]
// 缁撴瀯涓嬭处鍙锋暟閲忎笉鏄?1锛堝涓級鏃朵笉鍙帰娲伙紝涓嶆寜 name 鐚滄祴銆?
func TestResolveProbeCredential_Sub2APIExportAccountsShapeNotExactlyOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{
			"accounts": []map[string]any{
				{"name": "acc-a", "credentials": map[string]any{"api_key": "k1", "base_url": "https://a"}},
				{"name": "acc-b", "credentials": map[string]any{"api_key": "k2", "base_url": "https://b"}},
			},
		}})
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "tok"}
	account := AdminGroupAccountInfo{ID: "1443"}

	_, err := service.ResolveProbeCredential(session, account)
	if ProbeCredentialReason(err) != ReasonCredentialUnavailable {
		t.Fatalf("expected credential_unavailable for ambiguous data.accounts export, got %v", err)
	}
}

// TestResolveProbeCredential_Sub2APIRedactedIsUnavailable 楠岃瘉瀵煎嚭杩斿洖鐨?credentials 宸茶劚鏁?
// 锛堟病鏈変换浣曟槑鏂?key 瀛楁锛夋椂涓嶅彲鎺㈡椿銆?
func TestResolveProbeCredential_Sub2APIRedactedIsUnavailable(t *testing.T) {
	// 甯歌 list/detail 宸茶劚鏁忥細credentials 閲屾病鏈変换浣曟槑鏂?key 瀛楁锛堝彧鍓?note锛夛紝鏍囪涓嶅彲鎺㈡椿銆?
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{
			{"name": "acc", "credentials": map[string]any{"note": "redacted"}},
		}})
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "tok"}
	account := AdminGroupAccountInfo{ID: "55"}

	_, err := service.ResolveProbeCredential(session, account)
	if ProbeCredentialReason(err) != ReasonCredentialsRedacted {
		t.Fatalf("expected credentials_redacted, got %v", err)
	}
}

// TestResolveProbeCredential_Sub2APIExportUnavailable 楠岃瘉瀵煎嚭鎺ュ彛涓嶅彲杈撅紙404锛屾ā鎷熸棫鐗堟湰
// 璺敱琚?/:id 鎶㈠崰锛夋椂涓嶅彲鎺㈡椿锛宺eason=export_unavailable銆?
func TestResolveProbeCredential_Sub2APIExportUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "tok"}
	account := AdminGroupAccountInfo{ID: "55"}

	_, err := service.ResolveProbeCredential(session, account)
	if ProbeCredentialReason(err) != ReasonExportUnavailable {
		t.Fatalf("expected export_unavailable, got %v", err)
	}
}

// TestResolveProbeCredential_Sub2APIExportNotExactlyOne 楠岃瘉瀵煎嚭杩斿洖璐﹀彿鏁伴噺涓嶆槸 1锛堟棤娉曠‘璁?
// 鏄犲皠鍒板綋鍓嶈处鍙凤級鏃朵笉鍙帰娲伙紝涓嶆寜 name 鐚滄祴銆?
func TestResolveProbeCredential_Sub2APIExportNotExactlyOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{
			{"name": "acc-a", "credentials": map[string]any{"api_key": "k1", "base_url": "https://a"}},
			{"name": "acc-b", "credentials": map[string]any{"api_key": "k2", "base_url": "https://b"}},
		}})
	}))
	defer server.Close()

	service := newTestPlatformService(server.Client())
	session := Session{Platform: PlatformSub2API, BaseURL: server.URL, AccessToken: "tok"}
	account := AdminGroupAccountInfo{ID: "55"}

	_, err := service.ResolveProbeCredential(session, account)
	if ProbeCredentialReason(err) != ReasonCredentialUnavailable {
		t.Fatalf("expected credential_unavailable for ambiguous export, got %v", err)
	}
}
