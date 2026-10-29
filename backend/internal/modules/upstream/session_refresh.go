package upstream

import (
	"context"
	"log"
	"reflect"
	"strings"
	"sync"
)

// siteSessionLock 让同一站点的令牌续期串行执行：定时同步、手动同步、看板下钻和日结
// 不会拿同一个 refresh token 并发换新，也不会互相用旧会话覆盖新会话。
func (s *Service) siteSessionLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionLocks == nil {
		s.sessionLocks = make(map[string]*sync.Mutex)
	}
	lock := s.sessionLocks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		s.sessionLocks[id] = lock
	}
	return lock
}

// freshSiteSession 读取站点最新会话，临期（或 force 时）续期，并在拿到新令牌后立即写回缓存和数据库。
// 新令牌的保存不依赖后续指标请求是否成功，避免轮换型 refresh token 被丢失后站点永久失效。
// 写回前确认缓存里仍是本次读到的那份会话，不覆盖期间重新登录得到的新会话。
func (s *Service) freshSiteSession(ctx context.Context, id string, force bool) (Session, error) {
	lock := s.siteSessionLock(id)
	lock.Lock()
	defer lock.Unlock()

	site, err := s.cache.Get(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if site == nil || site.Session == nil {
		return Session{}, newRequestError(ErrorNotFound, "")
	}
	original := *site.Session
	var refreshed Session
	if force && original.Platform == PlatformSub2API && strings.TrimSpace(original.RefreshToken) != "" {
		refreshed, err = s.platformService.refreshSub2APISession(original)
	} else {
		refreshed, err = s.platformService.RefreshSession(original)
	}
	if err != nil {
		return Session{}, err
	}
	if reflect.DeepEqual(refreshed, original) {
		return refreshed, nil
	}
	// refreshSub2APISession 只重建登录字段，补回其它平台附加字段。
	if refreshed.AdminAPIKey == "" {
		refreshed.AdminAPIKey = original.AdminAPIKey
	}
	if refreshed.QuotaPerUnit == 0 {
		refreshed.QuotaPerUnit = original.QuotaPerUnit
	}
	latest, err := s.cache.Get(ctx, id)
	if err != nil || latest == nil || latest.Session == nil || !reflect.DeepEqual(*latest.Session, original) {
		// 期间会话已被重新登录替换或站点被删除：使用本次刷新结果完成当前请求，但不回写旧会话链。
		return refreshed, nil
	}
	latest.Session = &refreshed
	if err := s.setCachedSite(ctx, latest); err != nil {
		log.Printf("[upstream] 保存续期会话到缓存失败 id=%s err=%v", id, err)
		return refreshed, nil
	}
	if err := s.saveSite(ctx, latest); err != nil {
		log.Printf("[upstream] 保存续期会话到数据库失败 id=%s err=%v", id, err)
	}
	return refreshed, nil
}

// withSiteSession 用最新会话执行 fn；若上游返回 401 且站点支持 refresh token，强制续期后重试一次。
func (s *Service) withSiteSession(ctx context.Context, id string, fn func(Session) error) (Session, error) {
	session, err := s.freshSiteSession(ctx, id, false)
	if err != nil {
		return Session{}, err
	}
	err = fn(session)
	if err == nil || errorKey(err) != ErrorAuth || session.Platform != PlatformSub2API || strings.TrimSpace(session.RefreshToken) == "" {
		return session, err
	}
	retried, refreshErr := s.freshSiteSession(ctx, id, true)
	if refreshErr != nil {
		return session, err
	}
	return retried, fn(retried)
}
