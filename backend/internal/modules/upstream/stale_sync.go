package upstream

import (
	"context"
	"log"
	"sync"
	"time"
)

// SyncStaleForAccount 供看板成本采样调用：同步指定工作区中有会话、计费、且超过 maxAge
// 未成功同步的上游站点。后台定时同步关闭（默认）时，今日消费也能保持新鲜；
// 定时同步正常时几乎所有站点都足够新，这里不会产生额外请求。并发 4，随 ctx 截止。
func (s *Service) SyncStaleForAccount(ctx context.Context, userID, adminAccountID string, maxAge time.Duration) {
	sites, err := s.cache.ListByUser(ctx, userID)
	if err != nil {
		log.Printf("[upstream] stale sync list failed user_id=%s err=%v", userID, err)
		return
	}
	cutoff := time.Now().Add(-maxAge).UnixMilli()
	ids := make([]string, 0, len(sites))
	for _, site := range sites {
		if site == nil || site.AdminAccountID != adminAccountID || site.Session == nil || site.RechargeRate <= 0 {
			continue
		}
		if site.LastSyncedAt != nil && *site.LastSyncedAt >= cutoff {
			continue
		}
		if s.isSiteDeleted(site.ID) {
			continue
		}
		ids = append(ids, site.ID)
	}
	if len(ids) == 0 {
		return
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, id := range ids {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := s.sync(ctx, id); err != nil {
				log.Printf("[upstream] stale sync failed id=%s err=%v", id, err)
			}
		}(id)
	}
	wg.Wait()
}
