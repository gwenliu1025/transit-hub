package upstream

import (
	"context"
	"testing"
	"time"
)

type refreshSchedulerRepository struct {
	sites []Site
}

func (r *refreshSchedulerRepository) ListSites(context.Context) ([]Site, error) {
	return append([]Site(nil), r.sites...), nil
}

func (r *refreshSchedulerRepository) ListSitesForUser(_ context.Context, userID string) ([]Site, error) {
	var result []Site
	for _, site := range r.sites {
		if site.UserID == userID {
			result = append(result, site)
		}
	}
	return result, nil
}

func (*refreshSchedulerRepository) SaveSite(context.Context, Site) error { return nil }
func (*refreshSchedulerRepository) DeleteSite(context.Context, string, string) error {
	return nil
}

func TestSetWorkspaceRefreshConfigOnlySchedulesMatchingWorkspace(t *testing.T) {
	repo := &refreshSchedulerRepository{sites: []Site{
		{ID: "site-a", UserID: "user-a", AdminAccountID: "workspace-a", Session: &Session{}},
		{ID: "site-b", UserID: "user-b", AdminAccountID: "workspace-b", Session: &Session{}},
		{ID: "site-c", UserID: "user-a", AdminAccountID: "workspace-b", Session: &Session{}},
	}}
	service := NewService(nil, repo, nil, nil)

	service.SetWorkspaceRefreshConfig("user-a", "workspace-a", RefreshConfig{Enabled: true, Interval: time.Hour})
	service.SetWorkspaceRefreshConfig("user-b", "workspace-b", RefreshConfig{Enabled: true, Interval: time.Hour})
	service.SetWorkspaceRefreshConfig("user-b", "workspace-b", RefreshConfig{Enabled: false, Interval: time.Hour})

	service.mu.Lock()
	defer service.mu.Unlock()
	if _, ok := service.timers["site-a"]; !ok {
		t.Fatal("expected workspace-a site to be scheduled")
	}
	if _, ok := service.timers["site-b"]; ok {
		t.Fatal("workspace-b site must not be scheduled by workspace-a config")
	}
	if _, ok := service.timers["site-c"]; ok {
		t.Fatal("same user in a different workspace must not be scheduled")
	}
}
