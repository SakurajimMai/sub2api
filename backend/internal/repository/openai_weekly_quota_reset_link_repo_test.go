package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWeeklyQuotaResetLinkRepository_BaselineOnly(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := NewOpenAIWeeklyQuotaResetLinkRepository(db)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(7 * 24 * time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT target_group_id, last_observed_reset_at
		FROM openai_weekly_quota_reset_rules
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE`)).
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"target_group_id", "last_observed_reset_at"}).AddRow(int64(3), nil))
	mock.ExpectExec("UPDATE openai_weekly_quota_reset_rules").
		WithArgs(resetAt, int64(7*24*60*60), now, int64(9)).
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
		UserID: 1, Platform: service.PlatformOpenAI,
	}}, time.Now())

	require.Contains(t, sqlText,
		"EXCLUDED.weekly_window_start > user_platform_quotas.weekly_window_start")
	require.Contains(t, sqlText,
		"EXCLUDED.weekly_window_start = user_platform_quotas.weekly_window_start")
	require.Contains(t, sqlText,
		"GREATEST(user_platform_quotas.weekly_usage_usd, EXCLUDED.weekly_usage_usd)")
}

func TestOpenAIWeeklyQuotaResetLinkRepository_RetryReplaysCacheOnly(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := NewOpenAIWeeklyQuotaResetLinkRepository(db)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(7 * 24 * time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT target_group_id, last_observed_reset_at").
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"target_group_id", "last_observed_reset_at"}).AddRow(int64(3), resetAt))
	mock.ExpectQuery("SELECT id, status, updated_at, matched_users, skipped_users, reset_user_ids").
		WithArgs(int64(9), resetAt).
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
		WillReturnRows(sqlmock.NewRows([]string{"target_group_id", "last_observed_reset_at"}).AddRow(int64(3), resetAt))
	mock.ExpectQuery("SELECT id, status, updated_at, matched_users, skipped_users, reset_user_ids").
		WithArgs(int64(9), resetAt).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "updated_at", "matched_users", "skipped_users", "reset_user_ids"}).
			AddRow(int64(17), service.OpenAIWeeklyQuotaExecutionRunning, now.Add(-time.Second), 2, 0, "{11,12}"))
	mock.ExpectExec("UPDATE openai_weekly_quota_reset_rules SET last_observed_fetched_at=").
		WithArgs(now, int64(9)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := repo.ApplyObservedWeeklyWindow(context.Background(), service.OpenAIWeeklyQuotaObservation{
		RuleID: 9, OfficialResetAt: resetAt, OfficialWindowStart: now,
		OfficialWindowSeconds: 7 * 24 * 60 * 60, DetectedAt: now,
	})
	require.NoError(t, err)
	require.Equal(t, service.OpenAIWeeklyQuotaObservationUnchanged, result.Outcome)
	require.NoError(t, mock.ExpectationsWereMet())
}
