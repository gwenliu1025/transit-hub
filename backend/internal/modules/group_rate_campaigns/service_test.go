package group_rate_campaigns

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"transithub/backend/internal/modules/upstream"
)

// fakeOperator 是 AdminGroupOperator 的测试替身，记录调用次数以断言 Preview 等只读路径
// 从不触碰远端倍率写接口。
type fakeOperator struct {
	session     upstream.Session
	sessionErr  error
	groups      []upstream.AdminGroupInfo
	groupsErr   error
	updateCalls int
	updateErr   error
	// updates 记录每次远端调用实际使用的 (分组名, 倍率)，用于断言每个分组是否按自己的固定倍率更新。
	updates []multiplierUpdate
}

type multiplierUpdate struct {
	groupName  string
	multiplier float64
}

func (f *fakeOperator) RequireSession(ctx context.Context, userID string, adminAccountID string) (upstream.Session, error) {
	return f.session, f.sessionErr
}

func (f *fakeOperator) FetchAdminGroups(session upstream.Session) ([]upstream.AdminGroupInfo, error) {
	return f.groups, f.groupsErr
}

func (f *fakeOperator) UpdateAdminGroupMultiplier(session upstream.Session, group upstream.AdminGroupInfo, multiplier float64) error {
	f.updateCalls++
	f.updates = append(f.updates, multiplierUpdate{groupName: group.Name, multiplier: multiplier})
	return f.updateErr
}

// fakeNotifier 记录发送次数和每次发送的目标工作区/内容，用于断言通知次数与工作区。
type fakeNotifier struct {
	calls    int
	sent     []sentNotification
	ctxErrs  []error
	messages []string
}

type sentNotification struct {
	userID         string
	adminAccountID string
	botIDs         []string
}

func (f *fakeNotifier) SendToBotsForWorkspace(ctx context.Context, userID string, adminAccountID string, botIDs []string, message string) {
	f.calls++
	f.sent = append(f.sent, sentNotification{userID: userID, adminAccountID: adminAccountID, botIDs: botIDs})
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	f.messages = append(f.messages, message)
}

// fakeTypeLookup 是 GroupTypeLookup 的测试替身，按预设的 type/search/platform 返回固定分组名。
type fakeTypeLookup struct {
	byType map[string][]string
	err    error
}

func (f *fakeTypeLookup) ListGroupNames(ctx context.Context, userID string, adminAccountID string, search string, groupType string, platform string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if groupType != "" {
		return f.byType[groupType], nil
	}
	// currentFilter 模式：无 type 时按 search 返回预设集合。
	return f.byType[search], nil
}

// fakeRepository is an in-memory campaignRepository test double. It exists so that
// startCampaign/endCampaign/End/Cancel — the status-transition-heavy paths that a real
// *Repository can only exercise against a live Postgres connection — can be unit tested
// without a database, per this backend's "no test touches a real database" convention.
type fakeRepository struct {
	campaigns map[string]*Campaign
	items     map[string][]*CampaignItem
	// endAttempts / nextEndRetry / claimedAt 模拟 end_attempts、next_end_retry_at 与 ending 抢占时的 updated_at。
	endAttempts  map[string]int
	nextEndRetry map[string]*time.Time
	claimedAt    map[string]time.Time
	// ctxErrs 记录每次写入时的 ctx 状态，用于断言执行已与调用方取消信号解绑。
	ctxErrs []error
	// onUpdateItemApply 在开启写入单个分组结果后回调，用于模拟开启期间的并发动作。
	onUpdateItemApply func()
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{
		campaigns:    map[string]*Campaign{},
		items:        map[string][]*CampaignItem{},
		endAttempts:  map[string]int{},
		nextEndRetry: map[string]*time.Time{},
		claimedAt:    map[string]time.Time{},
	}
}

func (f *fakeRepository) EnsureSchema(ctx context.Context) error { return nil }

func (f *fakeRepository) Insert(ctx context.Context, c Campaign) error {
	cp := c
	f.campaigns[c.ID] = &cp
	return nil
}

func (f *fakeRepository) SaveItems(ctx context.Context, items []CampaignItem) error {
	for _, item := range items {
		cp := item
		cp.ApplyStatus = ItemPending
		cp.RestoreStatus = ItemPending
		f.items[item.CampaignID] = append(f.items[item.CampaignID], &cp)
	}
	return nil
}

func (f *fakeRepository) findItem(id string) *CampaignItem {
	for _, items := range f.items {
		for _, item := range items {
			if item.ID == id {
				return item
			}
		}
	}
	return nil
}

func (f *fakeRepository) UpdateItemApply(ctx context.Context, id string, originalMultiplier *float64, status string, reason string, appliedAt *time.Time) error {
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	item := f.findItem(id)
	if item == nil {
		return nil
	}
	item.OriginalMultiplier = originalMultiplier
	item.ApplyStatus = status
	item.ApplyReason = reason
	item.AppliedAt = appliedAt
	if f.onUpdateItemApply != nil {
		f.onUpdateItemApply()
	}
	return nil
}

func (f *fakeRepository) UpdateItemRestore(ctx context.Context, id string, restoredMultiplier *float64, status string, reason string, restoredAt *time.Time) error {
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	item := f.findItem(id)
	if item == nil {
		return nil
	}
	item.RestoredMultiplier = restoredMultiplier
	item.RestoreStatus = status
	item.RestoreReason = reason
	item.RestoredAt = restoredAt
	return nil
}

func (f *fakeRepository) ListItems(ctx context.Context, campaignID string) ([]CampaignItem, error) {
	items := f.items[campaignID]
	out := make([]CampaignItem, 0, len(items))
	for _, item := range items {
		out = append(out, *item)
	}
	return out, nil
}

func (f *fakeRepository) Get(ctx context.Context, userID string, adminAccountID string, id string) (*Campaign, error) {
	c, ok := f.campaigns[id]
	if !ok || c.UserID != userID || c.AdminAccountID != adminAccountID {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

func (f *fakeRepository) GetByID(ctx context.Context, id string) (*Campaign, error) {
	c, ok := f.campaigns[id]
	if !ok {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

func (f *fakeRepository) List(ctx context.Context, userID string, adminAccountID string, query ListQuery) ([]Campaign, int, error) {
	return nil, 0, nil
}

func (f *fakeRepository) ListDueScheduled(ctx context.Context, now time.Time) ([]string, error) {
	ids := make([]string, 0)
	for id, c := range f.campaigns {
		if c.Status == StatusScheduled && c.StartAt != nil && !c.StartAt.After(now) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// ListDueRunning mirrors listDueRunningSQL: it must scan both running and partial campaigns,
// since a partial campaign can still hold applied-but-not-restored items, but it skips
// campaigns whose retry backoff has not elapsed or whose end attempts are exhausted.
func (f *fakeRepository) ListDueRunning(ctx context.Context, now time.Time) ([]string, error) {
	ids := make([]string, 0)
	for id, c := range f.campaigns {
		if c.Status != StatusRunning && c.Status != StatusPartial {
			continue
		}
		if f.endAttempts[id] >= maxEndAttempts {
			continue
		}
		if retryAt := f.nextEndRetry[id]; retryAt != nil && retryAt.After(now) {
			continue
		}
		if c.EndMode == EndScheduled && c.EndAt != nil && !c.EndAt.After(now) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// ListStuckEnding mirrors listStuckEndingSQL using the time the campaign was claimed for ending.
func (f *fakeRepository) ListStuckEnding(ctx context.Context, staleBefore time.Time) ([]string, error) {
	ids := make([]string, 0)
	for id, c := range f.campaigns {
		if c.Status == StatusEnding && !f.claimedAt[id].After(staleBefore) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (f *fakeRepository) ClaimForRunning(ctx context.Context, id string) (bool, error) {
	c, ok := f.campaigns[id]
	if !ok || (c.Status != StatusDraft && c.Status != StatusScheduled) {
		return false, nil
	}
	c.Status = StatusRunning
	now := time.Now()
	c.StartedAt = &now
	return true, nil
}

// ClaimForEnding mirrors claimForEndingSQL: running and partial campaigns can both be
// claimed for the ending/restore path, and each claim counts as one end attempt.
func (f *fakeRepository) ClaimForEnding(ctx context.Context, id string) (int, bool, error) {
	c, ok := f.campaigns[id]
	if !ok || (c.Status != StatusRunning && c.Status != StatusPartial) {
		return 0, false, nil
	}
	c.Status = StatusEnding
	f.endAttempts[id]++
	f.claimedAt[id] = time.Now()
	return f.endAttempts[id], true, nil
}

// ClaimStuckEnding mirrors claimStuckEndingSQL.
func (f *fakeRepository) ClaimStuckEnding(ctx context.Context, id string, staleBefore time.Time) (int, bool, error) {
	c, ok := f.campaigns[id]
	if !ok || c.Status != StatusEnding || f.claimedAt[id].After(staleBefore) {
		return 0, false, nil
	}
	f.endAttempts[id]++
	f.claimedAt[id] = time.Now()
	return f.endAttempts[id], true, nil
}

// FinishStart mirrors finishStartSQL: only a campaign still in running can be finished.
func (f *fakeRepository) FinishStart(ctx context.Context, id string, status string) (bool, error) {
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	c, ok := f.campaigns[id]
	if !ok || c.Status != StatusRunning {
		return false, nil
	}
	c.Status = status
	return true, nil
}

// FinishEnd mirrors finishEndSQL: only a campaign still in ending can be finished.
func (f *fakeRepository) FinishEnd(ctx context.Context, id string, status string, nextRetryAt *time.Time) error {
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	c, ok := f.campaigns[id]
	if !ok || c.Status != StatusEnding {
		return nil
	}
	c.Status = status
	now := time.Now()
	c.EndedAt = &now
	f.nextEndRetry[id] = nextRetryAt
	return nil
}

func (f *fakeRepository) ClaimForCancel(ctx context.Context, id string) (bool, error) {
	c, ok := f.campaigns[id]
	if !ok || (c.Status != StatusDraft && c.Status != StatusScheduled) {
		return false, nil
	}
	c.Status = StatusCancelled
	return true, nil
}

func floatPtr(v float64) *float64 { return &v }

func adminGroups() []upstream.AdminGroupInfo {
	return []upstream.AdminGroupInfo{
		{ID: "g1", Name: "default", Multiplier: floatPtr(1.0)},
		{ID: "g2", Name: "vip", Multiplier: floatPtr(2.0)},
		{ID: "g3", Name: "svip", Multiplier: floatPtr(3.0)},
	}
}

func TestServicePreview(t *testing.T) {
	t.Run("computes per-group fixed campaign multiplier without touching repository or remote writes", func(t *testing.T) {
		operator := &fakeOperator{groups: adminGroups()}
		notifier := &fakeNotifier{}
		svc := NewService(nil, operator, notifier, nil, Config{})

		req := CreateCampaignRequest{
			Name: "activity",
			Selection: Selection{
				Mode: SelectionManual,
				Groups: []SelectionGroupRef{
					{GroupName: "vip", CampaignMultiplier: floatPtr(0.6)},
					{GroupName: "svip", CampaignMultiplier: floatPtr(0.8)},
				},
			},
			Adjustment: Adjustment{Mode: AdjustmentSet, Value: 0},
			Schedule:   Schedule{StartMode: StartNow, EndMode: EndManual},
		}
		resp, err := svc.Preview(context.Background(), "user1", "account1", req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Total != 2 {
			t.Fatalf("expected 2 items, got %d", resp.Total)
		}
		byName := map[string]PreviewItem{}
		for _, item := range resp.Items {
			byName[item.GroupName] = item
		}
		if byName["vip"].CampaignMultiplier != 0.6 || byName["vip"].OriginalMultiplier != 2.0 || byName["vip"].RestoredMultiplier != 2.0 {
			t.Errorf("unexpected vip item: %+v", byName["vip"])
		}
		if byName["svip"].CampaignMultiplier != 0.8 || byName["svip"].OriginalMultiplier != 3.0 || byName["svip"].RestoredMultiplier != 3.0 {
			t.Errorf("unexpected svip item: %+v", byName["svip"])
		}
		if operator.updateCalls != 0 {
			t.Errorf("expected no remote update calls during preview, got %d", operator.updateCalls)
		}
		if notifier.calls != 0 {
			t.Errorf("expected no notifications during preview, got %d", notifier.calls)
		}
	})

	t.Run("invalid request rejected before touching operator", func(t *testing.T) {
		operator := &fakeOperator{groups: adminGroups()}
		svc := NewService(nil, operator, nil, nil, Config{})

		req := CreateCampaignRequest{
			Name:       "",
			Adjustment: Adjustment{Mode: AdjustmentSet, Value: 1},
			Schedule:   Schedule{StartMode: StartNow, EndMode: EndManual},
		}
		_, err := svc.Preview(context.Background(), "user1", "account1", req)
		if err != ErrInvalidName {
			t.Fatalf("expected ErrInvalidName, got %v", err)
		}
	})

	t.Run("session error propagates", func(t *testing.T) {
		wantErr := errors.New("session expired")
		operator := &fakeOperator{sessionErr: wantErr}
		svc := NewService(nil, operator, nil, nil, Config{})

		req := CreateCampaignRequest{
			Name: "activity",
			Selection: Selection{
				Mode:   SelectionManual,
				Groups: []SelectionGroupRef{{GroupName: "vip", CampaignMultiplier: floatPtr(0.6)}},
			},
			Adjustment: Adjustment{Mode: AdjustmentSet, Value: 0},
			Schedule:   Schedule{StartMode: StartNow, EndMode: EndManual},
		}
		_, err := svc.Preview(context.Background(), "user1", "account1", req)
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected session error, got %v", err)
		}
	})

	t.Run("group not found among remote groups surfaces domain error", func(t *testing.T) {
		operator := &fakeOperator{groups: adminGroups()}
		svc := NewService(nil, operator, nil, nil, Config{})

		req := CreateCampaignRequest{
			Name: "activity",
			Selection: Selection{
				Mode:   SelectionManual,
				Groups: []SelectionGroupRef{{GroupName: "not-exists", CampaignMultiplier: floatPtr(0.6)}},
			},
			Adjustment: Adjustment{Mode: AdjustmentSet, Value: 0},
			Schedule:   Schedule{StartMode: StartNow, EndMode: EndManual},
		}
		_, err := svc.Preview(context.Background(), "user1", "account1", req)
		if err != ErrEmptySelection {
			t.Fatalf("expected ErrEmptySelection, got %v", err)
		}
	})
}

// TestCreateStartNowAppliesEachGroupsOwnFixedMultiplier is the end-to-end check for the
// per-group fixed multiplier model: two manually selected groups with different rates must
// each be updated remotely with their own value, not a single shared/computed value.
func TestCreateStartNowAppliesEachGroupsOwnFixedMultiplier(t *testing.T) {
	repo := newFakeRepository()
	operator := &fakeOperator{groups: adminGroups()}
	svc := &Service{repository: repo, operator: operator, config: Config{}}

	req := CreateCampaignRequest{
		Name: "flash sale",
		Selection: Selection{
			Mode: SelectionManual,
			Groups: []SelectionGroupRef{
				{GroupName: "vip", CampaignMultiplier: floatPtr(0.6)},
				{GroupName: "svip", CampaignMultiplier: floatPtr(0.9)},
			},
		},
		Adjustment: Adjustment{Mode: AdjustmentSet, Value: 0},
		Schedule:   Schedule{StartMode: StartNow, EndMode: EndManual},
	}

	detail, err := svc.Create(context.Background(), "user1", "account1", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if detail.Status != StatusRunning {
		t.Fatalf("expected campaign to be running, got %q", detail.Status)
	}
	if operator.updateCalls != 2 {
		t.Fatalf("expected 2 remote update calls, got %d", operator.updateCalls)
	}
	byGroup := map[string]float64{}
	for _, u := range operator.updates {
		byGroup[u.groupName] = u.multiplier
	}
	if byGroup["vip"] != 0.6 {
		t.Errorf("expected vip updated to 0.6, got %v", byGroup["vip"])
	}
	if byGroup["svip"] != 0.9 {
		t.Errorf("expected svip updated to 0.9, got %v", byGroup["svip"])
	}

	items, err := repo.ListItems(context.Background(), detail.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byName := map[string]CampaignItem{}
	for _, item := range items {
		byName[item.GroupName] = item
	}
	if byName["vip"].CampaignMultiplier != 0.6 || floatOrZero(byName["vip"].OriginalMultiplier) != 2.0 {
		t.Errorf("unexpected vip item: %+v", byName["vip"])
	}
	if byName["svip"].CampaignMultiplier != 0.9 || floatOrZero(byName["svip"].OriginalMultiplier) != 3.0 {
		t.Errorf("unexpected svip item: %+v", byName["svip"])
	}

	// Simulate the remote state after a successful start: vip/svip now sit at their campaign
	// multipliers. fakeOperator.groups is static and doesn't auto-mutate from UpdateAdminGroupMultiplier
	// calls, so this must be set explicitly for End to see a real difference from OriginalMultiplier.
	operator.groups = []upstream.AdminGroupInfo{
		{ID: "g1", Name: "default", Multiplier: floatPtr(1.0)},
		{ID: "g2", Name: "vip", Multiplier: floatPtr(0.6)},
		{ID: "g3", Name: "svip", Multiplier: floatPtr(0.9)},
	}

	// End must restore each item back to its own original_multiplier, not a shared value.
	detail, err = svc.End(context.Background(), "user1", "account1", detail.ID)
	if err != nil {
		t.Fatalf("unexpected error ending campaign: %v", err)
	}
	if detail.Status != StatusEnded {
		t.Fatalf("expected campaign to end as %q, got %q", StatusEnded, detail.Status)
	}
	if operator.updateCalls != 4 {
		t.Fatalf("expected 2 more remote restore calls (4 total), got %d", operator.updateCalls)
	}
	restoredByGroup := map[string]float64{}
	for _, u := range operator.updates[2:] {
		restoredByGroup[u.groupName] = u.multiplier
	}
	if restoredByGroup["vip"] != 2.0 {
		t.Errorf("expected vip restored to 2.0, got %v", restoredByGroup["vip"])
	}
	if restoredByGroup["svip"] != 3.0 {
		t.Errorf("expected svip restored to 3.0, got %v", restoredByGroup["svip"])
	}
}

func TestServiceResolveSelection(t *testing.T) {
	svcWithLookup := func(lookup GroupTypeLookup) *Service {
		return NewService(nil, &fakeOperator{}, nil, lookup, Config{})
	}

	t.Run("all mode returns every group", func(t *testing.T) {
		svc := svcWithLookup(nil)
		targets, err := svc.resolveSelection(context.Background(), "u", "a", Selection{Mode: SelectionAll}, adminGroups())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(targets) != 3 {
			t.Fatalf("expected 3 targets, got %d", len(targets))
		}
	})

	t.Run("manual mode filters by exact name", func(t *testing.T) {
		svc := svcWithLookup(nil)
		selection := Selection{Mode: SelectionManual, Groups: []SelectionGroupRef{{GroupName: "vip"}, {GroupName: " svip "}}}
		targets, err := svc.resolveSelection(context.Background(), "u", "a", selection, adminGroups())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(targets) != 2 {
			t.Fatalf("expected 2 targets, got %d", len(targets))
		}
	})

	t.Run("manual mode with no matches returns empty selection error", func(t *testing.T) {
		svc := svcWithLookup(nil)
		selection := Selection{Mode: SelectionManual, Groups: []SelectionGroupRef{{GroupName: "ghost"}}}
		_, err := svc.resolveSelection(context.Background(), "u", "a", selection, adminGroups())
		if err != ErrEmptySelection {
			t.Fatalf("expected ErrEmptySelection, got %v", err)
		}
	})

	t.Run("type mode unions across types and intersects with admin groups", func(t *testing.T) {
		lookup := &fakeTypeLookup{byType: map[string][]string{
			"promo":   {"vip", "ghost"},
			"premium": {"svip"},
		}}
		svc := svcWithLookup(lookup)
		selection := Selection{Mode: SelectionType, Types: []string{"promo", "premium"}}
		targets, err := svc.resolveSelection(context.Background(), "u", "a", selection, adminGroups())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(targets) != 2 {
			t.Fatalf("expected 2 targets (vip, svip), got %d", len(targets))
		}
	})

	t.Run("type mode with nil lookup returns nil names and thus empty selection", func(t *testing.T) {
		svc := svcWithLookup(nil)
		selection := Selection{Mode: SelectionType, Types: []string{"promo"}}
		_, err := svc.resolveSelection(context.Background(), "u", "a", selection, adminGroups())
		if err != ErrEmptySelection {
			t.Fatalf("expected ErrEmptySelection, got %v", err)
		}
	})

	t.Run("currentFilter mode delegates to lookup with filter fields", func(t *testing.T) {
		lookup := &fakeTypeLookup{byType: map[string][]string{"vip-search": {"vip"}}}
		svc := svcWithLookup(lookup)
		selection := Selection{Mode: SelectionCurrentFilter, Filter: SelectionFilter{Search: "vip-search"}}
		targets, err := svc.resolveSelection(context.Background(), "u", "a", selection, adminGroups())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(targets) != 1 || targets[0].Name != "vip" {
			t.Fatalf("expected only vip, got %+v", targets)
		}
	})

	t.Run("currentFilter mode with nil lookup returns empty selection error", func(t *testing.T) {
		svc := svcWithLookup(nil)
		selection := Selection{Mode: SelectionCurrentFilter}
		_, err := svc.resolveSelection(context.Background(), "u", "a", selection, adminGroups())
		if err != ErrEmptySelection {
			t.Fatalf("expected ErrEmptySelection, got %v", err)
		}
	})

	t.Run("unknown mode returns empty selection error", func(t *testing.T) {
		svc := svcWithLookup(nil)
		selection := Selection{Mode: SelectionMode("bogus")}
		_, err := svc.resolveSelection(context.Background(), "u", "a", selection, adminGroups())
		if err != ErrEmptySelection {
			t.Fatalf("expected ErrEmptySelection, got %v", err)
		}
	})

	t.Run("lookup error propagates", func(t *testing.T) {
		wantErr := errors.New("lookup failed")
		lookup := &fakeTypeLookup{err: wantErr}
		svc := svcWithLookup(lookup)
		selection := Selection{Mode: SelectionType, Types: []string{"promo"}}
		_, err := svc.resolveSelection(context.Background(), "u", "a", selection, adminGroups())
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected lookup error, got %v", err)
		}
	})
}

func TestNormalizeNotify(t *testing.T) {
	cfg := Config{
		DefaultNotifyBotIDs:  []string{"default-bot"},
		StartTemplateDefault: "start default",
		EndTemplateDefault:   "end default",
	}

	t.Run("disabled notify collapses to bare disabled struct", func(t *testing.T) {
		got := normalizeNotify(Notify{Enabled: false, BotIDs: []string{"explicit"}, StartTemplate: "x"}, cfg)
		if got.Enabled || len(got.BotIDs) != 0 || got.StartTemplate != "" || got.EndTemplate != "" {
			t.Fatalf("expected bare disabled struct, got %+v", got)
		}
	})

	t.Run("enabled notify fills empty fields from config defaults", func(t *testing.T) {
		got := normalizeNotify(Notify{Enabled: true}, cfg)
		if len(got.BotIDs) != 1 || got.BotIDs[0] != "default-bot" {
			t.Errorf("expected default bot ids, got %v", got.BotIDs)
		}
		if got.StartTemplate != "start default" {
			t.Errorf("expected default start template, got %q", got.StartTemplate)
		}
		if got.EndTemplate != "end default" {
			t.Errorf("expected default end template, got %q", got.EndTemplate)
		}
	})

	t.Run("enabled notify never overrides explicit non-empty values", func(t *testing.T) {
		explicit := Notify{Enabled: true, BotIDs: []string{"explicit-bot"}, StartTemplate: "custom start", EndTemplate: "custom end"}
		got := normalizeNotify(explicit, cfg)
		if got.BotIDs[0] != "explicit-bot" {
			t.Errorf("expected explicit bot ids preserved, got %v", got.BotIDs)
		}
		if got.StartTemplate != "custom start" {
			t.Errorf("expected explicit start template preserved, got %q", got.StartTemplate)
		}
		if got.EndTemplate != "custom end" {
			t.Errorf("expected explicit end template preserved, got %q", got.EndTemplate)
		}
	})
}

func TestNormalizeListQuery(t *testing.T) {
	tests := []struct {
		name         string
		in           ListQuery
		wantPage     int
		wantPageSize int
	}{
		{name: "defaults applied for zero values", in: ListQuery{}, wantPage: 1, wantPageSize: 20},
		{name: "negative page clamped to 1", in: ListQuery{Page: -5, PageSize: 10}, wantPage: 1, wantPageSize: 10},
		{name: "oversized page size clamped to 100", in: ListQuery{Page: 2, PageSize: 500}, wantPage: 2, wantPageSize: 100},
		{name: "status trimmed", in: ListQuery{Page: 1, PageSize: 10, Status: "  running  "}, wantPage: 1, wantPageSize: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeListQuery(tt.in)
			if got.Page != tt.wantPage {
				t.Errorf("expected page %d, got %d", tt.wantPage, got.Page)
			}
			if got.PageSize != tt.wantPageSize {
				t.Errorf("expected pageSize %d, got %d", tt.wantPageSize, got.PageSize)
			}
		})
	}
}

func TestTotalPages(t *testing.T) {
	tests := []struct {
		total    int
		pageSize int
		want     int
	}{
		{total: 0, pageSize: 10, want: 0},
		{total: 10, pageSize: 0, want: 0},
		{total: 10, pageSize: 10, want: 1},
		{total: 11, pageSize: 10, want: 2},
		{total: 25, pageSize: 10, want: 3},
	}
	for _, tt := range tests {
		if got := totalPages(tt.total, tt.pageSize); got != tt.want {
			t.Errorf("totalPages(%d, %d) = %d, want %d", tt.total, tt.pageSize, got, tt.want)
		}
	}
}

func TestToListItemAndToDetail(t *testing.T) {
	startedAt := time.Now().Add(-time.Hour)
	endedAt := time.Now()
	campaign := Campaign{
		ID:        "c1",
		UserID:    "user1",
		Name:      "activity",
		Status:    StatusEnded,
		StartMode: StartNow,
		EndMode:   EndManual,
		StartedAt: &startedAt,
		EndedAt:   &endedAt,
		Notify:    Notify{Enabled: true},
	}
	items := []CampaignItem{
		{GroupName: "vip", ApplyStatus: ItemApplied, RestoreStatus: ItemRestored},
		{GroupName: "svip", ApplyStatus: ItemFailed, RestoreStatus: ItemPending},
	}

	listItem := toListItem(campaign, items)
	if listItem.LastExecutedAt != campaign.EndedAt {
		t.Errorf("expected lastExecutedAt to prefer endedAt when set")
	}
	if listItem.Summary.Total != 2 || listItem.Summary.Applied != 1 || listItem.Summary.ApplyFailed != 1 {
		t.Errorf("unexpected summary: %+v", listItem.Summary)
	}
	if !listItem.NotifyEnabled {
		t.Errorf("expected notifyEnabled true")
	}

	detail := toDetail(campaign, items)
	if len(detail.Items) != 2 {
		t.Fatalf("expected 2 item views, got %d", len(detail.Items))
	}
	if detail.Items[0].GroupName != "vip" || detail.Items[1].GroupName != "svip" {
		t.Errorf("expected item order preserved, got %+v", detail.Items)
	}
}

func TestToListItemLastExecutedFallsBackToStartedAt(t *testing.T) {
	startedAt := time.Now()
	campaign := Campaign{StartedAt: &startedAt}
	listItem := toListItem(campaign, nil)
	if listItem.LastExecutedAt != campaign.StartedAt {
		t.Errorf("expected lastExecutedAt to fall back to startedAt when endedAt is nil")
	}
}

func TestFindGroup(t *testing.T) {
	groups := adminGroups()
	if g := findGroup(groups, " vip "); g == nil || g.Name != "vip" {
		t.Errorf("expected to find vip with trimmed match, got %+v", g)
	}
	if g := findGroup(groups, "missing"); g != nil {
		t.Errorf("expected nil for missing group, got %+v", g)
	}
}

func TestFloatOrZero(t *testing.T) {
	if floatOrZero(nil) != 0 {
		t.Errorf("expected 0 for nil pointer")
	}
	if floatOrZero(floatPtr(2.5)) != 2.5 {
		t.Errorf("expected 2.5")
	}
}

func TestToSet(t *testing.T) {
	set := toSet([]string{" a ", "b", "a"})
	if len(set) != 2 {
		t.Fatalf("expected 2 unique entries, got %d", len(set))
	}
	if _, ok := set["a"]; !ok {
		t.Errorf("expected trimmed key 'a' to be present")
	}
}

func TestFormatTime(t *testing.T) {
	if formatTime(nil) != "" {
		t.Errorf("expected empty string for nil time")
	}
	now := time.Now()
	if formatTime(&now) == "" {
		t.Errorf("expected non-empty formatted time")
	}
}

// partialCampaignFixture builds a repository pre-loaded with one partial campaign that has
// three items: one applied-but-not-restored (must be restored), one failed-to-apply (must
// never be restored), and one already-restored (must not be restored again).
func partialCampaignFixture(endMode EndMode, endAt *time.Time) (*fakeRepository, string) {
	repo := newFakeRepository()
	campaignID := "campaign-partial"
	repo.campaigns[campaignID] = &Campaign{
		ID:             campaignID,
		UserID:         "user1",
		AdminAccountID: "account1",
		Name:           "partial activity",
		Status:         StatusPartial,
		EndMode:        endMode,
		EndAt:          endAt,
	}
	repo.items[campaignID] = []*CampaignItem{
		{ID: "item-applied", CampaignID: campaignID, GroupName: "vip", OriginalMultiplier: floatPtr(1.0), ApplyStatus: ItemApplied, RestoreStatus: ItemPending},
		{ID: "item-failed", CampaignID: campaignID, GroupName: "svip", OriginalMultiplier: floatPtr(3.0), ApplyStatus: ItemFailed, RestoreStatus: ItemPending},
		{ID: "item-already-restored", CampaignID: campaignID, GroupName: "default", OriginalMultiplier: floatPtr(1.0), ApplyStatus: ItemApplied, RestoreStatus: ItemRestored},
	}
	return repo, campaignID
}

// remoteGroupsAfterPartialStart returns the admin's remote groups as they would look right
// after a partial start: "vip" was actually changed to 5.0 by the campaign, "svip" was never
// touched because it failed to apply, "default" already sits back at its original value.
func remoteGroupsAfterPartialStart() []upstream.AdminGroupInfo {
	return []upstream.AdminGroupInfo{
		{ID: "g1", Name: "default", Multiplier: floatPtr(1.0)},
		{ID: "g2", Name: "vip", Multiplier: floatPtr(5.0)},
		{ID: "g3", Name: "svip", Multiplier: floatPtr(3.0)},
	}
}

func TestEndManuallyRestoresPartialCampaign(t *testing.T) {
	repo, campaignID := partialCampaignFixture(EndManual, nil)
	operator := &fakeOperator{groups: remoteGroupsAfterPartialStart()}
	svc := &Service{repository: repo, operator: operator, config: Config{}}

	detail, err := svc.End(context.Background(), "user1", "account1", campaignID)
	if err != nil {
		t.Fatalf("expected End to accept a partial campaign, got error: %v", err)
	}
	if detail.Status != StatusEnded {
		t.Fatalf("expected campaign to end as %q once the only outstanding item is restored, got %q", StatusEnded, detail.Status)
	}

	// Only the applied-but-unrestored item should trigger a remote write.
	if operator.updateCalls != 1 {
		t.Fatalf("expected exactly 1 remote restore call, got %d", operator.updateCalls)
	}

	items := repo.items[campaignID]
	byID := map[string]*CampaignItem{}
	for _, item := range items {
		byID[item.ID] = item
	}
	if byID["item-applied"].RestoreStatus != ItemRestored {
		t.Errorf("expected applied item to be restored, got restoreStatus=%q", byID["item-applied"].RestoreStatus)
	}
	if byID["item-failed"].RestoreStatus != ItemPending {
		t.Errorf("expected failed-to-apply item to be left untouched, got restoreStatus=%q", byID["item-failed"].RestoreStatus)
	}
	if byID["item-already-restored"].RestoreStatus != ItemRestored {
		t.Errorf("expected already-restored item to remain restored")
	}
}

func TestEndRejectsCampaignsThatAreNotRunningOrPartial(t *testing.T) {
	repo := newFakeRepository()
	repo.campaigns["draft-campaign"] = &Campaign{ID: "draft-campaign", UserID: "user1", AdminAccountID: "account1", Status: StatusDraft}
	svc := &Service{repository: repo, operator: &fakeOperator{}, config: Config{}}

	_, err := svc.End(context.Background(), "user1", "account1", "draft-campaign")
	if err != ErrInvalidState {
		t.Fatalf("expected ErrInvalidState for a draft campaign, got %v", err)
	}
}

func TestSchedulerRestoresDuePartialCampaign(t *testing.T) {
	due := time.Now().Add(-time.Minute)
	repo, campaignID := partialCampaignFixture(EndScheduled, &due)
	operator := &fakeOperator{groups: remoteGroupsAfterPartialStart()}
	svc := &Service{repository: repo, operator: operator, config: Config{}}

	svc.runSchedulerTick(context.Background())

	campaign := repo.campaigns[campaignID]
	if campaign.Status != StatusEnded {
		t.Fatalf("expected scheduler to restore the due partial campaign to %q, got %q", StatusEnded, campaign.Status)
	}
	if operator.updateCalls != 1 {
		t.Fatalf("expected exactly 1 remote restore call from the scheduler tick, got %d", operator.updateCalls)
	}
}

func TestSchedulerIgnoresPartialCampaignNotYetDue(t *testing.T) {
	notDue := time.Now().Add(time.Hour)
	repo, campaignID := partialCampaignFixture(EndScheduled, &notDue)
	operator := &fakeOperator{groups: remoteGroupsAfterPartialStart()}
	svc := &Service{repository: repo, operator: operator, config: Config{}}

	svc.runSchedulerTick(context.Background())

	campaign := repo.campaigns[campaignID]
	if campaign.Status != StatusPartial {
		t.Fatalf("expected campaign not yet due to remain %q, got %q", StatusPartial, campaign.Status)
	}
	if operator.updateCalls != 0 {
		t.Fatalf("expected no remote calls before the end time is reached, got %d", operator.updateCalls)
	}
}

// TestEndingClaimIsIdempotentForPartialCampaigns proves that repeatedly triggering the
// ending flow for the same partial campaign (e.g. two overlapping scheduler ticks, or a
// scheduler tick racing a manual "end" click) restores each item at most once.
func TestEndingClaimIsIdempotentForPartialCampaigns(t *testing.T) {
	repo, campaignID := partialCampaignFixture(EndManual, nil)
	operator := &fakeOperator{groups: remoteGroupsAfterPartialStart()}
	svc := &Service{repository: repo, operator: operator, config: Config{}}

	svc.endCampaign(context.Background(), campaignID, endTriggerManual)
	firstCallCount := operator.updateCalls
	if firstCallCount != 1 {
		t.Fatalf("expected the first endCampaign call to restore exactly 1 item, got %d", firstCallCount)
	}

	// The campaign is now "ended", so a second invocation must not re-claim it or
	// perform any further remote writes, even though nothing prevents it from being called again.
	svc.endCampaign(context.Background(), campaignID, endTriggerScheduler)
	if operator.updateCalls != firstCallCount {
		t.Fatalf("expected no additional remote calls on a repeated endCampaign call, got %d (was %d)", operator.updateCalls, firstCallCount)
	}

	_, claimed, err := repo.ClaimForEnding(context.Background(), campaignID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claimed {
		t.Fatalf("expected ClaimForEnding to fail once the campaign has already ended")
	}
}

// notifyingCampaignFixture 构造一个已到期、开启通知的定时结束活动，只有一个已改价待恢复的分组 vip。
func notifyingCampaignFixture(status string) (*fakeRepository, string) {
	repo := newFakeRepository()
	campaignID := "campaign-notify"
	due := time.Now().Add(-time.Minute)
	repo.campaigns[campaignID] = &Campaign{
		ID:             campaignID,
		UserID:         "user1",
		AdminAccountID: "workspace-a",
		Name:           "notify activity",
		Status:         status,
		EndMode:        EndScheduled,
		EndAt:          &due,
		Notify:         Notify{Enabled: true, BotIDs: []string{"bot-a"}, StartTemplate: "start {activityName}", EndTemplate: "end restored={restoredCount} failed={restoreFailedCount}"},
	}
	repo.items[campaignID] = []*CampaignItem{
		{ID: "item-vip", CampaignID: campaignID, GroupName: "vip", OriginalMultiplier: floatPtr(1.0), ApplyStatus: ItemApplied, RestoreStatus: ItemPending},
	}
	return repo, campaignID
}

// makeEndRetryDue 模拟退避时间已过，让下一轮调度可以再次尝试恢复。
func makeEndRetryDue(repo *fakeRepository, campaignID string) {
	past := time.Now().Add(-time.Second)
	repo.nextEndRetry[campaignID] = &past
}

// TestSchedulerEndFailureRetriesBoundedAndNotifiesOnce 回归：定时结束恢复失败后，调度器曾每轮重新
// 抢占 partial 活动并重复发送结束通知。现在失败按退避有界重试，中间失败不通知，重试用尽时只通知一次。
func TestSchedulerEndFailureRetriesBoundedAndNotifiesOnce(t *testing.T) {
	repo, campaignID := notifyingCampaignFixture(StatusRunning)
	operator := &fakeOperator{groups: remoteGroupsAfterPartialStart(), updateErr: errors.New("remote down")}
	notifier := &fakeNotifier{}
	svc := &Service{repository: repo, operator: operator, notifier: notifier, config: Config{}}

	svc.runSchedulerTick(context.Background())
	if repo.campaigns[campaignID].Status != StatusPartial || operator.updateCalls != 1 {
		t.Fatalf("expected first failed attempt to leave partial after 1 remote call, got status=%q calls=%d", repo.campaigns[campaignID].Status, operator.updateCalls)
	}
	if repo.nextEndRetry[campaignID] == nil || !repo.nextEndRetry[campaignID].After(time.Now()) {
		t.Fatalf("expected a future retry time after the first failure, got %v", repo.nextEndRetry[campaignID])
	}
	if notifier.calls != 0 {
		t.Fatalf("intermediate scheduler failure must not notify, got %d", notifier.calls)
	}

	// 退避期内的调度轮次不能再次恢复，也不能再发通知。
	svc.runSchedulerTick(context.Background())
	if operator.updateCalls != 1 || notifier.calls != 0 {
		t.Fatalf("expected no retry during backoff, got calls=%d notifications=%d", operator.updateCalls, notifier.calls)
	}

	for attempt := 2; attempt <= maxEndAttempts; attempt++ {
		makeEndRetryDue(repo, campaignID)
		svc.runSchedulerTick(context.Background())
		if operator.updateCalls != attempt {
			t.Fatalf("expected retry attempt %d to call remote once, got %d calls", attempt, operator.updateCalls)
		}
	}
	if notifier.calls != 1 {
		t.Fatalf("expected exactly one end notification once retries are exhausted, got %d", notifier.calls)
	}
	if repo.nextEndRetry[campaignID] != nil {
		t.Fatalf("expected no further retry after the final attempt, got %v", repo.nextEndRetry[campaignID])
	}
	if got := notifier.messages[0]; got != "end restored=0 failed=1" {
		t.Fatalf("unexpected final end notification: %q", got)
	}

	// 重试用尽后调度器不再处理该活动，也不再重复通知。
	svc.runSchedulerTick(context.Background())
	if operator.updateCalls != maxEndAttempts || notifier.calls != 1 {
		t.Fatalf("expected scheduler to stop after exhausting retries, got calls=%d notifications=%d", operator.updateCalls, notifier.calls)
	}

	// 用户仍可手动结束；手动结果单独通知一次。
	operator.updateErr = nil
	detail, err := svc.End(context.Background(), "user1", "workspace-a", campaignID)
	if err != nil {
		t.Fatalf("manual end after exhausted retries failed: %v", err)
	}
	if detail.Status != StatusEnded || notifier.calls != 2 {
		t.Fatalf("expected manual end to restore and notify once, got status=%q notifications=%d", detail.Status, notifier.calls)
	}
}

// TestSchedulerEndRetrySuccessNotifiesOnce 验证首轮失败、重试成功时只发送一次"已结束"通知，且发往活动所属工作区。
func TestSchedulerEndRetrySuccessNotifiesOnce(t *testing.T) {
	repo, campaignID := notifyingCampaignFixture(StatusPartial)
	operator := &fakeOperator{groups: remoteGroupsAfterPartialStart(), updateErr: errors.New("remote down")}
	notifier := &fakeNotifier{}
	svc := &Service{repository: repo, operator: operator, notifier: notifier, config: Config{}}

	svc.runSchedulerTick(context.Background())
	operator.updateErr = nil
	makeEndRetryDue(repo, campaignID)
	svc.runSchedulerTick(context.Background())
	svc.runSchedulerTick(context.Background())

	if repo.campaigns[campaignID].Status != StatusEnded {
		t.Fatalf("expected retry to end the campaign, got %q", repo.campaigns[campaignID].Status)
	}
	if notifier.calls != 1 || notifier.messages[0] != "end restored=1 failed=0" {
		t.Fatalf("expected one success notification, got calls=%d messages=%v", notifier.calls, notifier.messages)
	}
	if sent := notifier.sent[0]; sent.userID != "user1" || sent.adminAccountID != "workspace-a" {
		t.Fatalf("campaign notification must target the campaign workspace, got %+v", sent)
	}
}

func TestNextEndRetryAtBackoffIsBounded(t *testing.T) {
	now := time.Now()
	if got := nextEndRetryAt(1, now); got == nil || !got.Equal(now.Add(endRetryBaseDelay)) {
		t.Fatalf("expected first retry after base delay, got %v", got)
	}
	if got := nextEndRetryAt(3, now); got == nil || !got.Equal(now.Add(4*endRetryBaseDelay)) {
		t.Fatalf("expected exponential backoff on the third attempt, got %v", got)
	}
	if got := nextEndRetryAt(maxEndAttempts, now); got != nil {
		t.Fatalf("expected no retry once attempts are exhausted, got %v", got)
	}
}

// TestStartSkipsGroupsWithUnknownOriginalMultiplier 回归：New API 分组可能缺倍率，曾被当作 0 记录并改价，
// 结束时又"恢复"成 0。现在原倍率未知的分组直接标记失败，不改价。
func TestStartSkipsGroupsWithUnknownOriginalMultiplier(t *testing.T) {
	repo := newFakeRepository()
	operator := &fakeOperator{groups: []upstream.AdminGroupInfo{
		{ID: "g2", Name: "vip", Multiplier: nil},
		{ID: "g3", Name: "svip", Multiplier: floatPtr(3.0)},
	}}
	svc := &Service{repository: repo, operator: operator, config: Config{}}

	detail, err := svc.Create(context.Background(), "user1", "account1", CreateCampaignRequest{
		Name: "nil original",
		Selection: Selection{Mode: SelectionManual, Groups: []SelectionGroupRef{
			{GroupName: "vip", CampaignMultiplier: floatPtr(0.5)},
			{GroupName: "svip", CampaignMultiplier: floatPtr(0.9)},
		}},
		Adjustment: Adjustment{Mode: AdjustmentSet, Value: 0},
		Schedule:   Schedule{StartMode: StartNow, EndMode: EndManual},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if detail.Status != StatusPartial {
		t.Fatalf("expected partial start when one group lacks an original multiplier, got %q", detail.Status)
	}
	if len(operator.updates) != 1 || operator.updates[0].groupName != "svip" {
		t.Fatalf("expected only svip to be changed remotely, got %+v", operator.updates)
	}
	for _, item := range repo.items[detail.ID] {
		if item.GroupName == "vip" && (item.ApplyStatus != ItemFailed || item.ApplyReason != reasonOriginalMultiplierUnavailable || item.OriginalMultiplier != nil) {
			t.Fatalf("expected vip item to fail with unknown original multiplier, got %+v", item)
		}
	}
}

// TestEndNeverRestoresUnknownOriginalAndRestoresWhenCurrentUnknown 覆盖结束阶段的两种 nil：
// 原倍率未知（历史数据）绝不恢复成 0；当前倍率未知时必须真实恢复，不能误判为 unchanged。
func TestEndNeverRestoresUnknownOriginalAndRestoresWhenCurrentUnknown(t *testing.T) {
	repo := newFakeRepository()
	campaignID := "campaign-nil"
	repo.campaigns[campaignID] = &Campaign{ID: campaignID, UserID: "user1", AdminAccountID: "account1", Status: StatusRunning, EndMode: EndManual}
	repo.items[campaignID] = []*CampaignItem{
		{ID: "item-legacy-nil", CampaignID: campaignID, GroupName: "vip", OriginalMultiplier: nil, CampaignMultiplier: 0.5, ApplyStatus: ItemApplied, RestoreStatus: ItemPending},
		{ID: "item-free", CampaignID: campaignID, GroupName: "free", OriginalMultiplier: floatPtr(0), CampaignMultiplier: 0.3, ApplyStatus: ItemApplied, RestoreStatus: ItemPending},
	}
	operator := &fakeOperator{groups: []upstream.AdminGroupInfo{
		{ID: "g2", Name: "vip", Multiplier: floatPtr(0.5)},
		{ID: "g4", Name: "free", Multiplier: nil},
	}}
	svc := &Service{repository: repo, operator: operator, config: Config{}}

	detail, err := svc.End(context.Background(), "user1", "account1", campaignID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if detail.Status != StatusPartial {
		t.Fatalf("expected partial because the legacy item cannot be restored, got %q", detail.Status)
	}
	if len(operator.updates) != 1 || operator.updates[0].groupName != "free" || operator.updates[0].multiplier != 0 {
		t.Fatalf("expected only free to be restored to 0 and vip untouched, got %+v", operator.updates)
	}
	byID := map[string]*CampaignItem{}
	for _, item := range repo.items[campaignID] {
		byID[item.ID] = item
	}
	if item := byID["item-legacy-nil"]; item.RestoreStatus != ItemFailed || item.RestoreReason != reasonOriginalMultiplierUnavailable {
		t.Fatalf("expected legacy nil-original item to fail restore, got %+v", item)
	}
	if item := byID["item-free"]; item.RestoreStatus != ItemRestored {
		t.Fatalf("expected free item to be restored when current multiplier is unknown, got %+v", item)
	}
}

// TestEndIgnoresCallerCancellation 回归：开启/结束曾绑定 HTTP 请求 ctx，客户端断开后落库失败，
// 分组停在 pending 或活动卡在 ending。现在执行与调用方取消解绑。
func TestEndIgnoresCallerCancellation(t *testing.T) {
	repo, campaignID := notifyingCampaignFixture(StatusRunning)
	operator := &fakeOperator{groups: remoteGroupsAfterPartialStart()}
	notifier := &fakeNotifier{}
	svc := &Service{repository: repo, operator: operator, notifier: notifier, config: Config{}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.End(ctx, "user1", "workspace-a", campaignID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.campaigns[campaignID].Status != StatusEnded {
		t.Fatalf("expected campaign to end despite caller cancellation, got %q", repo.campaigns[campaignID].Status)
	}
	for i, ctxErr := range append(append([]error{}, repo.ctxErrs...), notifier.ctxErrs...) {
		if ctxErr != nil {
			t.Fatalf("execution write #%d saw caller cancellation: %v", i, ctxErr)
		}
	}
	if len(repo.ctxErrs) == 0 || len(notifier.ctxErrs) != 1 {
		t.Fatalf("expected restore writes and one notification, got writes=%d notifications=%d", len(repo.ctxErrs), len(notifier.ctxErrs))
	}
}

// TestSchedulerRecoversStuckEndingCampaign 验证卡在 ending 超过阈值的活动会被调度器重新接管恢复，
// 刚进入 ending 的活动（可能仍在执行）不受影响。
func TestSchedulerRecoversStuckEndingCampaign(t *testing.T) {
	repo, campaignID := notifyingCampaignFixture(StatusEnding)
	repo.endAttempts[campaignID] = 1
	repo.claimedAt[campaignID] = time.Now().Add(-stuckEndingAfter - time.Minute)
	repo.campaigns["campaign-fresh"] = &Campaign{ID: "campaign-fresh", UserID: "user1", AdminAccountID: "workspace-a", Status: StatusEnding, EndMode: EndManual}
	repo.claimedAt["campaign-fresh"] = time.Now()
	operator := &fakeOperator{groups: remoteGroupsAfterPartialStart()}
	notifier := &fakeNotifier{}
	svc := &Service{repository: repo, operator: operator, notifier: notifier, config: Config{}}

	svc.runSchedulerTick(context.Background())

	if repo.campaigns[campaignID].Status != StatusEnded || operator.updateCalls != 1 {
		t.Fatalf("expected stuck campaign to be restored, got status=%q calls=%d", repo.campaigns[campaignID].Status, operator.updateCalls)
	}
	if repo.endAttempts[campaignID] != 2 || notifier.calls != 1 {
		t.Fatalf("expected recovery to count as a new attempt and notify once, got attempts=%d notifications=%d", repo.endAttempts[campaignID], notifier.calls)
	}
	if repo.campaigns["campaign-fresh"].Status != StatusEnding {
		t.Fatalf("a recently claimed ending campaign must not be taken over, got %q", repo.campaigns["campaign-fresh"].Status)
	}
}

// TestFinishStartDoesNotOverwriteConcurrentEnd 回归：开启执行期间活动被（其它进程）抢占结束时，
// FinishStart 曾无条件把状态覆盖回 running。
func TestFinishStartDoesNotOverwriteConcurrentEnd(t *testing.T) {
	repo := newFakeRepository()
	operator := &fakeOperator{groups: adminGroups()}
	svc := &Service{repository: repo, operator: operator, config: Config{}}
	campaignID := "campaign-race"
	repo.campaigns[campaignID] = &Campaign{
		ID: campaignID, UserID: "user1", AdminAccountID: "account1", Status: StatusScheduled, EndMode: EndManual,
		Selection: Selection{Mode: SelectionManual, Groups: []SelectionGroupRef{{GroupName: "vip", CampaignMultiplier: floatPtr(0.6)}}},
	}
	repo.onUpdateItemApply = func() {
		repo.onUpdateItemApply = nil
		if _, claimed, _ := repo.ClaimForEnding(context.Background(), campaignID); !claimed {
			t.Errorf("expected concurrent end claim to succeed while starting")
		}
	}

	svc.startCampaign(context.Background(), campaignID)

	if got := repo.campaigns[campaignID].Status; got != StatusEnding {
		t.Fatalf("FinishStart must not overwrite a concurrent end, got status %q", got)
	}
}

// blockingOperator 在第一次远端改价时阻塞，模拟开启执行进行到一半。
type blockingOperator struct {
	*fakeOperator
	started chan struct{}
	release chan struct{}
	blocked bool
}

func (b *blockingOperator) UpdateAdminGroupMultiplier(session upstream.Session, group upstream.AdminGroupInfo, multiplier float64) error {
	if !b.blocked {
		b.blocked = true
		close(b.started)
		<-b.release
	}
	return b.fakeOperator.UpdateAdminGroupMultiplier(session, group, multiplier)
}

// TestEndDuringStartWaitsAndRestoresAllAppliedGroups 验证开启尚未完成时到来的结束会等待开启落定，
// 从而恢复全部已改价分组，不会遗留停在活动倍率的分组。
func TestEndDuringStartWaitsAndRestoresAllAppliedGroups(t *testing.T) {
	repo := newFakeRepository()
	operator := &blockingOperator{fakeOperator: &fakeOperator{groups: adminGroups()}, started: make(chan struct{}), release: make(chan struct{})}
	svc := &Service{repository: repo, operator: operator, config: Config{}}
	campaignID := "campaign-serial"
	repo.campaigns[campaignID] = &Campaign{
		ID: campaignID, UserID: "user1", AdminAccountID: "account1", Status: StatusScheduled, EndMode: EndManual,
		Selection: Selection{Mode: SelectionManual, Groups: []SelectionGroupRef{
			{GroupName: "vip", CampaignMultiplier: floatPtr(0.6)},
			{GroupName: "svip", CampaignMultiplier: floatPtr(0.9)},
		}},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		svc.startCampaign(context.Background(), campaignID)
	}()
	<-operator.started
	go func() {
		defer wg.Done()
		svc.endCampaign(context.Background(), campaignID, endTriggerManual)
	}()
	time.Sleep(50 * time.Millisecond)
	close(operator.release)
	wg.Wait()

	if got := repo.campaigns[campaignID].Status; got != StatusEnded {
		t.Fatalf("expected end to run after start and finish as ended, got %q", got)
	}
	// fake 分组倍率不随改价变化，恢复阶段会判定为 unchanged；关键是每个已改价分组都被结束流程处理过，
	// 不会因为结束先于开启落定而遗留 restore_status=pending 的分组。
	if len(repo.items[campaignID]) != 2 {
		t.Fatalf("expected both groups to be recorded, got %d", len(repo.items[campaignID]))
	}
	for _, item := range repo.items[campaignID] {
		if item.ApplyStatus != ItemApplied || (item.RestoreStatus != ItemRestored && item.RestoreStatus != ItemUnchanged) {
			t.Fatalf("expected every applied group to be handled by end, got %+v", item)
		}
	}
}

func TestFormatTimeUsesBeijingTime(t *testing.T) {
	value := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := formatTime(&value); got != "2026-01-02 11:04:05" {
		t.Fatalf("expected notification time in Asia/Shanghai, got %q", got)
	}
}
