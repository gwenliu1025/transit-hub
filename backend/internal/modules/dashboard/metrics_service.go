package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"transithub/backend/internal/modules/upstream"
)

// UpstreamLister 抽象上游站点列表读取，由 upstream.Service 实现。
// 仪表盘只需要读取已同步的站点数据，不需要修改或触发同步。
// List 用于用户请求路径（自动使用当前工作区），
// ListForAccount 用于后台调度等需要显式指定工作区的内部流程。
type UpstreamLister interface {
	List(ctx context.Context, userID string) []upstream.Response
	ListForAccount(ctx context.Context, userID, adminAccountID string) []upstream.Response
	// KeyUsageToday 和 BalanceBreakdown 是「今日成本」「上游总余额」下钻弹窗的数据源，
	// 由 upstream.Service 实现（持有 session/cache，能校验站点归属和当前工作区）。
	KeyUsageToday(ctx context.Context, userID string) ([]upstream.KeyUsageTodayItem, error)
	BalanceBreakdown(ctx context.Context, userID string) ([]upstream.BalanceBreakdownItem, error)
}

// MetricsStore 保持日结与实时采样共用一个数据库写入契约，便于独立验证失败边界。
type MetricsStore interface {
	Upsert(context.Context, DailySnapshot) error
	ListRange(context.Context, string, string, int) ([]DailySnapshot, error)
	GetBalanceFilter(context.Context, string, string) (BalanceFilterConfig, error)
	SaveBalanceFilter(context.Context, BalanceFilterConfig) error
}

type dailyRevenueSource interface {
	FetchAdminUsageStatsContext(context.Context, upstream.Session, string, string) (float64, error)
}

type dailyPurchaseSource interface {
	WithDailyPurchaseForAccount(context.Context, string, string, string, func(context.Context, float64) error) error
}

var _ dailyRevenueSource = (*upstream.PlatformService)(nil)
var _ dailyPurchaseSource = (*upstream.Service)(nil)

// MetricsService 负责仪表盘指标的实时计算、历史快照存储与午夜调度。
// 与同包的 Service（admin 会话管理）职责分离，共享 SessionStore 和 PlatformClient。
type MetricsService struct {
	store       SessionStore
	platform    PlatformClient
	upstreams   UpstreamLister
	metricsRepo MetricsStore
	accounts    AdminAccountService
	sessionSync MySiteStateSync
	now         func() time.Time
}

var beijingLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return location
}()

func (s *MetricsService) SetMySiteSync(sync MySiteStateSync) {
	s.sessionSync = sync
}

func (s *MetricsService) freshAdminSession(ctx context.Context, userID string, adminAccountID string, record *AdminSession) (upstream.Session, error) {
	if s.sessionSync != nil {
		stored, exists, err := s.sessionSync.StoredSession(ctx, userID, adminAccountID)
		if err != nil {
			return upstream.Session{}, err
		}
		if !exists || sessionAppearsNewer(record.Session, stored) {
			if err := s.sessionSync.SyncAdminSession(ctx, userID, adminAccountID, record.Session, record.Identity); err != nil {
				return upstream.Session{}, err
			}
		}
		canonical, err := s.sessionSync.RequireSession(ctx, userID, adminAccountID)
		if err != nil {
			return upstream.Session{}, err
		}
		if !sessionEqual(canonical, record.Session) {
			record.Session = canonical
			record.LastRefreshedAt = nowMillis()
			if err := s.store.Save(ctx, userID, adminAccountID, *record); err != nil {
				return upstream.Session{}, err
			}
		}
		return canonical, nil
	}

	refreshed, err := s.platform.RefreshSession(record.Session)
	if err != nil {
		return upstream.Session{}, err
	}
	if !sessionEqual(refreshed, record.Session) {
		record.Session = refreshed
		record.LastRefreshedAt = nowMillis()
		if err := s.store.Save(ctx, userID, adminAccountID, *record); err != nil {
			return upstream.Session{}, err
		}
	}
	return refreshed, nil
}

func NewMetricsService(store SessionStore, platform PlatformClient, upstreams UpstreamLister, metricsRepo MetricsStore, accounts AdminAccountService) *MetricsService {
	return &MetricsService{store: store, platform: platform, upstreams: upstreams, metricsRepo: metricsRepo, accounts: accounts}
}

func (s *MetricsService) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *MetricsService) revenue(ctx context.Context, session upstream.Session, date string) (float64, error) {
	source, ok := s.platform.(dailyRevenueSource)
	if !ok {
		return 0, errors.New("dashboard daily revenue source unavailable")
	}
	value, err := source.FetchAdminUsageStatsContext(ctx, session, date, date)
	if err == nil {
		err = ctx.Err()
	}
	return value, err
}

// LiveMetrics 实时计算五项核心指标并返回。
// 同时将当天的指标作为快照 upsert 到数据库，确保趋势图数据持续积累。
//
// 计算逻辑：
//   - todayProfit:     管理员站点今日总实际消费，通过 sub2api /api/v1/admin/usage/stats 获取
//   - siteBalance:     管理员站点所有非 admin 用户余额之和，通过 sub2api /api/v1/admin/users 分页求和
//   - todayPurchase:   所有上游站点今日消费 × 站点倍率之和（复用已同步的内存数据，无额外请求）
//   - upstreamBalance: 所有上游站点余额 × 站点倍率之和（复用已同步的内存数据）
//   - netProfit:       todayProfit - todayPurchase
func (s *MetricsService) LiveMetrics(ctx context.Context, userID string) (MetricsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	// 获取并校验 admin 会话（平台感知：sub2api 检查 AccessToken，new-api 检查 Cookie+UserID）。
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return MetricsResponse{}, err
	}
	record, err := s.store.Get(ctx, userID, adminAccountID)
	if err != nil {
		return MetricsResponse{}, err
	}
	if record == nil || !record.Session.IsAuthenticated() {
		return MetricsResponse{}, requestError(ErrorAdminOnly)
	}

	// 如有必要先刷新令牌（new-api 不使用 refresh token，RefreshSession 会直接返回原会话）。
	session, err := s.freshAdminSession(ctx, userID, adminAccountID, record)
	if err != nil {
		return MetricsResponse{}, requestError(ErrorAdminOnly)
	}

	// 校验 admin 角色（平台中性）。
	if err := s.platform.VerifyAdmin(session); err != nil {
		return MetricsResponse{}, requestError(ErrorAdminOnly)
	}

	// 并行获取四项独立数据：今日盈利、站点余额、分组数量、上游指标。
	// 营收失败必须返回错误，防止零值污染持久化；其它实时读取保留既有口径。
	today := beijingMetricDate(s.clock())
	var (
		todayProfit     float64
		siteBalance     float64
		groupCount      int
		todayPurchase   float64
		upstreamBalance float64
		wg              sync.WaitGroup
		revenueErr      error
		balanceRecorded bool
	)

	// goroutine 1: 今日盈利额度（平台中性）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		profit, err := s.revenue(ctx, session, today)
		if err != nil {
			revenueErr = err
			log.Printf("dashboard metrics: fetch usage stats failed user_id=%s err=%v", userID, err)
			return
		}
		todayProfit = profit
	}()

	// goroutine 2: 站点用户总余额（平台中性）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		filterConfig, err := s.metricsRepo.GetBalanceFilter(ctx, userID, adminAccountID)
		if err != nil {
			log.Printf("dashboard metrics: load balance filter failed user_id=%s err=%v, using defaults", userID, err)
			filterConfig = BalanceFilterConfig{ExcludeAdmin: true, ExcludeBalances: []float64{}}
		}
		balanceResult, err := s.platform.FetchAdminSiteBalanceFiltered(session, upstream.BalanceFilter{
			ExcludeAdmin:    filterConfig.ExcludeAdmin,
			ExcludeBalances: filterConfig.ExcludeBalances,
		})
		if err != nil {
			log.Printf("dashboard metrics: fetch site balance failed user_id=%s err=%v", userID, err)
			return
		}
		siteBalance = balanceResult.Balance
		balanceRecorded = true
	}()

	// goroutine 3: 管理员站点分组数量（平台中性）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		groups, err := s.platform.FetchAdminAllGroups(session)
		if err != nil {
			log.Printf("dashboard metrics: fetch admin groups failed user_id=%s err=%v", userID, err)
			return
		}
		groupCount = len(groups)
	}()

	// goroutine 4: 今日进货额度与上游总余额（读取 Redis 缓存，无外部 API 调用）。
	// 使用 List（用户请求路径，自动过滤当前工作区站点）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		todayPurchase, upstreamBalance = aggregateUpstreamMetricsForDate(s.upstreams.List(ctx, userID), today)
	}()

	wg.Wait()
	if revenueErr != nil {
		return MetricsResponse{}, revenueErr
	}
	if err := ctx.Err(); err != nil {
		return MetricsResponse{}, err
	}
	if beijingMetricDate(s.clock()) != today {
		return MetricsResponse{}, ErrSnapshotNotWritten
	}

	netProfit := todayProfit - todayPurchase

	result := MetricsResponse{
		TodayProfit:     todayProfit,
		SiteBalance:     siteBalance,
		TodayPurchase:   todayPurchase,
		NetProfit:       netProfit,
		UpstreamBalance: upstreamBalance,
		GroupCount:      groupCount,
	}

	// 营收源失败或请求跨日时不写日内采样；数据库还会复查真正执行时的日期。
	if err := s.upsertSnapshot(ctx, userID, adminAccountID, today, result, balanceRecorded); err != nil {
		return MetricsResponse{}, err
	}

	return result, nil
}

// Trends 查询历史趋势数据，返回最近 days 天的每日快照（不含当天）。
// 当天的数据由前端通过 LiveMetrics 获取后追加到序列末尾。
func (s *MetricsService) Trends(ctx context.Context, userID string, days int) (TrendResponse, error) {
	if days != 7 && days != 30 {
		days = 7
	}
	// 按当前工作区过滤趋势数据。
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return TrendResponse{}, err
	}
	snapshots, err := s.metricsRepo.ListRange(ctx, userID, adminAccountID, days)
	if err != nil {
		return TrendResponse{}, err
	}
	points := make([]TrendPoint, 0, len(snapshots))
	for _, snap := range snapshots {
		points = append(points, TrendPoint{
			RevenueComplete:  snap.RevenueComplete,
			PurchaseComplete: snap.PurchaseComplete,
			BalanceRecorded:  snap.BalanceRecorded,
			Date:             snap.Date.Format("2006-01-02"),
			TodayProfit:      snap.TodayProfit,
			SiteBalance:      snap.SiteBalance,
			TodayPurchase:    snap.TodayPurchase,
			NetProfit:        snap.NetProfit,
			UpstreamBalance:  snap.UpstreamBalance,
		})
	}
	return TrendResponse{Points: points}, nil
}

// StartScheduler 启动时仅修复昨日营收；计划日结按当时冻结配置结算昨日成本。
func (s *MetricsService) StartScheduler(ctx context.Context) {
	go func() {
		if err := s.reconcileAll(ctx, false); err != nil {
			log.Printf("dashboard startup reconciliation failed: %v", err)
		}
		for {
			now := s.clock().In(beijingLocation)
			next := nextSettlementTime(now)
			timer := time.NewTimer(next.Sub(now))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				s.snapshotAll(ctx)
			}
		}
	}()
}

// 日志聚合存在落库水位，避开整点尚未收敛的窗口；启动补偿使用同一边界。
func nextSettlementTime(now time.Time) time.Time {
	now = now.In(beijingLocation)
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 5, 0, 0, beijingLocation)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func settlementWindowReady(now time.Time) bool {
	now = now.In(beijingLocation)
	return now.Hour() > 0 || now.Minute() >= 5
}

func (s *MetricsService) snapshotAll(ctx context.Context) {
	if err := s.reconcileAll(ctx, true); err != nil {
		log.Printf("dashboard daily settlement failed: %v", err)
	}
}

func (s *MetricsService) reconcileAll(ctx context.Context, purchase bool) error {
	if !settlementWindowReady(s.clock()) {
		return nil
	}
	// 单批次和单工作区都有限时；一次失败不会无限重试或切到其它身份。
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	refs, err := s.store.ActiveSessions(ctx)
	if err != nil {
		return err
	}
	date := previousBeijingMetricDate(s.clock())
	var failures []error
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := s.reconcileDate(ctx, ref.UserID, ref.AdminAccountID, date, purchase); err != nil {
			failures = append(failures, fmt.Errorf("workspace %s/%s: %w", ref.UserID, ref.AdminAccountID, err))
			continue
		}
		log.Printf("dashboard reconciliation saved user_id=%s admin_account_id=%s date=%s purchase=%t", ref.UserID, ref.AdminAccountID, date, purchase)
	}
	return errors.Join(failures...)
}

// ReconcileRevenue 仅从仍有完整日志的真实昨日恢复营收，不将今天倍率追溯应用于历史成本。
// 工作区归属由会话读取和数据库写入共同约束；调用方负责枚举已核对的补数范围。
func (s *MetricsService) ReconcileRevenue(ctx context.Context, userID, adminAccountID, date string) error {
	if date != previousBeijingMetricDate(s.clock()) || !settlementWindowReady(s.clock()) {
		return errors.New("dashboard revenue source only supports settled yesterday")
	}
	return s.reconcileDate(ctx, userID, adminAccountID, date, false)
}

// ApplyVerifiedRevenue 接收调用方已核对覆盖范围与完整性的权威日汇总。
// 此内部入口不查询有保留期的日志，也不补写历史成本或余额；不注册 HTTP 路由。
func (s *MetricsService) ApplyVerifiedRevenue(ctx context.Context, userID, adminAccountID, date string, amount float64) error {
	parsed, err := time.ParseInLocation("2006-01-02", date, beijingLocation)
	if err != nil || date >= beijingMetricDate(s.clock()) || userID == "" || adminAccountID == "" || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return errors.New("dashboard verified revenue requires historical date, workspace and finite nonnegative amount")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, err := metricsRandomID()
	if err != nil {
		return err
	}
	return s.metricsRepo.Upsert(ctx, DailySnapshot{ID: id, UserID: userID, AdminAccountID: adminAccountID, Date: parsed, TodayProfit: amount, RevenueComplete: true, CreatedAt: s.clock()})
}

func (s *MetricsService) reconcileDate(ctx context.Context, userID, adminAccountID, date string, purchase bool) error {
	parsed, err := time.ParseInLocation("2006-01-02", date, beijingLocation)
	if err != nil || date >= beijingMetricDate(s.clock()) || userID == "" || adminAccountID == "" {
		return errors.New("dashboard reconciliation requires a historical date and workspace")
	}
	if purchase && date != previousBeijingMetricDate(s.clock()) {
		return errors.New("dashboard purchase settlement requires yesterday")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	id, err := metricsRandomID()
	if err != nil {
		return err
	}
	snapshot := DailySnapshot{ID: id, UserID: userID, AdminAccountID: adminAccountID, Date: parsed, CreatedAt: s.clock()}
	var failures []error
	record, err := s.store.Get(ctx, userID, adminAccountID)
	if err == nil && (record == nil || !record.Session.IsAuthenticated()) {
		err = requestError(ErrorAdminOnly)
	}
	if err == nil {
		// 日结只消费当前会话，刷新与持久化仍归既有会话 owner。
		snapshot.TodayProfit, err = s.revenue(ctx, record.Session, date)
	}
	if err != nil {
		failures = append(failures, fmt.Errorf("revenue: %w", err))
	} else {
		snapshot.RevenueComplete = true
	}
	if purchase {
		source, ok := s.upstreams.(dailyPurchaseSource)
		consumed := false
		if !ok {
			err = errors.New("dashboard daily purchase source unavailable")
		} else {
			err = source.WithDailyPurchaseForAccount(ctx, userID, adminAccountID, date, func(commitCtx context.Context, amount float64) error {
				// 配置 owner 在此回调返回前保持冻结；日结唯一写入必须发生在其锁内。
				consumed = true
				if err := commitCtx.Err(); err != nil {
					return err
				}
				snapshot.TodayPurchase = amount
				snapshot.PurchaseComplete = true
				if err := s.metricsRepo.Upsert(commitCtx, snapshot); err != nil {
					return fmt.Errorf("persist: %w", err)
				}
				return nil
			})
		}
		if consumed {
			// 提交失败或接受状态未知时不再写一遍部分营收，避免隐式重试掩盖原错误。
			if err != nil {
				failures = append(failures, err)
			}
			return errors.Join(failures...)
		}
		if err == nil {
			err = errors.New("dashboard daily purchase source did not provide a result")
		}
		failures = append(failures, fmt.Errorf("purchase: %w", err))
	}

	if err := ctx.Err(); err != nil {
		return errors.Join(append(failures, err)...)
	}
	if snapshot.RevenueComplete || snapshot.PurchaseComplete {
		if err := s.metricsRepo.Upsert(ctx, snapshot); err != nil {
			failures = append(failures, fmt.Errorf("persist: %w", err))
		}
	}
	return errors.Join(failures...)
}

func aggregateUpstreamMetricsForDate(sites []upstream.Response, targetDate string) (todayPurchase, upstreamBalance float64) {
	for _, site := range sites {
		if site.RechargeRate <= 0 {
			continue
		}
		if site.Metrics.TodayConsume.Value != nil && syncedOnBeijingDate(site.LastSyncedAt, targetDate) {
			todayPurchase += *site.Metrics.TodayConsume.Value * site.RechargeRate
		}
		if site.Metrics.Balance.Value != nil {
			upstreamBalance += *site.Metrics.Balance.Value * site.RechargeRate
		}
	}
	return todayPurchase, upstreamBalance
}

func beijingMetricDate(now time.Time) string {
	return now.In(beijingLocation).Format("2006-01-02")
}

func previousBeijingMetricDate(now time.Time) string {
	return now.In(beijingLocation).AddDate(0, 0, -1).Format("2006-01-02")
}

func syncedOnBeijingDate(lastSyncedAt *int64, targetDate string) bool {
	if lastSyncedAt == nil {
		return false
	}
	return time.UnixMilli(*lastSyncedAt).In(beijingLocation).Format("2006-01-02") == targetDate
}

// upsertSnapshot 提交日内采样，数据库负责日期门禁与已完成字段保护。
func (s *MetricsService) upsertSnapshot(ctx context.Context, userID, adminAccountID, date string, metrics MetricsResponse, balanceRecorded bool) error {
	parsedDate, err := time.Parse("2006-01-02", date)
	if err != nil {
		return err
	}
	id, err := metricsRandomID()
	if err != nil {
		return err
	}
	snapshot := DailySnapshot{
		ID:              id,
		UserID:          userID,
		AdminAccountID:  adminAccountID,
		Date:            parsedDate,
		TodayProfit:     metrics.TodayProfit,
		SiteBalance:     metrics.SiteBalance,
		TodayPurchase:   metrics.TodayPurchase,
		NetProfit:       metrics.NetProfit,
		UpstreamBalance: metrics.UpstreamBalance,
		CreatedAt:       s.clock(),
		BalanceRecorded: balanceRecorded,
	}
	return s.metricsRepo.Upsert(ctx, snapshot)
}

// AdminGroups 获取管理员站点的所有分组列表（平台中性）。
func (s *MetricsService) AdminGroups(ctx context.Context, userID string) (AdminGroupsResponse, error) {
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return AdminGroupsResponse{}, err
	}
	record, err := s.store.Get(ctx, userID, adminAccountID)
	if err != nil {
		return AdminGroupsResponse{}, err
	}
	if record == nil || !record.Session.IsAuthenticated() {
		return AdminGroupsResponse{}, requestError(ErrorAdminOnly)
	}

	session, err := s.freshAdminSession(ctx, userID, adminAccountID, record)
	if err != nil {
		return AdminGroupsResponse{}, requestError(ErrorAdminOnly)
	}

	groups, err := s.platform.FetchAdminGroups(session)
	if err != nil {
		return AdminGroupsResponse{}, err
	}

	items := make([]AdminGroupItem, 0, len(groups))
	for _, g := range groups {
		platform := ""
		if g.Platform != nil {
			platform = *g.Platform
		}
		items = append(items, AdminGroupItem{
			ID:         g.ID,
			Name:       g.Name,
			Platform:   platform,
			Multiplier: g.MultiplierDisplay,
		})
	}
	return AdminGroupsResponse{Count: len(items), Groups: items}, nil
}

// GroupUsageToday 获取当前工作区「我的站点」所有分组今日的使用额度（平台中性）。
// 数据只在弹窗打开时按需请求，不参与 LiveMetrics 的批量指标计算。
func (s *MetricsService) GroupUsageToday(ctx context.Context, userID string) (GroupUsageTodayResponse, error) {
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return GroupUsageTodayResponse{}, err
	}
	record, err := s.store.Get(ctx, userID, adminAccountID)
	if err != nil {
		return GroupUsageTodayResponse{}, err
	}
	if record == nil || !record.Session.IsAuthenticated() {
		return GroupUsageTodayResponse{}, requestError(ErrorAdminOnly)
	}

	session, err := s.freshAdminSession(ctx, userID, adminAccountID, record)
	if err != nil {
		return GroupUsageTodayResponse{}, requestError(ErrorAdminOnly)
	}

	if err := s.platform.VerifyAdmin(session); err != nil {
		return GroupUsageTodayResponse{}, requestError(ErrorAdminOnly)
	}

	groups, err := s.platform.FetchAdminGroups(session)
	if err != nil {
		return GroupUsageTodayResponse{}, err
	}

	stats, err := s.platform.FetchAdminGroupDailyStats(session, groups)
	if err != nil {
		return GroupUsageTodayResponse{}, err
	}

	// 归一化：分组名去空格、空名跳过、重名分组合并求和；顺序按首次出现排列。
	order := make([]string, 0, len(stats))
	totals := make(map[string]float64, len(stats))
	for _, stat := range stats {
		name := strings.TrimSpace(stat.GroupName)
		if name == "" {
			continue
		}
		if _, exists := totals[name]; !exists {
			order = append(order, name)
		}
		totals[name] += stat.TodayActualCost
	}

	items := make([]GroupUsageTodayItem, 0, len(order))
	var total float64
	for _, name := range order {
		amount := totals[name]
		items = append(items, GroupUsageTodayItem{GroupName: name, TodayAmount: amount})
		total += amount
	}

	return GroupUsageTodayResponse{
		Date:   time.Now().Format("2006-01-02"),
		Total:  total,
		Groups: items,
	}, nil
}

// UpstreamKeyUsageToday 获取当前工作区所有上游站点中，今天有消费的 key 明细（仪表盘「今日成本」下钻）。
// 数据只在弹窗打开时按需请求，不参与 LiveMetrics 的批量指标计算。
// 排序、总额与筛选逻辑全部由 upstream.Service.KeyUsageToday 保证与 todayPurchase 口径一致，
// 这里只负责排序展示和响应封装。
func (s *MetricsService) UpstreamKeyUsageToday(ctx context.Context, userID string) (UpstreamKeyUsageTodayResponse, error) {
	items, err := s.upstreams.KeyUsageToday(ctx, userID)
	failedSites := 0
	totalSites := 0
	if err != nil {
		var collectionErr *upstream.KeyUsageCollectionError
		if !errors.As(err, &collectionErr) || collectionErr.TotalSites <= 0 || collectionErr.FailedSites >= collectionErr.TotalSites {
			return UpstreamKeyUsageTodayResponse{}, requestError(ErrorUpstreamKeyUsageUnavailable)
		}
		failedSites = collectionErr.FailedSites
		totalSites = collectionErr.TotalSites
		log.Printf("dashboard key usage: partial upstream failure user_id=%s failed_sites=%d total_sites=%d", userID, failedSites, totalSites)
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].TodayAmount > items[j].TodayAmount
	})

	responseItems := make([]UpstreamKeyUsageTodayItem, 0, len(items))
	var total float64
	for _, item := range items {
		responseItems = append(responseItems, UpstreamKeyUsageTodayItem{
			SiteID:       item.SiteID,
			SiteName:     item.SiteName,
			Platform:     string(item.Platform),
			KeyID:        item.KeyID,
			KeyName:      item.KeyName,
			GroupName:    item.GroupName,
			TodayAmount:  item.TodayAmount,
			RawAmount:    item.RawAmount,
			RechargeRate: item.RechargeRate,
		})
		total += item.TodayAmount
	}

	return UpstreamKeyUsageTodayResponse{
		Date:        time.Now().Format("2006-01-02"),
		Total:       total,
		Keys:        responseItems,
		FailedSites: failedSites,
		TotalSites:  totalSites,
	}, nil
}

// UpstreamBalanceBreakdown 获取当前工作区所有上游站点的余额明细（仪表盘「上游总余额」下钻）。
// 直接复用已同步缓存数据，不触发外部平台请求；未知余额（rechargeRate 未配置或尚未同步成功）的站点排在列表最后，
// total 只对已知余额求和，与 LiveMetrics 中 upstreamBalance 的计算口径一致。
func (s *MetricsService) UpstreamBalanceBreakdown(ctx context.Context, userID string) (UpstreamBalanceBreakdownResponse, error) {
	items, err := s.upstreams.BalanceBreakdown(ctx, userID)
	if err != nil {
		return UpstreamBalanceBreakdownResponse{}, err
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Balance == nil || items[j].Balance == nil {
			return items[i].Balance != nil
		}
		return *items[i].Balance > *items[j].Balance
	})

	responseItems := make([]UpstreamBalanceBreakdownItem, 0, len(items))
	var total float64
	for _, item := range items {
		responseItems = append(responseItems, UpstreamBalanceBreakdownItem{
			SiteID:       item.SiteID,
			SiteName:     item.SiteName,
			Platform:     string(item.Platform),
			Balance:      item.Balance,
			RawBalance:   item.RawBalance,
			RechargeRate: item.RechargeRate,
			LastSyncedAt: item.LastSyncedAt,
			Status:       string(item.Status),
		})
		if item.Balance != nil {
			total += *item.Balance
		}
	}

	return UpstreamBalanceBreakdownResponse{
		Total: total,
		Sites: responseItems,
	}, nil
}

// GetBalanceFilter 读取当前用户当前工作区的余额筛选配置。
func (s *MetricsService) GetBalanceFilter(ctx context.Context, userID string) (BalanceFilterConfig, error) {
	// 按当前工作区隔离筛选配置。
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return BalanceFilterConfig{}, err
	}
	return s.metricsRepo.GetBalanceFilter(ctx, userID, adminAccountID)
}

// SaveBalanceFilter 保存用户当前工作区的余额筛选配置。
func (s *MetricsService) SaveBalanceFilter(ctx context.Context, userID string, config BalanceFilterConfig) error {
	// 按当前工作区隔离筛选配置。
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return err
	}
	config.UserID = userID
	config.AdminAccountID = adminAccountID
	return s.metricsRepo.SaveBalanceFilter(ctx, config)
}

func (s *MetricsService) requireCurrentAdminAccount(ctx context.Context, userID string) (string, error) {
	if s.accounts == nil {
		return "", requestError(ErrorAdminOnly)
	}
	return s.accounts.RequireCurrentID(ctx, userID)
}

func metricsRandomID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
