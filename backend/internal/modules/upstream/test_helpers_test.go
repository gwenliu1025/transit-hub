package upstream

import (
	"net/http"
	"testing"
)

func TestNewHTTPClientNilDefaultsToSafeClient(t *testing.T) {
	_, err := NewHTTPClient(nil).requestJSON("http://127.0.0.1:1", requestOptions{})
	if err == nil {
		t.Fatal("nil 默认客户端应拒绝环回 HTTP")
	}
}

// newTestPlatformService 让旧 httptest 明文环回夹具显式选择不受限 URL 校验；
// 生产 NewPlatformService 仍默认只允许公网 HTTPS。
func newTestPlatformService(client *http.Client) *PlatformService {
	return newPlatformServiceWithURLValidator(NewHTTPClient(client), func(string) bool { return true })
}
