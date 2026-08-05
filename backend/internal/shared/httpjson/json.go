package httpjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// MaxRequestBodyBytes 是普通 JSON API 的统一请求体上限。
const MaxRequestBodyBytes int64 = 1 << 20

type ErrorResponse struct {
	Message string `json:"message"`
}

func Write(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func WriteError(w http.ResponseWriter, status int, message string) {
	Write(w, status, ErrorResponse{Message: message})
}

func Decode(r *http.Request, target any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodyBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > MaxRequestBodyBytes {
		return errors.New("JSON request body too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("invalid JSON body")
		}
		return err
	}
	return nil
}
