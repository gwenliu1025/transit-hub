package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	loginAttemptWindow = 15 * time.Minute
	loginMaxBackoff    = 5 * time.Minute
)

var recordLoginFailureScript = redis.NewScript(`
local attempts = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[1])
local exponent = attempts - 1
if exponent > 8 then exponent = 8 end
local delay = 1000 * (2 ^ exponent)
if delay > tonumber(ARGV[2]) then delay = tonumber(ARGV[2]) end
redis.call('PSETEX', KEYS[2], delay, '1')
return delay
`)

type RedisLoginLimiter struct {
	client *redis.Client
}

func NewRedisLoginLimiter(client *redis.Client) *RedisLoginLimiter {
	return &RedisLoginLimiter{client: client}
}

func (l *RedisLoginLimiter) Check(ctx context.Context, account string, clientIP string) (time.Duration, error) {
	_, blockedKey := loginLimitKeys(account, clientIP)
	ttl, err := l.client.PTTL(ctx, blockedKey).Result()
	if err != nil {
		return 0, err
	}
	if ttl <= 0 {
		return 0, nil
	}
	return ttl, nil
}

func (l *RedisLoginLimiter) RecordFailure(ctx context.Context, account string, clientIP string) error {
	attemptKey, blockedKey := loginLimitKeys(account, clientIP)
	return recordLoginFailureScript.Run(
		ctx,
		l.client,
		[]string{attemptKey, blockedKey},
		loginAttemptWindow.Milliseconds(),
		loginMaxBackoff.Milliseconds(),
	).Err()
}

func (l *RedisLoginLimiter) Reset(ctx context.Context, account string, clientIP string) error {
	attemptKey, blockedKey := loginLimitKeys(account, clientIP)
	return l.client.Del(ctx, attemptKey, blockedKey).Err()
}

func loginLimitKeys(account string, clientIP string) (string, string) {
	sum := sha256.Sum256([]byte(normalizeEmail(account) + "\x00" + strings.TrimSpace(clientIP)))
	suffix := hex.EncodeToString(sum[:])
	return "auth:login:attempts:" + suffix, "auth:login:blocked:" + suffix
}
