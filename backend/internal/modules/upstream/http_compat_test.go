package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProductionNormalizeURLHTTPCompatibility(t *testing.T) {
	service := NewPlatformService(NewHTTPClient(nil))
	for _, tc := range []struct{ raw, want string }{
		{" http://8.8.8.8:3200/ ", "http://8.8.8.8:3200"},
		{"https://8.8.8.8:8443/root///?token=ignored#fragment", "https://8.8.8.8:8443/root"},
		{"8.8.8.8:3200/", "https://8.8.8.8:3200"},
		{"http://[2606:4700:4700::1111]:3200/", "http://[2606:4700:4700::1111]:3200"},
	} {
		got, err := service.NormalizeURL(tc.raw)
		if err != nil || got != tc.want {
			t.Fatalf("生产规范化 %q = %q, %v，期望 %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{"", "http://127.0.0.1:3200", "http://10.0.0.1", "http://169.254.169.254", "http://[::1]", "http://user:password@8.8.8.8", "ftp://8.8.8.8", "http://8.8.8.8:abc", "://"} {
		got, err := service.NormalizeURL(raw)
		if got != "" || err == nil || errorKey(err) != ErrorInvalidURL {
			t.Fatalf("无效地址 %q 的错误映射不正确：%q, %v", raw, got, err)
		}
	}
}

func TestProductionHTTPClientAcceptsPublicHTTPBeforeCanceledTransport(t *testing.T) {
	client := NewHTTPClient(nil).client
	defer client.CloseIdleConnections()
	if client.Timeout != 60*time.Second {
		t.Fatal("默认上游请求超时预算变化")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, raw := range []string{"http://8.8.8.8:3200", "https://8.8.8.8:8443"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if _, err := client.Do(req); !errors.Is(err, context.Canceled) {
			t.Fatalf("默认工厂应通过公网协议校验并在实际传输取消，得到 %v", err)
		}
	}
}

type httpCompatibilityTransport func(*http.Request) (*http.Response, error)

func (f httpCompatibilityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// 复用仓储读取契约，仅记录本测试真正经过 SaveSite 的状态，不声明真实数据库验收。
type httpCompatibilityRepository struct {
	refreshSchedulerRepository
	saved []Site
}

func (r *httpCompatibilityRepository) SaveSite(_ context.Context, site Site) error {
	r.saved = append(r.saved, site)
	return nil
}

func TestPublicHTTPLoginSaveSyncAndFailedSessionRecovery(t *testing.T) {
	const baseURL = "http://8.8.8.8:3200"
	logins, selfRequests := 0, 0
	quota := 500000
	client := &http.Client{Transport: httpCompatibilityTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "http" || req.URL.Host != "8.8.8.8:3200" {
			t.Fatalf("请求地址被改写：%s", req.URL)
		}
		header := make(http.Header)
		body := `{"success":true,"data":{}}`
		switch req.URL.Path {
		case "/api/user/login":
			logins++
			var payload map[string]string
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if req.Method != http.MethodPost || payload["username"] != "fixture-user" || payload["password"] != "fixture-password" {
				t.Fatal("登录请求或凭据序列化错误")
			}
			header.Set("Set-Cookie", "session=fixture-session; Path=/")
			body = `{"success":true,"data":{"id":42}}`
		case "/api/user/self":
			selfRequests++
			if req.Header.Get("Cookie") != "session=fixture-session" || req.Header.Get("New-Api-User") != "42" {
				t.Fatal("同步未使用保存会话")
			}
			body = fmt.Sprintf(`{"success":true,"data":{"id":42,"quota":%d,"used_quota":0}}`, quota)
		case "/api/status", "/api/log/self/stat", "/api/user/self/groups", "/api/pricing":
		default:
			t.Fatalf("意外请求路径：%s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	// 保留生产 NormalizeURL，而不是旧测试 helper 的恒真校验器；仅业务响应由内存夹具提供。
	platform := NewPlatformService(NewHTTPClient(client))
	cache := newFakeSiteCache()
	repo := &httpCompatibilityRepository{}
	service := NewService(platform, repo, nil, cache)
	service.SetAdminAccountResolver(&fakeAccountResolver{current: map[string]string{"user": "workspace"}})
	defer service.Close()
	ctx := context.Background()
	created, err := service.Create(ctx, "user", CreateRequest{Name: "HTTP fixture", SiteURL: baseURL + "/", Platform: PlatformNewAPI, Account: "fixture-user", Password: "fixture-password", RechargeRate: 1})
	if err != nil || created.Status != StatusConnected || created.BaseURL != baseURL {
		t.Fatalf("HTTP 创建登录失败：%+v, %v", created, err)
	}
	if len(repo.saved) != 2 || repo.saved[1].Session == nil || repo.saved[1].Session.BaseURL != baseURL {
		t.Fatal("登录后的 HTTP 会话未经过正式保存入口")
	}
	quota = 1000000
	synced, err := service.Sync(ctx, "user", created.ID)
	if err != nil || synced.Status != StatusConnected || synced.Metrics.Balance.Value == nil || *synced.Metrics.Balance.Value != 2 {
		t.Fatalf("保存后的 HTTP 同步未更新指标：%+v, %v", synced, err)
	}
	if logins != 1 || selfRequests != 2 || len(repo.saved) != 3 || repo.saved[2].LastSyncedAt == nil {
		t.Fatal("同步错误地重新登录或未保存最新状态")
	}

	// 旧 URL 校验失败站点没有 Session：保留现有重新输入凭据的恢复入口，不自动恢复认证。
	failed := newTestSite("failed", "user", "workspace", 1, nil)
	failed.Platform, failed.BaseURL, failed.Status = PlatformNewAPI, baseURL, StatusError
	cache.add(failed)
	if _, err := service.Sync(ctx, "user", failed.ID); err == nil || errorKey(err) != ErrorNotFound {
		t.Fatal("无会话站点不应通过同步凭空生成认证")
	}
	recovered, err := service.Update(ctx, "user", failed.ID, UpdateRequest{Name: "HTTP recovered", SiteURL: baseURL, Platform: PlatformNewAPI, Account: "fixture-user", Password: "fixture-password", RechargeRate: 1})
	stored, _ := cache.Get(ctx, failed.ID)
	if err != nil || recovered.Status != StatusConnected || stored.Session == nil || logins != 2 {
		t.Fatalf("旧失败站点显式登录恢复失败：%+v, %v", recovered, err)
	}
	if _, err := service.Sync(ctx, "user", failed.ID); err != nil {
		t.Fatalf("恢复后同步失败：%v", err)
	}
}
