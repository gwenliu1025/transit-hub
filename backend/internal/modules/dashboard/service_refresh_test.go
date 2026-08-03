package dashboard

import (
	"context"
	"errors"
	"testing"
	"time"

	"transithub/backend/internal/modules/upstream"
)

// fakeMySiteSync 记录 SyncAdminSession 的调用参数，供测试断言刷新成功后是否同步到 my_site_states。
type fakeMySiteSync struct {
	called          bool
	userID          string
	adminAccountID  string
	session         upstream.Session
	storedSession   *upstream.Session
	requiredSession *upstream.Session
	identity        string
}

func (f *fakeMySiteSync) SyncAdminSession(ctx context.Context, userID string, adminAccountID string, session upstream.Session, identity string) error {
	f.called = true
	f.userID = userID
	f.adminAccountID = adminAccountID
	f.session = session
	f.identity = identity
	return nil
}

func (f *fakeMySiteSync) RequireSession(ctx context.Context, userID string, adminAccountID string) (upstream.Session, error) {
	if f.requiredSession != nil {
		return *f.requiredSession, nil
	}
	return f.session, nil
}

func (f *fakeMySiteSync) StoredSession(ctx context.Context, userID string, adminAccountID string) (upstream.Session, bool, error) {
	if f.storedSession != nil {
		return *f.storedSession, true, nil
	}
	return f.session, f.session.IsAuthenticated(), nil
}

type activeSessionStore struct {
	*fakeSessionStore
	refs []ActiveSessionRef
}

func (s *activeSessionStore) ActiveSessions(ctx context.Context) ([]ActiveSessionRef, error) {
	return s.refs, nil
}

func newRefreshTestService(store *fakeSessionStore, platform *fakePlatformClient, mySync *fakeMySiteSync) *Service {
	service := NewService(store, platform)
	service.SetAdminAccountService(&fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}})
	if mySync != nil {
		service.SetMySiteSync(mySync)
	}
	return service
}

// TestRefreshAdminSession_Success 覆盖：refresh 成功且 VerifyAdmin 成功，返回 authenticated=true，
// 且写回 store、并调用 mySiteSync.SyncAdminSession。
func TestRefreshAdminSession_Success(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{
		Platform: PlatformSub2API,
		Identity: "admin@example.com",
		Session:  authenticatedSession(),
	})
	refreshed := upstream.Session{Platform: upstream.PlatformSub2API, BaseURL: "https://example.com", AccessToken: "new-token"}
	platform := &fakePlatformClient{refreshSessionResult: &refreshed}
	mySync := &fakeMySiteSync{}
	service := newRefreshTestService(store, platform, mySync)

	resp, err := service.RefreshAdminSession(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !resp.Authenticated {
		t.Fatal("expected authenticated=true")
	}

	saved, _ := store.Get(context.Background(), "user-1", "account-1")
	if saved == nil || saved.Session.AccessToken != "new-token" {
		t.Fatalf("expected store to be updated with refreshed session, got %+v", saved)
	}

	if !mySync.called {
		t.Fatal("expected mySiteSync.SyncAdminSession to be called")
	}
	if mySync.session.AccessToken != "new-token" {
		t.Fatalf("expected mySiteSync to receive refreshed session, got %+v", mySync.session)
	}
}

// TestRefreshAdminSession_RefreshFailed 覆盖：RefreshSession 失败返回 ErrorAdminOnly，不写回 store。
func TestRefreshAdminSession_RefreshFailed(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{
		Platform: PlatformSub2API,
		Session:  authenticatedSession(),
	})
	platform := &fakePlatformClient{refreshSessionErr: errors.New("refresh token expired")}
	mySync := &fakeMySiteSync{}
	service := newRefreshTestService(store, platform, mySync)

	_, err := service.RefreshAdminSession(context.Background(), "user-1")
	assertAdminOnlyError(t, err)
	if mySync.called {
		t.Fatal("expected mySiteSync not to be called on refresh failure")
	}
}

// TestRefreshAdminSession_VerifyFailed 覆盖：refresh 成功但 VerifyAdmin 失败，返回 ErrorAdminOnly，不写回 store。
func TestRefreshAdminSession_VerifyFailed(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{
		Platform: PlatformSub2API,
		Session:  authenticatedSession(),
	})
	platform := &fakePlatformClient{verifyAdminErr: errors.New("not admin")}
	mySync := &fakeMySiteSync{}
	service := newRefreshTestService(store, platform, mySync)

	_, err := service.RefreshAdminSession(context.Background(), "user-1")
	assertAdminOnlyError(t, err)

	saved, _ := store.Get(context.Background(), "user-1", "account-1")
	if saved == nil || saved.Session.AccessToken != authenticatedSession().AccessToken {
		t.Fatalf("expected old session to remain untouched, got %+v", saved)
	}
	if mySync.called {
		t.Fatal("expected mySiteSync not to be called on verify failure")
	}
}

// TestRefreshAdminSession_NoSession 覆盖：当前无 admin session 时返回明确错误。
func TestRefreshAdminSession_NoSession(t *testing.T) {
	store := newFakeSessionStore()
	platform := &fakePlatformClient{}
	service := newRefreshTestService(store, platform, nil)

	_, err := service.RefreshAdminSession(context.Background(), "user-1")
	assertAdminOnlyError(t, err)
}

func TestStatusReconcilesRedisWithAuthoritativeSession(t *testing.T) {
	oldExpiry := int64(1000)
	newExpiry := int64(2000)
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{
		Platform: PlatformSub2API,
		Identity: "admin@example.com",
		Session: upstream.Session{
			Platform: PlatformSub2API, BaseURL: "https://example.com",
			AccessToken: "old-token", RefreshToken: "old-refresh", ExpiresAt: &oldExpiry,
		},
	})
	mySync := &fakeMySiteSync{session: upstream.Session{
		Platform: PlatformSub2API, BaseURL: "https://example.com",
		AccessToken: "new-token", RefreshToken: "new-refresh", ExpiresAt: &newExpiry,
	}}
	service := newRefreshTestService(store, &fakePlatformClient{}, mySync)

	status, err := service.Status(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if !status.Authenticated {
		t.Fatal("expected authenticated status")
	}
	saved, _ := store.Get(context.Background(), "user-1", "account-1")
	if saved == nil || saved.Session.AccessToken != "new-token" || saved.Session.RefreshToken != "new-refresh" {
		t.Fatalf("expected Redis session to reconcile to authoritative session, got %+v", saved)
	}
}

func TestStatusSyncsVerifiedSessionToMySiteState(t *testing.T) {
	store := newFakeSessionStore()
	record := AdminSession{
		Platform: PlatformSub2API,
		Identity: "admin@example.com",
		Session:  authenticatedSession(),
	}
	store.set("user-1", "account-1", record)
	mySync := &fakeMySiteSync{session: record.Session}
	service := newRefreshTestService(store, &fakePlatformClient{}, mySync)

	status, err := service.Status(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if !status.Authenticated {
		t.Fatal("expected authenticated status")
	}
	if !mySync.called {
		t.Fatal("expected verified session to be synchronized")
	}
	if mySync.session.AccessToken != record.Session.AccessToken {
		t.Fatalf("expected synchronized session %q, got %q", record.Session.AccessToken, mySync.session.AccessToken)
	}
}

func TestRefreshDueSessionsSyncsRefreshedSessionAfterSave(t *testing.T) {
	store := &activeSessionStore{
		fakeSessionStore: newFakeSessionStore(),
		refs:             []ActiveSessionRef{{UserID: "user-1", AdminAccountID: "account-1"}},
	}
	oldExpiry := time.Now().Add(-time.Minute).UnixMilli()
	newExpiry := time.Now().Add(time.Hour).UnixMilli()
	oldSession := upstream.Session{
		Platform: upstream.PlatformSub2API, BaseURL: "https://example.com",
		AccessToken: "old-token", RefreshToken: "old-refresh", ExpiresAt: &oldExpiry,
	}
	newSession := upstream.Session{
		Platform: upstream.PlatformSub2API, BaseURL: "https://example.com",
		AccessToken: "new-token", RefreshToken: "new-refresh", ExpiresAt: &newExpiry,
	}
	store.set("user-1", "account-1", AdminSession{
		Platform: PlatformSub2API,
		Identity: "admin@example.com",
		Session:  oldSession,
	})
	mySync := &fakeMySiteSync{storedSession: &oldSession, requiredSession: &newSession}
	service := NewService(store, &fakePlatformClient{})
	service.SetMySiteSync(mySync)

	service.refreshDueSessions(context.Background())

	saved, err := store.Get(context.Background(), "user-1", "account-1")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if saved == nil || saved.Session.AccessToken != "new-token" {
		t.Fatalf("expected refreshed session to be saved, got %+v", saved)
	}
	if !mySync.called {
		t.Fatal("expected saved refreshed session to be synchronized")
	}
	if mySync.session.AccessToken != "new-token" {
		t.Fatalf("expected synchronized refreshed token, got %q", mySync.session.AccessToken)
	}
}

func assertAdminOnlyError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var reqErr requestError
	if !errors.As(err, &reqErr) || reqErr.Error() != ErrorAdminOnly {
		t.Fatalf("expected ErrorAdminOnly, got %v", err)
	}
}
