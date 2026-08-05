package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEmailCodeUsesSixRandomDigits(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 32; i++ {
		code, err := generateEmailCode()
		if err != nil {
			t.Fatalf("生成验证码失败：%v", err)
		}
		if len(code) != 6 || code < "000000" || code > "999999" {
			t.Fatalf("验证码必须是六位数字，实际为 %q", code)
		}
		seen[code] = struct{}{}
	}
	if len(seen) == 1 {
		t.Fatal("连续生成的验证码不应全部相同")
	}
}

func TestEmailCodeResponseDoesNotExposeCode(t *testing.T) {
	if _, ok := reflect.TypeOf(EmailCodeResponse{}).FieldByName("Code"); ok {
		t.Fatal("验证码响应不得包含 code 字段")
	}
}

func TestServiceHasRegistrationEmailDeliveryBoundary(t *testing.T) {
	if _, ok := reflect.TypeOf(Service{}).FieldByName("emailSender"); !ok {
		t.Fatal("验证码生成前必须存在真实邮件投递边界，以便未配置时 fail-closed")
	}
}

func TestServiceHasLoginRateLimiter(t *testing.T) {
	if _, ok := reflect.TypeOf(Service{}).FieldByName("loginLimiter"); !ok {
		t.Fatal("登录服务必须接入按账号和客户端 IP 的限速器")
	}
}

func TestEmailCodeConsumptionReturnsWhetherAtomicClaimSucceeded(t *testing.T) {
	method, ok := reflect.TypeOf(&Repository{}).MethodByName("ConsumeEmailCode")
	if !ok || method.Type.NumOut() != 2 || method.Type.Out(0).Kind() != reflect.Bool {
		t.Fatal("验证码消费必须以原子条件更新返回是否成功")
	}
}

func TestRegistrationDisabledStillReturnsForbidden(t *testing.T) {
	mux := http.NewServeMux()
	RegisterRoutes(mux, nil, false)
	for _, path := range []string{"/api/auth/email-code", "/api/auth/register"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"email":"user@example.com"}`))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, req)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s 应返回 403，实际为 %d", path, response.Code)
		}
	}
}

func TestRequestClientIPIgnoresUntrustedForwardedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = "203.0.113.7:43210"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	if got := requestClientIP(req); got != "203.0.113.7" {
		t.Fatalf("客户端 IP 必须来自连接对端，实际为 %q", got)
	}
}

func TestLoginLimitKeyBindsNormalizedAccountAndClientIP(t *testing.T) {
	accountKey, _ := loginLimitKeys(" User@Example.com ", "203.0.113.7")
	normalizedKey, _ := loginLimitKeys("user@example.com", "203.0.113.7")
	if accountKey != normalizedKey {
		t.Fatal("账号规范化前后不应产生不同限速键")
	}
	ipKey, _ := loginLimitKeys("user@example.com", "203.0.113.8")
	if accountKey == ipKey {
		t.Fatal("不同客户端 IP 必须使用不同限速键")
	}
}

type errorLoginLimiter struct{}

func (errorLoginLimiter) Check(context.Context, string, string) (time.Duration, error) {
	return 0, errors.New("redis unavailable")
}
func (errorLoginLimiter) RecordFailure(context.Context, string, string) error { return nil }
func (errorLoginLimiter) Reset(context.Context, string, string) error         { return nil }

type fakeAuthRepository struct {
	passwordHash string
	userID       string
	savedEmail   string
	savedHash    string
	savedExpiry  time.Time
}

func (*fakeAuthRepository) EnsureSchema(context.Context) error      { return nil }
func (*fakeAuthRepository) CountUsers(context.Context) (int, error) { return 1, nil }

func (r *fakeAuthRepository) SaveEmailCode(_ context.Context, email string, codeHash string, expiry time.Time) error {
	r.savedEmail, r.savedHash, r.savedExpiry = email, codeHash, expiry
	return nil
}
func (*fakeAuthRepository) LatestEmailCode(context.Context, string) (*EmailVerification, error) {
	return nil, nil
}
func (*fakeAuthRepository) ConsumeEmailCode(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}
func (*fakeAuthRepository) CreateUser(context.Context, string, string) error { return nil }
func (r *fakeAuthRepository) PasswordHashByEmail(context.Context, string) (string, error) {
	return r.passwordHash, nil
}
func (r *fakeAuthRepository) UserIDByEmail(context.Context, string) (string, error) {
	return r.userID, nil
}
func (*fakeAuthRepository) CreateSession(context.Context, string, string, time.Time) error {
	return nil
}
func (*fakeAuthRepository) UserIDBySessionToken(context.Context, string) (string, error) {
	return "", nil
}

type recordingLoginLimiter struct {
	wait     time.Duration
	failures int
	resets   int
	account  string
	clientIP string
	resetErr error
}

type recordingEmailSender struct {
	email string
	code  string
}

func (s *recordingEmailSender) SendVerificationCode(_ context.Context, email string, code string) error {
	s.email, s.code = email, code
	return nil
}

func (l *recordingLoginLimiter) Check(_ context.Context, account string, clientIP string) (time.Duration, error) {
	l.account, l.clientIP = account, clientIP
	return l.wait, nil
}
func (l *recordingLoginLimiter) RecordFailure(context.Context, string, string) error {
	l.failures++
	return nil
}
func (l *recordingLoginLimiter) Reset(context.Context, string, string) error {
	l.resets++
	return l.resetErr
}

func TestLoginLimiterFailureFailsClosedWithoutPermanentLockResponse(t *testing.T) {
	service := NewService(nil)
	service.SetLoginLimiter(errorLoginLimiter{})
	_, err := service.Login(context.Background(), LoginRequest{Email: "user@example.com", Password: "secret"}, "203.0.113.7")
	var requestErr *RequestError
	if !errors.As(err, &requestErr) {
		t.Fatalf("预期认证请求错误，实际为 %v", err)
	}
	if requestErr.Status != http.StatusServiceUnavailable || requestErr.Message != "auth.errors.temporarilyUnavailable" {
		t.Fatalf("Redis 故障应返回临时 503，实际为 %#v", requestErr)
	}
	if requestErr.Status == http.StatusTooManyRequests {
		t.Fatal("Redis 故障不得伪装成永久限速")
	}
}

func TestMissingEmailSenderFailsClosedWithoutEcho(t *testing.T) {
	service := NewService(nil)
	response, err := service.RequestEmailCode(context.Background(), EmailCodeRequest{Email: "user@example.com"})
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("未配置投递器应返回临时不可用，实际响应=%#v 错误=%v", response, err)
	}
	payload, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(payload), "code") || strings.Contains(string(payload), "123456") {
		t.Fatalf("响应不得回显验证码：%s", payload)
	}
}

func TestEmailCodeIsDeliveredAndOnlyHashIsStoredWithExpiry(t *testing.T) {
	repository := &fakeAuthRepository{}
	sender := &recordingEmailSender{}
	service := NewService(repository)
	service.SetEmailCodeSender(sender)
	before := time.Now()
	response, err := service.RequestEmailCode(context.Background(), EmailCodeRequest{Email: " User@Example.com "})
	if err != nil || !response.Success {
		t.Fatalf("请求验证码失败：response=%#v err=%v", response, err)
	}
	if sender.email != "user@example.com" || len(sender.code) != 6 {
		t.Fatalf("邮件投递参数错误：email=%q code=%q", sender.email, sender.code)
	}
	if repository.savedEmail != sender.email || repository.savedHash != hashValue(sender.code) {
		t.Fatalf("仓库只能保存验证码哈希：email=%q hash=%q", repository.savedEmail, repository.savedHash)
	}
	if repository.savedHash == sender.code {
		t.Fatal("仓库不得保存验证码明文")
	}
	if repository.savedExpiry.Before(before.Add(emailCodeValidity-time.Second)) || repository.savedExpiry.After(time.Now().Add(emailCodeValidity+time.Second)) {
		t.Fatalf("验证码过期时间不符合预期：%v", repository.savedExpiry)
	}
}

func TestLoginFailureUsesAccountAndClientIPAndReturnsGenericError(t *testing.T) {
	repository := &fakeAuthRepository{}
	limiter := &recordingLoginLimiter{}
	service := NewService(repository)
	service.SetLoginLimiter(limiter)
	_, err := service.Login(context.Background(), LoginRequest{Email: " User@Example.com ", Password: "wrong"}, "203.0.113.7")
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Status != http.StatusUnauthorized || requestErr.Message != "auth.errors.invalidCredentials" {
		t.Fatalf("不存在账号必须返回通用凭据错误，实际为 %v", err)
	}
	if limiter.account != "user@example.com" || limiter.clientIP != "203.0.113.7" || limiter.failures != 1 {
		t.Fatalf("限速维度错误：account=%q ip=%q failures=%d", limiter.account, limiter.clientIP, limiter.failures)
	}
}

func TestLoginSuccessResetsRateLimit(t *testing.T) {
	hash, err := hashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	limiter := &recordingLoginLimiter{}
	service := NewService(&fakeAuthRepository{passwordHash: hash, userID: "usr-1"})
	service.SetLoginLimiter(limiter)
	if _, err := service.Login(context.Background(), LoginRequest{Email: "user@example.com", Password: "correct-password"}, "203.0.113.7"); err != nil {
		t.Fatalf("正确凭据登录失败：%v", err)
	}
	if limiter.resets != 1 || limiter.failures != 0 {
		t.Fatalf("成功登录应清零限速，resets=%d failures=%d", limiter.resets, limiter.failures)
	}
}

func TestBlockedLoginReturnsGenericRateLimitError(t *testing.T) {
	limiter := &recordingLoginLimiter{wait: time.Second}
	service := NewService(&fakeAuthRepository{})
	service.SetLoginLimiter(limiter)
	_, err := service.Login(context.Background(), LoginRequest{Email: "user@example.com", Password: "secret"}, "203.0.113.7")
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Status != http.StatusTooManyRequests || requestErr.Message != "auth.errors.loginRateLimited" {
		t.Fatalf("限速响应不符合预期：%v", err)
	}
}

func TestLoginDoesNotSucceedWhenRateLimitResetFails(t *testing.T) {
	hash, err := hashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	limiter := &recordingLoginLimiter{resetErr: errors.New("redis unavailable")}
	service := NewService(&fakeAuthRepository{passwordHash: hash, userID: "usr-1"})
	service.SetLoginLimiter(limiter)
	_, err = service.Login(context.Background(), LoginRequest{Email: "user@example.com", Password: "correct-password"}, "203.0.113.7")
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("清零失败必须 fail-closed，实际为 %v", err)
	}
}
