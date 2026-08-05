package migrations

import (
	"strings"
	"testing"
)

func TestRealConnectionOperationMigrationReservesBeforeRemoteCreation(t *testing.T) {
	sqlBytes, err := migrationFiles.ReadFile("000019_real_connection_operations.sql")
	if err != nil {
		t.Fatalf("读取真实对接幂等迁移失败: %v", err)
	}
	sql := strings.ToLower(string(sqlBytes))
	for _, want := range []string{
		"create table",
		"real_connection_operations",
		"unique (user_id, workspace_admin_account_id, operation_id)",
		"status",
		"request_hash",
		"remote_upstream_key_id",
		"remote_admin_resource_id",
		"compensation_error",
		"attempt_count",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("幂等迁移必须包含 %q: %s", want, sql)
		}
	}
}
