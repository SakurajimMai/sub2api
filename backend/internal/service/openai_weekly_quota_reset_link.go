package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	openAIWeeklyWindowMinSeconds = int64((6 * 24 * time.Hour) / time.Second)
	openAIWeeklyWindowMaxSeconds = int64((8 * 24 * time.Hour) / time.Second)
	openAIWeeklyWindowSeconds    = int64((7 * 24 * time.Hour) / time.Second)
)

// OpenAIWeeklyQuotaWindow 是从官方用量响应中识别出的七天窗口。
type OpenAIWeeklyQuotaWindow struct {
	ResetAt       time.Time
	WindowStart   time.Time
	WindowSeconds int64
}

const (
	OpenAIWeeklyQuotaObservationBaseline  = "baseline"
	OpenAIWeeklyQuotaObservationUnchanged = "unchanged"
	OpenAIWeeklyQuotaObservationStale     = "stale"
	OpenAIWeeklyQuotaObservationTriggered = "triggered"

	OpenAIWeeklyQuotaExecutionPending         = "pending"
	OpenAIWeeklyQuotaExecutionRunning         = "running"
	OpenAIWeeklyQuotaExecutionSucceeded       = "succeeded"
	OpenAIWeeklyQuotaExecutionRetryableFailed = "retryable_failed"
	OpenAIWeeklyQuotaExecutionPermanentFailed = "permanent_failed"
)

// OpenAIWeeklyQuotaResetRule 将一个官方 OpenAI 账号的七天窗口绑定到用户分组。
type OpenAIWeeklyQuotaResetRule struct {
	ID                        int64      `json:"id"`
	Name                      string     `json:"name"`
	Description               string     `json:"description"`
	Enabled                   bool       `json:"enabled"`
	SourceAccountID           int64      `json:"source_account_id"`
	SourceAccountName         string     `json:"source_account_name,omitempty"`
	TargetGroupID             int64      `json:"target_group_id"`
	TargetGroupName           string     `json:"target_group_name,omitempty"`
	LastObservedResetAt       *time.Time `json:"last_observed_reset_at,omitempty"`
	LastObservedWindowSeconds *int64     `json:"last_observed_window_seconds,omitempty"`
	LastObservedFetchedAt     *time.Time `json:"last_observed_fetched_at,omitempty"`
	LastRunAt                 *time.Time `json:"last_run_at,omitempty"`
	LastError                 string     `json:"last_error,omitempty"`
	CreatedAt                 time.Time  `json:"created_at"`
	UpdatedAt                 time.Time  `json:"updated_at"`
}

type OpenAIWeeklyQuotaResetRuleInput struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	Enabled         bool   `json:"enabled"`
	SourceAccountID int64  `json:"source_account_id"`
	TargetGroupID   int64  `json:"target_group_id"`
}

type OpenAIWeeklyQuotaResetExecution struct {
	ID                    int64      `json:"id"`
	RuleID                int64      `json:"rule_id"`
	RuleName              string     `json:"rule_name,omitempty"`
	SourceAccountID       int64      `json:"source_account_id"`
	TargetGroupID         int64      `json:"target_group_id"`
	OfficialResetAt       time.Time  `json:"official_reset_at"`
	OfficialWindowStart   time.Time  `json:"official_window_start"`
	OfficialWindowSeconds int64      `json:"official_window_seconds"`
	Status                string     `json:"status"`
	MatchedUsers          int        `json:"matched_users"`
	ResetUsers            int        `json:"reset_users"`
	SkippedUsers          int        `json:"skipped_users"`
	ResetUserIDs          []int64    `json:"reset_user_ids,omitempty"`
	ErrorMessage          string     `json:"error_message,omitempty"`
	DetectedAt            time.Time  `json:"detected_at"`
	CompletedAt           *time.Time `json:"completed_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

type OpenAIWeeklyQuotaObservation struct {
	RuleID                int64
	OfficialResetAt       time.Time
	OfficialWindowStart   time.Time
	OfficialWindowSeconds int64
	DetectedAt            time.Time
}

type OpenAIWeeklyQuotaObservationResult struct {
	Outcome      string  `json:"outcome"`
	ExecutionID  int64   `json:"execution_id,omitempty"`
	MatchedUsers int     `json:"matched_users,omitempty"`
	ResetUserIDs []int64 `json:"reset_user_ids,omitempty"`
	SkippedUsers int     `json:"skipped_users,omitempty"`
}

type OpenAIWeeklyQuotaResetLinkRepository interface {
	ListRules(ctx context.Context) ([]OpenAIWeeklyQuotaResetRule, error)
	GetRule(ctx context.Context, id int64) (*OpenAIWeeklyQuotaResetRule, error)
	CreateRule(ctx context.Context, input OpenAIWeeklyQuotaResetRuleInput) (*OpenAIWeeklyQuotaResetRule, error)
	UpdateRule(ctx context.Context, id int64, input OpenAIWeeklyQuotaResetRuleInput) (*OpenAIWeeklyQuotaResetRule, error)
	DeleteRule(ctx context.Context, id int64) error
	ListEnabledRules(ctx context.Context) ([]OpenAIWeeklyQuotaResetRule, error)
	ListExecutions(ctx context.Context, ruleID *int64, limit int) ([]OpenAIWeeklyQuotaResetExecution, error)
	ApplyObservedWeeklyWindow(ctx context.Context, observation OpenAIWeeklyQuotaObservation) (OpenAIWeeklyQuotaObservationResult, error)
	CompleteExecution(ctx context.Context, executionID int64, completedAt time.Time) error
	MarkExecutionRetryableFailed(ctx context.Context, executionID int64, message string, failedAt time.Time) error
	RecordRuleError(ctx context.Context, ruleID int64, message string, checkedAt time.Time) error
}

type openAIWeeklyQuotaAccountReader interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
}

type openAIWeeklyQuotaGroupReader interface {
	GetByID(ctx context.Context, id int64) (*Group, error)
}

type openAIWeeklyQuotaUsageReader interface {
	QueryUsageSnapshot(ctx context.Context, accountID int64) (*OpenAIQuotaUsage, error)
}

type openAIWeeklyQuotaResetCache interface {
	ResetUserPlatformWeeklyQuotaCache(ctx context.Context, userID int64, platform string, newStart time.Time, ttl time.Duration, markDirty bool) (bool, error)
}

var (
	ErrOpenAIWeeklyQuotaResetRuleNotFound = infraerrors.NotFound("OPENAI_WEEKLY_QUOTA_RESET_RULE_NOT_FOUND", "quota reset link rule not found")
	ErrOpenAIWeeklyQuotaResetInvalidInput = infraerrors.BadRequest("OPENAI_WEEKLY_QUOTA_RESET_INVALID_INPUT", "invalid quota reset link rule")
	ErrOpenAIWeeklyQuotaResetProRequired  = infraerrors.BadRequest("OPENAI_WEEKLY_QUOTA_RESET_PRO_REQUIRED", "source account must be an OpenAI Pro account")
	ErrOpenAIWeeklyQuotaWindowUnavailable = infraerrors.BadRequest("OPENAI_WEEKLY_QUOTA_WINDOW_UNAVAILABLE", "official weekly quota window is unavailable")
)

const (
	openAIWeeklyQuotaResetScanInterval = time.Minute
	openAIWeeklyQuotaResetCacheTTL     = 15 * time.Minute
	openAIWeeklyQuotaResetLeaderKey    = "jobs:openai-weekly-quota-reset-link"
)

// OpenAIWeeklyQuotaResetLinkService 负责规则管理和官方窗口主动检测。
type OpenAIWeeklyQuotaResetLinkService struct {
	repo           OpenAIWeeklyQuotaResetLinkRepository
	accounts       openAIWeeklyQuotaAccountReader
	groups         openAIWeeklyQuotaGroupReader
	usage          openAIWeeklyQuotaUsageReader
	cache          openAIWeeklyQuotaResetCache
	leader         LeaderLockCache
	markCacheDirty bool
	owner          string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

func NewOpenAIWeeklyQuotaResetLinkService(
	repo OpenAIWeeklyQuotaResetLinkRepository,
	accounts openAIWeeklyQuotaAccountReader,
	groups openAIWeeklyQuotaGroupReader,
	usage openAIWeeklyQuotaUsageReader,
	cache openAIWeeklyQuotaResetCache,
	leader LeaderLockCache,
	markCacheDirty bool,
) *OpenAIWeeklyQuotaResetLinkService {
	ctx, cancel := context.WithCancel(context.Background())
	return &OpenAIWeeklyQuotaResetLinkService{
		repo: repo, accounts: accounts, groups: groups, usage: usage, cache: cache,
		leader: leader, markCacheDirty: markCacheDirty,
		owner: fmt.Sprintf("weekly-quota-reset-%d", time.Now().UnixNano()),
		ctx:   ctx, cancel: cancel,
	}
}

func (s *OpenAIWeeklyQuotaResetLinkService) ListRules(ctx context.Context) ([]OpenAIWeeklyQuotaResetRule, error) {
	return s.repo.ListRules(ctx)
}

func (s *OpenAIWeeklyQuotaResetLinkService) GetRule(ctx context.Context, id int64) (*OpenAIWeeklyQuotaResetRule, error) {
	rule, err := s.repo.GetRule(ctx, id)
	if err == nil && rule == nil {
		return nil, ErrOpenAIWeeklyQuotaResetRuleNotFound
	}
	return rule, err
}

func (s *OpenAIWeeklyQuotaResetLinkService) CreateRule(ctx context.Context, input OpenAIWeeklyQuotaResetRuleInput) (*OpenAIWeeklyQuotaResetRule, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if err := s.validateRuleInput(ctx, input); err != nil {
		return nil, err
	}
	return s.repo.CreateRule(ctx, input)
}

func (s *OpenAIWeeklyQuotaResetLinkService) UpdateRule(ctx context.Context, id int64, input OpenAIWeeklyQuotaResetRuleInput) (*OpenAIWeeklyQuotaResetRule, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if id <= 0 {
		return nil, ErrOpenAIWeeklyQuotaResetInvalidInput
	}
	if err := s.validateRuleInput(ctx, input); err != nil {
		return nil, err
	}
	rule, err := s.repo.UpdateRule(ctx, id, input)
	if err == nil && rule == nil {
		return nil, ErrOpenAIWeeklyQuotaResetRuleNotFound
	}
	return rule, err
}

func (s *OpenAIWeeklyQuotaResetLinkService) DeleteRule(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrOpenAIWeeklyQuotaResetInvalidInput
	}
	return s.repo.DeleteRule(ctx, id)
}

func (s *OpenAIWeeklyQuotaResetLinkService) ListExecutions(ctx context.Context, ruleID *int64, limit int) ([]OpenAIWeeklyQuotaResetExecution, error) {
	return s.repo.ListExecutions(ctx, ruleID, limit)
}

func (s *OpenAIWeeklyQuotaResetLinkService) validateRuleInput(ctx context.Context, input OpenAIWeeklyQuotaResetRuleInput) error {
	if strings.TrimSpace(input.Name) == "" || input.SourceAccountID <= 0 || input.TargetGroupID <= 0 || s.accounts == nil || s.groups == nil {
		return ErrOpenAIWeeklyQuotaResetInvalidInput
	}
	account, err := s.accounts.GetByID(ctx, input.SourceAccountID)
	if err != nil {
		return err
	}
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || !account.IsActive() || account.IsShadow() {
		return ErrOpenAIWeeklyQuotaResetInvalidInput
	}
	if planType := strings.ToLower(strings.TrimSpace(account.GetCredential("plan_type"))); planType != "" && planType != "pro" {
		return ErrOpenAIWeeklyQuotaResetProRequired
	}
	group, err := s.groups.GetByID(ctx, input.TargetGroupID)
	if err != nil {
		return err
	}
	if group == nil || !group.IsActive() {
		return ErrOpenAIWeeklyQuotaResetInvalidInput
	}
	return nil
}

func (s *OpenAIWeeklyQuotaResetLinkService) CheckRuleNow(ctx context.Context, ruleID int64) (OpenAIWeeklyQuotaObservationResult, error) {
	rule, err := s.GetRule(ctx, ruleID)
	if err != nil {
		return OpenAIWeeklyQuotaObservationResult{}, err
	}
	if err := s.validateRuleBinding(ctx, *rule); err != nil {
		_ = s.repo.RecordRuleError(ctx, rule.ID, err.Error(), time.Now().UTC())
		return OpenAIWeeklyQuotaObservationResult{}, err
	}
	if s.usage == nil {
		return OpenAIWeeklyQuotaObservationResult{}, ErrOpenAIWeeklyQuotaWindowUnavailable
	}
	usage, err := s.usage.QueryUsageSnapshot(ctx, rule.SourceAccountID)
	if err != nil {
		_ = s.repo.RecordRuleError(ctx, rule.ID, err.Error(), time.Now().UTC())
		return OpenAIWeeklyQuotaObservationResult{}, err
	}
	return s.applyUsageToRule(ctx, *rule, usage, time.Now().UTC())
}

func (s *OpenAIWeeklyQuotaResetLinkService) validateRuleBinding(ctx context.Context, rule OpenAIWeeklyQuotaResetRule) error {
	// 测试或离线工具可以不注入校验端口；生产 Wire 始终注入两者。
	if s.accounts == nil || s.groups == nil {
		return nil
	}
	account, err := s.accounts.GetByID(ctx, rule.SourceAccountID)
	if err != nil {
		return err
	}
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || !account.IsActive() || account.IsShadow() {
		return ErrOpenAIWeeklyQuotaResetInvalidInput
	}
	if planType := strings.ToLower(strings.TrimSpace(account.GetCredential("plan_type"))); planType != "" && planType != "pro" {
		return ErrOpenAIWeeklyQuotaResetProRequired
	}
	group, err := s.groups.GetByID(ctx, rule.TargetGroupID)
	if err != nil {
		return err
	}
	if group == nil || !group.IsActive() {
		return ErrOpenAIWeeklyQuotaResetInvalidInput
	}
	return nil
}

func (s *OpenAIWeeklyQuotaResetLinkService) applyUsageToRule(ctx context.Context, rule OpenAIWeeklyQuotaResetRule, usage *OpenAIQuotaUsage, detectedAt time.Time) (OpenAIWeeklyQuotaObservationResult, error) {
	// 本地账号套餐可能尚未同步；每次检测都以本次官方响应做最终资格判断。
	if usage == nil || !strings.EqualFold(strings.TrimSpace(usage.PlanType), "pro") {
		_ = s.repo.RecordRuleError(ctx, rule.ID, ErrOpenAIWeeklyQuotaResetProRequired.Error(), detectedAt)
		return OpenAIWeeklyQuotaObservationResult{}, ErrOpenAIWeeklyQuotaResetProRequired
	}
	window, ok := selectOpenAIWeeklyWindow(usage)
	if !ok {
		_ = s.repo.RecordRuleError(ctx, rule.ID, ErrOpenAIWeeklyQuotaWindowUnavailable.Error(), detectedAt)
		return OpenAIWeeklyQuotaObservationResult{}, ErrOpenAIWeeklyQuotaWindowUnavailable
	}
	result, err := s.repo.ApplyObservedWeeklyWindow(ctx, OpenAIWeeklyQuotaObservation{
		RuleID: rule.ID, OfficialResetAt: window.ResetAt, OfficialWindowStart: window.WindowStart,
		OfficialWindowSeconds: window.WindowSeconds, DetectedAt: detectedAt,
	})
	if err != nil {
		return result, err
	}
	if result.Outcome != OpenAIWeeklyQuotaObservationTriggered {
		return result, nil
	}

	for _, userID := range result.ResetUserIDs {
		if s.cache == nil {
			continue
		}
		_, cacheErr := s.cache.ResetUserPlatformWeeklyQuotaCache(ctx, userID, PlatformOpenAI, window.WindowStart, openAIWeeklyQuotaResetCacheTTL, s.markCacheDirty)
		if cacheErr != nil {
			_ = s.repo.MarkExecutionRetryableFailed(ctx, result.ExecutionID, cacheErr.Error(), time.Now().UTC())
			_ = s.repo.RecordRuleError(ctx, rule.ID, cacheErr.Error(), time.Now().UTC())
			return result, fmt.Errorf("reset user %d weekly quota cache: %w", userID, cacheErr)
		}
	}
	if err := s.repo.CompleteExecution(ctx, result.ExecutionID, time.Now().UTC()); err != nil {
		_ = s.repo.MarkExecutionRetryableFailed(ctx, result.ExecutionID, err.Error(), time.Now().UTC())
		return result, err
	}
	return result, nil
}

func (s *OpenAIWeeklyQuotaResetLinkService) Start() {
	s.once.Do(func() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-s.ctx.Done():
				return
			case <-timer.C:
				s.scan(s.ctx)
			}
			ticker := time.NewTicker(openAIWeeklyQuotaResetScanInterval)
			defer ticker.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
					s.scan(s.ctx)
				}
			}
		}()
	})
}

func (s *OpenAIWeeklyQuotaResetLinkService) Stop() { s.cancel(); s.wg.Wait() }

func (s *OpenAIWeeklyQuotaResetLinkService) scan(ctx context.Context) {
	if s.repo == nil || s.usage == nil {
		return
	}
	release, ok := s.acquireScanLock(ctx)
	if !ok {
		return
	}
	defer release()
	rules, err := s.repo.ListEnabledRules(ctx)
	if err != nil {
		slog.Warn("openai_weekly_quota_reset_rules_scan_failed", "error", err)
		return
	}
	byAccount := make(map[int64]*OpenAIQuotaUsage)
	accountErrors := make(map[int64]error)
	for _, rule := range rules {
		if bindingErr := s.validateRuleBinding(ctx, rule); bindingErr != nil {
			_ = s.repo.RecordRuleError(ctx, rule.ID, bindingErr.Error(), time.Now().UTC())
			continue
		}
		usage, seen := byAccount[rule.SourceAccountID]
		if !seen && accountErrors[rule.SourceAccountID] == nil {
			usage, err = s.usage.QueryUsageSnapshot(ctx, rule.SourceAccountID)
			if err != nil {
				accountErrors[rule.SourceAccountID] = err
			} else {
				byAccount[rule.SourceAccountID] = usage
			}
		}
		if accountErr := accountErrors[rule.SourceAccountID]; accountErr != nil {
			_ = s.repo.RecordRuleError(ctx, rule.ID, accountErr.Error(), time.Now().UTC())
			continue
		}
		if _, err = s.applyUsageToRule(ctx, rule, usage, time.Now().UTC()); err != nil {
			slog.Warn("openai_weekly_quota_reset_rule_check_failed", "rule_id", rule.ID, "error", err)
		}
	}
}

func (s *OpenAIWeeklyQuotaResetLinkService) acquireScanLock(ctx context.Context) (func(), bool) {
	if s.leader == nil {
		return func() {}, true
	}
	ok, err := s.leader.TryAcquireLeaderLock(ctx, openAIWeeklyQuotaResetLeaderKey, s.owner, 55*time.Second)
	if err != nil {
		slog.Warn("openai_weekly_quota_reset_leader_unavailable", "error", err)
		return func() {}, true
	}
	if !ok {
		return nil, false
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.leader.ReleaseLeaderLock(releaseCtx, openAIWeeklyQuotaResetLeaderKey, s.owner)
	}, true
}

func selectOpenAIWeeklyWindow(usage *OpenAIQuotaUsage) (OpenAIWeeklyQuotaWindow, bool) {
	if usage == nil || usage.RateLimit == nil {
		return OpenAIWeeklyQuotaWindow{}, false
	}

	var selected *OpenAIRateLimitWindow
	selectedDistance := int64(0)
	for _, candidate := range []*OpenAIRateLimitWindow{
		usage.RateLimit.PrimaryWindow,
		usage.RateLimit.SecondaryWindow,
	} {
		if candidate == nil || candidate.ResetAt <= 0 ||
			candidate.LimitWindowSeconds < openAIWeeklyWindowMinSeconds ||
			candidate.LimitWindowSeconds > openAIWeeklyWindowMaxSeconds {
			continue
		}
		distance := candidate.LimitWindowSeconds - openAIWeeklyWindowSeconds
		if distance < 0 {
			distance = -distance
		}
		if selected == nil || distance < selectedDistance {
			selected = candidate
			selectedDistance = distance
		}
	}
	if selected == nil {
		return OpenAIWeeklyQuotaWindow{}, false
	}

	resetAt := time.Unix(selected.ResetAt, 0).UTC()
	return OpenAIWeeklyQuotaWindow{
		ResetAt:       resetAt,
		WindowStart:   resetAt.Add(-time.Duration(selected.LimitWindowSeconds) * time.Second),
		WindowSeconds: selected.LimitWindowSeconds,
	}, true
}
