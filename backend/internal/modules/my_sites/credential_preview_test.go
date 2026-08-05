package my_sites

import (
	"strings"
	"testing"
)

func TestSafeCredentialPreviewNeverReturnsFullSecret(t *testing.T) {
	for _, secret := range []string{"a", "short-key", "123456789012", "12345678901234567890"} {
		preview := safeCredentialPreview(secret)
		if preview == secret {
			t.Fatalf("凭据预览不得返回完整密钥：%q", secret)
		}
		if strings.Contains(preview, secret) {
			t.Fatalf("凭据预览不得包含完整密钥：preview=%q", preview)
		}
	}
}

func TestSafeCredentialPreviewKeepsEmptyValueEmpty(t *testing.T) {
	if got := safeCredentialPreview("   "); got != "" {
		t.Fatalf("空凭据预览 = %q，期望空字符串", got)
	}
}
