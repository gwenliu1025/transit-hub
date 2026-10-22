package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
)

const dailyUsageTimeout = 90 * time.Second

// 北京时间自然日边界是两个平台日统计的唯一解释，New API 的结束秒为闭区间。
func dailyUsageBounds(startDate, endDate string) (time.Time, time.Time, error) {
	loc := time.FixedZone("Asia/Shanghai", 8*60*60)
	start, err := time.ParseInLocation("2006-01-02", startDate, loc)
	if err != nil || start.Format("2006-01-02") != startDate {
		return time.Time{}, time.Time{}, errors.New("invalid usage start date")
	}
	end, err := time.ParseInLocation("2006-01-02", endDate, loc)
	if err != nil || end.Format("2006-01-02") != endDate || end.Before(start) {
		return time.Time{}, time.Time{}, errors.New("invalid usage end date")
	}
	return start, end.AddDate(0, 0, 1).Add(-time.Second), nil
}

// FetchAdminUsageStatsContext 查询北京时间指定日期范围的管理员收入；取消必须抵达网络层。
func (s *PlatformService) FetchAdminUsageStatsContext(ctx context.Context, session Session, startDate, endDate string) (float64, error) {
	return s.fetchDatedUsage(ctx, session, startDate, endDate, true)
}

func (s *PlatformService) fetchDatedUsage(ctx context.Context, session Session, startDate, endDate string, admin bool) (float64, error) {
	start, end, err := dailyUsageBounds(startDate, endDate)
	if err != nil {
		return 0, err
	}
	if !session.IsAuthenticated() {
		return 0, newRequestError(ErrorAuth, session.Platform)
	}
	ctx, cancel := context.WithTimeout(ctx, dailyUsageTimeout)
	defer cancel()
	var path string
	var options requestOptions
	switch session.Platform {
	case PlatformSub2API:
		path = "/api/v1/usage/stats"
		options = sub2APIUserAuthOptions(session)
		if admin {
			path = "/api/v1/admin/usage/stats"
			options = adminAuthOptions(session)
		} else if strings.TrimSpace(session.AccessToken) == "" {
			return 0, newRequestError(ErrorAuth, session.Platform)
		}
		query := url.Values{"start_date": {startDate}, "end_date": {endDate}, "timezone": {"Asia/Shanghai"}}
		path += "?" + query.Encode()
	case PlatformNewAPI:
		path = "/api/log/self/stat?type=2&start_timestamp=" + strconvInt(start.Unix()) + "&end_timestamp=" + strconvInt(end.Unix())
		options = newAPIAuthOptions(session)
	default:
		return 0, newRequestError(ErrorInvalidResponse, session.Platform)
	}
	response, err := s.httpClient.requestJSONContext(ctx, session.BaseURL+path, options)
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	data := dataRecord(response.Payload)
	keys := []string{"total_actual_cost", "totalActualCost"}
	if session.Platform == PlatformNewAPI {
		keys = []string{"quota"}
	}
	// 名义 total_cost 并不等于实际收费，字段缺失或损坏不能按零收费落账。
	var amount *float64
	for _, key := range keys {
		if value, exists := data[key]; exists {
			amount = readNumber(value)
			break
		}
	}
	if amount == nil || !isFinite(*amount) || *amount < 0 {
		return 0, newRequestError(ErrorInvalidResponse, session.Platform)
	}
	if session.Platform == PlatformNewAPI {
		if !isFinite(session.QuotaPerUnit) || session.QuotaPerUnit < 0 {
			return 0, newRequestError(ErrorInvalidResponse, session.Platform)
		}
		amount = quotaToUSDWithUnit(amount, session.QuotaPerUnit)
		if amount == nil {
			return 0, newRequestError(ErrorInvalidResponse, session.Platform)
		}
	}
	return *amount, nil
}

// WithDailyPurchaseForAccount 仅供计划日结：冻结配置、采集并在同一配置锁下提交。
// 它不证明历史倍率；启动补偿和历史修复必须另有当日配置证据，不能套用此方法。
func (s *Service) WithDailyPurchaseForAccount(ctx context.Context, userID, adminAccountID, date string, consume func(context.Context, float64) error) error {
	if consume == nil {
		return errors.New("daily purchase requires consumer")
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(adminAccountID) == "" {
		return errors.New("daily purchase requires workspace")
	}
	if _, _, err := dailyUsageBounds(date, date); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dailyUsageTimeout)
	defer cancel()
	workspace := refreshWorkspace{userID: userID, adminAccountID: adminAccountID}
	s.mu.Lock()
	generation, epoch := s.configGenerations[workspace], s.cacheEpoch
	sites, err := s.purchaseSitesLocked(ctx, workspace)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	amounts := make([]float64, len(sites))
	errs := make([]error, len(sites))
	jobs := make(chan int)
	var workers sync.WaitGroup
	// 固定四个工作线程，不为每个站点创建等待中的 goroutine。
	for n := 0; n < 4; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				site := sites[i]
				if site.RechargeRate == 0 {
					continue
				}
				if site.Session == nil {
					errs[i] = fmt.Errorf("daily purchase site %s: missing session", site.ID)
					cancel()
					continue
				}
				amount, fetchErr := s.platformService.fetchDatedUsage(ctx, *site.Session, date, date, false)
				if fetchErr != nil {
					errs[i] = fmt.Errorf("daily purchase site %s: %w", site.ID, fetchErr)
					cancel()
					continue
				}
				amount *= site.RechargeRate
				if !isFinite(amount) {
					errs[i] = fmt.Errorf("daily purchase site %s: invalid amount", site.ID)
					cancel()
					continue
				}
				amounts[i] = amount
			}
		}()
	}
dispatch:
	for i := range sites {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.purchaseSitesLocked(ctx, workspace)
	unchanged := generation == s.configGenerations[workspace] && epoch == s.cacheEpoch && samePurchaseSites(sites, current)
	if err != nil {
		return err
	}
	if !unchanged {
		return errors.New("daily purchase configuration changed during collection")
	}
	var total float64
	for _, amount := range amounts {
		total += amount
	}
	if !isFinite(total) {
		return errors.New("invalid daily purchase total")
	}
	// 消费者必须用同一有界上下文落账，不得再次进入上游配置接口。
	if err := ctx.Err(); err != nil {
		return err
	}
	return consume(ctx, total)
}

// purchaseSitesLocked 以持久配置集合核对运行缓存，缺成员不能由 Redis 的跳过行为掩盖。
func (s *Service) purchaseSitesLocked(ctx context.Context, workspace refreshWorkspace) ([]*Site, error) {
	if s.repository == nil {
		return nil, errors.New("daily purchase repository unavailable")
	}
	if s.cache == nil {
		return nil, errors.New("daily purchase cache unavailable")
	}
	authoritative, err := s.repository.ListSitesForUser(ctx, workspace.userID)
	if err != nil {
		return nil, err
	}
	cached, err := s.cache.ListByUser(ctx, workspace.userID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*Site)
	for _, site := range cached {
		if site == nil || site.UserID != workspace.userID || site.AdminAccountID != workspace.adminAccountID {
			continue
		}
		if _, exists := byID[site.ID]; exists {
			return nil, errors.New("daily purchase duplicate cached site")
		}
		byID[site.ID] = site
	}
	sites := make([]*Site, 0, len(authoritative))
	seen := make(map[string]bool)
	for i := range authoritative {
		configured := &authoritative[i]
		if configured.UserID != workspace.userID || configured.AdminAccountID != workspace.adminAccountID {
			continue
		}
		if seen[configured.ID] {
			return nil, errors.New("daily purchase duplicate configured site")
		}
		seen[configured.ID] = true
		site := byID[configured.ID]
		if site == nil {
			return nil, fmt.Errorf("daily purchase site %s: cache member missing", configured.ID)
		}
		if !samePurchaseConfig(configured, site) {
			return nil, fmt.Errorf("daily purchase site %s: configuration/cache mismatch", configured.ID)
		}
		if !isFinite(site.RechargeRate) || site.RechargeRate < 0 {
			return nil, fmt.Errorf("daily purchase site %s: invalid recharge rate", site.ID)
		}
		copySite := *site
		if site.Session != nil {
			session := *site.Session
			copySite.Session = &session
		}
		sites = append(sites, &copySite)
	}
	if len(sites) != len(byID) {
		return nil, errors.New("daily purchase configuration/cache membership mismatch")
	}
	return sites, nil
}

func samePurchaseSites(a, b []*Site) bool {
	if len(a) != len(b) {
		return false
	}
	byID := make(map[string]*Site, len(a))
	for _, site := range a {
		byID[site.ID] = site
	}
	for _, site := range b {
		if !samePurchaseConfig(byID[site.ID], site) {
			return false
		}
	}
	return true
}

// 仅配置与身份变化推进版本；指标刷新不会使正常日结失效。版本阻止 A→B→A 漏检。
func samePurchaseConfig(a, b *Site) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID && a.UserID == b.UserID && a.AdminAccountID == b.AdminAccountID && a.BaseURL == b.BaseURL && a.Platform == b.Platform && a.Account == b.Account && a.RechargeRate == b.RechargeRate && reflect.DeepEqual(a.Session, b.Session)
}

func (s *Service) bumpConfigLocked(site *Site) {
	if site == nil {
		return
	}
	if s.configGenerations == nil {
		s.configGenerations = make(map[refreshWorkspace]uint64)
	}
	s.configGenerations[refreshWorkspace{userID: site.UserID, adminAccountID: site.AdminAccountID}]++
}

func (s *Service) deleteCachedSite(ctx context.Context, id, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	site, err := s.cache.Get(ctx, id)
	if err != nil {
		return err
	}
	// 工作区清理已经推进全局 epoch 并释放版本，不再为已删除站点重建版本项。
	if _, deleted := s.deletedSites[id]; !deleted {
		s.bumpConfigLocked(site)
	}
	return s.cache.Delete(ctx, id, userID)
}
