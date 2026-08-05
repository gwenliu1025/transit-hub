package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const emailCodeValidity = 10 * time.Minute

type Service struct {
	repository   authRepository
	emailSender  EmailCodeSender
	loginLimiter LoginLimiter
}

type authRepository interface {
	EnsureSchema(ctx context.Context) error
	CountUsers(ctx context.Context) (int, error)
	SaveEmailCode(ctx context.Context, email string, codeHash string, expiresAt time.Time) error
	LatestEmailCode(ctx context.Context, email string) (*EmailVerification, error)
	ConsumeEmailCode(ctx context.Context, id string, codeHash string, now time.Time) (bool, error)
	CreateUser(ctx context.Context, email string, passwordHash string) error
	PasswordHashByEmail(ctx context.Context, email string) (string, error)
	UserIDByEmail(ctx context.Context, email string) (string, error)
	CreateSession(ctx context.Context, userID string, tokenHash string, expiresAt time.Time) error
	UserIDBySessionToken(ctx context.Context, tokenHash string) (string, error)
}

// EmailCodeSender 是公开注册前验证码的真实投递边界。未配置时接口必须 fail-closed。
type EmailCodeSender interface {
	SendVerificationCode(ctx context.Context, email string, code string) error
}

type LoginLimiter interface {
	Check(ctx context.Context, account string, clientIP string) (time.Duration, error)
	RecordFailure(ctx context.Context, account string, clientIP string) error
	Reset(ctx context.Context, account string, clientIP string) error
}

type EmailCodeRequest struct {
	Email string `json:"email"`
}

type EmailCodeResponse struct {
	Success bool `json:"success"`
}

type RegisterRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type PasswordLogin struct {
	Account  string `json:"account"`
	Password string `json:"password"`
}

type APIKeyLogin struct {
	APIKey string `json:"apiKey"`
}

type TokenResponse struct {
	Strategy    string `json:"strategy"`
	Subject     string `json:"subject"`
	AccessToken string `json:"accessToken"`
}

type CurrentUser struct {
	ID string
}

type RequestError struct {
	Status  int
	Message string
}

func (e *RequestError) Error() string {
	return e.Message
}

func NewService(repository authRepository) *Service {
	return &Service{repository: repository}
}

func (s *Service) SetEmailCodeSender(sender EmailCodeSender) {
	s.emailSender = sender
}

func (s *Service) SetLoginLimiter(limiter LoginLimiter) {
	s.loginLimiter = limiter
}

func (s *Service) EnsureSchema(ctx context.Context) error {
	return s.repository.EnsureSchema(ctx)
}

// BootstrapAdmin 在启动时检查是否需要创建首个管理员账号。
// 规则：用户表为空时使用 email/password 创建管理员；已有用户时不做任何事。
func (s *Service) BootstrapAdmin(ctx context.Context, email, password string) error {
	count, err := s.repository.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap admin: count users: %w", err)
	}
	if count > 0 {
		log.Printf("[auth] %d users exist, skipping admin bootstrap", count)
		return nil
	}

	// 没有用户，必须有管理员凭据
	email = normalizeEmail(email)
	password = strings.TrimSpace(password)
	if email == "" || password == "" {
		return fmt.Errorf("bootstrap admin: ADMIN_EMAIL and ADMIN_PASSWORD are required when no users exist")
	}

	hash, err := hashPassword(password)
	if err != nil {
		return fmt.Errorf("bootstrap admin: hash password: %w", err)
	}

	if err := s.repository.CreateUser(ctx, email, hash); err != nil {
		return fmt.Errorf("bootstrap admin: create user: %w", err)
	}

	log.Printf("[auth] admin account created for %s", email)
	return nil
}

func (s *Service) RequestEmailCode(ctx context.Context, dto EmailCodeRequest) (EmailCodeResponse, error) {
	email := normalizeEmail(dto.Email)
	if email == "" {
		return EmailCodeResponse{}, requestError(http.StatusBadRequest, "auth.errors.emailRequired")
	}

	// 注册前没有可用的真实投递器时明确拒绝，避免生成一个用户永远收不到的验证码。
	if s.emailSender == nil {
		return EmailCodeResponse{}, requestError(http.StatusServiceUnavailable, "auth.errors.registrationEmailUnavailable")
	}
	code, err := generateEmailCode()
	if err != nil {
		return EmailCodeResponse{}, err
	}
	if err := s.emailSender.SendVerificationCode(ctx, email, code); err != nil {
		return EmailCodeResponse{}, requestError(http.StatusServiceUnavailable, "auth.errors.registrationEmailUnavailable")
	}
	if err := s.repository.SaveEmailCode(ctx, email, hashValue(code), time.Now().Add(emailCodeValidity)); err != nil {
		return EmailCodeResponse{}, err
	}
	return EmailCodeResponse{Success: true}, nil
}

func (s *Service) Register(ctx context.Context, dto RegisterRequest) (TokenResponse, error) {
	email := normalizeEmail(dto.Email)
	password := strings.TrimSpace(dto.Password)
	code := strings.TrimSpace(dto.Code)
	if email == "" || password == "" || code == "" {
		return TokenResponse{}, requestError(http.StatusBadRequest, "auth.errors.invalidRegister")
	}
	// 数据库通过带哈希、未消费和未过期条件的单条 UPDATE 原子认领验证码。
	verification, err := s.repository.LatestEmailCode(ctx, email)
	if err != nil {
		return TokenResponse{}, err
	}
	if verification == nil || time.Now().After(verification.ExpiresAt) {
		return TokenResponse{}, requestError(http.StatusBadRequest, "auth.errors.invalidCode")
	}
	consumed, err := s.repository.ConsumeEmailCode(ctx, verification.ID, hashValue(code), time.Now())
	if err != nil {
		return TokenResponse{}, err
	}
	if !consumed {
		return TokenResponse{}, requestError(http.StatusBadRequest, "auth.errors.invalidCode")
	}
	// 验证码已经原子认领；后续失败时也不恢复，避免重放。客户端可重新申请验证码。
	// 密码必须用带盐哈希保存，避免明文或快速哈希落库。
	passwordHash, err := hashPassword(password)
	if err != nil {
		return TokenResponse{}, err
	}
	if err := s.repository.CreateUser(ctx, email, passwordHash); err != nil {
		if isUniqueViolation(err) {
			return TokenResponse{}, requestError(http.StatusConflict, "auth.errors.emailExists")
		}
		return TokenResponse{}, err
	}
	return s.createSession(ctx, "register", email)
}

func (s *Service) Login(ctx context.Context, dto LoginRequest, clientIPs ...string) (TokenResponse, error) {
	email := normalizeEmail(dto.Email)
	password := strings.TrimSpace(dto.Password)
	if email == "" || password == "" {
		return TokenResponse{}, requestError(http.StatusBadRequest, "auth.errors.invalidLogin")
	}
	clientIP := "unknown"
	if len(clientIPs) > 0 && strings.TrimSpace(clientIPs[0]) != "" {
		clientIP = strings.TrimSpace(clientIPs[0])
	}
	if s.loginLimiter != nil {
		wait, err := s.loginLimiter.Check(ctx, email, clientIP)
		if err != nil {
			return TokenResponse{}, requestError(http.StatusServiceUnavailable, "auth.errors.temporarilyUnavailable")
		}
		if wait > 0 {
			return TokenResponse{}, requestError(http.StatusTooManyRequests, "auth.errors.loginRateLimited")
		}
	}
	passwordHash, err := s.repository.PasswordHashByEmail(ctx, email)
	if err != nil {
		return TokenResponse{}, err
	}
	valid := passwordHash != "" && verifyPassword(passwordHash, password)
	if passwordHash == "" {
		_ = verifyPassword("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy", password)
	}
	if !valid {
		if s.loginLimiter != nil {
			if err := s.loginLimiter.RecordFailure(ctx, email, clientIP); err != nil {
				return TokenResponse{}, requestError(http.StatusServiceUnavailable, "auth.errors.temporarilyUnavailable")
			}
		}
		return TokenResponse{}, requestError(http.StatusUnauthorized, "auth.errors.invalidCredentials")
	}
	if s.loginLimiter != nil {
		if err := s.loginLimiter.Reset(ctx, email, clientIP); err != nil {
			return TokenResponse{}, requestError(http.StatusServiceUnavailable, "auth.errors.temporarilyUnavailable")
		}
	}
	return s.createSession(ctx, "login", email)
}

func (s *Service) LoginWithPassword(dto PasswordLogin) (TokenResponse, bool) {
	if strings.TrimSpace(dto.Account) == "" || strings.TrimSpace(dto.Password) == "" {
		return TokenResponse{}, false
	}
	return TokenResponse{Strategy: "password", Subject: dto.Account, AccessToken: "pending-implementation"}, true
}

func (s *Service) LoginWithAPIKey(dto APIKeyLogin) (TokenResponse, bool) {
	if strings.TrimSpace(dto.APIKey) == "" {
		return TokenResponse{}, false
	}
	return TokenResponse{Strategy: "api-key", Subject: dto.APIKey, AccessToken: "pending-implementation"}, true
}

func (s *Service) CurrentUser(ctx context.Context, accessToken string) (CurrentUser, error) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		return CurrentUser{}, requestError(http.StatusUnauthorized, "auth.errors.unauthorized")
	}
	userID, err := s.repository.UserIDBySessionToken(ctx, hashValue(token))
	if err != nil {
		return CurrentUser{}, err
	}
	if userID == "" {
		return CurrentUser{}, requestError(http.StatusUnauthorized, "auth.errors.unauthorized")
	}
	return CurrentUser{ID: userID}, nil
}

func (s *Service) createSession(ctx context.Context, strategy string, email string) (TokenResponse, error) {
	token, err := randomToken(32)
	if err != nil {
		return TokenResponse{}, err
	}
	userID, err := s.repository.UserIDByEmail(ctx, email)
	if err != nil {
		return TokenResponse{}, err
	}
	if err := s.repository.CreateSession(ctx, userID, hashValue(token), time.Now().Add(7*24*time.Hour)); err != nil {
		return TokenResponse{}, err
	}
	return TokenResponse{Strategy: strategy, Subject: email, AccessToken: token}, nil
}

func normalizeEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func hashValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func verifyPassword(passwordHash string, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) == nil
}

func randomToken(bytesCount int) (string, error) {
	data := make([]byte, bytesCount)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func generateEmailCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func requestError(status int, message string) *RequestError {
	return &RequestError{Status: status, Message: message}
}
