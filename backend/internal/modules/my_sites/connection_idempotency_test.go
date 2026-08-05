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
