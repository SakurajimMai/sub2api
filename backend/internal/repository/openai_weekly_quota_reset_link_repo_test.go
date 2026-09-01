package repository

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestPostgresInt64Array_EmptySliceIsNotSQLNull(t *testing.T) {
	encoded, err := postgresInt64Array(nil).Value()
	require.NoError(t, err)
	require.Equal(t, "{}", encoded)
}

func TestWeeklyUsageForGeneration_TreatsZeroAsValidOldGeneration(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	usage, generation := weeklyUsageForGeneration(1.25, 1, 0, &start, start, 99)
	require.Equal(t, int64(1), generation)
	require.InDelta(t, 1.25, usage, 1e-9, "延迟的 generation 0 成本不能写回 generation 1")
}

func TestOpenAIWeeklyQuotaResetLinkRepository_BaselineOnly(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := NewOpenAIWeeklyQuotaResetLinkRepository(db)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(7 * 24 * time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT target_group_id, last_observed_reset_at, source_identity_fingerprint, last_snapshot_event_id
		FROM openai_weekly_quota_reset_rules
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE`)).
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"target_group_id", "last_observed_reset_at", "source_identity_fingerprint", "last_snapshot_event_id"}).AddRow(int64(3), nil, "", ""))
	mock.ExpectExec("UPDATE openai_weekly_quota_reset_rules").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := repo.ApplyObservedWeeklyWindow(context.Background(), service.OpenAIWeeklyQuotaObservation{
		RuleID:                9,
		OfficialResetAt:       resetAt,
		OfficialWindowStart:   resetAt.Add(-7 * 24 * time.Hour),
		OfficialWindowSeconds: 7 * 24 * 60 * 60,
		DetectedAt:            now,
	})
	require.NoError(t, err)
	require.Equal(t, service.OpenAIWeeklyQuotaObservationBaseline, result.Outcome)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBatchSnapshotUsage_WeeklyWindowMonotonic(t *testing.T) {
	sqlText, _ := buildUserPlatformQuotaSnapshotUpsert([]UserPlatformQuotaSnapshot{{
		UserID: 1, Platform: service.PlatformOpenAI, WeeklyGeneration: 3,
	}}, time.Now())

	require.Contains(t, sqlText,
		"EXCLUDED.weekly_quota_generation > user_platform_quotas.weekly_quota_generation")
	require.Contains(t, sqlText,
		"EXCLUDED.weekly_quota_generation = user_platform_quotas.weekly_quota_generation")
	require.Contains(t, sqlText,
		"GREATEST(user_platform_quotas.weekly_usage_usd, EXCLUDED.weekly_usage_usd)")
	require.Contains(t, sqlText,
		"weekly_quota_generation = GREATEST(user_platform_quotas.weekly_quota_generation, EXCLUDED.weekly_quota_generation)")
}

func TestOpenAIWeeklyQuotaResetLinkRepository_RetryReplaysCacheOnly(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := NewOpenAIWeeklyQuotaResetLinkRepository(db)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(7 * 24 * time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT target_group_id, last_observed_reset_at").
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"target_group_id", "last_observed_reset_at", "source_identity_fingerprint", "last_snapshot_event_id"}).AddRow(int64(3), resetAt, "", ""))
	mock.ExpectQuery("FROM openai_weekly_quota_reset_executions WHERE rule_id=\\$1 AND reset_event_id=\\$2").
		WithArgs(int64(9), fmt.Sprintf("legacy-window:9:%d", resetAt.Unix())).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "updated_at", "matched_users", "skipped_users", "reset_user_ids"}).
			AddRow(int64(17), service.OpenAIWeeklyQuotaExecutionRetryableFailed, now.Add(-time.Minute), 3, 1, "{11,12}"))
	mock.ExpectExec("UPDATE openai_weekly_quota_reset_executions SET status=").
		WithArgs(service.OpenAIWeeklyQuotaExecutionRunning, now, int64(17)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE openai_weekly_quota_reset_rules SET last_observed_fetched_at=").
		WithArgs(now, int64(9)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := repo.ApplyObservedWeeklyWindow(context.Background(), service.OpenAIWeeklyQuotaObservation{
		RuleID: 9, OfficialResetAt: resetAt, OfficialWindowStart: now,
		OfficialWindowSeconds: 7 * 24 * 60 * 60, DetectedAt: now,
	})
	require.NoError(t, err)
	require.Equal(t, service.OpenAIWeeklyQuotaObservationTriggered, result.Outcome)
	require.Equal(t, []int64{11, 12}, result.ResetUserIDs)
	require.Equal(t, 3, result.MatchedUsers)
	require.Equal(t, 1, result.SkippedUsers)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAIWeeklyQuotaResetLinkRepository_RecentRunningExecutionIsNotReplayed(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := NewOpenAIWeeklyQuotaResetLinkRepository(db)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(7 * 24 * time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT target_group_id, last_observed_reset_at").
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"target_group_id", "last_observed_reset_at", "source_identity_fingerprint", "last_snapshot_event_id"}).AddRow(int64(3), resetAt, "", ""))
	mock.ExpectQuery("FROM openai_weekly_quota_reset_executions WHERE rule_id=\\$1 AND reset_event_id=\\$2").
		WithArgs(int64(9), fmt.Sprintf("legacy-window:9:%d", resetAt.Unix())).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "updated_at", "matched_users", "skipped_users", "reset_user_ids"}).
			AddRow(int64(17), service.OpenAIWeeklyQuotaExecutionRunning, now.Add(-time.Second), 2, 0, "{11,12}"))
	mock.ExpectCommit()

	result, err := repo.ApplyObservedWeeklyWindow(context.Background(), service.OpenAIWeeklyQuotaObservation{
		RuleID: 9, OfficialResetAt: resetAt, OfficialWindowStart: now,
		OfficialWindowSeconds: 7 * 24 * 60 * 60, DetectedAt: now,
	})
	require.NoError(t, err)
	require.Equal(t, service.OpenAIWeeklyQuotaObservationUnchanged, result.Outcome)
	require.NoError(t, mock.ExpectationsWereMet())
}
