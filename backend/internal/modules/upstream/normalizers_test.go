package upstream

import "testing"

func TestNewAPIGroupsPreservesAutoGroupWithoutNumericRatio(t *testing.T) {
	groups := newAPIGroups(
		map[string]any{},
		map[string]any{"usable_group": map[string]any{"auto": "automatic routing"}},
	)
	if len(groups) != 1 {
		t.Fatalf("expected auto group to remain visible, got %#v", groups)
	}
	if groups[0].Name != "auto" || groups[0].Multiplier != nil || groups[0].MultiplierMode != "auto" {
		t.Fatalf("unexpected auto group normalization: %#v", groups[0])
	}
}

func TestNewAPIGroupPlatformsJoinsVendorsInStableOrder(t *testing.T) {
	pricing := map[string]any{"data": []any{
		map[string]any{"model_name": "gpt", "owner_by": "openai", "enable_groups": []any{"vip"}},
		map[string]any{"model_name": "gemini", "owner_by": "gemini", "enable_groups": []any{"vip"}},
		map[string]any{"model_name": "claude", "owner_by": "anthropic", "enable_groups": []any{"vip"}},
		map[string]any{"model_name": "deepseek", "owner_by": "deepseek", "enable_groups": []any{"vip"}},
	}}
	// map 遍历顺序每次运行都可能不同，多次调用都必须得到同一排序结果。
	for i := 0; i < 50; i++ {
		platforms := newAPIGroupPlatforms(pricing)
		if platforms["vip"] == nil || *platforms["vip"] != "anthropic, deepseek, gemini, openai" {
			t.Fatalf("vendor list must be sorted, got %v", platforms["vip"])
		}
	}
}
