package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
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
	UsedPercent   *float64
	MeterKey      string
	Source        string
}

const (
	OpenAIWeeklyQuotaObservationBaseline        = "baseline"
	OpenAIWeeklyQuotaObservationUnchanged       = "unchanged"
	OpenAIWeeklyQuotaObservationStale           = "stale"
	OpenAIWeeklyQuotaObservationTriggered       = "triggered"
	OpenAIWeeklyQuotaObservationIdentityChanged = "identity_changed"

	OpenAIWeeklyQuotaExecutionPending         = "pending"
	OpenAIWeeklyQuotaExecutionRunning         = "running"
	OpenAIWeeklyQuotaExecutionSucceeded       = "succeeded"
	OpenAIWeeklyQuotaExecutionRetryableFailed = "retryable_failed"
	OpenAIWeeklyQuotaExecutionPermanentFailed = "permanent_failed"
	OpenAIWeeklyQuotaTargetPending            = "weekly_pending"
	OpenAIWeeklyQuotaTargetCachePrepared      = "weekly_cache_prepared"
	OpenAIWeeklyQuotaTargetDBApplied          = "weekly_db_applied"
	OpenAIWeeklyQuotaTargetSucceeded          = "succeeded"
)

const (
	OpenAIWeeklyQuotaStageBinding       = "binding"
	OpenAIWeeklyQuotaStageCredentials   = "credentials"
	OpenAIWeeklyQuotaStageUpstreamQuery = "upstream_query"
	OpenAIWeeklyQuotaStageResponseParse = "response_parse"
	OpenAIWeeklyQuotaStageDatabase      = "database"
	OpenAIWeeklyQuotaStageRedisSync     = "redis_sync"
)

type OpenAIWeeklyQuotaFailure struct {
	Stage     string     `json:"stage"`
	Reason    string     `json:"reason"`
	Message   string     `json:"message"`
	RequestID string     `json:"request_id,omitempty"`
	At        time.Time  `json:"at"`
	RetryAt   *time.Time `json:"-"`
}

type OpenAIWeeklyQuotaSourceIdentity struct {
	Fingerprint      string     `json:"-"`
	ChatGPTAccountID string     `json:"chatgpt_account_id,omitempty"`
	ChatGPTUserID    string     `json:"chatgpt_user_id,omitempty"`
	Email            string     `json:"email,omitempty"`
	PlanType         string     `json:"plan_type,omitempty"`
	IdentitySource   string     `json:"identity_source"`
	VerifiedAt       *time.Time `json:"verified_at,omitempty"`
}

type OpenAIWeeklyQuotaSnapshot struct {
	Identity          OpenAIWeeklyQuotaSourceIdentity
	MeterKey          string
	ResetAt           time.Time
	WindowStart       time.Time
	WindowSeconds     int64
	UsedPercent       *float64
	ObservedAt        time.Time
	Source            string
	CredentialVersion int64
	EventID           string
}

type OpenAIWeeklyQuotaResetEvidenceDecision struct {
	Confirmed    bool
	EvidenceKind string
	Reason       string
}

type OpenAIWeeklyQuotaResetOperationEvidence struct {
	SourceAccountID int64
	OperationID     string
	EventSource     string
	Before          *OpenAIQuotaUsage
	After           *OpenAIQuotaUsage
	WindowsReset    int
	ObservedAt      time.Time
}

type OpenAIWeeklyQuotaResetEvent struct {
	ID                  int64      `json:"id"`
	SourceAccountID     int64      `json:"source_account_id"`
	SourceAccountName   string     `json:"source_account_name,omitempty"`
	EventSource         string     `json:"event_source"`
	EvidenceKind        string     `json:"evidence_kind"`
	Status              string     `json:"status"`
	Reason              string     `json:"reason,omitempty"`
	OfficialResetAt     *time.Time `json:"official_reset_at,omitempty"`
	PreUsedPercent      *float64   `json:"pre_used_percent,omitempty"`
	PostUsedPercent     *float64   `json:"post_used_percent,omitempty"`
	ObservedAt          time.Time  `json:"observed_at"`
	ConfirmedAt         *time.Time `json:"confirmed_at,omitempty"`
	ResetEventID        string     `json:"-"`
	IdentityFingerprint string     `json:"-"`
	MeterKey            string     `json:"-"`
	WindowStart         *time.Time `json:"-"`
	WindowSeconds       *int64     `json:"-"`
	DispatchStatus      string     `json:"dispatch_status,omitempty"`
	LeaseOwner          string     `json:"-"`
}

type openAIWeeklyQuotaEventRepository interface {
	RecordResetEvent(ctx context.Context, eventID string, evidence OpenAIWeeklyQuotaResetOperationEvidence, decision OpenAIWeeklyQuotaResetEvidenceDecision, snapshot *OpenAIWeeklyQuotaSnapshot) error
}

type openAIWeeklyQuotaEventLister interface {
	ListResetEvents(ctx context.Context, limit int) ([]OpenAIWeeklyQuotaResetEvent, error)
}

type openAIWeeklyQuotaEventDispatcherRepository interface {
	ClaimResetEvents(ctx context.Context, owner string, limit int, now time.Time, lease time.Duration) ([]OpenAIWeeklyQuotaResetEvent, error)
	ConfirmResetEvent(ctx context.Context, eventID int64, owner string, snapshot OpenAIWeeklyQuotaSnapshot, evidenceKind string, at time.Time) error
	MarkResetEventDispatched(ctx context.Context, eventID int64, owner string, at time.Time) error
	RejectResetEvent(ctx context.Context, eventID int64, owner, reason string, at time.Time) error
	RetryResetEvent(ctx context.Context, eventID int64, owner, reason string, at, retryAt time.Time) error
}

type openAIWeeklyQuotaEventRuleRepository interface {
	ListResetEventRules(ctx context.Context, resetEventID string, sourceAccountID int64) ([]OpenAIWeeklyQuotaResetRule, error)
}

type OpenAIWeeklyQuotaSourceAccount struct {
	LocalAccountID    int64      `json:"local_account_id"`
	LocalAccountName  string     `json:"local_account_name"`
	ChatGPTAccountID  string     `json:"chatgpt_account_id,omitempty"`
	ChatGPTUserID     string     `json:"chatgpt_user_id,omitempty"`
	Email             string     `json:"email,omitempty"`
	PlanType          string     `json:"plan_type,omitempty"`
	IdentitySource    string     `json:"identity_source"`
	LastVerifiedAt    *time.Time `json:"last_verified_at,omitempty"`
	Supported         bool       `json:"supported"`
	UnsupportedReason string     `json:"unsupported_reason,omitempty"`
}

// OpenAIWeeklyQuotaResetRule 将一个官方 OpenAI 账号的七天窗口绑定到用户分组。
type OpenAIWeeklyQuotaResetRule struct {
	ID                        int64                           `json:"id"`
	Name                      string                          `json:"name"`
	Description               string                          `json:"description"`
	Enabled                   bool                            `json:"enabled"`
	SourceAccountID           int64                           `json:"source_account_id"`
	SourceAccountName         string                          `json:"source_account_name,omitempty"`
	SourceIdentityFingerprint string                          `json:"-"`
	SourceIdentity            *OpenAIWeeklyQuotaSourceAccount `json:"source_identity,omitempty"`
	TargetGroupID             int64                           `json:"target_group_id"`
	TargetGroupName           string                          `json:"target_group_name,omitempty"`
	LastObservedResetAt       *time.Time                      `json:"last_observed_reset_at,omitempty"`
	LastObservedWindowSeconds *int64                          `json:"last_observed_window_seconds,omitempty"`
	LastObservedFetchedAt     *time.Time                      `json:"last_observed_fetched_at,omitempty"`
	LastRunAt                 *time.Time                      `json:"last_run_at,omitempty"`
	LastError                 string                          `json:"last_error,omitempty"`
	LastAttemptAt             *time.Time                      `json:"last_attempt_at,omitempty"`
	LastQuerySuccessAt        *time.Time                      `json:"last_query_success_at,omitempty"`
	LastExecutionSuccessAt    *time.Time                      `json:"last_execution_success_at,omitempty"`
	QueryStatus               string                          `json:"query_status"`
	QueryFailure              *OpenAIWeeklyQuotaFailure       `json:"query_failure,omitempty"`
	ExecutionStatus           string                          `json:"execution_status"`
	ExecutionFailure          *OpenAIWeeklyQuotaFailure       `json:"execution_failure,omitempty"`
	LastSnapshotMeterKey      string                          `json:"last_snapshot_meter_key,omitempty"`
	LastSnapshotSource        string                          `json:"last_snapshot_source,omitempty"`
	LastSnapshotObservedAt    *time.Time                      `json:"last_snapshot_observed_at,omitempty"`
	LastSnapshotUsedPercent   *float64                        `json:"last_snapshot_used_percent,omitempty"`
	LastSnapshotEventID       string                          `json:"last_snapshot_event_id,omitempty"`
	NextQueryAt               *time.Time                      `json:"next_query_at,omitempty"`
	CreatedAt                 time.Time                       `json:"created_at"`
	UpdatedAt                 time.Time                       `json:"updated_at"`
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
	ResetEventID          string     `json:"reset_event_id,omitempty"`
	EventSource           string     `json:"event_source,omitempty"`
	EvidenceKind          string     `json:"evidence_kind,omitempty"`
	OfficialResetAt       time.Time  `json:"official_reset_at"`
	OfficialWindowStart   time.Time  `json:"official_window_start"`
	OfficialWindowSeconds int64      `json:"official_window_seconds"`
	Status                string     `json:"status"`
	MatchedUsers          int        `json:"matched_users"`
	ResetUsers            int        `json:"reset_users"`
	SkippedUsers          int        `json:"skipped_users"`
	ResetUserIDs          []int64    `json:"reset_user_ids,omitempty"`
	ErrorMessage          string     `json:"error_message,omitempty"`
	Stage                 string     `json:"stage,omitempty"`
	ErrorReason           string     `json:"error_reason,omitempty"`
	ErrorRequestID        string     `json:"error_request_id,omitempty"`
	DetectedAt            time.Time  `json:"detected_at"`
	CompletedAt           *time.Time `json:"completed_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

type OpenAIWeeklyQuotaObservation struct {
	RuleID                int64
	ResetEventID          string
	EventSource           string
	EvidenceKind          string
	Identity              OpenAIWeeklyQuotaSourceIdentity
	MeterKey              string
	UsedPercent           *float64
	OfficialResetAt       time.Time
	OfficialWindowStart   time.Time
	OfficialWindowSeconds int64
	DetectedAt            time.Time
}

type OpenAIWeeklyQuotaObservationResult struct {
	Outcome        string                         `json:"outcome"`
	ExecutionID    int64                          `json:"execution_id,omitempty"`
	MatchedUsers   int                            `json:"matched_users,omitempty"`
	UsersWithQuota int                            `json:"users_with_quota,omitempty"`
	NoQuotaUsers   int                            `json:"no_quota_users,omitempty"`
	DuplicateUsers int                            `json:"duplicate_users,omitempty"`
	ZeroReason     string                         `json:"zero_reason,omitempty"`
	ResetUserIDs   []int64                        `json:"reset_user_ids,omitempty"`
	Targets        []OpenAIWeeklyQuotaResetTarget `json:"-"`
	SkippedUsers   int                            `json:"skipped_users,omitempty"`
}

type OpenAIWeeklyQuotaResetTarget struct {
	ID                 int64
	ExecutionID        int64
	RuleID             int64
	ResetEventID       string
	UserID             int64
	Platform           string
	PreviousGeneration int64
	TargetGeneration   int64
	QuotaWindowStart   time.Time
	Status             string
	LeaseOwner         string
}

type openAIWeeklyQuotaTargetRepository interface {
	ClaimResetTargets(ctx context.Context, executionID int64, owner string, limit int, now time.Time, lease time.Duration) ([]OpenAIWeeklyQuotaResetTarget, error)
	MarkTargetCachePrepared(ctx context.Context, target OpenAIWeeklyQuotaResetTarget, at time.Time) error
	ApplyTargetDatabaseReset(ctx context.Context, target OpenAIWeeklyQuotaResetTarget, at time.Time) error
	MarkTargetSucceeded(ctx context.Context, target OpenAIWeeklyQuotaResetTarget, at time.Time) (executionCompleted bool, error error)
	MarkTargetRetryable(ctx context.Context, target OpenAIWeeklyQuotaResetTarget, failure OpenAIWeeklyQuotaFailure) error
}

type openAIWeeklyQuotaGenerationCache interface {
	PrepareUserPlatformWeeklyQuotaReset(ctx context.Context, userID int64, platform, eventID string, targetGeneration int64, ttl time.Duration) (bool, error)
	FinalizeUserPlatformWeeklyQuotaReset(ctx context.Context, userID int64, platform, eventID string, targetGeneration int64, newStart time.Time, ttl time.Duration, markDirty bool) (bool, error)
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
	RecordQuerySuccess(ctx context.Context, ruleID int64, snapshot OpenAIWeeklyQuotaSnapshot) error
	RecordQueryFailure(ctx context.Context, ruleID int64, failure OpenAIWeeklyQuotaFailure) error
	RecordExecutionFailure(ctx context.Context, ruleID, executionID int64, failure OpenAIWeeklyQuotaFailure) error
	RecordExecutionSuccess(ctx context.Context, ruleID, executionID int64, completedAt time.Time) error
	RebaselineRuleIdentity(ctx context.Context, ruleID int64, identity OpenAIWeeklyQuotaSourceIdentity, snapshot OpenAIWeeklyQuotaSnapshot) error
}

type openAIWeeklyQuotaAccountReader interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
}

type openAIWeeklyQuotaAccountLister interface {
	ListAllWithFilters(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode string) ([]Account, error)
}

type openAIWeeklyQuotaVerificationReader interface {
	LatestRuleVerificationAt(ctx context.Context, accountID int64) (*time.Time, error)
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
	ErrOpenAIWeeklyQuotaResetRuleNotFound      = infraerrors.NotFound("OPENAI_WEEKLY_QUOTA_RESET_RULE_NOT_FOUND", "quota reset link rule not found")
	ErrOpenAIWeeklyQuotaResetInvalidInput      = infraerrors.BadRequest("OPENAI_WEEKLY_QUOTA_RESET_INVALID_INPUT", "invalid quota reset link rule")
	ErrOpenAIWeeklyQuotaResetProRequired       = infraerrors.BadRequest("OPENAI_WEEKLY_QUOTA_RESET_PRO_REQUIRED", "source account must be an OpenAI Pro account")
	ErrOpenAIWeeklyQuotaWindowUnavailable      = infraerrors.BadRequest("OPENAI_WEEKLY_QUOTA_WINDOW_UNAVAILABLE", "official weekly quota window is unavailable")
	ErrOpenAIWeeklyQuotaQueryCooldown          = infraerrors.New(429, "OPENAI_WEEKLY_QUOTA_QUERY_COOLDOWN", "OpenAI weekly quota query is cooling down")
	ErrOpenAIWeeklyQuotaAccountIdentityMissing = infraerrors.BadRequest("OPENAI_WEEKLY_QUOTA_ACCOUNT_IDENTITY_MISSING", "source account is missing its ChatGPT account identity; re-authorize it")
	ErrOpenAIWeeklyQuotaClaimLost              = errors.New("openai weekly quota work claim lost")
)

const (
	openAIWeeklyQuotaResetScanInterval = time.Minute
	openAIWeeklyQuotaResetCacheTTL     = 15 * time.Minute
	openAIWeeklyQuotaResetLeaderKey    = "jobs:openai-weekly-quota-reset-link"
	openAIWeeklyQuotaResetTargetLease  = 2 * time.Minute
	openAIWeeklyQuotaResetFenceTTL     = 8 * 24 * time.Hour
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

func (s *OpenAIWeeklyQuotaResetLinkService) ListSourceAccounts(ctx context.Context) ([]OpenAIWeeklyQuotaSourceAccount, error) {
	lister, ok := s.accounts.(openAIWeeklyQuotaAccountLister)
	if !ok {
		return nil, infraerrors.InternalServer("OPENAI_WEEKLY_QUOTA_ACCOUNT_LIST_UNAVAILABLE", "OpenAI quota source account list is unavailable")
	}
	accounts, err := lister.ListAllWithFilters(ctx, PlatformOpenAI, AccountTypeOAuth, "", "", 0, "")
	if err != nil {
		return nil, err
	}
	items := make([]OpenAIWeeklyQuotaSourceAccount, 0, len(accounts))
	verificationReader, hasVerificationReader := s.repo.(openAIWeeklyQuotaVerificationReader)
	for i := range accounts {
		if accounts[i].IsShadow() {
			continue
		}
		item := buildOpenAIWeeklyQuotaSourceAccount(&accounts[i])
		if hasVerificationReader {
			if verifiedAt, verifyErr := verificationReader.LatestRuleVerificationAt(ctx, accounts[i].ID); verifyErr == nil && verifiedAt != nil {
				item.LastVerifiedAt = verifiedAt
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Supported != items[j].Supported {
			return items[i].Supported
		}
		if items[i].LocalAccountName != items[j].LocalAccountName {
			return items[i].LocalAccountName < items[j].LocalAccountName
		}
		return items[i].LocalAccountID < items[j].LocalAccountID
	})
	return items, nil
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

func (s *OpenAIWeeklyQuotaResetLinkService) ListResetEvents(ctx context.Context, limit int) ([]OpenAIWeeklyQuotaResetEvent, error) {
	lister, ok := s.repo.(openAIWeeklyQuotaEventLister)
	if !ok {
		return []OpenAIWeeklyQuotaResetEvent{}, nil
	}
	return lister.ListResetEvents(ctx, limit)
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
	if openAIWeeklyQuotaAccountID(account) == "" {
		return ErrOpenAIWeeklyQuotaAccountIdentityMissing
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
	if rule.NextQueryAt != nil && time.Now().UTC().Before(*rule.NextQueryAt) {
		return OpenAIWeeklyQuotaObservationResult{}, ErrOpenAIWeeklyQuotaQueryCooldown
	}
	if err := s.validateRuleBinding(ctx, *rule); err != nil {
		_ = s.repo.RecordQueryFailure(ctx, rule.ID, newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageBinding, infraerrors.Reason(err), "The quota linkage binding is invalid", time.Now().UTC()))
		return OpenAIWeeklyQuotaObservationResult{}, err
	}
	if s.usage == nil {
		return OpenAIWeeklyQuotaObservationResult{}, ErrOpenAIWeeklyQuotaWindowUnavailable
	}
	releaseQuery, acquired := s.acquireAccountQueryLock(ctx, rule.SourceAccountID)
	if !acquired {
		return OpenAIWeeklyQuotaObservationResult{}, ErrOpenAIWeeklyQuotaQueryCooldown
	}
	defer releaseQuery()
	usage, err := s.usage.QueryUsageSnapshot(ctx, rule.SourceAccountID)
	if err != nil {
		failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageUpstreamQuery, infraerrors.Reason(err), "Failed to query the OpenAI weekly quota", time.Now().UTC())
		failure.RetryAt = openAIWeeklyQuotaRetryAt(failure.Reason, failure.At)
		_ = s.repo.RecordQueryFailure(ctx, rule.ID, failure)
		return OpenAIWeeklyQuotaObservationResult{}, err
	}
	detectedAt := time.Now().UTC()
	snapshot, snapshotErr := buildOpenAIWeeklyQuotaSnapshot(usage, detectedAt)
	if snapshotErr != nil {
		_ = s.repo.RecordQueryFailure(ctx, rule.ID, newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageResponseParse, infraerrors.Reason(snapshotErr), "The OpenAI weekly quota response is incomplete", detectedAt))
		return OpenAIWeeklyQuotaObservationResult{}, snapshotErr
	}
	if err := s.repo.RecordQuerySuccess(ctx, rule.ID, snapshot); err != nil {
		return OpenAIWeeklyQuotaObservationResult{}, err
	}
	if rule.SourceIdentityFingerprint != "" && rule.SourceIdentityFingerprint != snapshot.Identity.Fingerprint {
		if err := s.repo.RebaselineRuleIdentity(ctx, rule.ID, snapshot.Identity, snapshot); err != nil {
			return OpenAIWeeklyQuotaObservationResult{}, err
		}
		return OpenAIWeeklyQuotaObservationResult{Outcome: OpenAIWeeklyQuotaObservationIdentityChanged}, nil
	}
	return s.applySnapshotToRule(ctx, *rule, snapshot)
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
	if openAIWeeklyQuotaAccountID(account) == "" {
		return ErrOpenAIWeeklyQuotaAccountIdentityMissing
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
		_ = s.repo.RecordQueryFailure(ctx, rule.ID, newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageResponseParse, infraerrors.Reason(ErrOpenAIWeeklyQuotaResetProRequired), "The source account is not an OpenAI Pro account", detectedAt))
		return OpenAIWeeklyQuotaObservationResult{}, ErrOpenAIWeeklyQuotaResetProRequired
	}
	snapshot, err := buildOpenAIWeeklyQuotaSnapshot(usage, detectedAt)
	if err != nil {
		_ = s.repo.RecordQueryFailure(ctx, rule.ID, newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageResponseParse, infraerrors.Reason(err), "The OpenAI weekly quota response is incomplete", detectedAt))
		return OpenAIWeeklyQuotaObservationResult{}, err
	}
	if err := s.repo.RecordQuerySuccess(ctx, rule.ID, snapshot); err != nil {
		failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageDatabase, "OPENAI_WEEKLY_QUOTA_QUERY_STATE_FAILED", "Failed to persist the successful quota query state", detectedAt)
		return OpenAIWeeklyQuotaObservationResult{}, infraerrors.InternalServer(failure.Reason, failure.Message)
	}
	return s.applySnapshotToRule(ctx, rule, snapshot)
}

func (s *OpenAIWeeklyQuotaResetLinkService) applySnapshotToRule(ctx context.Context, rule OpenAIWeeklyQuotaResetRule, snapshot OpenAIWeeklyQuotaSnapshot) (OpenAIWeeklyQuotaObservationResult, error) {
	return s.applySnapshotEventToRule(ctx, rule, snapshot, "poll", "weekly_window_advanced")
}

func (s *OpenAIWeeklyQuotaResetLinkService) applySnapshotEventToRule(ctx context.Context, rule OpenAIWeeklyQuotaResetRule, snapshot OpenAIWeeklyQuotaSnapshot, eventSource, evidenceKind string) (OpenAIWeeklyQuotaObservationResult, error) {
	if (rule.SourceIdentityFingerprint == "" && rule.LastObservedResetAt != nil) ||
		(rule.SourceIdentityFingerprint != "" && rule.SourceIdentityFingerprint != snapshot.Identity.Fingerprint) {
		if err := s.repo.RebaselineRuleIdentity(ctx, rule.ID, snapshot.Identity, snapshot); err != nil {
			return OpenAIWeeklyQuotaObservationResult{}, err
		}
		return OpenAIWeeklyQuotaObservationResult{Outcome: OpenAIWeeklyQuotaObservationIdentityChanged}, nil
	}
	result, err := s.repo.ApplyObservedWeeklyWindow(ctx, OpenAIWeeklyQuotaObservation{
		RuleID: rule.ID, ResetEventID: snapshot.EventID, EventSource: eventSource, EvidenceKind: evidenceKind,
		Identity: snapshot.Identity, MeterKey: snapshot.MeterKey, UsedPercent: snapshot.UsedPercent,
		OfficialResetAt: snapshot.ResetAt, OfficialWindowStart: snapshot.WindowStart,
		OfficialWindowSeconds: snapshot.WindowSeconds, DetectedAt: snapshot.ObservedAt,
	})
	if err != nil {
		failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageDatabase, "OPENAI_WEEKLY_QUOTA_DATABASE_FAILED", "Failed to persist the weekly quota reset event", snapshot.ObservedAt)
		_ = s.repo.RecordExecutionFailure(ctx, rule.ID, result.ExecutionID, failure)
		return result, infraerrors.InternalServer(failure.Reason, failure.Message)
	}
	if result.Outcome != OpenAIWeeklyQuotaObservationTriggered {
		return result, nil
	}
	if targetRepo, ok := s.repo.(openAIWeeklyQuotaTargetRepository); ok {
		if err := s.processResetTargets(ctx, targetRepo, result.ExecutionID); err != nil {
			return result, err
		}
		return result, nil
	}
	for _, userID := range result.ResetUserIDs {
		if s.cache == nil {
			continue
		}
		_, cacheErr := s.cache.ResetUserPlatformWeeklyQuotaCache(ctx, userID, PlatformOpenAI, snapshot.WindowStart, openAIWeeklyQuotaResetCacheTTL, s.markCacheDirty)
		if cacheErr != nil {
			failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageRedisSync, "OPENAI_WEEKLY_QUOTA_CACHE_FAILED", "Failed to synchronize the weekly quota cache", time.Now().UTC())
			_ = s.repo.MarkExecutionRetryableFailed(ctx, result.ExecutionID, failure.Message, failure.At)
			_ = s.repo.RecordExecutionFailure(ctx, rule.ID, result.ExecutionID, failure)
			return result, infraerrors.InternalServer(failure.Reason, failure.Message)
		}
	}
	if err := s.repo.CompleteExecution(ctx, result.ExecutionID, time.Now().UTC()); err != nil {
		failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageDatabase, "OPENAI_WEEKLY_QUOTA_DATABASE_FAILED", "Failed to complete the weekly quota execution", time.Now().UTC())
		_ = s.repo.MarkExecutionRetryableFailed(ctx, result.ExecutionID, failure.Message, failure.At)
		_ = s.repo.RecordExecutionFailure(ctx, rule.ID, result.ExecutionID, failure)
		return result, infraerrors.InternalServer(failure.Reason, failure.Message)
	}
	_ = s.repo.RecordExecutionSuccess(ctx, rule.ID, result.ExecutionID, time.Now().UTC())
	return result, nil
}

func (s *OpenAIWeeklyQuotaResetLinkService) dispatchResetEvents(ctx context.Context, repo openAIWeeklyQuotaEventDispatcherRepository) error {
	for batch := 0; batch < 8; batch++ {
		events, err := repo.ClaimResetEvents(ctx, s.owner, 25, time.Now().UTC(), openAIWeeklyQuotaResetTargetLease)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		for _, event := range events {
			if err := s.dispatchResetEvent(ctx, repo, event); err != nil {
				_ = repo.RetryResetEvent(ctx, event.ID, event.LeaseOwner, infraerrors.Reason(err), time.Now().UTC(), time.Now().UTC().Add(time.Minute))
			}
		}
	}
	return nil
}

func (s *OpenAIWeeklyQuotaResetLinkService) dispatchResetEvent(ctx context.Context, repo openAIWeeklyQuotaEventDispatcherRepository, event OpenAIWeeklyQuotaResetEvent) error {
	identityMatches, err := s.resetEventIdentityMatches(ctx, event)
	if err != nil {
		if errors.Is(err, ErrOpenAIWeeklyQuotaResetProRequired) || errors.Is(err, ErrOpenAIWeeklyQuotaResetInvalidInput) || errors.Is(err, ErrOpenAIWeeklyQuotaAccountIdentityMissing) {
			return repo.RejectResetEvent(ctx, event.ID, event.LeaseOwner, "source_account_not_supported", time.Now().UTC())
		}
		return err
	}
	if !identityMatches {
		return repo.RejectResetEvent(ctx, event.ID, event.LeaseOwner, "identity_changed", time.Now().UTC())
	}
	if event.Status == "candidate" {
		if !weeklyQuotaEvidenceCanBeConfirmedLater(event.Reason) {
			return repo.RejectResetEvent(ctx, event.ID, event.LeaseOwner, event.Reason, time.Now().UTC())
		}
		if time.Now().UTC().After(event.ObservedAt.Add(10 * time.Minute)) {
			return repo.RejectResetEvent(ctx, event.ID, event.LeaseOwner, "confirmation_expired", time.Now().UTC())
		}
		if event.Reason == "weekly_usage_unknown" && event.PreUsedPercent == nil {
			return repo.RejectResetEvent(ctx, event.ID, event.LeaseOwner, event.Reason, time.Now().UTC())
		}
		if s.usage == nil {
			return ErrOpenAIWeeklyQuotaWindowUnavailable
		}
		release, acquired := s.acquireAccountQueryLock(ctx, event.SourceAccountID)
		if !acquired {
			return ErrOpenAIWeeklyQuotaQueryCooldown
		}
		usage, err := s.usage.QueryUsageSnapshot(ctx, event.SourceAccountID)
		release()
		if err != nil {
			return err
		}
		snapshot, err := buildOpenAIWeeklyQuotaSnapshot(usage, time.Now().UTC())
		if err != nil {
			return err
		}
		if event.IdentityFingerprint != "" && event.IdentityFingerprint != snapshot.Identity.Fingerprint {
			return repo.RejectResetEvent(ctx, event.ID, event.LeaseOwner, "identity_changed", time.Now().UTC())
		}
		confirmed := false
		evidenceKind := ""
		if event.OfficialResetAt != nil && snapshot.ResetAt.After(*event.OfficialResetAt) {
			// 窗口已经推进但本事件没有同窗口证据，不能把它当成一次确认的重置。
			return repo.RejectResetEvent(ctx, event.ID, event.LeaseOwner, "weekly_window_advanced_without_confirmation", time.Now().UTC())
		} else if event.OfficialResetAt != nil && snapshot.ResetAt.Before(*event.OfficialResetAt) {
			return ErrOpenAIWeeklyQuotaQueryCooldown
		} else if event.PreUsedPercent != nil && snapshot.UsedPercent != nil && *snapshot.UsedPercent+0.001 < *event.PreUsedPercent {
			confirmed = true
			evidenceKind = "authorized_reset_weekly_usage_decreased"
		}
		if !confirmed {
			return ErrOpenAIWeeklyQuotaQueryCooldown
		}
		snapshot.EventID = event.ResetEventID
		snapshot.Source = event.EventSource
		if err := repo.ConfirmResetEvent(ctx, event.ID, event.LeaseOwner, snapshot, evidenceKind, time.Now().UTC()); err != nil {
			return err
		}
		event.Status = "confirmed"
		event.EvidenceKind = evidenceKind
		event.OfficialResetAt = &snapshot.ResetAt
		event.WindowStart = &snapshot.WindowStart
		event.WindowSeconds = &snapshot.WindowSeconds
		event.PostUsedPercent = snapshot.UsedPercent
		event.IdentityFingerprint = snapshot.Identity.Fingerprint
		event.ObservedAt = snapshot.ObservedAt
	}
	if event.Status != "confirmed" || event.OfficialResetAt == nil || event.WindowStart == nil || event.WindowSeconds == nil {
		return nil
	}
	snapshot := OpenAIWeeklyQuotaSnapshot{
		Identity: OpenAIWeeklyQuotaSourceIdentity{Fingerprint: event.IdentityFingerprint},
		MeterKey: event.MeterKey, ResetAt: *event.OfficialResetAt, WindowStart: *event.WindowStart,
		WindowSeconds: *event.WindowSeconds, UsedPercent: event.PostUsedPercent,
		ObservedAt: event.ObservedAt, Source: event.EventSource, EventID: event.ResetEventID,
	}
	rules, err := s.rulesForResetEvent(ctx, event.ResetEventID, event.SourceAccountID)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if bindingErr := s.validateRuleBinding(ctx, rule); bindingErr != nil {
			if !isPermanentWeeklyQuotaBindingError(bindingErr) {
				// 账户/分组读取失败可能只是短暂数据库故障，事件必须保留待重试。
				return bindingErr
			}
			_ = s.repo.RecordQueryFailure(ctx, rule.ID, newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageBinding, infraerrors.Reason(bindingErr), "The quota linkage binding is invalid", time.Now().UTC()))
			continue
		}
		if rule.LastSnapshotEventID == event.ResetEventID {
			continue
		}
		if event.IdentityFingerprint != "" && rule.SourceIdentityFingerprint != event.IdentityFingerprint {
			continue
		}
		if _, err := s.applySnapshotEventToRule(ctx, rule, snapshot, event.EventSource, event.EvidenceKind); err != nil {
			return err
		}
	}
	return repo.MarkResetEventDispatched(ctx, event.ID, event.LeaseOwner, time.Now().UTC())
}

func (s *OpenAIWeeklyQuotaResetLinkService) resetEventIdentityMatches(ctx context.Context, event OpenAIWeeklyQuotaResetEvent) (bool, error) {
	if event.IdentityFingerprint == "" {
		return false, nil
	}
	if s.accounts == nil {
		return true, nil
	}
	account, err := s.accounts.GetByID(ctx, event.SourceAccountID)
	if err != nil {
		return false, err
	}
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || !account.IsActive() || account.IsShadow() {
		return false, ErrOpenAIWeeklyQuotaResetInvalidInput
	}
	if planType := strings.TrimSpace(account.GetCredential("plan_type")); planType != "" && !strings.EqualFold(planType, "pro") {
		return false, ErrOpenAIWeeklyQuotaResetProRequired
	}
	if openAIWeeklyQuotaAccountID(account) == "" {
		return false, ErrOpenAIWeeklyQuotaAccountIdentityMissing
	}
	return openAIWeeklyQuotaIdentityFingerprint(account) == event.IdentityFingerprint, nil
}

func weeklyQuotaEvidenceCanBeConfirmedLater(reason string) bool {
	switch reason {
	case "weekly_window_unavailable", "weekly_usage_unknown":
		return true
	default:
		return false
	}
}

func isPermanentWeeklyQuotaBindingError(err error) bool {
	switch infraerrors.Reason(err) {
	case infraerrors.Reason(ErrOpenAIWeeklyQuotaResetInvalidInput),
		infraerrors.Reason(ErrOpenAIWeeklyQuotaResetProRequired),
		infraerrors.Reason(ErrOpenAIWeeklyQuotaAccountIdentityMissing):
		return true
	default:
		return false
	}
}

func (s *OpenAIWeeklyQuotaResetLinkService) rulesForResetEvent(ctx context.Context, eventID string, sourceAccountID int64) ([]OpenAIWeeklyQuotaResetRule, error) {
	if eventRules, ok := s.repo.(openAIWeeklyQuotaEventRuleRepository); ok {
		return eventRules.ListResetEventRules(ctx, eventID, sourceAccountID)
	}
	rules, err := s.repo.ListEnabledRules(ctx)
	if err != nil {
		return nil, err
	}
	filtered := make([]OpenAIWeeklyQuotaResetRule, 0)
	for _, rule := range rules {
		if rule.SourceAccountID == sourceAccountID {
			filtered = append(filtered, rule)
		}
	}
	return filtered, nil
}

func (s *OpenAIWeeklyQuotaResetLinkService) ObserveAuthorizedResetOperation(ctx context.Context, evidence OpenAIWeeklyQuotaResetOperationEvidence) (OpenAIWeeklyQuotaResetEvidenceDecision, error) {
	decision := evaluateAuthorizedWeeklyResetEvidence(evidence.Before, evidence.After, evidence.WindowsReset)
	observedAt := evidence.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	var snapshot *OpenAIWeeklyQuotaSnapshot
	if evidence.After != nil {
		if built, err := buildOpenAIWeeklyQuotaSnapshot(evidence.After, observedAt); err == nil {
			snapshot = &built
		}
	}
	identityFingerprint := ""
	if snapshot != nil {
		identityFingerprint = snapshot.Identity.Fingerprint
	} else if evidence.Before != nil {
		if built, err := buildOpenAIWeeklyQuotaSnapshot(evidence.Before, observedAt); err == nil {
			identityFingerprint = built.Identity.Fingerprint
			snapshot = &built
		}
	}
	operationID := strings.TrimSpace(evidence.OperationID)
	if operationID == "" {
		return decision, infraerrors.BadRequest("OPENAI_WEEKLY_QUOTA_OPERATION_ID_REQUIRED", "A reset operation identifier is required")
	}
	seed := fmt.Sprintf("operation|%d|%s|%s|%s", evidence.SourceAccountID, identityFingerprint, evidence.EventSource, operationID)
	sum := sha256.Sum256([]byte(seed))
	eventID := "operation:" + hex.EncodeToString(sum[:16])
	if snapshot != nil {
		if decision.EvidenceKind == "authorized_reset_weekly_window_advanced" {
			eventID = snapshot.EventID
		}
		snapshot.EventID = eventID
		snapshot.Source = evidence.EventSource
	}
	if eventRepo, ok := s.repo.(openAIWeeklyQuotaEventRepository); ok {
		if err := eventRepo.RecordResetEvent(ctx, eventID, evidence, decision, snapshot); err != nil {
			return decision, err
		}
	}
	if !decision.Confirmed || snapshot == nil {
		return decision, nil
	}
	if dispatcher, ok := s.repo.(openAIWeeklyQuotaEventDispatcherRepository); ok {
		return decision, s.dispatchResetEvents(ctx, dispatcher)
	}
	rules, err := s.rulesForResetEvent(ctx, eventID, evidence.SourceAccountID)
	if err != nil {
		return decision, err
	}
	for _, rule := range rules {
		if _, err := s.applySnapshotEventToRule(ctx, rule, *snapshot, evidence.EventSource, decision.EvidenceKind); err != nil {
			return decision, err
		}
	}
	return decision, nil
}

func (s *OpenAIWeeklyQuotaResetLinkService) processResetTargets(ctx context.Context, targetRepo openAIWeeklyQuotaTargetRepository, executionID int64) error {
	generationCache, ok := s.cache.(openAIWeeklyQuotaGenerationCache)
	if !ok {
		failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageRedisSync, "OPENAI_WEEKLY_QUOTA_CACHE_NOT_CONFIGURED", "Weekly quota generation cache is not configured", time.Now().UTC())
		return infraerrors.InternalServer(failure.Reason, failure.Message)
	}
	for batch := 0; batch < 16; batch++ {
		targets, err := targetRepo.ClaimResetTargets(ctx, executionID, s.owner, 100, time.Now().UTC(), openAIWeeklyQuotaResetTargetLease)
		if err != nil {
			failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageDatabase, "OPENAI_WEEKLY_QUOTA_TARGET_CLAIM_FAILED", "Failed to claim pending weekly quota reset targets", time.Now().UTC())
			return infraerrors.InternalServer(failure.Reason, failure.Message)
		}
		if len(targets) == 0 {
			return nil
		}
		for _, target := range targets {
			if err := s.processResetTarget(ctx, targetRepo, generationCache, target); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *OpenAIWeeklyQuotaResetLinkService) processResetTarget(ctx context.Context, targetRepo openAIWeeklyQuotaTargetRepository, cache openAIWeeklyQuotaGenerationCache, target OpenAIWeeklyQuotaResetTarget) error {
	if target.Status == OpenAIWeeklyQuotaTargetPending {
		prepared, err := cache.PrepareUserPlatformWeeklyQuotaReset(ctx, target.UserID, target.Platform, target.ResetEventID, target.TargetGeneration, openAIWeeklyQuotaResetFenceTTL)
		if err != nil || !prepared {
			failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageRedisSync, "OPENAI_WEEKLY_QUOTA_CACHE_PREPARE_FAILED", "Failed to prepare the weekly quota generation switch", time.Now().UTC())
			_ = targetRepo.MarkTargetRetryable(ctx, target, failure)
			return infraerrors.InternalServer(failure.Reason, failure.Message)
		}
		if err := targetRepo.MarkTargetCachePrepared(ctx, target, time.Now().UTC()); err != nil {
			if errors.Is(err, ErrOpenAIWeeklyQuotaClaimLost) {
				return nil
			}
			failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageDatabase, "OPENAI_WEEKLY_QUOTA_TARGET_STATE_FAILED", "Failed to persist the weekly quota target state", time.Now().UTC())
			_ = targetRepo.MarkTargetRetryable(ctx, target, failure)
			return infraerrors.InternalServer(failure.Reason, failure.Message)
		}
		target.Status = OpenAIWeeklyQuotaTargetCachePrepared
	}
	if target.Status == OpenAIWeeklyQuotaTargetCachePrepared {
		if err := targetRepo.ApplyTargetDatabaseReset(ctx, target, time.Now().UTC()); err != nil {
			if errors.Is(err, ErrOpenAIWeeklyQuotaClaimLost) {
				return nil
			}
			failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageDatabase, "OPENAI_WEEKLY_QUOTA_DATABASE_FAILED", "Failed to apply the weekly quota generation", time.Now().UTC())
			_ = targetRepo.MarkTargetRetryable(ctx, target, failure)
			return infraerrors.InternalServer(failure.Reason, failure.Message)
		}
		target.Status = OpenAIWeeklyQuotaTargetDBApplied
	}
	if target.Status == OpenAIWeeklyQuotaTargetDBApplied {
		finalized, err := cache.FinalizeUserPlatformWeeklyQuotaReset(ctx, target.UserID, target.Platform, target.ResetEventID, target.TargetGeneration, target.QuotaWindowStart, openAIWeeklyQuotaResetFenceTTL, s.markCacheDirty)
		if err != nil || !finalized {
			failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageRedisSync, "OPENAI_WEEKLY_QUOTA_CACHE_FINALIZE_FAILED", "Failed to finalize the weekly quota generation switch", time.Now().UTC())
			_ = targetRepo.MarkTargetRetryable(ctx, target, failure)
			return infraerrors.InternalServer(failure.Reason, failure.Message)
		}
		completed, err := targetRepo.MarkTargetSucceeded(ctx, target, time.Now().UTC())
		if err != nil {
			if errors.Is(err, ErrOpenAIWeeklyQuotaClaimLost) {
				return nil
			}
			failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageDatabase, "OPENAI_WEEKLY_QUOTA_TARGET_STATE_FAILED", "Failed to complete the weekly quota target", time.Now().UTC())
			_ = targetRepo.MarkTargetRetryable(ctx, target, failure)
			return infraerrors.InternalServer(failure.Reason, failure.Message)
		}
		if completed {
			_ = s.repo.RecordExecutionSuccess(ctx, target.RuleID, target.ExecutionID, time.Now().UTC())
		}
	}
	return nil
}

func (s *OpenAIWeeklyQuotaResetLinkService) Start() {
	s.once.Do(func() {
		setOpenAIWeeklyQuotaResetNotifier(s)
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

func (s *OpenAIWeeklyQuotaResetLinkService) Stop() {
	clearOpenAIWeeklyQuotaResetNotifier(s)
	s.cancel()
	s.wg.Wait()
}

func (s *OpenAIWeeklyQuotaResetLinkService) scan(ctx context.Context) {
	if s.repo == nil || s.usage == nil {
		return
	}
	release, ok := s.acquireScanLock(ctx)
	if !ok {
		return
	}
	defer release()
	if targetRepo, ok := s.repo.(openAIWeeklyQuotaTargetRepository); ok {
		if err := s.processResetTargets(ctx, targetRepo, 0); err != nil {
			slog.Warn("openai_weekly_quota_reset_compensation_failed", "error", err)
		}
	}
	rules, err := s.repo.ListEnabledRules(ctx)
	if err != nil {
		slog.Warn("openai_weekly_quota_reset_rules_scan_failed", "error", err)
		return
	}
	byAccount := make(map[int64]*OpenAIQuotaUsage)
	accountErrors := make(map[int64]error)
	for _, rule := range rules {
		if rule.NextQueryAt != nil && time.Now().UTC().Before(*rule.NextQueryAt) {
			continue
		}
		if bindingErr := s.validateRuleBinding(ctx, rule); bindingErr != nil {
			_ = s.repo.RecordQueryFailure(ctx, rule.ID, newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageBinding, infraerrors.Reason(bindingErr), "The quota linkage binding is invalid", time.Now().UTC()))
			continue
		}
		usage, seen := byAccount[rule.SourceAccountID]
		if !seen && accountErrors[rule.SourceAccountID] == nil {
			releaseQuery, acquired := s.acquireAccountQueryLock(ctx, rule.SourceAccountID)
			if !acquired {
				// 正常的多实例竞争不是查询失败；持锁实例会更新规则状态。
				continue
			} else {
				usage, err = s.usage.QueryUsageSnapshot(ctx, rule.SourceAccountID)
				releaseQuery()
				if err != nil {
					accountErrors[rule.SourceAccountID] = err
				} else {
					byAccount[rule.SourceAccountID] = usage
				}
			}
		}
		if accountErr := accountErrors[rule.SourceAccountID]; accountErr != nil {
			failure := newOpenAIWeeklyQuotaFailure(ctx, OpenAIWeeklyQuotaStageUpstreamQuery, infraerrors.Reason(accountErr), "Failed to query the OpenAI weekly quota", time.Now().UTC())
			failure.RetryAt = openAIWeeklyQuotaRetryAt(failure.Reason, failure.At)
			_ = s.repo.RecordQueryFailure(ctx, rule.ID, failure)
			continue
		}
		if _, err = s.applyUsageToRule(ctx, rule, usage, time.Now().UTC()); err != nil {
			slog.Warn("openai_weekly_quota_reset_rule_check_failed", "rule_id", rule.ID, "error", err)
		}
	}
	// 先完成本轮规则的绑定和上游身份核验，再重放持久化事件，
	// 避免换号授权后旧账号事件先一步清空新账号用户额度。
	if eventRepo, ok := s.repo.(openAIWeeklyQuotaEventDispatcherRepository); ok {
		if err := s.dispatchResetEvents(ctx, eventRepo); err != nil {
			slog.Warn("openai_weekly_quota_reset_event_dispatch_failed", "error", err)
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

func (s *OpenAIWeeklyQuotaResetLinkService) acquireAccountQueryLock(ctx context.Context, accountID int64) (func(), bool) {
	if s.leader == nil {
		return func() {}, true
	}
	key := fmt.Sprintf("jobs:openai-weekly-quota-query:%d", accountID)
	owner := fmt.Sprintf("%s:%d", s.owner, accountID)
	ok, err := s.leader.TryAcquireLeaderLock(ctx, key, owner, 25*time.Second)
	if err != nil {
		slog.Warn("openai_weekly_quota_query_lock_unavailable", "account_id", accountID, "error", err)
		return func() {}, true
	}
	if !ok {
		return nil, false
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.leader.ReleaseLeaderLock(releaseCtx, key, owner)
	}, true
}

func newOpenAIWeeklyQuotaFailure(ctx context.Context, stage, reason, message string, at time.Time) OpenAIWeeklyQuotaFailure {
	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	return OpenAIWeeklyQuotaFailure{
		Stage: strings.TrimSpace(stage), Reason: strings.TrimSpace(reason),
		Message: strings.TrimSpace(message), RequestID: strings.TrimSpace(requestID), At: at.UTC(),
	}
}

func openAIWeeklyQuotaRetryAt(reason string, at time.Time) *time.Time {
	var delay time.Duration
	switch reason {
	case "OPENAI_QUOTA_RATE_LIMITED":
		delay = 30 * time.Second
	case "OPENAI_QUOTA_UPSTREAM_ERROR", "OPENAI_QUOTA_REQUEST_FAILED":
		delay = 5 * time.Second
	default:
		return nil
	}
	retryAt := at.Add(delay).UTC()
	return &retryAt
}

func buildOpenAIWeeklyQuotaSnapshot(usage *OpenAIQuotaUsage, observedAt time.Time) (OpenAIWeeklyQuotaSnapshot, error) {
	if usage == nil || !strings.EqualFold(strings.TrimSpace(usage.PlanType), "pro") {
		return OpenAIWeeklyQuotaSnapshot{}, ErrOpenAIWeeklyQuotaResetProRequired
	}
	window, ok := selectOpenAIWeeklyWindow(usage)
	if !ok {
		return OpenAIWeeklyQuotaSnapshot{}, ErrOpenAIWeeklyQuotaWindowUnavailable
	}
	identity := openAIWeeklyQuotaSourceIdentityFromUsage(usage, observedAt)
	if identity.Fingerprint == "" {
		return OpenAIWeeklyQuotaSnapshot{}, ErrOpenAIWeeklyQuotaAccountIdentityMissing
	}
	eventSeed := fmt.Sprintf("poll|%s|%s|%d|%d", identity.Fingerprint, window.MeterKey, window.ResetAt.Unix(), window.WindowSeconds)
	eventSum := sha256.Sum256([]byte(eventSeed))
	return OpenAIWeeklyQuotaSnapshot{
		Identity: identity, MeterKey: window.MeterKey, ResetAt: window.ResetAt,
		WindowStart: window.WindowStart, WindowSeconds: window.WindowSeconds,
		UsedPercent: window.UsedPercent, ObservedAt: observedAt.UTC(), Source: window.Source,
		CredentialVersion: usage.CredentialVersion, EventID: "poll:" + hex.EncodeToString(eventSum[:16]),
	}, nil
}

func evaluateAuthorizedWeeklyResetEvidence(before, after *OpenAIQuotaUsage, windowsReset int) OpenAIWeeklyQuotaResetEvidenceDecision {
	if windowsReset <= 0 {
		return OpenAIWeeklyQuotaResetEvidenceDecision{Reason: "no_windows_reset"}
	}
	beforeWeekly, beforeOK := selectOpenAIWeeklyWindow(before)
	afterWeekly, afterOK := selectOpenAIWeeklyWindow(after)
	if !beforeOK || !afterOK {
		return OpenAIWeeklyQuotaResetEvidenceDecision{Reason: "weekly_window_unavailable"}
	}
	if afterWeekly.ResetAt.Before(beforeWeekly.ResetAt) {
		return OpenAIWeeklyQuotaResetEvidenceDecision{Reason: "stale_weekly_snapshot"}
	}
	if afterWeekly.ResetAt.After(beforeWeekly.ResetAt) {
		return OpenAIWeeklyQuotaResetEvidenceDecision{Confirmed: true, EvidenceKind: "authorized_reset_weekly_window_advanced"}
	}
	if beforeWeekly.UsedPercent == nil || afterWeekly.UsedPercent == nil {
		return OpenAIWeeklyQuotaResetEvidenceDecision{Reason: "weekly_usage_unknown"}
	}
	if *afterWeekly.UsedPercent+0.001 < *beforeWeekly.UsedPercent {
		return OpenAIWeeklyQuotaResetEvidenceDecision{Confirmed: true, EvidenceKind: "authorized_reset_weekly_usage_decreased"}
	}
	if openAIFiveHourUsageDecreased(before, after) {
		return OpenAIWeeklyQuotaResetEvidenceDecision{Reason: "five_hour_only"}
	}
	return OpenAIWeeklyQuotaResetEvidenceDecision{Reason: "weekly_reset_not_confirmed"}
}

func openAIFiveHourUsageDecreased(before, after *OpenAIQuotaUsage) bool {
	selectFiveHour := func(usage *OpenAIQuotaUsage) *float64 {
		if usage == nil || usage.RateLimit == nil {
			return nil
		}
		for _, window := range []*OpenAIRateLimitWindow{usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow} {
			if window == nil || window.LimitWindowSeconds < int64(4*time.Hour/time.Second) || window.LimitWindowSeconds > int64(6*time.Hour/time.Second) {
				continue
			}
			if !window.UsedPercentKnown && window.UsedPercent == 0 {
				return nil
			}
			value := window.UsedPercent
			return &value
		}
		return nil
	}
	beforeUsed := selectFiveHour(before)
	afterUsed := selectFiveHour(after)
	return beforeUsed != nil && afterUsed != nil && *afterUsed+0.001 < *beforeUsed
}

func openAIWeeklyQuotaSourceIdentityFromUsage(usage *OpenAIQuotaUsage, verifiedAt time.Time) OpenAIWeeklyQuotaSourceIdentity {
	accountID := strings.TrimSpace(usage.AccountID)
	userID := strings.TrimSpace(usage.UserID)
	namespace := strings.TrimSpace(usage.CredentialIdentity)
	if accountID != "" {
		namespace = "chatgpt:" + accountID
		if userID != "" {
			namespace += ":user:" + userID
		}
	}
	identitySource := strings.TrimSpace(usage.CredentialIdentitySource)
	if identitySource == "" {
		identitySource = "oauth"
	}
	if namespace == "" {
		return OpenAIWeeklyQuotaSourceIdentity{
			ChatGPTAccountID: accountID, ChatGPTUserID: userID,
			Email: strings.TrimSpace(usage.Email), PlanType: strings.TrimSpace(usage.PlanType),
			IdentitySource: identitySource, VerifiedAt: timePtrUTC(verifiedAt),
		}
	}
	sum := sha256.Sum256([]byte(namespace))
	return OpenAIWeeklyQuotaSourceIdentity{
		Fingerprint: hex.EncodeToString(sum[:]), ChatGPTAccountID: accountID,
		ChatGPTUserID: userID, Email: strings.TrimSpace(usage.Email),
		PlanType: strings.TrimSpace(usage.PlanType), IdentitySource: identitySource,
		VerifiedAt: timePtrUTC(verifiedAt),
	}
}

func timePtrUTC(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

func buildOpenAIWeeklyQuotaSourceAccount(account *Account) OpenAIWeeklyQuotaSourceAccount {
	if account == nil {
		return OpenAIWeeklyQuotaSourceAccount{Supported: false, UnsupportedReason: "account_not_found"}
	}
	source := openAIQuotaIdentitySource(account)
	var verifiedAt *time.Time
	if account.Extra != nil {
		if raw, ok := account.Extra["codex_usage_updated_at"].(string); ok {
			if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil {
				verifiedAt = timePtrUTC(parsed)
			}
		}
	}
	dto := OpenAIWeeklyQuotaSourceAccount{
		LocalAccountID: account.ID, LocalAccountName: account.Name,
		ChatGPTAccountID: openAIWeeklyQuotaAccountID(account),
		ChatGPTUserID:    strings.TrimSpace(account.GetCredential("chatgpt_user_id")),
		Email:            strings.TrimSpace(account.GetCredential("email")),
		PlanType:         strings.TrimSpace(account.GetCredential("plan_type")),
		IdentitySource:   source, LastVerifiedAt: verifiedAt,
	}
	dto.Supported = account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth && account.IsActive() && !account.IsShadow() && strings.EqualFold(dto.PlanType, "pro") && dto.ChatGPTAccountID != ""
	if !dto.Supported {
		switch {
		case account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth:
			dto.UnsupportedReason = "unsupported_account_type"
		case account.IsShadow():
			dto.UnsupportedReason = "shadow_account"
		case !account.IsActive():
			dto.UnsupportedReason = "inactive_account"
		case dto.ChatGPTAccountID == "":
			dto.UnsupportedReason = "missing_chatgpt_account_id"
		default:
			dto.UnsupportedReason = "pro_plan_required"
		}
	}
	return dto
}

func openAIWeeklyQuotaAccountID(account *Account) string {
	if account == nil {
		return ""
	}
	if accountID := strings.TrimSpace(account.GetCredential("chatgpt_account_id")); accountID != "" {
		return accountID
	}
	return strings.TrimSpace(account.GetCredential("organization_id"))
}

func openAIWeeklyQuotaIdentityFingerprint(account *Account) string {
	accountID := openAIWeeklyQuotaAccountID(account)
	userID := ""
	if account != nil {
		userID = strings.TrimSpace(account.GetCredential("chatgpt_user_id"))
	}
	namespace := ""
	if accountID != "" {
		namespace = "chatgpt:" + accountID
		if userID != "" {
			namespace += ":user:" + userID
		}
	} else if account != nil {
		namespace = strings.TrimSpace(account.GetCredential("credential_identity"))
	}
	if namespace == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(namespace))
	return hex.EncodeToString(sum[:])
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
	var usedPercent *float64
	if selected.UsedPercentKnown || selected.UsedPercent != 0 {
		value := selected.UsedPercent
		usedPercent = &value
	}
	source := strings.TrimSpace(usage.SnapshotSource)
	if source == "" {
		source = "wham_usage"
	}
	return OpenAIWeeklyQuotaWindow{
		ResetAt:       resetAt,
		WindowStart:   resetAt.Add(-time.Duration(selected.LimitWindowSeconds) * time.Second),
		WindowSeconds: selected.LimitWindowSeconds,
		UsedPercent:   usedPercent,
		MeterKey:      "codex_weekly",
		Source:        source,
	}, true
}

var openAIWeeklyQuotaResetNotifierRegistry struct {
	sync.RWMutex
	service *OpenAIWeeklyQuotaResetLinkService
}

func setOpenAIWeeklyQuotaResetNotifier(service *OpenAIWeeklyQuotaResetLinkService) {
	openAIWeeklyQuotaResetNotifierRegistry.Lock()
	openAIWeeklyQuotaResetNotifierRegistry.service = service
	openAIWeeklyQuotaResetNotifierRegistry.Unlock()
}

func clearOpenAIWeeklyQuotaResetNotifier(service *OpenAIWeeklyQuotaResetLinkService) {
	openAIWeeklyQuotaResetNotifierRegistry.Lock()
	if openAIWeeklyQuotaResetNotifierRegistry.service == service {
		openAIWeeklyQuotaResetNotifierRegistry.service = nil
	}
	openAIWeeklyQuotaResetNotifierRegistry.Unlock()
}

func NotifyOpenAIWeeklyQuotaResetOperation(ctx context.Context, evidence OpenAIWeeklyQuotaResetOperationEvidence) (OpenAIWeeklyQuotaResetEvidenceDecision, error) {
	openAIWeeklyQuotaResetNotifierRegistry.RLock()
	service := openAIWeeklyQuotaResetNotifierRegistry.service
	openAIWeeklyQuotaResetNotifierRegistry.RUnlock()
	if service == nil {
		return evaluateAuthorizedWeeklyResetEvidence(evidence.Before, evidence.After, evidence.WindowsReset), nil
	}
	return service.ObserveAuthorizedResetOperation(ctx, evidence)
}
