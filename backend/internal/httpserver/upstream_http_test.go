package httpserver

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"transithub/backend/internal/security/egress"
)

func TestServerUpstreamClientUsesPublicHTTPPolicy(t *testing.T) {
	client := newUpstreamHTTPClient()
	defer client.CloseIdleConnections()
	if client.Timeout != upstreamRequestTimeout {
		t.Fatal("服务装配丢失请求超时")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, raw := range []string{"http://8.8.8.8:3200", "https://8.8.8.8:8443"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if _, err := client.Do(req); !errors.Is(err, context.Canceled) {
			t.Fatalf("服务装配未接通公网 HTTP/HTTPS：%v", err)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1", nil)
	if _, err := client.Do(req); !errors.Is(err, egress.ErrTargetNotPublic) {
		t.Fatalf("服务装配丢失私网阻断：%v", err)
	}
}
