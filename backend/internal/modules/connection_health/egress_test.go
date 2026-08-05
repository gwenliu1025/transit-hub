package connection_health

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func newTestRealProbeRunner() *RealProbeRunner {
	return NewRealProbeRunnerWithClient(http.DefaultClient)
}

func newTestModelDiscoveryRunner() *ModelDiscoveryRunner {
	return NewModelDiscoveryRunnerWithClient(http.DefaultClient)
}

func TestProductionProbeRunnerRejectsLoopbackHTTP(t *testing.T) {
	outcome := NewRealProbeRunner().Probe(context.Background(), ProbeRequest{
		BaseURL:     "http://127.0.0.1:1",
		UpstreamKey: "secret",
		ModelName:   "gpt-4o-mini",
	})
	if outcome.Result != ResultNetworkFluctuation || !strings.Contains(outcome.Detail, "出站目标必须是公网地址") {
		t.Fatalf("生产探活构造器应拒绝环回 HTTP，得到 %+v", outcome)
	}
}

func TestProductionModelDiscoveryRejectsLoopbackHTTP(t *testing.T) {
	_, err := NewModelDiscoveryRunner().ListModels(context.Background(), "http://127.0.0.1:1", "secret")
	if err == nil || err.Error() != ErrorModelListUnavailable {
		t.Fatalf("生产模型发现构造器应拒绝环回 HTTP，得到 %v", err)
	}
}
