package settings

import "testing"

func TestStrategyChangedCallbackIncludesWorkspaceIdentity(t *testing.T) {
	service := &Service{}
	service.OnStrategyChanged = func(userID, adminAccountID string, _ StrategySettings) {
		if userID != "user-a" || adminAccountID != "workspace-a" {
			t.Fatalf("unexpected workspace: %s/%s", userID, adminAccountID)
		}
	}
	service.notifyStrategyChanged("user-a", "workspace-a", StrategySettings{})
}
