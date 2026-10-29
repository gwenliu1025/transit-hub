package my_sites

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"transithub/backend/internal/modules/upstream"
)

func TestConnectionOperationIdentityGeneratesStableIDWhenClientOmitsOperationID(t *testing.T) {
	req := RealConnectRequest{
		UpstreamSiteID:    "site-1",
		UpstreamGroupID:   "group-1",
		UpstreamGroupName: "vip",
		GroupType:         "openai",
		ChannelType:       1,
		OwnGroupIDs:       []string{"g2", "g1"},
	}
	firstID, firstHash, err := connectionOperationIdentity("user-1", "workspace-1", req)
	if err != nil {
		t.Fatalf("生成幂等标识失败: %v", err)
	}
	secondID, secondHash, err := connectionOperationIdentity("user-1", "workspace-1", req)
	if err != nil {
		t.Fatalf("重复生成幂等标识失败: %v", err)
	}
	if firstID == "" || firstHash == "" || firstID != secondID || firstHash != secondHash {
		t.Fatalf("空 operationId 必须生成稳定标识: first=(%q,%q), second=(%q,%q)", firstID, firstHash, secondID, secondHash)
	}
	if !strings.HasPrefix(firstID, "real-connect-") {
		t.Fatalf("服务端生成的标识必须带场景前缀，得到 %q", firstID)
	}

	req.OwnGroupIDs = []string{"g1", "g2"}
	thirdID, _, err := connectionOperationIdentity("user-1", "workspace-1", req)
	if err != nil {
		t.Fatalf("规范化列表后生成幂等标识失败: %v", err)
	}
	if thirdID != firstID {
		t.Fatalf("同一请求的列表顺序变化不应改变幂等标识: first=%q third=%q", firstID, thirdID)
	}
}

func TestRealConnectConcurrentSameIntentCreatesRemoteResourcesOnce(t *testing.T) {
	var mu sync.Mutex
	var keyCreates, adminCreates int
	remoteStarted := make(chan struct{})
	allowRemote := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"role": "admin"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/groups":
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": "7", "name": "vip", "platform": "openai", "status": "active"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/keys":
			mu.Lock()
			keyCreates++
			first := keyCreates == 1
			mu.Unlock()
			if first {
				close(remoteStarted)
				<-allowRemote
			}
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"id": 11, "key": "sk-created"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts":
			mu.Lock()
			adminCreates++
			mu.Unlock()
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"id": 22}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	session := platformTestSession(upstream.PlatformSub2API, server.URL)
	stateRepo := &testStateRepo{state: &State{UserID: "user-1", AdminAccountID: "admin-1", Session: session, Mappings: []GroupMapping{}}}
	store := newTestConnectionOperationStore()
	lookup := testUpstreamLookup{sites: map[string]*upstream.Site{
		"site-1": {ID: "site-1", UserID: "user-1", AdminAccountID: "admin-1", Name: "source", BaseURL: server.URL, Platform: upstream.PlatformSub2API, Session: &session, Metrics: upstream.Metrics{Groups: []upstream.GroupInfo{{ID: "7", Name: "vip", Platform: stringPointer("openai")}}}},
	}}
	service := NewService(stateRepo, upstream.NewPlatformService(upstream.NewHTTPClient(server.Client())), lookup)
	service.SetAdminAccountResolver(testAdminResolver{currentID: "admin-1"})
	service.connRepository = store
	req := RealConnectRequest{UpstreamSiteID: "site-1", UpstreamGroupID: "7", UpstreamGroupName: "vip", GroupType: "openai", OwnGroupIDs: []string{"7"}}
	firstDone := make(chan error, 1)
	go func() { _, err := service.RealConnect(context.Background(), "user-1", req); firstDone <- err }()
	select {
	case <-remoteStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("首个请求未进入远端创建")
	}
	secondDone := make(chan error, 1)
	go func() { _, err := service.RealConnect(context.Background(), "user-1", req); secondDone <- err }()
	select {
	case err := <-secondDone:
		if err == nil {
			t.Fatal("竞争请求必须在远端副作用前被拒绝或返回已完成结果")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("竞争请求未及时结束")
	}
	close(allowRemote)
	if err := <-firstDone; err != nil {
		t.Fatalf("首个请求失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if keyCreates != 1 || adminCreates != 1 {
		t.Fatalf("同一意图只能创建一次远端资源: keys=%d admins=%d", keyCreates, adminCreates)
	}
}

func TestRealConnectReservationFailureDoesNotCreateRemoteResources(t *testing.T) {
	var remoteCreates int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"role": "admin"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/groups":
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": "7", "name": "vip", "platform": "openai", "status": "active"}}})
		case r.Method == http.MethodPost:
			remoteCreates++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	session := platformTestSession(upstream.PlatformSub2API, server.URL)
	stateRepo := &testStateRepo{state: &State{UserID: "user-1", AdminAccountID: "admin-1", Session: session, Mappings: []GroupMapping{}}}
	store := newTestConnectionOperationStore()
	store.reserveErr = errors.New("reservation unavailable")
	lookup := testUpstreamLookup{sites: map[string]*upstream.Site{"site-1": {ID: "site-1", UserID: "user-1", AdminAccountID: "admin-1", Name: "source", BaseURL: server.URL, Platform: upstream.PlatformSub2API, Session: &session, Metrics: upstream.Metrics{Groups: []upstream.GroupInfo{{ID: "7", Name: "vip", Platform: stringPointer("openai")}}}}}}
	service := NewService(stateRepo, upstream.NewPlatformService(upstream.NewHTTPClient(server.Client())), lookup)
	service.SetAdminAccountResolver(testAdminResolver{currentID: "admin-1"})
	service.connRepository = store
	_, err := service.RealConnect(context.Background(), "user-1", RealConnectRequest{UpstreamSiteID: "site-1", UpstreamGroupID: "7", UpstreamGroupName: "vip", GroupType: "openai", OwnGroupIDs: []string{"7"}})
	if err == nil || !strings.Contains(err.Error(), "reservation unavailable") {
		t.Fatalf("必须返回预留失败，得到 %v", err)
	}
	if remoteCreates != 0 {
		t.Fatalf("原子预留失败时不得调用远端，调用次数=%d", remoteCreates)
	}
}

func TestFailedOperationWithoutCompensationErrorCanBeSafelyRetried(t *testing.T) {
	store := newTestConnectionOperationStore()
	claim, err := store.ReserveRealConnectionOperation(context.Background(), "user-1", "admin-1", "op-1", "hash-1", "owner-1")
	if err != nil || !claim.Owned {
		t.Fatalf("首次预留失败: claim=%+v err=%v", claim, err)
	}
	if err := store.MarkRealConnectionOperationProvisioning(context.Background(), "user-1", "admin-1", "op-1", "owner-1"); err != nil {
		t.Fatalf("进入执行状态失败: %v", err)
	}
	if err := store.FailRealConnectionOperation(context.Background(), "user-1", "admin-1", "op-1", "owner-1", RealConnectionOperationFailed, "remote timeout", "", "11", ""); err != nil {
		t.Fatalf("记录失败状态失败: %v", err)
	}
	retry, err := store.ReserveRealConnectionOperation(context.Background(), "user-1", "admin-1", "op-1", "hash-1", "owner-2")
	if err != nil || !retry.Owned {
		t.Fatalf("已补偿失败意图必须可重试: claim=%+v err=%v", retry, err)
	}
}

func TestCompensationFailureBlocksAutomaticRetry(t *testing.T) {
	store := newTestConnectionOperationStore()
	claim, err := store.ReserveRealConnectionOperation(context.Background(), "user-1", "admin-1", "op-1", "hash-1", "owner-1")
	if err != nil || !claim.Owned {
		t.Fatalf("首次预留失败: claim=%+v err=%v", claim, err)
	}
	if err := store.MarkRealConnectionOperationProvisioning(context.Background(), "user-1", "admin-1", "op-1", "owner-1"); err != nil {
		t.Fatalf("进入执行状态失败: %v", err)
	}
	if err := store.FailRealConnectionOperation(context.Background(), "user-1", "admin-1", "op-1", "owner-1", RealConnectionOperationCompensationFailed, "persist failed", "delete key failed", "11", "22"); err != nil {
		t.Fatalf("记录补偿失败状态失败: %v", err)
	}
	retry, err := store.ReserveRealConnectionOperation(context.Background(), "user-1", "admin-1", "op-1", "hash-1", "owner-2")
	if err != nil {
		t.Fatalf("读取补偿失败状态失败: %v", err)
	}
	if retry.Owned || retry.Status != RealConnectionOperationCompensationFailed {
		t.Fatalf("存在孤儿远端资源时不得自动重试: claim=%+v", retry)
	}
}

func TestOperationIDCannotBeReusedForDifferentIntent(t *testing.T) {
	store := newTestConnectionOperationStore()
	if _, err := store.ReserveRealConnectionOperation(context.Background(), "user-1", "admin-1", "op-1", "hash-1", "owner-1"); err != nil {
		t.Fatalf("首次预留失败: %v", err)
	}
	if _, err := store.ReserveRealConnectionOperation(context.Background(), "user-1", "admin-1", "op-1", "hash-2", "owner-2"); !errors.Is(err, ErrRealConnectionOperationConflict) {
		t.Fatalf("同一 operationId 绑定不同请求必须拒绝，得到 %v", err)
	}
}

// idempotencyFixture 只访问 httptest，统计远端 Key 与账号的创建次数；blockFirstKey 时首个 Key 创建会等待放行。
type idempotencyFixture struct {
	service      *Service
	store        *testConnectionOperationStore
	mu           sync.Mutex
	keyCreates   int
	adminCreates int
	keyStarted   chan struct{}
	allowKey     chan struct{}
}

func newIdempotencyFixture(t *testing.T, blockFirstKey bool) *idempotencyFixture {
	t.Helper()
	f := &idempotencyFixture{store: newTestConnectionOperationStore(), keyStarted: make(chan struct{}), allowKey: make(chan struct{})}
	if !blockFirstKey {
		close(f.allowKey)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"role": "admin"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/groups":
			writeConnectionTestJSON(w, map[string]any{"data": []map[string]any{{"id": "7", "name": "vip", "platform": "openai", "status": "active"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/keys":
			f.mu.Lock()
			f.keyCreates++
			first := f.keyCreates == 1
			f.mu.Unlock()
			if first {
				close(f.keyStarted)
				<-f.allowKey
			}
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"id": 11, "key": "sk-created"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts":
			f.mu.Lock()
			f.adminCreates++
			f.mu.Unlock()
			writeConnectionTestJSON(w, map[string]any{"data": map[string]any{"id": 22}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	session := platformTestSession(upstream.PlatformSub2API, server.URL)
	stateRepo := &testStateRepo{state: &State{UserID: "user-1", AdminAccountID: "admin-1", Session: session, Mappings: []GroupMapping{}}}
	lookup := testUpstreamLookup{sites: map[string]*upstream.Site{
		"site-1": {ID: "site-1", UserID: "user-1", AdminAccountID: "admin-1", Name: "source", BaseURL: server.URL, Platform: upstream.PlatformSub2API, Session: &session, Metrics: upstream.Metrics{Groups: []upstream.GroupInfo{{ID: "7", Name: "vip", Platform: stringPointer("openai")}}}},
	}}
	f.service = NewService(stateRepo, upstream.NewPlatformService(upstream.NewHTTPClient(server.Client())), lookup)
	f.service.SetAdminAccountResolver(testAdminResolver{currentID: "admin-1"})
	f.service.connRepository = f.store
	return f
}

func (f *idempotencyFixture) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keyCreates, f.adminCreates
}

func idempotencyRequest(operationID string) RealConnectRequest {
	noPricing := false
	return RealConnectRequest{UpstreamSiteID: "site-1", UpstreamGroupID: "7", UpstreamGroupName: "vip", GroupType: "openai", OwnGroupIDs: []string{"7"}, AddToPricingMapping: &noPricing, OperationID: operationID}
}

func TestRealConnectRetryWithSameOperationIDReturnsCreatedConnection(t *testing.T) {
	f := newIdempotencyFixture(t, false)
	first, err := f.service.RealConnect(context.Background(), "user-1", idempotencyRequest("op-timeout"))
	if err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	// 客户端超时后用同一 operationId 重试：应返回已创建的连接，而不是被重复检查拒绝为 409。
	retry, err := f.service.RealConnect(context.Background(), "user-1", idempotencyRequest("op-timeout"))
	if err != nil {
		t.Fatalf("同一 operationId 重试必须返回已创建连接: %v", err)
	}
	keys, admins := f.counts()
	if retry.Connection.ID != first.Connection.ID || keys != 1 || admins != 1 {
		t.Fatalf("重试不得重复创建: first=%s retry=%s keys=%d admins=%d", first.Connection.ID, retry.Connection.ID, keys, admins)
	}
	// 不同 operationId 针对同一目标仍按重复连接拒绝。
	if _, err := f.service.RealConnect(context.Background(), "user-1", idempotencyRequest("op-other")); err == nil || err.Error() != ErrorConnectionExists {
		t.Fatalf("同一目标的新意图必须返回已存在: %v", err)
	}
}

func TestRealConnectConcurrentDifferentOperationsCreateRemoteResourcesOnce(t *testing.T) {
	f := newIdempotencyFixture(t, true)
	firstDone := make(chan error, 1)
	go func() {
		_, err := f.service.RealConnect(context.Background(), "user-1", idempotencyRequest("op-a"))
		firstDone <- err
	}()
	select {
	case <-f.keyStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("首个请求未进入远端创建")
	}
	// 不同 operationId 的并发请求不能都通过重复检查。
	if _, err := f.service.RealConnect(context.Background(), "user-1", idempotencyRequest("op-b")); err == nil || err.Error() != ErrorConnectionInProgress {
		t.Fatalf("并发的同目标请求必须立即返回进行中: %v", err)
	}
	close(f.allowKey)
	if err := <-firstDone; err != nil {
		t.Fatalf("首个请求失败: %v", err)
	}
	if _, err := f.service.RealConnect(context.Background(), "user-1", idempotencyRequest("op-b")); err == nil || err.Error() != ErrorConnectionExists {
		t.Fatalf("首个请求完成后同目标新意图必须返回已存在: %v", err)
	}
	if keys, admins := f.counts(); keys != 1 || admins != 1 {
		t.Fatalf("同一目标只能创建一次远端资源: keys=%d admins=%d", keys, admins)
	}
}

func TestRealConnectWithoutOperationIDCanReconnectAfterUnlink(t *testing.T) {
	f := newIdempotencyFixture(t, false)
	first, err := f.service.RealConnect(context.Background(), "user-1", idempotencyRequest(""))
	if err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	keepPricing := false
	if err := f.service.RealDisconnect(context.Background(), "user-1", RealDisconnectRequest{ConnectionID: first.Connection.ID, Mode: "unlink", RemovePricingMapping: &keepPricing}); err != nil {
		t.Fatalf("取消关联失败: %v", err)
	}
	// 相同参数派生出相同 operationId；旧意图的连接已删除，必须允许重新创建而不是 500。
	second, err := f.service.RealConnect(context.Background(), "user-1", idempotencyRequest(""))
	if err != nil {
		t.Fatalf("取消关联后以相同参数重新对接失败: %v", err)
	}
	if keys, admins := f.counts(); second.Connection.ID == first.Connection.ID || keys != 2 || admins != 2 {
		t.Fatalf("重新对接应创建新连接: first=%s second=%s keys=%d admins=%d", first.Connection.ID, second.Connection.ID, keys, admins)
	}
}

type testConnectionOperationStore struct {
	testConnRepo
	mu         sync.Mutex
	operations map[string]*RealConnectionOperation
	reserveErr error
}

func newTestConnectionOperationStore() *testConnectionOperationStore {
	return &testConnectionOperationStore{operations: make(map[string]*RealConnectionOperation)}
}

func (s *testConnectionOperationStore) ReserveRealConnectionOperation(_ context.Context, userID, adminAccountID, operationID, requestHash, ownerToken string) (RealConnectionOperationClaim, error) {
	if s.reserveErr != nil {
		return RealConnectionOperationClaim{}, s.reserveErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := userID + "\x00" + adminAccountID + "\x00" + operationID
	if op, ok := s.operations[key]; ok {
		if op.RequestHash != requestHash {
			return RealConnectionOperationClaim{}, ErrRealConnectionOperationConflict
		}
		if op.Status == RealConnectionOperationSucceeded && s.connection != nil {
			conn := *s.connection
			return RealConnectionOperationClaim{Status: op.Status, Connection: &conn}, nil
		}
		if op.Status == RealConnectionOperationSucceeded {
			// 与仓储一致：已完成意图的连接被删除后允许重新占用。
			op.OwnerToken, op.Status = ownerToken, RealConnectionOperationReserved
			op.UpstreamKeyID, op.AdminResourceID = "", ""
			return RealConnectionOperationClaim{Owned: true, Status: op.Status}, nil
		}
		if op.Status == RealConnectionOperationFailed && op.CompensationError == "" {
			op.OwnerToken, op.Status = ownerToken, RealConnectionOperationReserved
			op.UpstreamKeyID, op.AdminResourceID = "", ""
			return RealConnectionOperationClaim{Owned: true, Status: op.Status}, nil
		}
		return RealConnectionOperationClaim{Status: op.Status}, nil
	}
	s.operations[key] = &RealConnectionOperation{UserID: userID, AdminAccountID: adminAccountID, OperationID: operationID, RequestHash: requestHash, OwnerToken: ownerToken, Status: RealConnectionOperationReserved}
	return RealConnectionOperationClaim{Owned: true, Status: RealConnectionOperationReserved}, nil
}

func (s *testConnectionOperationStore) MarkRealConnectionOperationProvisioning(_ context.Context, userID, adminAccountID, operationID, ownerToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operation(userID, adminAccountID, operationID)
	if op == nil || op.OwnerToken != ownerToken {
		return ErrRealConnectionOperationNotOwner
	}
	op.Status = RealConnectionOperationProvisioning
	return nil
}

func (s *testConnectionOperationStore) RecordRealConnectionOperationRemote(_ context.Context, userID, adminAccountID, operationID, ownerToken, upstreamKeyID, adminResourceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operation(userID, adminAccountID, operationID)
	if op == nil || op.OwnerToken != ownerToken {
		return ErrRealConnectionOperationNotOwner
	}
	op.UpstreamKeyID, op.AdminResourceID = upstreamKeyID, adminResourceID
	return nil
}

func (s *testConnectionOperationStore) CompleteRealConnectionOperation(_ context.Context, userID, adminAccountID, operationID, ownerToken string, conn RealConnection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operation(userID, adminAccountID, operationID)
	if op == nil || op.OwnerToken != ownerToken {
		return ErrRealConnectionOperationNotOwner
	}
	op.Status = RealConnectionOperationSucceeded
	s.connection = &conn
	return nil
}

func (s *testConnectionOperationStore) FailRealConnectionOperation(_ context.Context, userID, adminAccountID, operationID, ownerToken, status, lastError, compensationError, upstreamKeyID, adminResourceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operation(userID, adminAccountID, operationID)
	if op == nil || op.OwnerToken != ownerToken {
		return ErrRealConnectionOperationNotOwner
	}
	op.Status, op.LastError, op.CompensationError = status, lastError, compensationError
	op.UpstreamKeyID, op.AdminResourceID = upstreamKeyID, adminResourceID
	return nil
}

func (s *testConnectionOperationStore) operation(userID, adminAccountID, operationID string) *RealConnectionOperation {
	return s.operations[userID+"\x00"+adminAccountID+"\x00"+operationID]
}
