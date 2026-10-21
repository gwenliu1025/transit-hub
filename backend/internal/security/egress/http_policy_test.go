package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestValidatePublicHTTPProtocolsAndTargets(t *testing.T) {
	resolver := staticResolver{
		"public.example": {{IP: net.ParseIP("93.184.216.34")}},
		"mixed.example":  {{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("10.0.0.1")}},
	}
	for _, raw := range []string{"http://public.example", "https://public.example:8443/api", "http://8.8.8.8:3200/", "https://8.8.8.8:8443", "http://[2606:4700:4700::1111]:3200"} {
		t.Run(raw, func(t *testing.T) {
			parsed, err := ValidatePublicHTTP(context.Background(), raw, resolver)
			if err != nil || parsed.String() != raw {
				t.Fatalf("公网 HTTP/HTTPS 地址未被保留：%v, %v", parsed, err)
			}
		})
	}
	for _, raw := range []string{"ftp://public.example", "file:///etc/passwd", "http://user:password@public.example", "http://127.0.0.1", "http://[::1]", "http://10.0.0.1", "http://169.254.169.254", "http://mixed.example"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ValidatePublicHTTP(context.Background(), raw, resolver); !errors.Is(err, ErrTargetNotPublic) {
				t.Fatalf("不安全目标未被拒绝：%v", err)
			}
		})
	}
}

// 只替换最底层协议传输；公网校验、首包门禁和真实 http.Client 重定向逻辑保持生产实现。
func newHTTPPolicyTestClient(t *testing.T, handler func(*http.Request) (*http.Response, error)) *http.Client {
	t.Helper()
	client := NewPublicHTTPClient(time.Second, nil)
	transport := client.Transport.(*publicTransport).transport
	transport.RegisterProtocol("http", roundTripperFunc(handler))
	transport.RegisterProtocol("https", roundTripperFunc(handler))
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func policyResponse(req *http.Request, code int, location string) *http.Response {
	header := make(http.Header)
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader("ok")), Request: req}
}

func TestPublicHTTPClientFirstRequestAndHTTPSOnlyIsolation(t *testing.T) {
	calls := 0
	client := newHTTPPolicyTestClient(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return policyResponse(req, http.StatusOK, ""), nil
	})
	for _, raw := range []string{"http://8.8.8.8:3200/", "https://8.8.8.8:8443/"} {
		resp, err := client.Get(raw)
		if err != nil {
			t.Fatalf("公网首包应进入传输：%v", err)
		}
		resp.Body.Close()
	}
	for _, raw := range []string{"http://127.0.0.1", "http://169.254.169.254", "ftp://8.8.8.8"} {
		if _, err := client.Get(raw); !errors.Is(err, ErrTargetNotPublic) {
			t.Fatalf("不安全首包应在传输前拒绝：%v", err)
		}
	}
	if _, err := NewPublicHTTPSClient(time.Second, nil).Get("http://8.8.8.8"); !errors.Is(err, ErrTargetNotPublic) {
		t.Fatalf("HTTPS-only 消费者被意外放宽：%v", err)
	}
	if calls != 2 {
		t.Fatalf("不安全请求触及底层传输，calls=%d", calls)
	}
}

func TestPublicHTTPClientRedirectBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, start, target, method, body string
		status                            int
		wantErr                           bool
		wantCredentials                   bool
	}{
		{"同源307登录", "http://8.8.8.8:3200/start", "http://8.8.8.8:3200/next", "POST", "password=fixture", 307, false, true},
		{"同源308登录", "http://8.8.8.8:3200/start", "http://8.8.8.8:3200/next", "POST", "password=fixture", 308, false, true},
		{"跨源307登录", "http://8.8.8.8/start", "http://1.1.1.1/next", "POST", "password=fixture", 307, true, false},
		{"跨源308登录", "http://8.8.8.8/start", "https://1.1.1.1/next", "POST", "password=fixture", 308, true, false},
		{"不同端口重放", "http://8.8.8.8:3200/start", "http://8.8.8.8:3201/next", "POST", "password=fixture", 307, true, false},
		{"HTTPS降级", "https://8.8.8.8/start", "http://8.8.8.8/next", "GET", "", 302, true, false},
		{"私网跳转", "http://8.8.8.8/start", "http://127.0.0.1/next", "GET", "", 302, true, false},
		{"跨源GET", "http://8.8.8.8/start", "http://1.1.1.1/next", "GET", "", 302, false, false},
		{"HTTP升级GET", "http://8.8.8.8/start", "https://8.8.8.8/next", "GET", "", 302, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := newHTTPPolicyTestClient(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return policyResponse(req, tc.status, tc.target), nil
				}
				if tc.wantErr {
					t.Fatal("拒绝的重定向已进入传输")
				}
				for _, header := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Api-Key", "New-Api-User"} {
					if (req.Header.Get(header) != "") != tc.wantCredentials {
						t.Fatalf("重定向凭据 %s 边界错误", header)
					}
				}
				if tc.body != "" {
					body, _ := io.ReadAll(req.Body)
					if req.Method != tc.method || string(body) != tc.body {
						t.Fatal("同源重定向未保留登录请求")
					}
				}
				return policyResponse(req, http.StatusOK, ""), nil
			})
			req, _ := http.NewRequest(tc.method, tc.start, strings.NewReader(tc.body))
			for _, header := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Api-Key", "New-Api-User"} {
				req.Header.Set(header, "fixture")
			}
			resp, err := client.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("重定向结果错误：%v", err)
			}
			wantCalls := 2
			if tc.wantErr {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("传输次数=%d，期望=%d", calls, wantCalls)
			}
		})
	}
}

func TestPublicHTTPClientRedirectLimit(t *testing.T) {
	calls := 0
	client := newHTTPPolicyTestClient(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return policyResponse(req, http.StatusFound, "http://8.8.8.8/loop"), nil
	})
	resp, err := client.Get("http://8.8.8.8/start")
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, errTooManyRedirects) || calls != 10 {
		t.Fatalf("重定向限额失效：%v，calls=%d", err, calls)
	}
}

func TestPublicHTTPClientRebindingBeforeDial(t *testing.T) {
	resolver := &sequenceResolver{answers: [][]net.IPAddr{
		{{IP: net.ParseIP("93.184.216.34")}},
		{{IP: net.ParseIP("127.0.0.1")}},
	}}
	_, err := NewPublicHTTPClient(time.Second, resolver).Get("http://public.example:3200/")
	if !errors.Is(err, ErrTargetNotPublic) || resolver.calls != 2 {
		t.Fatalf("首包校验之后的重绑定未在拨号前拒绝：%v，calls=%d", err, resolver.calls)
	}
}

func TestPublicHTTPClientCancellationAndTimeout(t *testing.T) {
	client := NewPublicHTTPClient(250*time.Millisecond, nil)
	if client.Timeout != 250*time.Millisecond {
		t.Fatal("客户端超时预算未保留")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://8.8.8.8:3200", nil)
	if _, err := client.Do(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消未传播到传输：%v", err)
	}
}
