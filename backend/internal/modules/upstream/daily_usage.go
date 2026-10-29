package upstream

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
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

// DailyPurchaseResult 是一次日结成本采集的结果，金额已乘以各站点 rechargeRate。
// Complete 表示所有计费站点都取得了指定日的精确消费；否则 Amount 只包含
// 成功站点的精确值，以及失败站点在该日最后一次同步时缓存的当日消费（尽量不丢成本）。
type DailyPurchaseResult struct {
	Amount      float64
	Complete    bool
	FailedSites int
	TotalSites  int
	// Cause 汇总失败站点的原因，供日志与重试判断；Complete 时为 nil。
	Cause error
}

// WithDailyPurchaseForAccount 供日结使用：冻结配置、逐站采集并在同一配置锁下提交。
// 单个站点失败不再拖垮整个工作区：失败站点回退到该日最后一次同步的缓存消费，
// 结果标记为不完整，由调用方决定是否稍后重试。全部计费站点都没有任何数据时返回错误、不调用 consume。
func (s *Service) WithDailyPurchaseForAccount(ctx context.Context, userID, adminAccountID, date string, consume func(context.Context, DailyPurchaseResult) error) error {
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
	recorded := make([]bool, len(sites))
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
					recorded[i] = true
					continue
				}
				var fetchErr error
				if site.Session == nil {
					fetchErr = errors.New("missing session")
				} else {
					var amount float64
					// 日结前先续期会话（并持久化），401 时强制续期重试一次；刷新关闭时令牌也不会过期失效。
					_, fetchErr = s.withSiteSession(ctx, site.ID, func(session Session) error {
						var err error
						amount, err = s.platformService.fetchDatedUsage(ctx, session, date, date, false)
						return err
					})
					if fetchErr == nil {
						amount *= site.RechargeRate
						if isFinite(amount) {
							amounts[i], recorded[i] = amount, true
							continue
						}
						fetchErr = errors.New("invalid amount")
					}
				}
				errs[i] = fmt.Errorf("daily purchase site %s: %w", site.ID, fetchErr)
				// 失败站点若在该日有过成功同步，用最后一次缓存的当日消费兜底，避免整站成本记成 0。
				if value, ok := cachedConsumeOnDate(site, date); ok {
					amounts[i], recorded[i] = value, true
				}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	result := DailyPurchaseResult{Complete: true}
	anyRecorded := len(sites) == 0
	for i, site := range sites {
		if site.RechargeRate != 0 {
			result.TotalSites++
		}
		if errs[i] != nil {
			result.FailedSites++
			result.Complete = false
		}
		if recorded[i] {
			anyRecorded = true
		}
	}
	result.Cause = errors.Join(errs...)
	if !anyRecorded || (result.TotalSites > 0 && result.FailedSites == result.TotalSites && !hasPositive(amounts)) {
		// 没有任何站点给出数据时不落账，保留原有采样。
		if result.Cause == nil {
			result.Cause = errors.New("daily purchase has no usable source")
		}
		return result.Cause
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
	for _, amount := range amounts {
		result.Amount += amount
	}
	if !isFinite(result.Amount) {
		return errors.New("invalid daily purchase total")
	}
	if !result.Complete {
		log.Printf("[upstream] daily purchase partial user_id=%s admin_account_id=%s date=%s failed_sites=%d total_sites=%d err=%v",
			userID, adminAccountID, date, result.FailedSites, result.TotalSites, result.Cause)
	}
	// 消费者必须用同一有界上下文落账，不得再次进入上游配置接口。
	if err := ctx.Err(); err != nil {
		return err
	}
	return consume(ctx, result)
}

// cachedConsumeOnDate 返回站点在指定北京日最后一次成功同步时的当日消费（已乘倍率）。
func cachedConsumeOnDate(site *Site, date string) (float64, bool) {
	if site == nil || site.LastSyncedAt == nil || site.Metrics.TodayConsume.Value == nil {
		return 0, false
	}
	loc := time.FixedZone("Asia/Shanghai", 8*60*60)
	if time.UnixMilli(*site.LastSyncedAt).In(loc).Format("2006-01-02") != date {
		return 0, false
	}
	value := *site.Metrics.TodayConsume.Value * site.RechargeRate
	if !isFinite(value) || value < 0 {
		return 0, false
	}
	return value, true
}

func hasPositive(values []float64) bool {
	for _, value := range values {
		if value > 0 {
			return true
		}
	}
	return false
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

// 仅配置与身份变化推进版本；指标刷新和令牌续期不会使正常日结失效。版本阻止 A→B→A 漏检。
// 会话令牌不参与比较：同一账号的令牌续期不改变成本口径，若参与比较，
// 定时同步续期或缓存先于数据库写入都会让整晚日结以“配置变化”失败。
func samePurchaseConfig(a, b *Site) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID && a.UserID == b.UserID && a.AdminAccountID == b.AdminAccountID && a.BaseURL == b.BaseURL && a.Platform == b.Platform && a.Account == b.Account && a.RechargeRate == b.RechargeRate && (a.Session == nil) == (b.Session == nil)
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
