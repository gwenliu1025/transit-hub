package connection_health

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"transithub/backend/internal/security/egress"
)

func TestProductionHealthFactoriesAcceptPublicHTTPAndRetainPublicBoundary(t *testing.T) {
	for name, client := range map[string]*http.Client{
		"模型发现": NewModelDiscoveryRunner().client,
		"真实探活": NewRealProbeRunner().client,
	} {
		t.Run(name, func(t *testing.T) {
			defer client.CloseIdleConnections()
			if client.Timeout != ProbeTimeout {
				t.Fatal("探活超时预算变化")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			for _, raw := range []string{"http://8.8.8.8:3200/v1/models", "https://8.8.8.8:8443/v1/models"} {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
				if _, err := client.Do(req); !errors.Is(err, context.Canceled) {
					t.Fatalf("默认工厂未通过公网协议校验进入可取消传输：%v", err)
				}
			}
			for _, raw := range []string{"http://127.0.0.1/v1/models", "http://169.254.169.254/v1/models", "ftp://8.8.8.8/v1/models"} {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
				if _, err := client.Do(req); !errors.Is(err, egress.ErrTargetNotPublic) {
					t.Fatalf("默认工厂未保留公网门禁：%v", err)
				}
			}
		})
	}
}
