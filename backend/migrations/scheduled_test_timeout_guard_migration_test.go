package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScheduledTestTimeoutGuardMigrationDefinesDurableExecutionAndOwnership(t *testing.T) {
	content, err := FS.ReadFile("224_scheduled_test_timeout_guard.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "timeout_protection_mode VARCHAR(16) NOT NULL DEFAULT 'off'")
	require.Contains(t, sql, "claim_token UUID")
	require.Contains(t, sql, "claim_plan_version BIGINT")
	require.Contains(t, sql, "claim_expires_at TIMESTAMPTZ")
	require.Contains(t, sql, "SET classification = CASE WHEN status = 'success' THEN 'success' ELSE 'failure' END")
	require.Contains(t, sql, "ALTER COLUMN classification SET NOT NULL")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS scheduled_test_account_ownership")
	require.Contains(t, sql, "previous_schedulable BOOLEAN NOT NULL")
	require.Contains(t, sql, "previous_error_message TEXT")
	require.NotContains(t, sql, "previous_error_message TEXT NOT NULL")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS scheduled_test_protection_events")
	require.Contains(t, sql, "platform VARCHAR(32) NOT NULL")
	require.Contains(t, sql, "UNIQUE (plan_id, execution_id)")
	require.NotContains(t, sql, "plan_id BIGINT NOT NULL REFERENCES scheduled_test_plans")
	require.Contains(t, sql, "WHERE protection_action = 'inactivated'")
	require.Contains(t, sql, "NEW.status IS DISTINCT FROM OLD.status OR NEW.schedulable IS DISTINCT FROM OLD.schedulable OR NEW.error_message IS DISTINCT FROM OLD.error_message")
	require.Contains(t, sql, "AFTER UPDATE OF status, schedulable, error_message ON accounts")
	require.Contains(t, sql, "CREATE TRIGGER trg_release_scheduled_test_ownership_on_manual_status")

	indexContent, err := FS.ReadFile("225_scheduled_test_execution_unique_notx.sql")
	require.NoError(t, err)
	indexSQL := strings.Join(strings.Fields(string(indexContent)), " ")
	require.Contains(t, indexSQL, "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS")
	require.Contains(t, indexSQL, "ON scheduled_test_results(plan_id, execution_id)")
}
