//go:build unit

package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration232AddsWeeklyQuotaGenerationsAndDurableResetTargets(t *testing.T) {
	content, err := FS.ReadFile("232_openai_weekly_quota_linkage_v2.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "weekly_quota_generation BIGINT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "weekly_reserved_generation BIGINT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS openai_weekly_quota_reset_events")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS openai_weekly_quota_reset_execution_users")
	require.Contains(t, sql, "UNIQUE (reset_event_id, user_id, platform)")
	require.Contains(t, sql, "weekly_pending")
	require.Contains(t, sql, "last_query_success_at")
	require.Contains(t, sql, "last_execution_success_at")
	require.Contains(t, sql, "source_identity_fingerprint")
	require.Contains(t, sql, "ALTER COLUMN reset_event_id SET DEFAULT ('legacy:' || gen_random_uuid()::TEXT)")
	require.Contains(t, sql, "DROP INDEX IF EXISTS idx_openai_weekly_reset_execution_window")
	require.Contains(t, sql, "UNIQUE INDEX IF NOT EXISTS idx_openai_weekly_reset_execution_event")
}

func TestMigration231RemainsHistoricalBaseline(t *testing.T) {
	content, err := FS.ReadFile("231_openai_weekly_quota_reset_links.sql")
	require.NoError(t, err)

	sql := string(content)
	require.NotContains(t, sql, "weekly_quota_generation")
	require.NotContains(t, sql, "reset_event_id")
	require.Contains(t, sql, "reset_user_ids BIGINT[] NOT NULL DEFAULT '{}'")
}
