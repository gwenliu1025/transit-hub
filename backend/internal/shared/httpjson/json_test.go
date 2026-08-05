package httpjson

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeRejectsOversizedJSONBody(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"value":"`+strings.Repeat("x", int(MaxRequestBodyBytes))+`"}`))
	var target struct {
		Value string `json:"value"`
	}
	if err := Decode(req, &target); err == nil {
		t.Fatal("超大 JSON 请求体必须被拒绝")
	}
}

func TestDecodeAcceptsOrdinaryJSONBody(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"value":"ok"}`))
	var target struct {
		Value string `json:"value"`
	}
	if err := Decode(req, &target); err != nil {
		t.Fatalf("普通 JSON 请求体被拒绝：%v", err)
	}
	if target.Value != "ok" {
		t.Fatalf("解码值 = %q", target.Value)
	}
}

func TestDecodeRejectsOversizedTrailingGarbage(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"value":"ok"}`+strings.Repeat(" ", int(MaxRequestBodyBytes))))
	var target struct {
		Value string `json:"value"`
	}
	if err := Decode(req, &target); err == nil {
		t.Fatal("合法 JSON 后附加的超大垃圾数据必须被拒绝")
	}
}

func TestDecodeRejectsSecondJSONValue(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"value":"first"} {"value":"second"}`))
	var target struct {
		Value string `json:"value"`
	}
	if err := Decode(req, &target); err == nil {
		t.Fatal("请求体包含第二个 JSON 值时必须被拒绝")
	}
}
