package upstream

import (
	"strings"
	"testing"
)

func TestParseJSONRejectsOversizedResponse(t *testing.T) {
	body := `{"value":"` + strings.Repeat("x", int(maxJSONResponseBytes)) + `"}`
	if _, err := parseJSON(strings.NewReader(body), "https://public.example"); err == nil {
		t.Fatal("超大上游响应体必须被拒绝")
	}
}
