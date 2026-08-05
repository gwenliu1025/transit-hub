package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

type staticResolver map[string][]net.IPAddr

func (r staticResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return r[host], nil
}

func TestNewPublicHTTPSClientRejectsUnsafeRedirect(t *testing.T) {
	resolver := staticResolver{
		"public.example":  {{IP: net.ParseIP("93.184.216.34")}},
		"private.example": {{IP: net.ParseIP("10.0.0.8")}},
	}
	client := NewPublicHTTPSClient(time.Second, resolver)
	req, err := http.NewRequest(http.MethodGet, "https://public.example/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := http.NewRequest(http.MethodGet, "https://private.example/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(redirect, []*http.Request{req}); err == nil {
		t.Fatal("重定向到私网目标必须被拒绝")
	}
}

func TestNewPublicHTTPSClientStripsCredentialsOnCrossOriginRedirect(t *testing.T) {
	resolver := staticResolver{
		"public.example": {{IP: net.ParseIP("93.184.216.34")}},
		"other.example":  {{IP: net.ParseIP("93.184.216.35")}},
	}
	client := NewPublicHTTPSClient(time.Second, resolver)
	original, _ := http.NewRequest(http.MethodGet, "https://public.example/start", nil)
	redirect, _ := http.NewRequest(http.MethodGet, "https://other.example/next", nil)
	redirect.Header.Set("Authorization", "Bearer secret")
	redirect.Header.Set("Cookie", "session=secret")
	redirect.Header.Set("X-Api-Key", "secret")
	redirect.Header.Set("New-Api-User", "42")
	if err := client.CheckRedirect(redirect, []*http.Request{original}); err != nil {
		t.Fatalf("合法公网重定向被拒绝：%v", err)
	}
	for _, header := range []string{"Authorization", "Cookie", "X-Api-Key", "New-Api-User"} {
		if redirect.Header.Get(header) != "" {
			t.Fatalf("跨来源重定向仍携带 %s", header)
		}
	}
}

func TestValidatePublicHTTPSRejectsUnsafeTargets(t *testing.T) {
	resolver := staticResolver{
		"public.example":  {{IP: net.ParseIP("93.184.216.34")}},
		"private.example": {{IP: net.ParseIP("10.0.0.8")}},
		"mixed.example":   {{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("127.0.0.1")}},
	}
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"明文 HTTP", "http://public.example"},
		{"URL 凭据", "https://user:pass@public.example"},
		{"环回地址", "https://127.0.0.1"},
		{"IPv6 环回", "https://[::1]"},
		{"CGNAT", "https://100.64.0.1"},
		{"基准测试网段", "https://198.18.0.1"},
		{"IPv4 文档网段", "https://192.0.2.1"},
		{"IPv6 文档网段", "https://[2001:db8::1]"},
		{"私网 DNS", "https://private.example"},
		{"混合 DNS", "https://mixed.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ValidatePublicHTTPS(context.Background(), tc.raw, resolver); err == nil {
				t.Fatalf("目标 %q 应被拒绝", tc.raw)
			}
		})
	}
}

func TestValidatePublicHTTPSAcceptsPublicTarget(t *testing.T) {
	resolver := staticResolver{"public.example": {{IP: net.ParseIP("93.184.216.34")}}}
	parsed, err := ValidatePublicHTTPS(context.Background(), "https://public.example/api", resolver)
	if err != nil {
		t.Fatalf("合法公网 HTTPS 被拒绝：%v", err)
	}
	if parsed.String() != "https://public.example/api" {
		t.Fatalf("规范化地址 = %q", parsed.String())
	}
}

func TestValidatePublicHostAllowsProtocolSpecificPublicTargets(t *testing.T) {
	resolver := staticResolver{"mail.example": {{IP: net.ParseIP("93.184.216.34")}}}
	if err := ValidatePublicHost(context.Background(), "mail.example", resolver); err != nil {
		t.Fatalf("合法公网 SMTP/代理目标被拒绝：%v", err)
	}
	if err := ValidatePublicHost(context.Background(), "10.0.0.1", resolver); err == nil {
		t.Fatal("私网 SMTP/代理目标应被拒绝")
	}
}

type sequenceResolver struct {
	answers [][]net.IPAddr
	calls   int
}

func (r *sequenceResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	index := r.calls
	r.calls++
	if index >= len(r.answers) {
		index = len(r.answers) - 1
	}
	return r.answers[index], nil
}

func TestPublicNetworkDialerRevalidatesDNSBeforeEveryDial(t *testing.T) {
	resolver := &sequenceResolver{answers: [][]net.IPAddr{
		{{IP: net.ParseIP("93.184.216.34")}},
		{{IP: net.ParseIP("127.0.0.1")}},
	}}
	dialer := NewPublicNetworkDialer(time.Second, resolver)
	dialer.dial = func(_ context.Context, _ string, address string) (net.Conn, error) {
		if address != "93.184.216.34:443" {
			t.Fatalf("应拨号本次已校验的 IP，得到 %q", address)
		}
		return nil, errors.New("测试拨号停止")
	}

	_, firstErr := dialer.DialContext(context.Background(), "tcp", "public.example:443")
	if firstErr == nil || errors.Is(firstErr, ErrTargetNotPublic) {
		t.Fatalf("第一次公网解析应进入实际拨号，得到 %v", firstErr)
	}
	_, secondErr := dialer.DialContext(context.Background(), "tcp", "public.example:443")
	if !errors.Is(secondErr, ErrTargetNotPublic) {
		t.Fatalf("第二次重绑定到环回应被拒绝，得到 %v", secondErr)
	}
	if resolver.calls != 2 {
		t.Fatalf("每次拨号都应重新解析 DNS，调用次数=%d", resolver.calls)
	}
}

func TestNewPublicHTTPSClientRejectsPlainHTTPBeforeTransport(t *testing.T) {
	client := NewPublicHTTPSClient(time.Second, staticResolver{
		"public.example": {{IP: net.ParseIP("93.184.216.34")}},
	})
	req, err := http.NewRequest(http.MethodGet, "http://public.example/path", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(req); !errors.Is(err, ErrTargetNotPublic) {
		t.Fatalf("明文 HTTP 应在传输前被拒绝，得到 %v", err)
	}
}

func TestNewPublicHTTPSClientRejectsUnsafeProxy(t *testing.T) {
	resolver := staticResolver{
		"public.example":  {{IP: net.ParseIP("93.184.216.34")}},
		"private.example": {{IP: net.ParseIP("10.0.0.8")}},
	}
	proxy, err := url.Parse("http://private.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	client := NewPublicHTTPSClientWithProxy(time.Second, resolver, proxy)
	req, _ := http.NewRequest(http.MethodGet, "https://public.example/path", nil)
	if _, err := client.Do(req); !errors.Is(err, ErrTargetNotPublic) {
		t.Fatalf("私网代理应在拨号前被拒绝，得到 %v", err)
	}
}

func TestValidatePublicProxyURLPreservesSupportedProtocol(t *testing.T) {
	resolver := staticResolver{"proxy.example": {{IP: net.ParseIP("93.184.216.34")}}}
	for _, raw := range []string{"http://proxy.example:8080", "https://proxy.example:8443", "socks5://proxy.example:1080"} {
		parsed, err := ValidatePublicProxyURL(context.Background(), raw, resolver)
		if err != nil {
			t.Fatalf("合法公网代理 %q 被拒绝：%v", raw, err)
		}
		if parsed.String() != raw {
			t.Fatalf("代理地址被意外改写：%q", parsed.String())
		}
	}
}

func TestValidatePublicProxyURLRejectsUnsafeTargets(t *testing.T) {
	resolver := staticResolver{
		"public.example":  {{IP: net.ParseIP("93.184.216.34")}},
		"private.example": {{IP: net.ParseIP("192.168.1.2")}},
	}
	for _, raw := range []string{
		"ftp://public.example:21",
		"http://user:pass@public.example:8080",
		"socks5://private.example:1080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
	} {
		if _, err := ValidatePublicProxyURL(context.Background(), raw, resolver); err == nil {
			t.Fatalf("不安全代理 %q 应被拒绝", raw)
		}
	}
}
