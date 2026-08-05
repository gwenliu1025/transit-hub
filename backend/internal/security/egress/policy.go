// Package egress 统一约束 TransitHub 的服务端出站目标。
package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var ErrTargetNotPublic = errors.New("出站目标必须是公网地址")

var errTooManyRedirects = errors.New("重定向次数过多")

type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// PublicNetworkDialer 在每次连接前重新解析并校验目标主机，供 SMTP/TLS、Telegram proxy
// 等不能统一为 HTTPS URL 的协议复用同一公网边界。
type PublicNetworkDialer struct {
	resolver Resolver
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
}

func NewPublicNetworkDialer(timeout time.Duration, resolver Resolver) *PublicNetworkDialer {
	dialer := &net.Dialer{Timeout: timeout}
	return &PublicNetworkDialer{resolver: resolver, dial: dialer.DialContext}
}

func (d *PublicNetworkDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrTargetNotPublic
	}
	addresses, err := resolvePublicIPs(ctx, host, d.resolver)
	if err != nil {
		return nil, err
	}
	var dialErr error
	for _, ip := range addresses {
		conn, err := d.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, dialErr
}

// NewPublicHTTPSClient 创建一个只允许公网 HTTPS 目标的客户端。
// DNS 在每次建立连接前重新解析并校验，重定向也使用同一策略。
func NewPublicHTTPSClient(timeout time.Duration, resolver Resolver) *http.Client {
	return newPublicHTTPSClient(timeout, resolver, nil)
}

// NewPublicHTTPSClientWithProxy 创建同样受公网 HTTPS 策略保护、并通过指定公网代理出站的客户端。
func NewPublicHTTPSClientWithProxy(timeout time.Duration, resolver Resolver, proxyURL *url.URL) *http.Client {
	return newPublicHTTPSClient(timeout, resolver, proxyURL)
}

func newPublicHTTPSClient(timeout time.Duration, resolver Resolver, proxyURL *url.URL) *http.Client {
	dialer := NewPublicNetworkDialer(timeout, resolver)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	} else {
		// 服务端安全客户端不继承环境代理，避免绕过目标主机的公网校验边界。
		transport.Proxy = nil
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errTooManyRedirects
		}
		if _, err := ValidatePublicHTTPS(req.Context(), req.URL.String(), resolver); err != nil {
			return err
		}
		if len(via) > 0 && !sameOrigin(via[0].URL, req.URL) {
			for _, header := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Api-Key", "New-Api-User"} {
				req.Header.Del(header)
			}
		}
		return nil
	}
	client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if _, err := ValidatePublicHTTPS(req.Context(), req.URL.String(), resolver); err != nil {
			return nil, err
		}
		return transport.RoundTrip(req)
	})
	return client
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func ValidatePublicHTTPS(ctx context.Context, raw string, resolver Resolver) (*url.URL, error) {
	parsed, err := parsePublicURL(ctx, raw, resolver)
	if err != nil || parsed.Scheme != "https" {
		return nil, ErrTargetNotPublic
	}
	return parsed, nil
}

// ValidatePublicProxyURL 保留 Telegram 现有 HTTP、HTTPS 与 SOCKS5 代理能力，
// 但拒绝 userinfo、非公网主机和其他协议。
func ValidatePublicProxyURL(ctx context.Context, raw string, resolver Resolver) (*url.URL, error) {
	parsed, err := parsePublicURL(ctx, raw, resolver)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return parsed, nil
	default:
		return nil, ErrTargetNotPublic
	}
}

func parsePublicURL(ctx context.Context, raw string, resolver Resolver) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" || parsed.User != nil {
		return nil, ErrTargetNotPublic
	}
	if err := ValidatePublicHost(ctx, parsed.Hostname(), resolver); err != nil {
		return nil, err
	}
	return parsed, nil
}

func ValidatePublicHost(ctx context.Context, host string, resolver Resolver) error {
	_, err := resolvePublicIPs(ctx, host, resolver)
	return err
}

func resolvePublicIPs(ctx context.Context, host string, resolver Resolver) ([]net.IP, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, ErrTargetNotPublic
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return nil, ErrTargetNotPublic
		}
		return []net.IP{ip}, nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, ErrTargetNotPublic
	}
	result := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		if !isPublicIP(address.IP) {
			return nil, ErrTargetNotPublic
		}
		result = append(result, address.IP)
	}
	return result, nil
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// nonPublicPrefixes 覆盖 RFC 6890/5737/6598 等特殊用途地址；Go 的 IsPrivate
// 只覆盖 RFC1918 和 IPv6 ULA，无法单独代表“公网可达”。
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
}
