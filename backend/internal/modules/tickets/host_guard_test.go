package tickets

import (
	"context"
	"testing"
)

func TestNormalizeSrcHostRejectsUnsafeTargets(t *testing.T) {
	for _, raw := range []string{
		"http://example.com",
		"https://user:pass@example.com",
		"https://127.0.0.1",
		"https://[::1]",
		"https://169.254.169.254",
	} {
		if _, err := normalizeSrcHost(context.Background(), raw); err == nil {
			t.Fatalf("不安全工单来源 %q 应被拒绝", raw)
		}
	}
}

func TestNewSub2APIClientNilDefaultsToSafeClient(t *testing.T) {
	_, err := NewSub2APIClient(nil).FetchCurrentUser("http://127.0.0.1:1", "secret")
	if err == nil {
		t.Fatal("nil 默认客户端应拒绝环回 HTTP")
	}
}
