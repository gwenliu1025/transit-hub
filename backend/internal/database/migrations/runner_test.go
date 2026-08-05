package migrations

import (
	"strings"
	"testing"
)

func TestStrategyModeMigrationAddsPriorityModeBeforeBackfill(t *testing.T) {
	sqlBytes, err := migrationFiles.ReadFile("000018_connection_health_strategy_mode.sql")
	if err != nil {
		t.Fatalf("读取迁移文件失败：%v", err)
	}

	sql := strings.ToLower(string(sqlBytes))
	addPriorityMode := strings.Index(sql, "add column if not exists priority_mode")
	backfill := strings.Index(sql, "update connection_health_policies as policy")

	if addPriorityMode < 0 {
		t.Fatal("迁移必须先为运行时旧表补齐 priority_mode 列")
	}
	if backfill < 0 {
		t.Fatal("迁移缺少 strategy_mode 兼容回填")
	}
	if addPriorityMode > backfill {
		t.Fatal("priority_mode 必须在引用该列的兼容回填之前创建")
	}
}
