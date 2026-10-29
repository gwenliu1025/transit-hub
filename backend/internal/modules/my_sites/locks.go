package my_sites

import (
	"strings"
	"sync"

	"transithub/backend/internal/modules/upstream"
)

// keyedMutex 为同一个键提供进程内互斥，空闲后自动回收，零值可直接使用。
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedMutexEntry
}

type keyedMutexEntry struct {
	mu   sync.Mutex
	refs int
}

// lock 阻塞直到获得 key 对应的锁，返回的函数用于释放。
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*keyedMutexEntry)
	}
	entry := k.locks[key]
	if entry == nil {
		entry = &keyedMutexEntry{}
		k.locks[key] = entry
	}
	entry.refs++
	k.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// keyedTryLock 只做非阻塞占位：同一个键已被持有时立即返回 false。
type keyedTryLock struct {
	mu   sync.Mutex
	held map[string]struct{}
}

func (k *keyedTryLock) tryLock(key string) (func(), bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.held == nil {
		k.held = make(map[string]struct{})
	}
	if _, busy := k.held[key]; busy {
		return nil, false
	}
	k.held[key] = struct{}{}
	return func() {
		k.mu.Lock()
		delete(k.held, key)
		k.mu.Unlock()
	}, true
}

// adminSiteLockKey 以管理端站点身份（平台 + 规范化地址）作为倍率写入的串行化键。
// New API 的 GroupRatio 是整表读改写，同一站点上的自动调价与活动调价必须串行；
// 一个工作区只绑定一个管理端站点，因此该键同时覆盖 (userID, adminAccountID) 维度。
func adminSiteLockKey(session upstream.Session) string {
	baseURL := strings.TrimRight(strings.ToLower(strings.TrimSpace(session.BaseURL)), "/")
	return string(session.Platform) + "|" + baseURL
}

// connectionTargetLockKey 串行化同一工作区内对同一上游分组的创建/关联。
func connectionTargetLockKey(userID, adminAccountID, siteID, groupID string) string {
	return strings.Join([]string{strings.TrimSpace(userID), strings.TrimSpace(adminAccountID), strings.TrimSpace(siteID), strings.TrimSpace(groupID)}, "\x00")
}
