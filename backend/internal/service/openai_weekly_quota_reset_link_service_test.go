package service

import (
	"context"
	"errors"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type weeklyResetRepoStub struct {
	rule               *OpenAIWeeklyQuotaResetRule
	apply              OpenAIWeeklyQuotaObservationResult
	applyCalls         int
	completed          int64
	retryable          int64
	retryError         string
	ruleError          string
	applyErr           error
	querySuccesses     []OpenAIWeeklyQuotaSnapshot
	queryFailures      []OpenAIWeeklyQuotaFailure
	executionFailures  []OpenAIWeeklyQuotaFailure
	executionSuccesses int
	rebaselineCalls    int
	observations       []OpenAIWeeklyQuotaObservation
}

func (s *weeklyResetRepoStub) ListRules(context.Context) ([]OpenAIWeeklyQuotaResetRule, error) {
	return nil, nil
}
func (s *weeklyResetRepoStub) GetRule(context.Context, int64) (*OpenAIWeeklyQuotaResetRule, error) {
	return s.rule, nil
}
func (s *weeklyResetRepoStub) CreateRule(_ context.Context, in OpenAIWeeklyQuotaResetRuleInput) (*OpenAIWeeklyQuotaResetRule, error) {
	return &OpenAIWeeklyQuotaResetRule{Name: in.Name}, nil
}
func (s *weeklyResetRepoStub) UpdateRule(_ context.Context, _ int64, in OpenAIWeeklyQuotaResetRuleInput) (*OpenAIWeeklyQuotaResetRule, error) {
	return &OpenAIWeeklyQuotaResetRule{Name: in.Name}, nil
}
func (s *weeklyResetRepoStub) DeleteRule(context.Context, int64) error { return nil }
func (s *weeklyResetRepoStub) ListEnabledRules(context.Context) ([]OpenAIWeeklyQuotaResetRule, error) {
	if s.rule == nil {
		return nil, nil
	}
	return []OpenAIWeeklyQuotaResetRule{*s.rule}, nil
}
func (s *weeklyResetRepoStub) ListExecutions(context.Context, *int64, int) ([]OpenAIWeeklyQuotaResetExecution, error) {
	return nil, nil
}
func (s *weeklyResetRepoStub) ApplyObservedWeeklyWindow(_ context.Context, observation OpenAIWeeklyQuotaObservation) (OpenAIWeeklyQuotaObservationResult, error) {
	s.applyCalls++
	s.observations = append(s.observations, observation)
	return s.apply, s.applyErr
}

func (s *weeklyResetRepoStub) RecordQuerySuccess(_ context.Context, _ int64, snapshot OpenAIWeeklyQuotaSnapshot) error {
	s.querySuccesses = append(s.querySuccesses, snapshot)
	return nil
}

func (s *weeklyResetRepoStub) RecordQueryFailure(_ context.Context, _ int64, failure OpenAIWeeklyQuotaFailure) error {
	s.queryFailures = append(s.queryFailures, failure)
	return nil
}

func (s *weeklyResetRepoStub) RecordExecutionFailure(_ context.Context, _ int64, _ int64, failure OpenAIWeeklyQuotaFailure) error {
	s.executionFailures = append(s.executionFailures, failure)
	return nil
}

func (s *weeklyResetRepoStub) RecordExecutionSuccess(context.Context, int64, int64, time.Time) error {
	s.executionSuccesses++
	return nil
}

func (s *weeklyResetRepoStub) RebaselineRuleIdentity(_ context.Context, _ int64, _ OpenAIWeeklyQuotaSourceIdentity, _ OpenAIWeeklyQuotaSnapshot) error {
	s.rebaselineCalls++
	return nil
}
func (s *weeklyResetRepoStub) CompleteExecution(_ context.Context, id int64, _ time.Time) error {
	s.completed = id
	return nil
}
func (s *weeklyResetRepoStub) MarkExecutionRetryableFailed(_ context.Context, id int64, message string, _ time.Time) error {
	s.retryable, s.retryError = id, message
	return nil
}
func (s *weeklyResetRepoStub) RecordRuleError(_ context.Context, _ int64, message string, _ time.Time) error {
	s.ruleError = message
	return nil
}

type weeklyResetAccountReaderStub struct{ account *Account }

func (s weeklyResetAccountReaderStub) GetByID(context.Context, int64) (*Account, error) {
	return s.account, nil
}

type weeklyResetGroupReaderStub struct{ group *Group }

func (s weeklyResetGroupReaderStub) GetByID(context.Context, int64) (*Group, error) {
	return s.group, nil
}

type weeklyResetUsageReaderStub struct{ usage *OpenAIQuotaUsage }

func (s weeklyResetUsageReaderStub) QueryUsageSnapshot(context.Context, int64) (*OpenAIQuotaUsage, error) {
	return s.usage, nil
}

type weeklyResetCacheStub struct {
	err           error
	users         []int64
	starts        []time.Time
	prepareCalls  int
	finalizeCalls int
	finalizeErr   error
}

func (s *weeklyResetCacheStub) PrepareUserPlatformWeeklyQuotaReset(context.Context, int64, string, string, int64, time.Duration) (bool, error) {
	s.prepareCalls++
	return true, nil
}

func (s *weeklyResetCacheStub) FinalizeUserPlatformWeeklyQuotaReset(_ context.Context, _ int64, _ string, _ string, _ int64, _ time.Time, _ time.Duration, _ bool) (bool, error) {
	s.finalizeCalls++
	return true, s.finalizeErr
}

type weeklyTargetRepoStub struct {
	*weeklyResetRepoStub
	target       serviceTargetState
	claimCalls   int
	dbApplyCalls int
	retryCalls   int
	successCalls int
}

type serviceTargetState struct {
	target OpenAIWeeklyQuotaResetTarget
	done   bool
}

func (s *weeklyTargetRepoStub) ClaimResetTargets(_ context.Context, _ int64, _ string, _ int, _ time.Time, _ time.Duration) ([]OpenAIWeeklyQuotaResetTarget, error) {
	s.claimCalls++
	if s.target.done {
		return nil, nil
	}
	return []OpenAIWeeklyQuotaResetTarget{s.target.target}, nil
}

func (s *weeklyTargetRepoStub) MarkTargetCachePrepared(context.Context, OpenAIWeeklyQuotaResetTarget, time.Time) error {
	s.target.target.Status = OpenAIWeeklyQuotaTargetCachePrepared
	return nil
}

func (s *weeklyTargetRepoStub) ApplyTargetDatabaseReset(context.Context, OpenAIWeeklyQuotaResetTarget, time.Time) error {
	s.dbApplyCalls++
	s.target.target.Status = OpenAIWeeklyQuotaTargetDBApplied
	return nil
}

func (s *weeklyTargetRepoStub) MarkTargetSucceeded(context.Context, OpenAIWeeklyQuotaResetTarget, time.Time) (bool, error) {
	s.successCalls++
	s.target.done = true
	return true, nil
}

func (s *weeklyTargetRepoStub) MarkTargetRetryable(_ context.Context, target OpenAIWeeklyQuotaResetTarget, _ OpenAIWeeklyQuotaFailure) error {
	s.retryCalls++
	s.target.target = target
	return nil
}

func (s *weeklyResetCacheStub) ResetUserPlatformWeeklyQuotaCache(_ context.Context, userID int64, _ string, start time.Time, _ time.Duration, _ bool) (bool, error) {
	s.users = append(s.users, userID)
	s.starts = append(s.starts, start)
	return true, s.err
}

func TestOpenAIWeeklyQuotaResetLinkService_CheckRuleResetsCache(t *testing.T) {
	resetAt := time.Date(2026, 8, 31, 1, 30, 0, 0, time.UTC)
	repo := &weeklyResetRepoStub{
		rule:  &OpenAIWeeklyQuotaResetRule{ID: 9, Enabled: true, SourceAccountID: 5, TargetGroupID: 3},
		apply: OpenAIWeeklyQuotaObservationResult{Outcome: OpenAIWeeklyQuotaObservationTriggered, ExecutionID: 17, ResetUserIDs: []int64{11, 12}},
	}
	cache := &weeklyResetCacheStub{}
	svc := NewOpenAIWeeklyQuotaResetLinkService(repo, nil, nil,
		weeklyResetUsageReaderStub{usage: &OpenAIQuotaUsage{AccountID: "acct-5", UserID: "user-5", CredentialIdentity: "chatgpt:acct-5:user:user-5", PlanType: "pro", RateLimit: &OpenAIRateLimit{SecondaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: resetAt.Unix()}}}},
		cache, nil, true)

	result, err := svc.CheckRuleNow(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, OpenAIWeeklyQuotaObservationTriggered, result.Outcome)
	require.Equal(t, []int64{11, 12}, cache.users)
	require.Equal(t, resetAt.Add(-7*24*time.Hour), cache.starts[0])
	require.Equal(t, int64(17), repo.completed)
}

func TestOpenAIWeeklyQuotaResetLinkService_CacheFailureIsRetryable(t *testing.T) {
	resetAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	repo := &weeklyResetRepoStub{rule: &OpenAIWeeklyQuotaResetRule{ID: 9, Enabled: true, SourceAccountID: 5}, apply: OpenAIWeeklyQuotaObservationResult{Outcome: OpenAIWeeklyQuotaObservationTriggered, ExecutionID: 18, ResetUserIDs: []int64{11}}}
	cache := &weeklyResetCacheStub{err: errors.New("redis down")}
	svc := NewOpenAIWeeklyQuotaResetLinkService(repo, nil, nil,
		weeklyResetUsageReaderStub{usage: &OpenAIQuotaUsage{AccountID: "acct-5", UserID: "user-5", CredentialIdentity: "chatgpt:acct-5:user:user-5", PlanType: "pro", RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: resetAt.Unix()}}}},
		cache, nil, true)

	_, err := svc.CheckRuleNow(context.Background(), 9)
	require.Equal(t, "OPENAI_WEEKLY_QUOTA_CACHE_FAILED", infraerrors.Reason(err))
	require.Equal(t, int64(18), repo.retryable)
	require.Equal(t, "Failed to synchronize the weekly quota cache", repo.retryError)
}

func TestOpenAIWeeklyQuotaResetLinkService_RejectsShadowAccount(t *testing.T) {
	parent := int64(1)
	svc := NewOpenAIWeeklyQuotaResetLinkService(&weeklyResetRepoStub{},
		weeklyResetAccountReaderStub{account: &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, ParentAccountID: &parent}},
		weeklyResetGroupReaderStub{group: &Group{ID: 3, Status: StatusActive}}, nil, nil, nil, false)

	_, err := svc.CreateRule(context.Background(), OpenAIWeeklyQuotaResetRuleInput{Name: "link", SourceAccountID: 2, TargetGroupID: 3})
	require.Error(t, err)
}

func TestOpenAIWeeklyQuotaResetLinkService_RejectsKnownNonProAccount(t *testing.T) {
	svc := NewOpenAIWeeklyQuotaResetLinkService(&weeklyResetRepoStub{},
		weeklyResetAccountReaderStub{account: &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"plan_type": "plus"}}},
		weeklyResetGroupReaderStub{group: &Group{ID: 3, Status: StatusActive}}, nil, nil, nil, false)

	_, err := svc.CreateRule(context.Background(), OpenAIWeeklyQuotaResetRuleInput{Name: "link", SourceAccountID: 2, TargetGroupID: 3})
	require.ErrorIs(t, err, ErrOpenAIWeeklyQuotaResetProRequired)
}

func TestOpenAIWeeklyQuotaResetLinkService_OfficialNonProNeverTriggersReset(t *testing.T) {
	resetAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	repo := &weeklyResetRepoStub{
		rule:  &OpenAIWeeklyQuotaResetRule{ID: 9, Enabled: true, SourceAccountID: 5, TargetGroupID: 3},
		apply: OpenAIWeeklyQuotaObservationResult{Outcome: OpenAIWeeklyQuotaObservationTriggered, ExecutionID: 19, ResetUserIDs: []int64{11}},
	}
	cache := &weeklyResetCacheStub{}
	svc := NewOpenAIWeeklyQuotaResetLinkService(repo, nil, nil,
		weeklyResetUsageReaderStub{usage: &OpenAIQuotaUsage{PlanType: "team", RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: resetAt.Unix()}}}},
		cache, nil, true)

	_, err := svc.CheckRuleNow(context.Background(), 9)
	require.ErrorIs(t, err, ErrOpenAIWeeklyQuotaResetProRequired)
	require.Zero(t, repo.applyCalls)
	require.Zero(t, repo.completed)
	require.Empty(t, cache.users)
	require.Len(t, repo.queryFailures, 1)
	require.Equal(t, "OPENAI_WEEKLY_QUOTA_RESET_PRO_REQUIRED", repo.queryFailures[0].Reason)
}

func TestOpenAIWeeklyQuotaResetLinkService_QuerySuccessDatabaseFailureKeepsStagesSeparate(t *testing.T) {
	resetAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	repo := &weeklyResetRepoStub{
		rule:     &OpenAIWeeklyQuotaResetRule{ID: 9, SourceAccountID: 5, TargetGroupID: 3},
		applyErr: errors.New("pq: sensitive database detail"),
	}
	svc := NewOpenAIWeeklyQuotaResetLinkService(repo, nil, nil,
		weeklyResetUsageReaderStub{usage: &OpenAIQuotaUsage{
			AccountID: "acct", UserID: "user", PlanType: "pro", SnapshotSource: "wham_usage",
			RateLimit: &OpenAIRateLimit{SecondaryWindow: &OpenAIRateLimitWindow{
				UsedPercent: 55, UsedPercentKnown: true, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: resetAt.Unix(),
			}},
		}}, nil, nil, false)

	_, err := svc.CheckRuleNow(context.Background(), 9)
	require.Error(t, err)
	require.Len(t, repo.querySuccesses, 1)
	require.Empty(t, repo.queryFailures)
	require.Len(t, repo.executionFailures, 1)
	require.Equal(t, OpenAIWeeklyQuotaStageDatabase, repo.executionFailures[0].Stage)
	require.Equal(t, "OPENAI_WEEKLY_QUOTA_DATABASE_FAILED", repo.executionFailures[0].Reason)
	require.NotContains(t, repo.executionFailures[0].Message, "sensitive")
}

func TestBuildOpenAIWeeklyQuotaSourceAccountIsCredentialSafe(t *testing.T) {
	verified := "2026-09-01T08:00:00Z"
	account := &Account{
		ID: 42, Name: "production-pro", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "acct-123", "chatgpt_user_id": "user-456",
			"email": "real@example.com", "plan_type": "pro",
			"access_token": "secret-access", "refresh_token": "secret-refresh",
		},
		Extra: map[string]any{"codex_usage_updated_at": verified},
	}

	dto := buildOpenAIWeeklyQuotaSourceAccount(account)
	require.Equal(t, int64(42), dto.LocalAccountID)
	require.Equal(t, "production-pro", dto.LocalAccountName)
	require.Equal(t, "acct-123", dto.ChatGPTAccountID)
	require.Equal(t, "user-456", dto.ChatGPTUserID)
	require.Equal(t, "real@example.com", dto.Email)
	require.Equal(t, "pro", dto.PlanType)
	require.Equal(t, "oauth", dto.IdentitySource)
	require.NotNil(t, dto.LastVerifiedAt)
	require.NotContains(t, dto.LocalAccountName, "secret")
}

func TestOpenAIWeeklyQuotaResetLinkService_IdentityChangeRebaselinesWithoutReset(t *testing.T) {
	resetAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	repo := &weeklyResetRepoStub{rule: &OpenAIWeeklyQuotaResetRule{
		ID: 9, SourceAccountID: 5, TargetGroupID: 3, SourceIdentityFingerprint: "old-fingerprint",
	}}
	svc := NewOpenAIWeeklyQuotaResetLinkService(repo, nil, nil,
		weeklyResetUsageReaderStub{usage: &OpenAIQuotaUsage{
			AccountID: "new-account", UserID: "new-user", PlanType: "pro", SnapshotSource: "wham_usage",
			RateLimit: &OpenAIRateLimit{SecondaryWindow: &OpenAIRateLimitWindow{
				LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: resetAt.Unix(),
			}},
		}}, nil, nil, false)

	result, err := svc.CheckRuleNow(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, OpenAIWeeklyQuotaObservationIdentityChanged, result.Outcome)
	require.Equal(t, 1, repo.rebaselineCalls)
	require.Zero(t, repo.applyCalls)
}

func TestOpenAIWeeklyQuotaResetLinkService_RestartCompensatesCacheOnlyAfterDBApplied(t *testing.T) {
	baseRepo := &weeklyResetRepoStub{}
	targetRepo := &weeklyTargetRepoStub{
		weeklyResetRepoStub: baseRepo,
		target: serviceTargetState{target: OpenAIWeeklyQuotaResetTarget{
			ID: 31, ExecutionID: 17, RuleID: 9, ResetEventID: "event-5", UserID: 11,
			Platform: PlatformOpenAI, PreviousGeneration: 4, TargetGeneration: 5,
			QuotaWindowStart: time.Now().UTC(), Status: OpenAIWeeklyQuotaTargetDBApplied,
		}},
	}
	cache := &weeklyResetCacheStub{finalizeErr: errors.New("redis unavailable")}
	svc := NewOpenAIWeeklyQuotaResetLinkService(targetRepo, nil, nil, nil, cache, nil, true)

	err := svc.processResetTargets(context.Background(), targetRepo, 17)
	require.Error(t, err)
	require.Zero(t, targetRepo.dbApplyCalls, "DB 已应用后不得再次清零")
	require.Equal(t, 1, cache.finalizeCalls)
	require.Equal(t, 1, targetRepo.retryCalls)

	cache.finalizeErr = nil
	err = svc.processResetTargets(context.Background(), targetRepo, 17)
	require.NoError(t, err)
	require.Zero(t, targetRepo.dbApplyCalls, "重启补偿仍不得再次清零 DB")
	require.Equal(t, 2, cache.finalizeCalls)
	require.Equal(t, 1, targetRepo.successCalls)
	require.Equal(t, 1, baseRepo.executionSuccesses)
}

func TestOpenAIWeeklyQuotaResetLinkService_AuthorizedWindowAdvanceSharesPollEventID(t *testing.T) {
	now := time.Now().UTC()
	weeklyUsage := func(resetAt time.Time, used float64) *OpenAIQuotaUsage {
		return &OpenAIQuotaUsage{AccountID: "acct", UserID: "user", PlanType: "pro", SnapshotSource: "wham_usage", RateLimit: &OpenAIRateLimit{
			SecondaryWindow: &OpenAIRateLimitWindow{UsedPercent: used, UsedPercentKnown: true, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: resetAt.Unix()},
		}}
	}
	repo := &weeklyResetRepoStub{
		rule:  &OpenAIWeeklyQuotaResetRule{ID: 9, Enabled: true, SourceAccountID: 5},
		apply: OpenAIWeeklyQuotaObservationResult{Outcome: OpenAIWeeklyQuotaObservationUnchanged},
	}
	svc := NewOpenAIWeeklyQuotaResetLinkService(repo, nil, nil, nil, nil, nil, false)
	before := weeklyUsage(now.Add(24*time.Hour), 80)
	after := weeklyUsage(now.Add(8*24*time.Hour), 0)

	decision, err := svc.ObserveAuthorizedResetOperation(context.Background(), OpenAIWeeklyQuotaResetOperationEvidence{
		SourceAccountID: 5, OperationID: "manual-operation", EventSource: "manual_reset",
		Before: before, After: after, WindowsReset: 2, ObservedAt: now,
	})
	require.NoError(t, err)
	require.True(t, decision.Confirmed)
	require.Len(t, repo.observations, 1)
	expected, err := buildOpenAIWeeklyQuotaSnapshot(after, now)
	require.NoError(t, err)
	require.Equal(t, expected.EventID, repo.observations[0].ResetEventID)
	require.Contains(t, repo.observations[0].ResetEventID, "poll:")
}
