//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func createWeeklyResetIntegrationRule(t *testing.T, suffix string) (*openAIWeeklyQuotaResetLinkRepository, *service.OpenAIWeeklyQuotaResetRule, *service.Account, *service.Group) {
	t.Helper()
	ctx := context.Background()
	account := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name: "weekly-reset-account-" + suffix, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
	})
	group := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name: "weekly-reset-group-" + suffix, Platform: service.PlatformOpenAI, RateMultiplier: 1,
	})
	repo := NewOpenAIWeeklyQuotaResetLinkRepository(integrationDB).(*openAIWeeklyQuotaResetLinkRepository)
	rule, err := repo.CreateRule(ctx, service.OpenAIWeeklyQuotaResetRuleInput{
		Name: "weekly-reset-rule-" + suffix, Enabled: true, SourceAccountID: account.ID, TargetGroupID: group.ID,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM openai_weekly_quota_reset_execution_users WHERE execution_id IN (SELECT id FROM openai_weekly_quota_reset_executions WHERE rule_id=$1)", rule.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM openai_weekly_quota_reset_executions WHERE rule_id=$1", rule.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM openai_weekly_quota_reset_events WHERE source_account_id=$1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM openai_weekly_quota_reset_rules WHERE id=$1", rule.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id=$1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id=$1", group.ID)
	})
	return repo, rule, account, group
}

func TestOpenAIWeeklyQuotaResetLinkRepository_ZeroUsersPersistsEmptyArray(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.NewString()
	repo, rule, _, _ := createWeeklyResetIntegrationRule(t, suffix)

	detectedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	firstResetAt := detectedAt.Add(7 * 24 * time.Hour)
	baseline, err := repo.ApplyObservedWeeklyWindow(ctx, service.OpenAIWeeklyQuotaObservation{
		RuleID: rule.ID, OfficialResetAt: firstResetAt,
		OfficialWindowStart: detectedAt, OfficialWindowSeconds: 7 * 24 * 60 * 60,
		DetectedAt: detectedAt,
	})
	require.NoError(t, err)
	require.Equal(t, service.OpenAIWeeklyQuotaObservationBaseline, baseline.Outcome)

	nextResetAt := firstResetAt.Add(7 * 24 * time.Hour)
	result, err := repo.ApplyObservedWeeklyWindow(ctx, service.OpenAIWeeklyQuotaObservation{
		RuleID: rule.ID, OfficialResetAt: nextResetAt,
		OfficialWindowStart: firstResetAt, OfficialWindowSeconds: 7 * 24 * 60 * 60,
		DetectedAt: detectedAt.Add(time.Minute),
	})
	require.NoError(t, err)
	require.Equal(t, service.OpenAIWeeklyQuotaObservationTriggered, result.Outcome)
	require.NotNil(t, result.ResetUserIDs)
	require.Empty(t, result.ResetUserIDs)
	require.Zero(t, result.MatchedUsers)

	var stored []int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT reset_user_ids FROM openai_weekly_quota_reset_executions WHERE id = $1",
		result.ExecutionID,
	).Scan(pq.Array(&stored)))
	require.NotNil(t, stored)
	require.Empty(t, stored)
}

func TestOpenAIWeeklyQuotaResetLinkRepository_SameResetAtDistinctConfirmedEvents(t *testing.T) {
	ctx := context.Background()
	repo, rule, _, _ := createWeeklyResetIntegrationRule(t, uuid.NewString())
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	resetAt := now.Add(7 * 24 * time.Hour)
	identity := service.OpenAIWeeklyQuotaSourceIdentity{Fingerprint: "identity-a", ChatGPTAccountID: "acct-a", ChatGPTUserID: "user-a", PlanType: "pro", IdentitySource: "oauth"}

	baseline, err := repo.ApplyObservedWeeklyWindow(ctx, service.OpenAIWeeklyQuotaObservation{
		RuleID: rule.ID, ResetEventID: "poll-baseline", EventSource: "poll", Identity: identity,
		MeterKey: "codex_weekly", OfficialResetAt: resetAt, OfficialWindowStart: now,
		OfficialWindowSeconds: 7 * 24 * 60 * 60, DetectedAt: now,
	})
	require.NoError(t, err)
	require.Equal(t, service.OpenAIWeeklyQuotaObservationBaseline, baseline.Outcome)

	for _, eventID := range []string{"manual-reset-a", "manual-reset-b"} {
		result, applyErr := repo.ApplyObservedWeeklyWindow(ctx, service.OpenAIWeeklyQuotaObservation{
			RuleID: rule.ID, ResetEventID: eventID, EventSource: "manual_reset",
			EvidenceKind: "authorized_reset_weekly_usage_decreased", Identity: identity,
			MeterKey: "codex_weekly", OfficialResetAt: resetAt, OfficialWindowStart: now,
			OfficialWindowSeconds: 7 * 24 * 60 * 60, DetectedAt: now.Add(time.Minute),
		})
		require.NoError(t, applyErr)
		require.Equal(t, service.OpenAIWeeklyQuotaObservationTriggered, result.Outcome)
	}

	var executions int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM openai_weekly_quota_reset_executions WHERE rule_id=$1", rule.ID,
	).Scan(&executions))
	require.Equal(t, 2, executions, "同 reset_at 的两个确认事件必须可区分")
}

func TestOpenAIWeeklyQuotaResetLinkRepository_ConcurrentSameEventCreatesOneExecution(t *testing.T) {
	ctx := context.Background()
	repo, rule, _, _ := createWeeklyResetIntegrationRule(t, uuid.NewString())
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	resetAt := now.Add(7 * 24 * time.Hour)
	_, err := repo.ApplyObservedWeeklyWindow(ctx, service.OpenAIWeeklyQuotaObservation{
		RuleID: rule.ID, ResetEventID: "baseline", EventSource: "poll",
		OfficialResetAt: resetAt, OfficialWindowStart: now, OfficialWindowSeconds: 7 * 24 * 60 * 60, DetectedAt: now,
	})
	require.NoError(t, err)

	observation := service.OpenAIWeeklyQuotaObservation{
		RuleID: rule.ID, ResetEventID: "shared-event", EventSource: "manual_reset",
		EvidenceKind:    "authorized_reset_weekly_usage_decreased",
		OfficialResetAt: resetAt, OfficialWindowStart: now, OfficialWindowSeconds: 7 * 24 * 60 * 60, DetectedAt: now.Add(time.Minute),
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, applyErr := repo.ApplyObservedWeeklyWindow(ctx, observation)
			errs <- applyErr
		}()
	}
	wg.Wait()
	close(errs)
	for applyErr := range errs {
		require.NoError(t, applyErr)
	}
	var executions int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM openai_weekly_quota_reset_executions WHERE rule_id=$1 AND reset_event_id='shared-event'", rule.ID,
	).Scan(&executions))
	require.Equal(t, 1, executions)
}

func TestOpenAIWeeklyQuotaResetLinkRepository_NewWindowAppliesOneUserGeneration(t *testing.T) {
	ctx := context.Background()
	repo, rule, _, group := createWeeklyResetIntegrationRule(t, uuid.NewString())
	user := mustCreateUser(t, integrationEntClient, &service.User{AllowedGroups: []int64{group.ID}})
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	_, err := integrationDB.ExecContext(ctx, `INSERT INTO user_platform_quotas
		(user_id,platform,weekly_usage_usd,weekly_window_start,daily_window_start,monthly_window_start)
		VALUES ($1,'openai',12.5,$2,$2,$2)`, user.ID, now)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM user_platform_quotas WHERE user_id=$1", user.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM user_allowed_groups WHERE user_id=$1", user.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM users WHERE id=$1", user.ID)
	})

	firstResetAt := now.Add(7 * 24 * time.Hour)
	_, err = repo.ApplyObservedWeeklyWindow(ctx, service.OpenAIWeeklyQuotaObservation{
		RuleID: rule.ID, ResetEventID: "baseline", EventSource: "poll",
		OfficialResetAt: firstResetAt, OfficialWindowStart: now,
		OfficialWindowSeconds: 7 * 24 * 60 * 60, DetectedAt: now,
	})
	require.NoError(t, err)
	result, err := repo.ApplyObservedWeeklyWindow(ctx, service.OpenAIWeeklyQuotaObservation{
		RuleID: rule.ID, ResetEventID: "new-window", EventSource: "poll",
		OfficialResetAt: firstResetAt.Add(7 * 24 * time.Hour), OfficialWindowStart: firstResetAt,
		OfficialWindowSeconds: 7 * 24 * 60 * 60, DetectedAt: now.Add(time.Minute),
	})
	require.NoError(t, err)
	require.Equal(t, []int64{user.ID}, result.ResetUserIDs)
	require.Len(t, result.Targets, 1)
	require.Equal(t, int64(1), result.Targets[0].TargetGeneration)

	target := result.Targets[0]
	require.NoError(t, repo.MarkTargetCachePrepared(ctx, target, now.Add(2*time.Minute)))
	target.Status = service.OpenAIWeeklyQuotaTargetCachePrepared
	require.NoError(t, repo.ApplyTargetDatabaseReset(ctx, target, now.Add(2*time.Minute)))
	target.Status = service.OpenAIWeeklyQuotaTargetDBApplied
	completed, err := repo.MarkTargetSucceeded(ctx, target, now.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, completed)

	var usage float64
	var generation int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT weekly_usage_usd,weekly_quota_generation
		FROM user_platform_quotas WHERE user_id=$1 AND platform='openai'`, user.ID).Scan(&usage, &generation))
	require.Zero(t, usage)
	require.Equal(t, int64(1), generation)
}
