package users

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUserJSONDoesNotExposePasswordHashField(t *testing.T) {
	payload, err := json.Marshal(User{})
	if err != nil {
		t.Fatalf("编码用户响应失败：%v", err)
	}
	if strings.Contains(string(payload), "passwordHash") {
		t.Fatalf("用户目录响应不得包含 passwordHash 字段：%s", payload)
	}
}
