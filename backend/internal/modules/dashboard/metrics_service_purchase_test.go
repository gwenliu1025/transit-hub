package dashboard

import (
	"testing"
	"time"

	"transithub/backend/internal/modules/upstream"
)

func TestAggregateUpstreamMetricsForDate_FiltersPurchaseByBeijingSyncDate(t *testing.T) {
	targetDate := "2026-08-04"
	sites := []upstream.Response{
		purchaseTestSite("same-day-connected", upstream.StatusConnected, 10, 2, purchaseTestTimestamp(t, "2026-08-04T00:30:00+08:00")),
		purchaseTestSite("same-day-syncing", upstream.StatusSyncing, 5, 3, purchaseTestTimestamp(t, "2026-08-04T23:30:00+08:00")),
		purchaseTestSite("previous-day-connected", upstream.StatusConnected, 20, 2, purchaseTestTimestamp(t, "2026-08-03T23:59:59+08:00")),
		purchaseTestSite("previous-day-error", upstream.StatusError, 30, 2, purchaseTestTimestamp(t, "2026-08-03T12:00:00+08:00")),
		purchaseTestSite("missing-sync-time", upstream.StatusConnected, 40, 2, nil),
		purchaseTestSite("missing-consume", upstream.StatusConnected, 0, 2, purchaseTestTimestamp(t, "2026-08-04T12:00:00+08:00")),
		purchaseTestSite("missing-rate", upstream.StatusConnected, 50, 0, purchaseTestTimestamp(t, "2026-08-04T12:00:00+08:00")),
	}
	sites[5].Metrics.TodayConsume.Value = nil

	todayPurchase, _ := aggregateUpstreamMetricsForDate(sites, targetDate)

	if todayPurchase != 35 {
		t.Fatalf("todayPurchase = %.2f, want 35.00", todayPurchase)
	}
}

func TestAggregateUpstreamMetricsForDate_DoesNotFilterBalanceBySyncDate(t *testing.T) {
	targetDate := "2026-08-04"
	sites := []upstream.Response{
		purchaseTestSite("same-day", upstream.StatusConnected, 1, 2, purchaseTestTimestamp(t, "2026-08-04T12:00:00+08:00")),
		purchaseTestSite("previous-day", upstream.StatusError, 1, 3, purchaseTestTimestamp(t, "2026-08-03T12:00:00+08:00")),
		purchaseTestSite("missing-sync-time", upstream.StatusConnected, 1, 4, nil),
		purchaseTestSite("missing-rate", upstream.StatusConnected, 1, 0, purchaseTestTimestamp(t, "2026-08-04T12:00:00+08:00")),
	}

	_, upstreamBalance := aggregateUpstreamMetricsForDate(sites, targetDate)

	if upstreamBalance != 90 {
		t.Fatalf("upstreamBalance = %.2f, want 90.00", upstreamBalance)
	}
}

func TestBeijingMetricDates_UseBeijingCalendarDay(t *testing.T) {
	now := time.Date(2026, 8, 3, 16, 30, 0, 0, time.UTC)

	if got := beijingMetricDate(now); got != "2026-08-04" {
		t.Fatalf("beijingMetricDate() = %q, want %q", got, "2026-08-04")
	}
	if got := previousBeijingMetricDate(now); got != "2026-08-03" {
		t.Fatalf("previousBeijingMetricDate() = %q, want %q", got, "2026-08-03")
	}
}

func purchaseTestSite(id string, status upstream.Status, consume, rechargeRate float64, lastSyncedAt *int64) upstream.Response {
	balance := consume * 10
	return upstream.Response{
		ID:           id,
		Status:       status,
		RechargeRate: rechargeRate,
		LastSyncedAt: lastSyncedAt,
		Metrics: upstream.Metrics{
			TodayConsume: upstream.MetricValue{Value: &consume},
			Balance:      upstream.MetricValue{Value: &balance},
		},
	}
}

func purchaseTestTimestamp(t *testing.T, value string) *int64 {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", value, err)
	}
	timestamp := parsed.UnixMilli()
	return &timestamp
}
