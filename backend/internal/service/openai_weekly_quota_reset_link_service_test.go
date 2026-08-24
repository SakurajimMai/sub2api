package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type weeklyResetRepoStub struct {
	rule       *OpenAIWeeklyQuotaResetRule
	apply      OpenAIWeeklyQuotaObservationResult
	applyCalls int
	completed  int64
	retryable  int64
	retryError string
	ruleError  string
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
func (s *weeklyResetRepoStub) ApplyObservedWeeklyWindow(context.Context, OpenAIWeeklyQuotaObservation) (OpenAIWeeklyQuotaObservationResult, error) {
	s.applyCalls++
	return s.apply, nil
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
	err    error
	users  []int64
	starts []time.Time
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
		weeklyResetUsageReaderStub{usage: &OpenAIQuotaUsage{PlanType: "pro", RateLimit: &OpenAIRateLimit{SecondaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: resetAt.Unix()}}}},
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
		weeklyResetUsageReaderStub{usage: &OpenAIQuotaUsage{PlanType: "pro", RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: resetAt.Unix()}}}},
		cache, nil, true)

	_, err := svc.CheckRuleNow(context.Background(), 9)
	require.ErrorContains(t, err, "redis down")
	require.Equal(t, int64(18), repo.retryable)
	require.Contains(t, repo.retryError, "redis down")
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
	require.Contains(t, repo.ruleError, "Pro")
}
