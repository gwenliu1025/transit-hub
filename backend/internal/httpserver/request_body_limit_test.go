package httpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"transithub/backend/internal/shared/httpjson"
)

func TestLimitJSONRequestBodyReturns413WhenTooLarge(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(strings.Repeat("x", int(httpjson.MaxRequestBodyBytes)+1)))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	if !limitJSONRequestBody(recorder, req) {
		t.Fatal("超大 JSON 请求体必须在路由前被拦截")
	}
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 JSON 请求体应返回 413，实际为 %d", recorder.Code)
	}
}

func TestLimitJSONRequestBodyPreservesOrdinaryJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"user@example.com"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	recorder := httptest.NewRecorder()
	if limitJSONRequestBody(recorder, req) {
		t.Fatal("普通 JSON 请求体不应被拦截")
	}
	body, err := io.ReadAll(req.Body)
	if err != nil || string(body) != `{"email":"user@example.com"}` {
		t.Fatalf("普通 JSON 请求体未保持原样：body=%q err=%v", body, err)
	}
}

func TestLimitJSONRequestBodyDoesNotAffectMultipartUploads(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/tickets", strings.NewReader(strings.Repeat("x", int(httpjson.MaxRequestBodyBytes)+1)))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=test")
	recorder := httptest.NewRecorder()
	if limitJSONRequestBody(recorder, req) {
		t.Fatal("multipart 上传不应被 JSON 限制器拦截")
	}
}
