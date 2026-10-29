package my_sites

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"transithub/backend/internal/modules/upstream"
)

// connectionContext contains the two independently authenticated sides of a
// connection. Their platforms may differ and must never be inferred from one another.
type connectionContext struct {
	adminAccountID  string
	state           *State
	upstreamSite    *upstream.Site
	upstreamSession upstream.Session
	groupType       string
	// cachedGroupType 是上游站点缓存中该分组的平台（New API 为厂商列表），不读取快照里可编辑的类型。
	cachedGroupType string
	groupName       string
	multiplierLabel string
}

// ownGroupSelection 是从管理端实时读取、去重并保持请求顺序的自有分组。
type ownGroupSelection struct {
	ids       []string
	names     []string
	platforms []string
}

func addToPricingMapping(value *bool) bool {
	return value == nil || *value
}

func normalizeOperationID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		return "", requestError(ErrorRequest)
	}
	return value, nil
}

// connectionOperationIdentity 为真实对接请求生成可重试的幂等身份。
// 客户端未提供 operationId 时，使用规范化请求内容派生稳定标识；列表顺序不影响意图。
func connectionOperationIdentity(userID, adminAccountID string, req RealConnectRequest) (string, string, error) {
	return operationIdentity("real-connect", userID, adminAccountID, req.OperationID, struct {
		UpstreamSiteID    string   `json:"upstreamSiteId"`
		UpstreamGroupID   string   `json:"upstreamGroupId"`
		UpstreamGroupName string   `json:"upstreamGroupName"`
		GroupType         string   `json:"groupType"`
		ChannelType       int      `json:"channelType"`
		OwnGroupIDs       []string `json:"ownGroupIds"`
		AddToPricing      bool     `json:"addToPricingMapping"`
	}{
		UpstreamSiteID: strings.TrimSpace(req.UpstreamSiteID), UpstreamGroupID: strings.TrimSpace(req.UpstreamGroupID),
		UpstreamGroupName: strings.TrimSpace(req.UpstreamGroupName), GroupType: strings.TrimSpace(req.GroupType),
		ChannelType: req.ChannelType, OwnGroupIDs: sortedTrimmed(req.OwnGroupIDs), AddToPricing: addToPricingMapping(req.AddToPricingMapping),
	})
}

func operationIdentity(kind, userID, adminAccountID, supplied string, payload any) (string, string, error) {
	encoded, err := json.Marshal(struct {
		Kind           string `json:"kind"`
		UserID         string `json:"userId"`
		AdminAccountID string `json:"adminAccountId"`
		Payload        any    `json:"payload"`
	}{kind, strings.TrimSpace(userID), strings.TrimSpace(adminAccountID), payload})
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(encoded)
	hash := hex.EncodeToString(sum[:])
	operationID, err := normalizeOperationID(strings.TrimSpace(supplied))
	if err != nil {
		return "", "", err
	}
	if operationID == "" {
		operationID = kind + "-" + hash
	}
	return operationID, hash, nil
}

func sortedTrimmed(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func (s *Service) prepareConnectionContext(ctx context.Context, userID, siteID, groupID, groupName, requestedType string, requireAdminResourceType bool) (connectionContext, error) {
	adminAccountID, err := s.currentAdminAccountID(ctx, userID)
	if err != nil {
		return connectionContext{}, err
	}
	state, err := s.authenticatedState(ctx, userID, adminAccountID)
	if err != nil {
		return connectionContext{}, err
	}
	upstreamSite, err := s.upstreamLookup.GetSite(ctx, strings.TrimSpace(siteID))
	if err != nil || upstreamSite == nil || upstreamSite.Session == nil || upstreamSite.UserID != userID || upstreamSite.AdminAccountID != adminAccountID {
		return connectionContext{}, requestError(ErrorRequest)
	}

	cachedGroupType, multiplierLabel := resolveGroupInfo(upstreamSite.Metrics.Groups, strings.TrimSpace(groupID))
	groupType := cachedGroupType
	if requireAdminResourceType && state.Session.Platform == upstream.PlatformSub2API {
		// 账号协议需要结合所选自有分组的平台才能最终确定，这里只提前拒绝非法显式选择。
		if err := validateRequestedGroupType(requestedType); err != nil {
			return connectionContext{}, err
		}
		groupType = ""
	} else if groupType == "" {
		groupType = strings.ToLower(strings.TrimSpace(requestedType))
	}
	resolvedName := strings.TrimSpace(groupName)
	if resolvedName == "" {
		resolvedName = strings.TrimSpace(groupID)
	}
	if resolvedName == "" {
		return connectionContext{}, requestError(ErrorRequest)
	}

	return connectionContext{
		adminAccountID:  adminAccountID,
		state:           state,
		upstreamSite:    upstreamSite,
		upstreamSession: *upstreamSite.Session,
		groupType:       groupType,
		cachedGroupType: cachedGroupType,
		groupName:       resolvedName,
		multiplierLabel: multiplierLabel,
	}, nil
}

func (s *Service) idempotentConnection(ctx context.Context, userID, adminAccountID, operationID string) (*RealConnection, error) {
	if operationID == "" || s.connRepository == nil {
		return nil, nil
	}
	repo, ok := s.connRepository.(IdempotentRealConnectionRepository)
	if !ok {
		return nil, nil
	}
	return repo.GetRealConnectionByOperationID(ctx, userID, adminAccountID, operationID)
}

func (s *Service) rejectDuplicateTarget(ctx context.Context, userID, adminAccountID, siteID, groupID, groupName string) error {
	if s.connRepository == nil {
		return nil
	}
	connections, err := s.connRepository.ListRealConnections(ctx, userID, adminAccountID)
	if err != nil {
		return err
	}
	for _, conn := range connections {
		if conn.UpstreamSiteID != siteID || conn.Status != "" && conn.Status != ConnectionStatusActive {
			continue
		}
		if conn.UpstreamGroupID == groupID || (conn.UpstreamGroupID == "" && groupID == "" && conn.UpstreamGroupName == groupName) {
			return requestError(ErrorConnectionExists)
		}
	}
	return nil
}

func (s *Service) resolveAdminGroups(ctx context.Context, state *State, requestedIDs []string) (ownGroupSelection, error) {
	if state == nil || len(requestedIDs) == 0 {
		return ownGroupSelection{}, requestError(ErrorRequest)
	}
	groups, err := s.platformService.FetchAdminAllGroups(state.Session)
	if err != nil {
		return ownGroupSelection{}, err
	}
	byID := make(map[string]upstream.AdminGroupInfo, len(groups))
	for _, group := range groups {
		byID[group.ID] = group
	}
	selection := ownGroupSelection{
		ids:       make([]string, 0, len(requestedIDs)),
		names:     make([]string, 0, len(requestedIDs)),
		platforms: make([]string, 0, len(requestedIDs)),
	}
	seen := make(map[string]struct{}, len(requestedIDs))
	for _, requestedID := range requestedIDs {
		requestedID = strings.TrimSpace(requestedID)
		group, ok := byID[requestedID]
		if !ok {
			return ownGroupSelection{}, requestError(ErrorRequest)
		}
		if _, exists := seen[group.ID]; exists {
			continue
		}
		seen[group.ID] = struct{}{}
		selection.ids = append(selection.ids, group.ID)
		selection.names = append(selection.names, group.Name)
		selection.platforms = append(selection.platforms, normalizeProtocolValue(group.Platform))
	}
	if len(selection.ids) == 0 {
		return ownGroupSelection{}, requestError(ErrorRequest)
	}
	return selection, nil
}

func (s *Service) realConnectManaged(ctx context.Context, userID string, req RealConnectRequest) (RealConnectResponse, error) {
	if strings.TrimSpace(req.UpstreamSiteID) == "" || strings.TrimSpace(req.UpstreamGroupID) == "" || len(req.OwnGroupIDs) == 0 {
		return RealConnectResponse{}, requestError(ErrorRequest)
	}
	connectionCtx, err := s.prepareConnectionContext(ctx, userID, req.UpstreamSiteID, req.UpstreamGroupID, req.UpstreamGroupName, req.GroupType, true)
	if err != nil {
		return RealConnectResponse{}, err
	}
	adminIsNewAPI := connectionCtx.state.Session.Platform == upstream.PlatformNewAPI
	if adminIsNewAPI && req.ChannelType <= 0 {
		return RealConnectResponse{}, requestError(ErrorChannelTypeRequired)
	}
	operationID, requestHash, err := connectionOperationIdentity(userID, connectionCtx.adminAccountID, req)
	if err != nil {
		return RealConnectResponse{}, err
	}
	// 任何远端创建之前读取自有分组；Sub2API 账号协议必须与每个所选分组的平台一致。
	ownGroups, err := s.resolveAdminGroups(ctx, connectionCtx.state, req.OwnGroupIDs)
	if err != nil {
		return RealConnectResponse{}, err
	}
	if !adminIsNewAPI {
		connectionCtx.groupType, err = resolveManagedAccountGroupType(connectionCtx.cachedGroupType, req.GroupType, connectionCtx.upstreamSession.Platform == upstream.PlatformNewAPI, ownGroups.platforms)
		if err != nil {
			return RealConnectResponse{}, err
		}
	}
	ownGroupIDs, ownGroupNames := ownGroups.ids, ownGroups.names

	// 同一上游分组的创建与关联串行化，避免不同 operationId 的并发请求都通过重复检查。
	release, locked := s.targetLocks.tryLock(connectionTargetLockKey(userID, connectionCtx.adminAccountID, req.UpstreamSiteID, req.UpstreamGroupID))
	if !locked {
		return RealConnectResponse{}, requestError(ErrorConnectionInProgress)
	}
	defer release()

	operationStore, hasOperationStore := s.connRepository.(RealConnectionOperationRepository)
	ownerToken, err := randomConnID()
	if err != nil {
		return RealConnectResponse{}, err
	}
	// 幂等结果优先于重复检查：客户端超时后用同一 operationId 重试应拿回已创建的连接而不是 409。
	if hasOperationStore {
		claim, claimErr := operationStore.ReserveRealConnectionOperation(ctx, userID, connectionCtx.adminAccountID, operationID, requestHash, ownerToken)
		if claimErr != nil {
			if errors.Is(claimErr, ErrRealConnectionOperationConflict) {
				return RealConnectResponse{}, requestError(ErrorRequest)
			}
			return RealConnectResponse{}, claimErr
		}
		if claim.Connection != nil {
			return RealConnectResponse{Connection: publicRealConnection(*claim.Connection)}, nil
		}
		if !claim.Owned {
			if claim.Status == RealConnectionOperationReserved || claim.Status == RealConnectionOperationProvisioning {
				return RealConnectResponse{}, requestError(ErrorConnectionInProgress)
			}
			return RealConnectResponse{}, requestError(ErrorRequest)
		}
	} else if existing, err := s.idempotentConnection(ctx, userID, connectionCtx.adminAccountID, operationID); err != nil {
		return RealConnectResponse{}, err
	} else if existing != nil {
		return RealConnectResponse{Connection: publicRealConnection(*existing)}, nil
	}
	if err := s.rejectDuplicateTarget(ctx, userID, connectionCtx.adminAccountID, req.UpstreamSiteID, req.UpstreamGroupID, connectionCtx.groupName); err != nil {
		if hasOperationStore {
			// 未产生远端资源，记为可重试的失败；对方取消对接后同一 operationId 可重新占用。
			_ = operationStore.FailRealConnectionOperation(ctx, userID, connectionCtx.adminAccountID, operationID, ownerToken, RealConnectionOperationFailed, err.Error(), "", "", "")
		}
		return RealConnectResponse{}, err
	}
	if hasOperationStore {
		if err := operationStore.MarkRealConnectionOperationProvisioning(ctx, userID, connectionCtx.adminAccountID, operationID, ownerToken); err != nil {
			return RealConnectResponse{}, err
		}
	}

	connID, err := randomConnID()
	if err != nil {
		return RealConnectResponse{}, err
	}
	resourceName := fmt.Sprintf("%s-%s-%s", randomKeyPrefix(), connectionCtx.upstreamSite.Name, connectionCtx.groupName)
	keyID, key, err := s.createUpstreamCredential(connectionCtx.upstreamSession, resourceName, req.UpstreamGroupID)
	if err != nil {
		if hasOperationStore {
			_ = operationStore.FailRealConnectionOperation(ctx, userID, connectionCtx.adminAccountID, operationID, ownerToken, RealConnectionOperationFailed, err.Error(), "", keyID, "")
		}
		return RealConnectResponse{}, err
	}
	rollbackKeyErr := func() error {
		rollbackErr := s.deleteUpstreamCredential(connectionCtx.upstreamSession, keyID)
		if rollbackErr != nil {
			log.Printf("[real-connect] compensate upstream credential failed platform=%s id=%s err=%v", connectionCtx.upstreamSession.Platform, keyID, rollbackErr)
		}
		return rollbackErr
	}
	if hasOperationStore {
		if err := operationStore.RecordRealConnectionOperationRemote(ctx, userID, connectionCtx.adminAccountID, operationID, ownerToken, keyID, ""); err != nil {
			compensationErr := rollbackKeyErr()
			status, compText := RealConnectionOperationFailed, ""
			if compensationErr != nil {
				status, compText = RealConnectionOperationCompensationFailed, compensationErr.Error()
			}
			_ = operationStore.FailRealConnectionOperation(ctx, userID, connectionCtx.adminAccountID, operationID, ownerToken, status, err.Error(), compText, keyID, "")
			return RealConnectResponse{}, err
		}
	}

	adminResourceID, adminResourceName, err := s.createAdminResource(connectionCtx, req.ChannelType, ownGroupIDs, key)
	if err != nil {
		compensationErr := rollbackKeyErr()
		if hasOperationStore {
			status, compText := RealConnectionOperationFailed, ""
			if compensationErr != nil {
				status, compText = RealConnectionOperationCompensationFailed, compensationErr.Error()
			}
			_ = operationStore.FailRealConnectionOperation(ctx, userID, connectionCtx.adminAccountID, operationID, ownerToken, status, err.Error(), compText, keyID, adminResourceID)
		}
		return RealConnectResponse{}, err
	}
	rollbackAdminErr := func() error {
		rollbackErr := s.deleteAdminResource(connectionCtx.state.Session, adminResourceID)
		if rollbackErr != nil {
			log.Printf("[real-connect] compensate admin resource failed platform=%s id=%s err=%v", connectionCtx.state.Session.Platform, adminResourceID, rollbackErr)
		}
		return rollbackErr
	}
	if hasOperationStore {
		if err := operationStore.RecordRealConnectionOperationRemote(ctx, userID, connectionCtx.adminAccountID, operationID, ownerToken, keyID, adminResourceID); err != nil {
			adminRollbackErr := rollbackAdminErr()
			keyRollbackErr := rollbackKeyErr()
			status, compText := RealConnectionOperationFailed, ""
			if adminRollbackErr != nil || keyRollbackErr != nil {
				status = RealConnectionOperationCompensationFailed
				if adminRollbackErr != nil {
					compText = adminRollbackErr.Error()
				}
				if keyRollbackErr != nil {
					if compText != "" {
						compText += "; "
					}
					compText += keyRollbackErr.Error()
				}
			}
			_ = operationStore.FailRealConnectionOperation(ctx, userID, connectionCtx.adminAccountID, operationID, ownerToken, status, err.Error(), compText, keyID, adminResourceID)
			return RealConnectResponse{}, err
		}
	}

	conn := RealConnection{
		ID:                      connID,
		UserID:                  userID,
		WorkspaceAdminAccountID: connectionCtx.adminAccountID,
		UpstreamSiteID:          req.UpstreamSiteID,
		UpstreamGroupID:         req.UpstreamGroupID,
		UpstreamGroupName:       connectionCtx.groupName,
		UpstreamKeyID:           keyID,
		UpstreamKey:             key,
		AdminAccountID:          adminResourceID,
		AdminAccountName:        adminResourceName,
		OwnGroupIDs:             ownGroupIDs,
		OwnGroupNames:           ownGroupNames,
		GroupType:               connectionCtx.groupType,
		ProvisioningMode:        ProvisioningModeManaged,
		Status:                  ConnectionStatusActive,
		UpstreamPlatform:        string(connectionCtx.upstreamSession.Platform),
		AdminPlatform:           string(connectionCtx.state.Session.Platform),
		PricingMappingEnabled:   addToPricingMapping(req.AddToPricingMapping),
		OperationID:             operationID,
		CanDeleteRemote:         true,
		CreatedAt:               time.Now().Format(time.RFC3339),
	}
	var persistErr error
	if hasOperationStore {
		persistErr = operationStore.CompleteRealConnectionOperation(ctx, userID, connectionCtx.adminAccountID, operationID, ownerToken, conn)
	} else {
		persistErr = s.persistConnection(ctx, conn)
	}
	if persistErr != nil {
		adminRollbackErr := rollbackAdminErr()
		keyRollbackErr := rollbackKeyErr()
		if hasOperationStore {
			status, compText := RealConnectionOperationFailed, ""
			var compensationErr error
			if adminRollbackErr != nil {
				compensationErr = adminRollbackErr
			}
			if keyRollbackErr != nil {
				if compensationErr == nil {
					compensationErr = keyRollbackErr
				} else {
					compensationErr = fmt.Errorf("%v; %w", compensationErr, keyRollbackErr)
				}
			}
			if compensationErr != nil {
				status, compText = RealConnectionOperationCompensationFailed, compensationErr.Error()
			}
			_ = operationStore.FailRealConnectionOperation(ctx, userID, connectionCtx.adminAccountID, operationID, ownerToken, status, persistErr.Error(), compText, keyID, adminResourceID)
		}
		return RealConnectResponse{}, persistErr
	}
	return RealConnectResponse{Connection: publicRealConnection(conn)}, nil
}

func (s *Service) createUpstreamCredential(session upstream.Session, name, groupID string) (string, string, error) {
	switch session.Platform {
	case upstream.PlatformNewAPI:
		return s.platformService.CreateNewAPIToken(session, name, groupID)
	case upstream.PlatformSub2API:
		numericGroupID, err := strconv.Atoi(groupID)
		if err != nil {
			return "", "", requestError(ErrorRequest)
		}
		return s.platformService.CreateSub2APIKey(session, name, numericGroupID)
	default:
		return "", "", requestError(ErrorRequest)
	}
}

func (s *Service) deleteUpstreamCredential(session upstream.Session, keyID string) error {
	if strings.TrimSpace(keyID) == "" {
		return nil
	}
	if session.Platform == upstream.PlatformNewAPI {
		return s.platformService.DeleteNewAPIToken(session, keyID)
	}
	return s.platformService.DeleteSub2APIKey(session, keyID)
}

func (s *Service) createAdminResource(connectionCtx connectionContext, requestedChannelType int, ownGroupIDs []string, key string) (string, string, error) {
	if connectionCtx.state.Session.Platform == upstream.PlatformNewAPI {
		// 渠道类型由调用方显式给出；缺失时不再静默回退为 OpenAI。
		channelType := requestedChannelType
		if channelType <= 0 {
			return "", "", requestError(ErrorChannelTypeRequired)
		}
		name := fmt.Sprintf("%s-【%s】-%s", newAPIChannelTypeName(channelType), connectionCtx.upstreamSite.Name, connectionCtx.groupName)
		id, err := s.platformService.CreateNewAPIChannel(connectionCtx.state.Session, name, connectionCtx.upstreamSite.BaseURL, key, channelType, ownGroupIDs)
		return id, name, err
	}

	numericGroupIDs, err := stringsToInts(ownGroupIDs)
	if err != nil {
		return "", "", requestError(ErrorRequest)
	}
	rateLabel := connectionCtx.multiplierLabel
	if rateLabel == "" {
		rateLabel = connectionCtx.groupName
	}
	name := fmt.Sprintf("%s-【%s】-%s", groupTypePrefix(connectionCtx.groupType), connectionCtx.upstreamSite.Name, rateLabel)
	payload := buildAccountPayload(connectionCtx.groupType, connectionCtx.upstreamSite.BaseURL, key, numericGroupIDs, name)
	id, err := s.platformService.CreateSub2APIAdminAccount(connectionCtx.state.Session, payload)
	return id, name, err
}

func (s *Service) deleteAdminResource(session upstream.Session, resourceID string) error {
	if strings.TrimSpace(resourceID) == "" {
		return nil
	}
	if session.Platform == upstream.PlatformNewAPI {
		return s.platformService.DeleteNewAPIChannel(session, resourceID)
	}
	return s.platformService.DeleteSub2APIAdminAccount(session, resourceID)
}

func (s *Service) persistConnection(ctx context.Context, conn RealConnection) error {
	if s.connRepository == nil {
		return requestError(ErrorRequest)
	}
	if repository, ok := s.connRepository.(AtomicRealConnectionRepository); ok {
		return repository.SaveRealConnectionWithPricingMapping(ctx, conn)
	}
	if err := s.connRepository.SaveRealConnection(ctx, conn); err != nil {
		return err
	}
	// Older/in-memory repositories do not expose a transaction boundary. Keep
	// their existing behavior for tests and rolling deployments.
	if conn.PricingMappingEnabled {
		s.addUpstreamMapping(ctx, conn.UserID, conn.WorkspaceAdminAccountID, conn.OwnGroupIDs, conn.UpstreamSiteID, conn.UpstreamGroupName)
	}
	return nil
}

func (s *Service) listOwnedUpstreamKeys(ctx context.Context, userID, adminAccountID, siteID string) (*upstream.Site, []upstream.Sub2APIKeyItem, error) {
	upstreamSite, err := s.upstreamLookup.GetSite(ctx, strings.TrimSpace(siteID))
	if err != nil || upstreamSite == nil || upstreamSite.Session == nil || upstreamSite.UserID != userID || upstreamSite.AdminAccountID != adminAccountID {
		return nil, nil, requestError(ErrorRequest)
	}
	var keys []upstream.Sub2APIKeyItem
	if upstreamSite.Session.Platform == upstream.PlatformNewAPI {
		keys, err = s.platformService.ListNewAPITokens(*upstreamSite.Session)
	} else {
		keys, err = s.platformService.ListSub2APIKeys(*upstreamSite.Session)
	}
	return upstreamSite, keys, err
}

func credentialMatchesGroup(item upstream.Sub2APIKeyItem, groupID, groupName string) bool {
	groupID = strings.TrimSpace(groupID)
	groupName = strings.TrimSpace(groupName)
	return (item.GroupID != "" && (item.GroupID == groupID || item.GroupID == groupName)) ||
		(item.GroupName != "" && (item.GroupName == groupName || item.GroupName == groupID))
}

// ListUpstreamCredentials returns only credentials that belong to the selected
// upstream group and strips the full secret before crossing the HTTP boundary.
func (s *Service) ListUpstreamCredentials(ctx context.Context, userID, siteID, groupID, groupName string) ([]UpstreamCredentialOption, error) {
	adminAccountID, err := s.currentAdminAccountID(ctx, userID)
	if err != nil {
		return nil, err
	}
	_, keys, err := s.listOwnedUpstreamKeys(ctx, userID, adminAccountID, siteID)
	if err != nil {
		return nil, err
	}
	result := make([]UpstreamCredentialOption, 0, len(keys))
	for _, key := range keys {
		if !credentialMatchesGroup(key, groupID, groupName) {
			continue
		}
		result = append(result, UpstreamCredentialOption{
			ID: key.ID, Name: key.Name, GroupID: key.GroupID, GroupName: key.GroupName,
			Status: key.Status, KeyPreview: safeCredentialPreview(key.Key),
		})
	}
	return result, nil
}

func safeCredentialPreview(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 4 {
		return strings.Repeat("*", len(value))
	}
	return value[:2] + "..." + value[len(value)-2:]
}

// ListAdminResources lists existing accounts/channels in one current-admin
// group. Selection is revalidated by RealBind; this endpoint is presentation only.
func (s *Service) ListAdminResources(ctx context.Context, userID, adminGroupID string) ([]AdminResourceOption, error) {
	adminAccountID, err := s.currentAdminAccountID(ctx, userID)
	if err != nil {
		return nil, err
	}
	state, err := s.authenticatedState(ctx, userID, adminAccountID)
	if err != nil {
		return nil, err
	}
	group, err := s.findAdminGroup(ctx, state, adminGroupID)
	if err != nil {
		return nil, err
	}
	resources, err := s.platformService.ListAdminGroupAccounts(state.Session, group)
	if err != nil {
		return nil, err
	}
	result := make([]AdminResourceOption, 0, len(resources))
	for _, resource := range resources {
		groupIDs := resource.GroupIDs
		if len(groupIDs) == 0 {
			groupIDs = []string{group.ID}
		}
		result = append(result, AdminResourceOption{
			ID: resource.ID, Name: resource.Name, Type: resource.Type,
			Status: resource.Status, Platform: resource.Platform, GroupIDs: groupIDs,
		})
	}
	return result, nil
}

func (s *Service) findAdminGroup(ctx context.Context, state *State, groupID string) (upstream.AdminGroupInfo, error) {
	groups, err := s.platformService.FetchAdminAllGroups(state.Session)
	if err != nil {
		return upstream.AdminGroupInfo{}, err
	}
	groupID = strings.TrimSpace(groupID)
	for _, group := range groups {
		if group.ID == groupID || group.Name == groupID {
			return group, nil
		}
	}
	return upstream.AdminGroupInfo{}, requestError(ErrorRequest)
}

func (s *Service) resolveExistingAdminResource(ctx context.Context, state *State, groupID, resourceID string) (upstream.AdminGroupAccountInfo, []string, []string, error) {
	group, err := s.findAdminGroup(ctx, state, groupID)
	if err != nil {
		return upstream.AdminGroupAccountInfo{}, nil, nil, err
	}
	resources, err := s.platformService.ListAdminGroupAccounts(state.Session, group)
	if err != nil {
		return upstream.AdminGroupAccountInfo{}, nil, nil, err
	}
	var selected *upstream.AdminGroupAccountInfo
	for i := range resources {
		if resources[i].ID == strings.TrimSpace(resourceID) {
			selected = &resources[i]
			break
		}
	}
	if selected == nil {
		return upstream.AdminGroupAccountInfo{}, nil, nil, requestError(ErrorRequest)
	}
	groupIDs := selected.GroupIDs
	if len(groupIDs) == 0 {
		groupIDs = []string{group.ID}
	}
	returnResource := *selected
	ownGroups, err := s.resolveAdminGroups(ctx, state, groupIDs)
	if err != nil {
		return upstream.AdminGroupAccountInfo{}, nil, nil, err
	}
	return returnResource, ownGroups.ids, ownGroups.names, nil
}

func (s *Service) resolveExistingCredential(site *upstream.Site, keys []upstream.Sub2APIKeyItem, keyID, groupID, groupName string, allowLegacy bool, legacyKey string) (string, error) {
	for _, item := range keys {
		if item.ID != strings.TrimSpace(keyID) {
			continue
		}
		if !allowLegacy && !credentialMatchesGroup(item, groupID, groupName) {
			return "", requestError(ErrorRequest)
		}
		if site.Session.Platform == upstream.PlatformNewAPI {
			return s.platformService.FetchNewAPITokenKey(*site.Session, item.ID)
		}
		if strings.TrimSpace(item.Key) == "" {
			return "", requestError(ErrorRequest)
		}
		return item.Key, nil
	}
	// Old clients historically sent the Sub2API key value themselves. Preserve
	// that fallback only for legacy requests without an admin resource selection.
	if allowLegacy && site.Session.Platform == upstream.PlatformSub2API && strings.TrimSpace(legacyKey) != "" {
		return strings.TrimSpace(legacyKey), nil
	}
	return "", requestError(ErrorRequest)
}

func (s *Service) realBindExisting(ctx context.Context, userID string, req RealBindRequest) (RealConnectResponse, error) {
	if strings.TrimSpace(req.UpstreamSiteID) == "" || strings.TrimSpace(req.UpstreamGroupID) == "" || strings.TrimSpace(req.UpstreamKeyID) == "" {
		return RealConnectResponse{}, requestError(ErrorRequest)
	}
	operationID, err := normalizeOperationID(req.OperationID)
	if err != nil {
		return RealConnectResponse{}, err
	}
	connectionCtx, err := s.prepareConnectionContext(ctx, userID, req.UpstreamSiteID, req.UpstreamGroupID, req.UpstreamGroupName, req.GroupType, false)
	if err != nil {
		return RealConnectResponse{}, err
	}
	if existing, err := s.idempotentConnection(ctx, userID, connectionCtx.adminAccountID, operationID); err != nil {
		return RealConnectResponse{}, err
	} else if existing != nil {
		return RealConnectResponse{Connection: publicRealConnection(*existing)}, nil
	}
	// 与自动创建共用同一目标锁，重复检查到落库之间不允许同一上游分组的并发写入。
	release, locked := s.targetLocks.tryLock(connectionTargetLockKey(userID, connectionCtx.adminAccountID, req.UpstreamSiteID, req.UpstreamGroupID))
	if !locked {
		return RealConnectResponse{}, requestError(ErrorConnectionInProgress)
	}
	defer release()
	if err := s.rejectDuplicateTarget(ctx, userID, connectionCtx.adminAccountID, req.UpstreamSiteID, req.UpstreamGroupID, connectionCtx.groupName); err != nil {
		return RealConnectResponse{}, err
	}

	legacyRequest := strings.TrimSpace(req.AdminGroupID) == "" && strings.TrimSpace(req.AdminResourceID) == ""
	if !legacyRequest && (strings.TrimSpace(req.AdminGroupID) == "" || strings.TrimSpace(req.AdminResourceID) == "") {
		return RealConnectResponse{}, requestError(ErrorRequest)
	}
	_, keys, err := s.listOwnedUpstreamKeys(ctx, userID, connectionCtx.adminAccountID, req.UpstreamSiteID)
	if err != nil {
		return RealConnectResponse{}, err
	}
	key, err := s.resolveExistingCredential(connectionCtx.upstreamSite, keys, req.UpstreamKeyID, req.UpstreamGroupID, connectionCtx.groupName, legacyRequest, req.UpstreamKey)
	if err != nil {
		return RealConnectResponse{}, err
	}

	mode := ProvisioningModeExisting
	adminResourceID := strings.TrimSpace(req.AdminResourceID)
	adminResourceName := ""
	var ownGroupIDs, ownGroupNames []string
	if legacyRequest {
		if len(req.OwnGroupIDs) == 0 {
			return RealConnectResponse{}, requestError(ErrorRequest)
		}
		mode = ProvisioningModeLegacy
		var ownGroups ownGroupSelection
		ownGroups, err = s.resolveAdminGroups(ctx, connectionCtx.state, req.OwnGroupIDs)
		ownGroupIDs, ownGroupNames = ownGroups.ids, ownGroups.names
	} else {
		resource, resolvedIDs, resolvedNames, resolveErr := s.resolveExistingAdminResource(ctx, connectionCtx.state, req.AdminGroupID, req.AdminResourceID)
		if resolveErr != nil {
			return RealConnectResponse{}, resolveErr
		}
		adminResourceName = resource.Name
		ownGroupIDs, ownGroupNames = resolvedIDs, resolvedNames
	}
	if err != nil {
		return RealConnectResponse{}, err
	}
	connID, err := randomConnID()
	if err != nil {
		return RealConnectResponse{}, err
	}
	conn := RealConnection{
		ID: connID, UserID: userID, WorkspaceAdminAccountID: connectionCtx.adminAccountID,
		UpstreamSiteID: req.UpstreamSiteID, UpstreamGroupID: req.UpstreamGroupID,
		UpstreamGroupName: connectionCtx.groupName, UpstreamKeyID: strings.TrimSpace(req.UpstreamKeyID),
		UpstreamKey: key, AdminAccountID: adminResourceID, AdminAccountName: adminResourceName,
		OwnGroupIDs: ownGroupIDs, OwnGroupNames: ownGroupNames, GroupType: connectionCtx.groupType,
		ProvisioningMode: mode, Status: ConnectionStatusActive,
		UpstreamPlatform: string(connectionCtx.upstreamSession.Platform), AdminPlatform: string(connectionCtx.state.Session.Platform),
		PricingMappingEnabled: addToPricingMapping(req.AddToPricingMapping), OperationID: operationID,
		CanDeleteRemote: false, CreatedAt: time.Now().Format(time.RFC3339),
	}
	if err := s.persistConnection(ctx, conn); err != nil {
		return RealConnectResponse{}, err
	}
	return RealConnectResponse{Connection: publicRealConnection(conn)}, nil
}

func (s *Service) realDisconnectConnection(ctx context.Context, userID string, req RealDisconnectRequest) error {
	if strings.TrimSpace(req.ConnectionID) == "" || (req.Mode != "unlink" && req.Mode != "full") || s.connRepository == nil {
		return requestError(ErrorRequest)
	}
	adminAccountID, err := s.currentAdminAccountID(ctx, userID)
	if err != nil {
		return err
	}
	conn, err := s.connRepository.GetRealConnection(ctx, req.ConnectionID, userID, adminAccountID)
	if err != nil {
		return err
	}
	if conn == nil {
		return requestError(ErrorRequest)
	}

	if req.Mode == "full" {
		legacyManaged := conn.ProvisioningMode == ProvisioningModeLegacy && strings.TrimSpace(conn.AdminAccountID) != ""
		if conn.ProvisioningMode != ProvisioningModeManaged && !legacyManaged {
			return requestError(ErrorManagedDeleteOnly)
		}
		state, err := s.authenticatedState(ctx, userID, adminAccountID)
		if err != nil {
			return err
		}
		upstreamSite, err := s.upstreamLookup.GetSite(ctx, conn.UpstreamSiteID)
		if err != nil || upstreamSite == nil || upstreamSite.Session == nil || upstreamSite.UserID != userID || upstreamSite.AdminAccountID != adminAccountID {
			return requestError(ErrorRequest)
		}
		adminSession := state.Session
		if conn.AdminPlatform != "" && conn.AdminPlatform != string(adminSession.Platform) {
			return requestError(ErrorRequest)
		}
		upstreamSession := *upstreamSite.Session
		if conn.UpstreamPlatform != "" && conn.UpstreamPlatform != string(upstreamSession.Platform) {
			return requestError(ErrorRequest)
		}
		// 完整删除可能在两步之间失败；重试时已删除的一侧返回 404，按已完成处理以便继续清理。
		if err := s.deleteAdminResource(adminSession, conn.AdminAccountID); err != nil && !isRemoteNotFound(err) {
			return err
		}
		if err := s.deleteUpstreamCredential(upstreamSession, conn.UpstreamKeyID); err != nil && !isRemoteNotFound(err) {
			return err
		}
	}

	removePricing := conn.PricingMappingEnabled
	if req.RemovePricingMapping != nil {
		removePricing = *req.RemovePricingMapping
	}
	if repository, ok := s.connRepository.(ScopedRealDisconnectRepository); ok {
		return repository.DeleteRealConnectionWithPricingMapping(ctx, *conn, removePricing)
	}
	if removePricing {
		return s.removeUpstreamMappingAndDeleteConnection(ctx, userID, adminAccountID, req.ConnectionID, conn.UpstreamSiteID, conn.UpstreamGroupName)
	}
	return s.connRepository.DeleteRealConnection(ctx, req.ConnectionID, userID, adminAccountID)
}

// isRemoteNotFound 仅识别远端明确返回的 404/410；网络错误、权限错误和无法解析的响应都不视为已删除。
func isRemoteNotFound(err error) bool {
	var remoteErr *upstream.RequestError
	if !errors.As(err, &remoteErr) {
		return false
	}
	return remoteErr.StatusCode == http.StatusNotFound || remoteErr.StatusCode == http.StatusGone
}

// UpstreamGroupProtocol 返回创建账号时后端实际依据的上游分组平台缓存，供前端按真实平台筛选自有分组。
func (s *Service) UpstreamGroupProtocol(ctx context.Context, userID, siteID, groupID string) (UpstreamGroupProtocolResponse, error) {
	siteID, groupID = strings.TrimSpace(siteID), strings.TrimSpace(groupID)
	if siteID == "" || groupID == "" {
		return UpstreamGroupProtocolResponse{}, requestError(ErrorRequest)
	}
	adminAccountID, err := s.currentAdminAccountID(ctx, userID)
	if err != nil {
		return UpstreamGroupProtocolResponse{}, err
	}
	site, err := s.upstreamLookup.GetSite(ctx, siteID)
	if err != nil || site == nil || site.UserID != userID || site.AdminAccountID != adminAccountID {
		return UpstreamGroupProtocolResponse{}, requestError(ErrorRequest)
	}
	platform := site.Platform
	if site.Session != nil && site.Session.Platform != "" {
		platform = site.Session.Platform
	}
	groupType, _ := resolveGroupInfo(site.Metrics.Groups, groupID)
	return UpstreamGroupProtocolResponse{UpstreamPlatform: string(platform), GroupType: groupType}, nil
}

func publicRealConnection(conn RealConnection) RealConnection {
	conn.UpstreamKey = ""
	conn.OperationID = ""
	conn.CanDeleteRemote = conn.ProvisioningMode == ProvisioningModeManaged ||
		(conn.ProvisioningMode == ProvisioningModeLegacy && conn.AdminAccountID != "")
	return conn
}
