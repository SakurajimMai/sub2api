package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type openAIWeeklyQuotaResetLinkRepository struct{ db *sql.DB }

const openAIWeeklyQuotaExecutionLease = 2 * time.Minute

func NewOpenAIWeeklyQuotaResetLinkRepository(db *sql.DB) service.OpenAIWeeklyQuotaResetLinkRepository {
	return &openAIWeeklyQuotaResetLinkRepository{db: db}
}

func postgresInt64Array(ids []int64) driver.Valuer {
	if ids == nil {
		ids = []int64{}
	}
	return pq.Array(ids)
}

const openAIWeeklyRuleSelect = `SELECT r.id, r.name, r.description, r.enabled,
	r.source_account_id, COALESCE(a.name, ''), r.target_group_id, COALESCE(g.name, ''),
	r.last_observed_reset_at, r.last_observed_window_seconds, r.last_observed_fetched_at,
	r.last_run_at, r.last_error,
	r.source_identity_fingerprint, r.source_chatgpt_account_id, r.source_chatgpt_user_id,
	r.source_email, r.source_plan_type, r.source_identity_source, r.source_identity_verified_at,
	r.last_snapshot_meter_key, r.last_snapshot_source, r.last_snapshot_observed_at, r.last_snapshot_used_percent, r.last_snapshot_event_id,
	r.last_attempt_at, r.last_query_success_at, r.last_execution_success_at,
	r.query_status, r.query_error_stage, r.query_error_reason, r.query_error_message, r.query_error_request_id,
	r.execution_status, r.execution_error_stage, r.execution_error_reason, r.execution_error_message, r.execution_error_request_id,
	r.next_query_at, r.created_at, r.updated_at
	FROM openai_weekly_quota_reset_rules r
	LEFT JOIN accounts a ON a.id = r.source_account_id
	LEFT JOIN groups g ON g.id = r.target_group_id`

type weeklyRuleRowScanner interface{ Scan(dest ...any) error }

func scanOpenAIWeeklyRule(row weeklyRuleRowScanner) (*service.OpenAIWeeklyQuotaResetRule, error) {
	var r service.OpenAIWeeklyQuotaResetRule
	var identity service.OpenAIWeeklyQuotaSourceAccount
	var identityVerifiedAt *time.Time
	var queryStage, queryReason, queryMessage, queryRequestID string
	var executionStage, executionReason, executionMessage, executionRequestID string
	err := row.Scan(&r.ID, &r.Name, &r.Description, &r.Enabled,
		&r.SourceAccountID, &r.SourceAccountName, &r.TargetGroupID, &r.TargetGroupName,
		&r.LastObservedResetAt, &r.LastObservedWindowSeconds, &r.LastObservedFetchedAt,
		&r.LastRunAt, &r.LastError,
		&r.SourceIdentityFingerprint, &identity.ChatGPTAccountID, &identity.ChatGPTUserID,
		&identity.Email, &identity.PlanType, &identity.IdentitySource, &identityVerifiedAt,
		&r.LastSnapshotMeterKey, &r.LastSnapshotSource, &r.LastSnapshotObservedAt, &r.LastSnapshotUsedPercent, &r.LastSnapshotEventID,
		&r.LastAttemptAt, &r.LastQuerySuccessAt, &r.LastExecutionSuccessAt,
		&r.QueryStatus, &queryStage, &queryReason, &queryMessage, &queryRequestID,
		&r.ExecutionStatus, &executionStage, &executionReason, &executionMessage, &executionRequestID,
		&r.NextQueryAt, &r.CreatedAt, &r.UpdatedAt)
	identity.LocalAccountID = r.SourceAccountID
	identity.LocalAccountName = r.SourceAccountName
	identity.LastVerifiedAt = identityVerifiedAt
	identity.Supported = true
	r.SourceIdentity = &identity
	if queryReason != "" {
		r.QueryFailure = &service.OpenAIWeeklyQuotaFailure{Stage: queryStage, Reason: queryReason, Message: weeklyQuotaSafeFailureMessage(queryStage), RequestID: queryRequestID}
		if r.LastAttemptAt != nil {
			r.QueryFailure.At = *r.LastAttemptAt
		}
	}
	if executionReason != "" {
		r.ExecutionFailure = &service.OpenAIWeeklyQuotaFailure{Stage: executionStage, Reason: executionReason, Message: weeklyQuotaSafeFailureMessage(executionStage), RequestID: executionRequestID}
		if r.LastRunAt != nil {
			r.ExecutionFailure.At = *r.LastRunAt
		} else {
			r.ExecutionFailure.At = r.UpdatedAt
		}
	}
	// LastError 只从结构化阶段重建，绝不透传旧节点写入的原始数据库文本。
	r.LastError = ""
	if r.ExecutionFailure != nil {
		r.LastError = r.ExecutionFailure.Message
	} else if r.QueryFailure != nil {
		r.LastError = r.QueryFailure.Message
	}
	return &r, err
}

func (r *openAIWeeklyQuotaResetLinkRepository) ListRules(ctx context.Context) ([]service.OpenAIWeeklyQuotaResetRule, error) {
	return r.listRules(ctx, false)
}

func (r *openAIWeeklyQuotaResetLinkRepository) LatestRuleVerificationAt(ctx context.Context, accountID int64) (*time.Time, error) {
	var verifiedAt *time.Time
	err := r.db.QueryRowContext(ctx, `SELECT MAX(source_identity_verified_at)
		FROM openai_weekly_quota_reset_rules
		WHERE source_account_id=$1 AND deleted_at IS NULL`, accountID).Scan(&verifiedAt)
	return verifiedAt, err
}

func (r *openAIWeeklyQuotaResetLinkRepository) ListEnabledRules(ctx context.Context) ([]service.OpenAIWeeklyQuotaResetRule, error) {
	return r.listRules(ctx, true)
}

func (r *openAIWeeklyQuotaResetLinkRepository) listRules(ctx context.Context, enabledOnly bool) ([]service.OpenAIWeeklyQuotaResetRule, error) {
	query := openAIWeeklyRuleSelect + ` WHERE r.deleted_at IS NULL`
	if enabledOnly {
		query += ` AND r.enabled = TRUE`
	}
	query += ` ORDER BY r.id DESC`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]service.OpenAIWeeklyQuotaResetRule, 0)
	for rows.Next() {
		item, scanErr := scanOpenAIWeeklyRule(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, *item)
	}
	return result, rows.Err()
}

func (r *openAIWeeklyQuotaResetLinkRepository) GetRule(ctx context.Context, id int64) (*service.OpenAIWeeklyQuotaResetRule, error) {
	item, err := scanOpenAIWeeklyRule(r.db.QueryRowContext(ctx, openAIWeeklyRuleSelect+` WHERE r.id = $1 AND r.deleted_at IS NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return item, err
}

func (r *openAIWeeklyQuotaResetLinkRepository) CreateRule(ctx context.Context, input service.OpenAIWeeklyQuotaResetRuleInput) (*service.OpenAIWeeklyQuotaResetRule, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `INSERT INTO openai_weekly_quota_reset_rules
		(name, description, enabled, source_account_id, target_group_id)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`, input.Name, input.Description, input.Enabled, input.SourceAccountID, input.TargetGroupID).Scan(&id)
	if err != nil {
		return nil, err
	}
	return r.GetRule(ctx, id)
}

func (r *openAIWeeklyQuotaResetLinkRepository) UpdateRule(ctx context.Context, id int64, input service.OpenAIWeeklyQuotaResetRuleInput) (*service.OpenAIWeeklyQuotaResetRule, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET
		name=$1, description=$2, enabled=$3,
		last_observed_reset_at=CASE WHEN source_account_id<>$4 THEN NULL ELSE last_observed_reset_at END,
		last_observed_window_seconds=CASE WHEN source_account_id<>$4 THEN NULL ELSE last_observed_window_seconds END,
		last_observed_fetched_at=CASE WHEN source_account_id<>$4 THEN NULL ELSE last_observed_fetched_at END,
		last_run_at=CASE WHEN source_account_id<>$4 THEN NULL ELSE last_run_at END,
		last_error=CASE WHEN source_account_id<>$4 THEN '' ELSE last_error END,
		source_identity_fingerprint=CASE WHEN source_account_id<>$4 THEN '' ELSE source_identity_fingerprint END,
		source_chatgpt_account_id=CASE WHEN source_account_id<>$4 THEN '' ELSE source_chatgpt_account_id END,
		source_chatgpt_user_id=CASE WHEN source_account_id<>$4 THEN '' ELSE source_chatgpt_user_id END,
		source_email=CASE WHEN source_account_id<>$4 THEN '' ELSE source_email END,
		source_plan_type=CASE WHEN source_account_id<>$4 THEN '' ELSE source_plan_type END,
		source_identity_source=CASE WHEN source_account_id<>$4 THEN '' ELSE source_identity_source END,
		source_identity_verified_at=CASE WHEN source_account_id<>$4 THEN NULL ELSE source_identity_verified_at END,
		last_snapshot_event_id=CASE WHEN source_account_id<>$4 THEN '' ELSE last_snapshot_event_id END,
		last_snapshot_meter_key=CASE WHEN source_account_id<>$4 THEN '' ELSE last_snapshot_meter_key END,
		last_snapshot_source=CASE WHEN source_account_id<>$4 THEN '' ELSE last_snapshot_source END,
		last_snapshot_observed_at=CASE WHEN source_account_id<>$4 THEN NULL ELSE last_snapshot_observed_at END,
		last_snapshot_used_percent=CASE WHEN source_account_id<>$4 THEN NULL ELSE last_snapshot_used_percent END,
		last_attempt_at=CASE WHEN source_account_id<>$4 THEN NULL ELSE last_attempt_at END,
		last_query_success_at=CASE WHEN source_account_id<>$4 THEN NULL ELSE last_query_success_at END,
		last_execution_success_at=CASE WHEN source_account_id<>$4 THEN NULL ELSE last_execution_success_at END,
		query_status=CASE WHEN source_account_id<>$4 THEN 'never' ELSE query_status END,
		query_error_stage=CASE WHEN source_account_id<>$4 THEN '' ELSE query_error_stage END,
		query_error_reason=CASE WHEN source_account_id<>$4 THEN '' ELSE query_error_reason END,
		query_error_message=CASE WHEN source_account_id<>$4 THEN '' ELSE query_error_message END,
		query_error_request_id=CASE WHEN source_account_id<>$4 THEN '' ELSE query_error_request_id END,
		query_failure_count=CASE WHEN source_account_id<>$4 THEN 0 ELSE query_failure_count END,
		next_query_at=CASE WHEN source_account_id<>$4 THEN NULL ELSE next_query_at END,
		execution_status=CASE WHEN source_account_id<>$4 THEN 'never' ELSE execution_status END,
		execution_error_stage=CASE WHEN source_account_id<>$4 THEN '' ELSE execution_error_stage END,
		execution_error_reason=CASE WHEN source_account_id<>$4 THEN '' ELSE execution_error_reason END,
		execution_error_message=CASE WHEN source_account_id<>$4 THEN '' ELSE execution_error_message END,
		execution_error_request_id=CASE WHEN source_account_id<>$4 THEN '' ELSE execution_error_request_id END,
		source_account_id=$4, target_group_id=$5,
		updated_at=NOW() WHERE id=$6 AND deleted_at IS NULL`, input.Name, input.Description, input.Enabled, input.SourceAccountID, input.TargetGroupID, id)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return nil, err
	}
	return r.GetRule(ctx, id)
}

func (r *openAIWeeklyQuotaResetLinkRepository) DeleteRule(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET deleted_at=NOW(), updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`, id)
	return err
}

func (r *openAIWeeklyQuotaResetLinkRepository) ListExecutions(ctx context.Context, ruleID *int64, limit int) ([]service.OpenAIWeeklyQuotaResetExecution, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT e.id, e.rule_id, COALESCE(r.name,''), e.source_account_id, e.target_group_id,
		e.reset_event_id,e.event_source,e.evidence_kind,
		e.official_reset_at, e.official_window_start, e.official_window_seconds, e.status,
		e.matched_users, e.reset_users, e.skipped_users, e.error_message,e.stage,e.error_reason,e.error_request_id,e.detected_at,
		e.completed_at, e.created_at, e.updated_at
		FROM openai_weekly_quota_reset_executions e
		LEFT JOIN openai_weekly_quota_reset_rules r ON r.id=e.rule_id`
	args := []any{}
	if ruleID != nil {
		query += ` WHERE e.rule_id=$1`
		args = append(args, *ruleID)
	}
	query += fmt.Sprintf(` ORDER BY e.id DESC LIMIT %d`, limit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]service.OpenAIWeeklyQuotaResetExecution, 0)
	for rows.Next() {
		var e service.OpenAIWeeklyQuotaResetExecution
		if err := rows.Scan(&e.ID, &e.RuleID, &e.RuleName, &e.SourceAccountID, &e.TargetGroupID,
			&e.ResetEventID, &e.EventSource, &e.EvidenceKind,
			&e.OfficialResetAt, &e.OfficialWindowStart, &e.OfficialWindowSeconds, &e.Status,
			&e.MatchedUsers, &e.ResetUsers, &e.SkippedUsers, &e.ErrorMessage, &e.Stage, &e.ErrorReason, &e.ErrorRequestID, &e.DetectedAt,
			&e.CompletedAt, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		if e.ErrorReason == "" {
			e.ErrorMessage = ""
		} else {
			e.ErrorMessage = weeklyQuotaSafeFailureMessage(e.Stage)
		}
		items = append(items, e)
	}
	return items, rows.Err()
}

// weeklyQuotaSafeFailureMessage 为历史记录和 API DTO 提供固定文本。
// 数据库中可能残留旧版本写入的上游响应或 SQL 文本，不能直接返回给管理员界面。
func weeklyQuotaSafeFailureMessage(stage string) string {
	switch stage {
	case service.OpenAIWeeklyQuotaStageBinding:
		return "The quota linkage binding is invalid"
	case service.OpenAIWeeklyQuotaStageCredentials:
		return "The OpenAI credentials are unavailable"
	case service.OpenAIWeeklyQuotaStageUpstreamQuery:
		return "Failed to query the OpenAI weekly quota"
	case service.OpenAIWeeklyQuotaStageResponseParse:
		return "The OpenAI weekly quota response is incomplete"
	case service.OpenAIWeeklyQuotaStageDatabase:
		return "Failed to update the weekly quota linkage"
	case service.OpenAIWeeklyQuotaStageRedisSync:
		return "Failed to synchronize the weekly quota cache"
	default:
		return "The weekly quota linkage operation failed"
	}
}

func (r *openAIWeeklyQuotaResetLinkRepository) ApplyObservedWeeklyWindow(ctx context.Context, o service.OpenAIWeeklyQuotaObservation) (result service.OpenAIWeeklyQuotaObservationResult, err error) {
	result.ResetUserIDs = make([]int64, 0)
	result.Targets = make([]service.OpenAIWeeklyQuotaResetTarget, 0)
	if o.ResetEventID == "" {
		o.ResetEventID = fmt.Sprintf("legacy-window:%d:%d", o.RuleID, o.OfficialResetAt.Unix())
	}
	if o.EventSource == "" {
		o.EventSource = "poll"
	}
	if o.EvidenceKind == "" {
		o.EvidenceKind = "weekly_window_advanced"
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var groupID int64
	var previous sql.NullTime
	var storedIdentity string
	var lastSnapshotEventID string
	err = tx.QueryRowContext(ctx, `SELECT target_group_id, last_observed_reset_at, source_identity_fingerprint, last_snapshot_event_id
		FROM openai_weekly_quota_reset_rules
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE`, o.RuleID).Scan(&groupID, &previous, &storedIdentity, &lastSnapshotEventID)
	if err != nil {
		return result, err
	}
	if previous.Valid && o.Identity.Fingerprint != "" && (storedIdentity == "" || storedIdentity != o.Identity.Fingerprint) {
		_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET
			source_identity_fingerprint=$1, source_chatgpt_account_id=$2, source_chatgpt_user_id=$3,
			source_email=$4, source_plan_type=$5, source_identity_source=$6, source_identity_verified_at=$7,
			last_observed_reset_at=$8, last_observed_window_seconds=$9, last_snapshot_event_id=$10,
			execution_status='baseline', execution_error_stage='', execution_error_reason='',
			execution_error_message='', execution_error_request_id='', last_error='', updated_at=$11 WHERE id=$12`,
			o.Identity.Fingerprint, o.Identity.ChatGPTAccountID, o.Identity.ChatGPTUserID,
			o.Identity.Email, o.Identity.PlanType, o.Identity.IdentitySource, o.Identity.VerifiedAt,
			o.OfficialResetAt, o.OfficialWindowSeconds, o.ResetEventID, o.DetectedAt, o.RuleID)
		if err != nil {
			return result, err
		}
		result.Outcome = service.OpenAIWeeklyQuotaObservationIdentityChanged
		err = tx.Commit()
		return result, err
	}

	if !previous.Valid {
		_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules
			SET last_observed_reset_at=$1, last_observed_window_seconds=$2,
			last_observed_fetched_at=$3, last_snapshot_event_id=$4,
			source_identity_fingerprint=$5, source_chatgpt_account_id=$6, source_chatgpt_user_id=$7,
			source_email=$8, source_plan_type=$9, source_identity_source=$10, source_identity_verified_at=$11,
			execution_status='baseline', last_error='', updated_at=$3 WHERE id=$12`,
			o.OfficialResetAt, o.OfficialWindowSeconds, o.DetectedAt, o.ResetEventID,
			o.Identity.Fingerprint, o.Identity.ChatGPTAccountID, o.Identity.ChatGPTUserID,
			o.Identity.Email, o.Identity.PlanType, o.Identity.IdentitySource, o.Identity.VerifiedAt, o.RuleID)
		if err != nil {
			return result, err
		}
		result.Outcome = service.OpenAIWeeklyQuotaObservationBaseline
		err = tx.Commit()
		return result, err
	}

	if o.OfficialResetAt.Before(previous.Time) {
		_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET last_observed_fetched_at=$1, updated_at=$1 WHERE id=$2`, o.DetectedAt, o.RuleID)
		if err != nil {
			return result, err
		}
		result.Outcome = service.OpenAIWeeklyQuotaObservationStale
		err = tx.Commit()
		return result, err
	}

	var executionID int64
	var executionStatus string
	var executionUpdatedAt time.Time
	err = tx.QueryRowContext(ctx, `SELECT id, status, updated_at, matched_users, skipped_users, reset_user_ids
		FROM openai_weekly_quota_reset_executions WHERE rule_id=$1 AND reset_event_id=$2`, o.RuleID, o.ResetEventID).
		Scan(&executionID, &executionStatus, &executionUpdatedAt, &result.MatchedUsers, &result.SkippedUsers, pq.Array(&result.ResetUserIDs))
	if err == nil {
		if executionStatus == service.OpenAIWeeklyQuotaExecutionSucceeded || executionStatus == service.OpenAIWeeklyQuotaExecutionPermanentFailed {
			result.Outcome = service.OpenAIWeeklyQuotaObservationUnchanged
			err = tx.Commit()
			return result, err
		}
		if (executionStatus == service.OpenAIWeeklyQuotaExecutionRunning || executionStatus == service.OpenAIWeeklyQuotaExecutionPending) &&
			o.DetectedAt.Before(executionUpdatedAt.Add(openAIWeeklyQuotaExecutionLease)) {
			result.Outcome = service.OpenAIWeeklyQuotaObservationUnchanged
			err = tx.Commit()
			return result, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_executions SET status=$1, error_message='', completed_at=NULL, updated_at=$2 WHERE id=$3`, service.OpenAIWeeklyQuotaExecutionRunning, o.DetectedAt, executionID)
		if err != nil {
			return result, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET last_observed_fetched_at=$1, last_run_at=$1, last_error='', updated_at=$1 WHERE id=$2`, o.DetectedAt, o.RuleID)
		if err != nil {
			return result, err
		}
		result.Outcome = service.OpenAIWeeklyQuotaObservationTriggered
		result.ExecutionID = executionID
		err = tx.Commit()
		return result, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	// 基线/身份变更会记录当前事件 ID。事件重试必须先查执行记录，
	// 否则未完成的执行会被错误地当成已处理而无法补偿。
	if lastSnapshotEventID != "" && lastSnapshotEventID == o.ResetEventID {
		result.Outcome = service.OpenAIWeeklyQuotaObservationUnchanged
		err = tx.Commit()
		return result, err
	}
	if o.EventSource == "poll" && o.OfficialResetAt.Equal(previous.Time) {
		result.Outcome = service.OpenAIWeeklyQuotaObservationUnchanged
		err = tx.Commit()
		return result, err
	}

	var eventRowID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO openai_weekly_quota_reset_events
		(source_account_id, source_identity_fingerprint, reset_event_id, meter_key, event_source,
		evidence_kind, status, official_reset_at, official_window_start, official_window_seconds,
		post_used_percent, observed_at, confirmed_at, created_at, updated_at)
		SELECT source_account_id, $2, $3, $4, $5, $6, 'confirmed', $7, $8, $9, $10, $11, $11, $11, $11
		FROM openai_weekly_quota_reset_rules WHERE id=$1
		ON CONFLICT (source_account_id, source_identity_fingerprint, reset_event_id) DO NOTHING RETURNING id`,
		o.RuleID, o.Identity.Fingerprint, o.ResetEventID, o.MeterKey, o.EventSource, o.EvidenceKind,
		o.OfficialResetAt, o.OfficialWindowStart, o.OfficialWindowSeconds, o.UsedPercent, o.DetectedAt).Scan(&eventRowID)
	eventInserted := err == nil
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT id FROM openai_weekly_quota_reset_events
			WHERE source_account_id=(SELECT source_account_id FROM openai_weekly_quota_reset_rules WHERE id=$1)
			AND source_identity_fingerprint=$2 AND reset_event_id=$3`, o.RuleID, o.Identity.Fingerprint, o.ResetEventID).Scan(&eventRowID)
	}
	if err != nil {
		return result, err
	}
	if eventInserted {
		_, err = tx.ExecContext(ctx, `INSERT INTO openai_weekly_quota_reset_event_rules
			(event_id,rule_id,source_account_id,target_group_id,created_at)
			SELECT $1,id,source_account_id,target_group_id,$2 FROM openai_weekly_quota_reset_rules
			WHERE source_account_id=(SELECT source_account_id FROM openai_weekly_quota_reset_rules WHERE id=$3)
			AND enabled=TRUE AND deleted_at IS NULL ON CONFLICT DO NOTHING`, eventRowID, o.DetectedAt, o.RuleID)
		if err != nil {
			return result, err
		}
	}

	err = tx.QueryRowContext(ctx, `INSERT INTO openai_weekly_quota_reset_executions
		(rule_id, source_account_id, target_group_id, reset_event_id, event_source, evidence_kind,
		official_reset_at, official_window_start, official_window_seconds, status, stage,
		detected_at, created_at, updated_at)
		SELECT id, source_account_id, target_group_id, $2, $3, $4, $5, $6, $7, $8, 'weekly_pending', $9, $9, $9
		FROM openai_weekly_quota_reset_rules WHERE id=$1 RETURNING id`,
		o.RuleID, o.ResetEventID, o.EventSource, o.EvidenceKind, o.OfficialResetAt,
		o.OfficialWindowStart, o.OfficialWindowSeconds, service.OpenAIWeeklyQuotaExecutionRunning, o.DetectedAt).Scan(&executionID)
	if err != nil {
		return result, err
	}

	err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT u.id)
		FROM user_allowed_groups uag JOIN users u ON u.id=uag.user_id
		WHERE uag.group_id=$1 AND u.deleted_at IS NULL AND u.status='active'`, groupID).Scan(&result.MatchedUsers)
	if err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT q.user_id, q.weekly_quota_generation, q.weekly_reserved_generation
		FROM user_platform_quotas q
		JOIN user_allowed_groups uag ON uag.user_id=q.user_id
		JOIN users u ON u.id=q.user_id
		WHERE uag.group_id=$1
		AND q.platform='openai' AND q.deleted_at IS NULL
		AND u.deleted_at IS NULL AND u.status='active'
		ORDER BY q.user_id FOR UPDATE OF q`, groupID)
	if err != nil {
		return result, err
	}
	type candidate struct{ userID, generation, reserved int64 }
	candidates := make([]candidate, 0)
	for rows.Next() {
		var item candidate
		if scanErr := rows.Scan(&item.userID, &item.generation, &item.reserved); scanErr != nil {
			_ = rows.Close()
			return result, scanErr
		}
		candidates = append(candidates, item)
	}
	err = rows.Close()
	if err != nil {
		return result, err
	}
	result.UsersWithQuota = len(candidates)
	result.NoQuotaUsers = result.MatchedUsers - result.UsersWithQuota
	quotaWindowStart := timezone.StartOfWeek(o.DetectedAt)
	for _, item := range candidates {
		targetGeneration := max(item.generation, item.reserved) + 1
		var targetID int64
		sharedTarget := false
		insertErr := tx.QueryRowContext(ctx, `INSERT INTO openai_weekly_quota_reset_execution_users
			(execution_id, reset_event_id, user_id, platform, previous_generation, target_generation,
			 quota_window_start, status, created_at, updated_at)
			VALUES ($1,$2,$3,'openai',$4,$5,$6,'weekly_pending',$7,$7)
			ON CONFLICT (reset_event_id, user_id, platform) DO NOTHING RETURNING id`,
			executionID, o.ResetEventID, item.userID, item.generation, targetGeneration, quotaWindowStart, o.DetectedAt).Scan(&targetID)
		if errors.Is(insertErr, sql.ErrNoRows) {
			result.DuplicateUsers++
			sharedTarget = true
			insertErr = tx.QueryRowContext(ctx, `SELECT id FROM openai_weekly_quota_reset_execution_users
				WHERE reset_event_id=$1 AND user_id=$2 AND platform='openai' FOR UPDATE`, o.ResetEventID, item.userID).Scan(&targetID)
		}
		if insertErr != nil {
			return result, insertErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO openai_weekly_quota_reset_execution_user_links
			(execution_id,target_id,created_at) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, executionID, targetID, o.DetectedAt)
		if err != nil {
			return result, err
		}
		result.ResetUserIDs = append(result.ResetUserIDs, item.userID)
		if sharedTarget {
			continue
		}
		_, err = tx.ExecContext(ctx, `UPDATE user_platform_quotas SET weekly_reserved_generation=$1, updated_at=$2
			WHERE user_id=$3 AND platform='openai' AND deleted_at IS NULL`, targetGeneration, o.DetectedAt, item.userID)
		if err != nil {
			return result, err
		}
		result.Targets = append(result.Targets, service.OpenAIWeeklyQuotaResetTarget{
			ID: targetID, ExecutionID: executionID, RuleID: o.RuleID, ResetEventID: o.ResetEventID,
			UserID: item.userID, Platform: service.PlatformOpenAI, PreviousGeneration: item.generation,
			TargetGeneration: targetGeneration, QuotaWindowStart: quotaWindowStart, Status: service.OpenAIWeeklyQuotaTargetPending,
		})
	}
	result.SkippedUsers = result.NoQuotaUsers
	if result.SkippedUsers < 0 {
		result.SkippedUsers = 0
	}
	var outstandingTargets int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM openai_weekly_quota_reset_execution_user_links l
		JOIN openai_weekly_quota_reset_execution_users t ON t.id=l.target_id
		WHERE l.execution_id=$1 AND t.status<>'succeeded'`, executionID).Scan(&outstandingTargets)
	if err != nil {
		return result, err
	}
	hasOutstandingTargets := outstandingTargets > 0
	_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_executions SET
		matched_users=$1, reset_users=$2, skipped_users=$3, reset_user_ids=$4,
		status=$5, stage=$6, completed_at=$7, updated_at=$8 WHERE id=$9`,
		result.MatchedUsers, len(result.ResetUserIDs), result.SkippedUsers, postgresInt64Array(result.ResetUserIDs),
		func() string {
			if !hasOutstandingTargets {
				return service.OpenAIWeeklyQuotaExecutionSucceeded
			}
			return service.OpenAIWeeklyQuotaExecutionRunning
		}(),
		func() string {
			if !hasOutstandingTargets {
				return "succeeded"
			}
			return "weekly_pending"
		}(),
		func() any {
			if !hasOutstandingTargets {
				return o.DetectedAt
			}
			return nil
		}(), o.DetectedAt, executionID)
	if err != nil {
		return result, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET
		last_observed_reset_at=$1, last_observed_window_seconds=$2,
		last_observed_fetched_at=$3, last_run_at=$3, last_snapshot_event_id=$4,
		source_identity_fingerprint=CASE WHEN $5<>'' THEN $5 ELSE source_identity_fingerprint END,
		execution_status=$6, last_execution_success_at=CASE WHEN $6='succeeded' THEN $3 ELSE last_execution_success_at END,
		last_error='', updated_at=$3 WHERE id=$7`,
		o.OfficialResetAt, o.OfficialWindowSeconds, o.DetectedAt, o.ResetEventID, o.Identity.Fingerprint,
		func() string {
			if !hasOutstandingTargets {
				return "succeeded"
			}
			return "running"
		}(), o.RuleID)
	if err != nil {
		return result, err
	}
	result.Outcome = service.OpenAIWeeklyQuotaObservationTriggered
	result.ExecutionID = executionID
	if !hasOutstandingTargets {
		switch {
		case result.MatchedUsers == 0:
			result.ZeroReason = "no_group_users"
		case result.UsersWithQuota == 0:
			result.ZeroReason = "no_platform_quota"
		default:
			result.ZeroReason = "duplicate_or_already_applied"
		}
	}
	err = tx.Commit()
	return result, err
}

func (r *openAIWeeklyQuotaResetLinkRepository) CompleteExecution(ctx context.Context, id int64, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_executions SET status=$1, error_message='', completed_at=$2, updated_at=$2 WHERE id=$3`, service.OpenAIWeeklyQuotaExecutionSucceeded, at, id)
	return err
}

func (r *openAIWeeklyQuotaResetLinkRepository) MarkExecutionRetryableFailed(ctx context.Context, id int64, message string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_executions SET status=$1, error_message=$2, completed_at=$3, updated_at=$3 WHERE id=$4`, service.OpenAIWeeklyQuotaExecutionRetryableFailed, message, at, id)
	return err
}

func (r *openAIWeeklyQuotaResetLinkRepository) RecordRuleError(ctx context.Context, id int64, message string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET last_observed_fetched_at=$1, last_error=$2, updated_at=$1 WHERE id=$3 AND deleted_at IS NULL`, at, message, id)
	return err
}

func (r *openAIWeeklyQuotaResetLinkRepository) RecordQuerySuccess(ctx context.Context, id int64, snapshot service.OpenAIWeeklyQuotaSnapshot) error {
	_, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET
		last_attempt_at=$1, last_query_success_at=$1, query_status='succeeded',
		query_error_stage='', query_error_reason='', query_error_message='', query_error_request_id='',
		query_failure_count=0, next_query_at=NULL,
		last_snapshot_meter_key=$2, last_snapshot_source=$3, last_snapshot_observed_at=$1,
		last_snapshot_used_percent=$4, source_chatgpt_account_id=$5, source_chatgpt_user_id=$6,
		source_email=$7, source_plan_type=$8, source_identity_source=$9,
		source_identity_verified_at=$10, last_observed_fetched_at=$1,
		last_error=CASE WHEN execution_error_message<>'' THEN execution_error_message ELSE '' END, updated_at=$1
		WHERE id=$11 AND deleted_at IS NULL`,
		snapshot.ObservedAt, snapshot.MeterKey, snapshot.Source, snapshot.UsedPercent,
		snapshot.Identity.ChatGPTAccountID, snapshot.Identity.ChatGPTUserID, snapshot.Identity.Email,
		snapshot.Identity.PlanType, snapshot.Identity.IdentitySource, snapshot.Identity.VerifiedAt, id)
	return err
}

func (r *openAIWeeklyQuotaResetLinkRepository) RecordQueryFailure(ctx context.Context, id int64, failure service.OpenAIWeeklyQuotaFailure) error {
	_, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET
		last_attempt_at=$1, query_status='failed', query_failure_count=query_failure_count+1,
		query_error_stage=$2, query_error_reason=$3, query_error_message=$4, query_error_request_id=$5,
		last_observed_fetched_at=$1, last_error=$4, next_query_at=$6, updated_at=$1
		WHERE id=$7 AND deleted_at IS NULL`, failure.At, failure.Stage, failure.Reason, failure.Message, failure.RequestID, failure.RetryAt, id)
	return err
}

func (r *openAIWeeklyQuotaResetLinkRepository) RecordExecutionFailure(ctx context.Context, ruleID, executionID int64, failure service.OpenAIWeeklyQuotaFailure) error {
	if executionID > 0 {
		_, _ = r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_executions SET
			stage=$1, error_reason=$2, error_message=$3, error_request_id=$4, updated_at=$5 WHERE id=$6`,
			failure.Stage, failure.Reason, failure.Message, failure.RequestID, failure.At, executionID)
	}
	_, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET
		execution_status='failed', execution_error_stage=$1, execution_error_reason=$2,
		execution_error_message=$3, execution_error_request_id=$4, last_error=$3, updated_at=$5
		WHERE id=$6 AND deleted_at IS NULL`, failure.Stage, failure.Reason, failure.Message, failure.RequestID, failure.At, ruleID)
	return err
}

func (r *openAIWeeklyQuotaResetLinkRepository) RecordExecutionSuccess(ctx context.Context, ruleID, executionID int64, at time.Time) error {
	if executionID > 0 {
		_, _ = r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_executions SET stage='succeeded', updated_at=$1 WHERE id=$2`, at, executionID)
	}
	_, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET
		last_execution_success_at=$1, execution_status='succeeded', execution_error_stage='',
		execution_error_reason='', execution_error_message='', execution_error_request_id='',
		last_error=query_error_message, updated_at=$1 WHERE id=$2 AND deleted_at IS NULL`, at, ruleID)
	return err
}

func (r *openAIWeeklyQuotaResetLinkRepository) RebaselineRuleIdentity(ctx context.Context, ruleID int64, identity service.OpenAIWeeklyQuotaSourceIdentity, snapshot service.OpenAIWeeklyQuotaSnapshot) error {
	_, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET
		source_identity_fingerprint=$1, source_chatgpt_account_id=$2, source_chatgpt_user_id=$3,
		source_email=$4, source_plan_type=$5, source_identity_source=$6, source_identity_verified_at=$7,
		last_observed_reset_at=$8, last_observed_window_seconds=$9,
		last_snapshot_event_id=$10, last_snapshot_meter_key=$11, last_snapshot_source=$12,
		last_snapshot_observed_at=$13, last_snapshot_used_percent=$14,
		query_status='succeeded', query_error_stage='', query_error_reason='', query_error_message='',
		query_error_request_id='', query_failure_count=0, next_query_at=NULL,
		execution_status='baseline', execution_error_stage='', execution_error_reason='',
		execution_error_message='', execution_error_request_id='', last_error='', updated_at=$13
		WHERE id=$15 AND deleted_at IS NULL`,
		identity.Fingerprint, identity.ChatGPTAccountID, identity.ChatGPTUserID, identity.Email,
		identity.PlanType, identity.IdentitySource, identity.VerifiedAt,
		snapshot.ResetAt, snapshot.WindowSeconds, snapshot.EventID, snapshot.MeterKey, snapshot.Source,
		snapshot.ObservedAt, snapshot.UsedPercent, ruleID)
	return err
}

func (r *openAIWeeklyQuotaResetLinkRepository) ClaimResetTargets(ctx context.Context, executionID int64, owner string, limit int, now time.Time, lease time.Duration) ([]service.OpenAIWeeklyQuotaResetTarget, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `WITH claimed AS (
		SELECT t.id FROM openai_weekly_quota_reset_execution_users t
		WHERE t.status <> 'succeeded' AND ($1 = 0 OR t.execution_id = $1)
		AND (t.lease_until IS NULL OR t.lease_until < $2)
		AND NOT EXISTS (SELECT 1 FROM openai_weekly_quota_reset_execution_users earlier
			WHERE earlier.user_id=t.user_id AND earlier.platform=t.platform
			AND earlier.target_generation<t.target_generation AND earlier.status<>'succeeded')
		ORDER BY t.id LIMIT $4 FOR UPDATE SKIP LOCKED
	)
	UPDATE openai_weekly_quota_reset_execution_users t SET
		lease_owner=$3, lease_until=$5, attempts=t.attempts+1, updated_at=$2
	FROM claimed WHERE t.id=claimed.id
	RETURNING t.id, t.execution_id,
		(SELECT e.rule_id FROM openai_weekly_quota_reset_executions e WHERE e.id=t.execution_id),
		t.reset_event_id, t.user_id, t.platform, t.previous_generation, t.target_generation,
		t.quota_window_start, t.status, t.lease_owner`, executionID, now, owner, limit, now.Add(lease))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	targets := make([]service.OpenAIWeeklyQuotaResetTarget, 0)
	for rows.Next() {
		var target service.OpenAIWeeklyQuotaResetTarget
		if err := rows.Scan(&target.ID, &target.ExecutionID, &target.RuleID, &target.ResetEventID,
			&target.UserID, &target.Platform, &target.PreviousGeneration, &target.TargetGeneration,
			&target.QuotaWindowStart, &target.Status, &target.LeaseOwner); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (r *openAIWeeklyQuotaResetLinkRepository) MarkTargetCachePrepared(ctx context.Context, target service.OpenAIWeeklyQuotaResetTarget, at time.Time) error {
	result, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_execution_users SET
		status='weekly_cache_prepared', cache_prepared_at=COALESCE(cache_prepared_at,$1),
		last_error_reason='', last_error_message='', last_error_request_id='', updated_at=$1
		WHERE id=$2 AND status='weekly_pending' AND lease_owner=$3`, at, target.ID, target.LeaseOwner)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return service.ErrOpenAIWeeklyQuotaClaimLost
	}
	return nil
}

func (r *openAIWeeklyQuotaResetLinkRepository) ApplyTargetDatabaseReset(ctx context.Context, target service.OpenAIWeeklyQuotaResetTarget, at time.Time) (err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var status, leaseOwner string
	err = tx.QueryRowContext(ctx, `SELECT status,lease_owner FROM openai_weekly_quota_reset_execution_users WHERE id=$1 FOR UPDATE`, target.ID).Scan(&status, &leaseOwner)
	if err != nil {
		return err
	}
	if status != service.OpenAIWeeklyQuotaTargetCachePrepared || leaseOwner != target.LeaseOwner {
		return service.ErrOpenAIWeeklyQuotaClaimLost
	}
	result, err := tx.ExecContext(ctx, `UPDATE user_platform_quotas SET
		weekly_usage_usd=CASE WHEN weekly_quota_generation < $1 THEN 0 ELSE weekly_usage_usd END,
		weekly_window_start=CASE WHEN weekly_quota_generation < $1
			OR (weekly_quota_generation = $1 AND weekly_window_start IS NULL) THEN $2 ELSE weekly_window_start END,
		weekly_quota_generation=GREATEST(weekly_quota_generation,$1),
		weekly_reserved_generation=GREATEST(weekly_reserved_generation,$1), updated_at=$3
		WHERE user_id=$4 AND platform=$5 AND deleted_at IS NULL
		AND weekly_reserved_generation <= $1`,
		target.TargetGeneration, target.QuotaWindowStart, at, target.UserID, target.Platform)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		// 更高代次已经提交时，旧目标只需完成自己的状态推进，不能回写窗口或用量。
		var currentGeneration int64
		if err := tx.QueryRowContext(ctx, `SELECT weekly_quota_generation FROM user_platform_quotas
			WHERE user_id=$1 AND platform=$2 AND deleted_at IS NULL FOR SHARE`, target.UserID, target.Platform).Scan(&currentGeneration); err != nil {
			return err
		}
		if currentGeneration < target.TargetGeneration {
			return service.ErrOpenAIWeeklyQuotaClaimLost
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_execution_users SET
		status='weekly_db_applied', db_applied_at=COALESCE(db_applied_at,$1), updated_at=$1
		WHERE id=$2 AND status='weekly_cache_prepared' AND lease_owner=$3`, at, target.ID, target.LeaseOwner)
	if err != nil {
		return err
	}
	err = tx.Commit()
	return err
}

func (r *openAIWeeklyQuotaResetLinkRepository) MarkTargetSucceeded(ctx context.Context, target service.OpenAIWeeklyQuotaResetTarget, at time.Time) (completed bool, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var executionID int64
	var targetStatus, leaseOwner string
	err = tx.QueryRowContext(ctx, `SELECT execution_id,status,lease_owner FROM openai_weekly_quota_reset_execution_users WHERE id=$1 FOR UPDATE`, target.ID).Scan(&executionID, &targetStatus, &leaseOwner)
	if err != nil {
		return false, err
	}
	if targetStatus != service.OpenAIWeeklyQuotaTargetDBApplied || leaseOwner != target.LeaseOwner {
		return false, service.ErrOpenAIWeeklyQuotaClaimLost
	}
	_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_execution_users SET
		status='succeeded', cache_applied_at=COALESCE(cache_applied_at,$1), lease_owner='', lease_until=NULL,
		last_error_reason='', last_error_message='', last_error_request_id='', updated_at=$1
		WHERE id=$2 AND status='weekly_db_applied' AND lease_owner=$3`, at, target.ID, target.LeaseOwner)
	if err != nil {
		return false, err
	}
	executionRows, err := tx.QueryContext(ctx, `SELECT e.id FROM openai_weekly_quota_reset_executions e
		JOIN openai_weekly_quota_reset_execution_user_links l ON l.execution_id=e.id
		WHERE l.target_id=$1 ORDER BY e.id FOR UPDATE OF e`, target.ID)
	if err != nil {
		return false, err
	}
	linkedExecutions := make([]int64, 0)
	for executionRows.Next() {
		var linkedID int64
		if err := executionRows.Scan(&linkedID); err != nil {
			_ = executionRows.Close()
			return false, err
		}
		linkedExecutions = append(linkedExecutions, linkedID)
	}
	if err := executionRows.Close(); err != nil {
		return false, err
	}
	for _, linkedExecutionID := range linkedExecutions {
		var remaining int
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM openai_weekly_quota_reset_execution_user_links l
			JOIN openai_weekly_quota_reset_execution_users t ON t.id=l.target_id
			WHERE l.execution_id=$1 AND t.status<>'succeeded'`, linkedExecutionID).Scan(&remaining)
		if err != nil {
			return false, err
		}
		if remaining != 0 {
			continue
		}
		_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_executions SET
			status=$1, stage='succeeded', error_reason='', error_message='', error_request_id='',
			completed_at=$2, updated_at=$2 WHERE id=$3`, service.OpenAIWeeklyQuotaExecutionSucceeded, at, linkedExecutionID)
		if err != nil {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules r SET
			last_execution_success_at=$1, execution_status='succeeded', execution_error_stage='',
			execution_error_reason='', execution_error_message='', execution_error_request_id='',
			last_error=r.query_error_message, updated_at=$1
			FROM openai_weekly_quota_reset_executions e WHERE e.id=$2 AND r.id=e.rule_id`, at, linkedExecutionID)
		if err != nil {
			return false, err
		}
		if linkedExecutionID == executionID {
			completed = true
		}
	}
	err = tx.Commit()
	return completed, err
}

func (r *openAIWeeklyQuotaResetLinkRepository) MarkTargetRetryable(ctx context.Context, target service.OpenAIWeeklyQuotaResetTarget, failure service.OpenAIWeeklyQuotaFailure) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_execution_users SET
		lease_owner='', lease_until=NULL, last_error_reason=$1, last_error_message=$2,
		last_error_request_id=$3, updated_at=$4 WHERE id=$5 AND lease_owner=$6 AND status<>'succeeded'`,
		failure.Reason, failure.Message, failure.RequestID, failure.At, target.ID, target.LeaseOwner)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return service.ErrOpenAIWeeklyQuotaClaimLost
	}
	executionResult, err := tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_executions SET
		status=$1, stage=$2, error_reason=$3, error_message=$4, error_request_id=$5, updated_at=$6 WHERE id=$7 AND status<>$8`,
		service.OpenAIWeeklyQuotaExecutionRetryableFailed, failure.Stage, failure.Reason, failure.Message,
		failure.RequestID, failure.At, target.ExecutionID, service.OpenAIWeeklyQuotaExecutionSucceeded)
	if err != nil {
		return err
	}
	executionUpdated, err := executionResult.RowsAffected()
	if err != nil {
		return err
	}
	if executionUpdated == 0 {
		return service.ErrOpenAIWeeklyQuotaClaimLost
	}
	_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules r SET
		execution_status='retryable_failed', execution_error_stage=$1, execution_error_reason=$2,
		execution_error_message=$3, execution_error_request_id=$4, last_error=$3, updated_at=$5
		FROM openai_weekly_quota_reset_executions e
		WHERE e.id=$6 AND e.status=$7 AND r.id=e.rule_id`,
		failure.Stage, failure.Reason, failure.Message, failure.RequestID, failure.At,
		target.ExecutionID, service.OpenAIWeeklyQuotaExecutionRetryableFailed)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *openAIWeeklyQuotaResetLinkRepository) RecordResetEvent(ctx context.Context, eventID string, evidence service.OpenAIWeeklyQuotaResetOperationEvidence, decision service.OpenAIWeeklyQuotaResetEvidenceDecision, snapshot *service.OpenAIWeeklyQuotaSnapshot) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	status := "candidate"
	dispatchStatus := "pending"
	if decision.Confirmed {
		status = "confirmed"
	} else if !weeklyQuotaEventShouldRemainCandidate(decision, evidence) {
		status = "rejected"
		dispatchStatus = "dispatched"
	}
	var identityFingerprint, meterKey string
	eventSource := strings.TrimSpace(evidence.EventSource)
	if eventSource == "" {
		eventSource = "manual_reset"
	}
	var resetAt, windowStart *time.Time
	var windowSeconds *int64
	var postUsed *float64
	if snapshot != nil {
		identityFingerprint = snapshot.Identity.Fingerprint
		meterKey = snapshot.MeterKey
		resetAt, windowStart = &snapshot.ResetAt, &snapshot.WindowStart
		windowSeconds = &snapshot.WindowSeconds
		if evidence.After != nil {
			postUsed = snapshot.UsedPercent
		}
	}
	if meterKey == "" {
		meterKey = "codex_weekly"
	}
	preUsed := weeklyUsagePercent(evidence.Before)
	observedAt := evidence.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	var storedEventID int64
	var insertedEvent bool
	err = tx.QueryRowContext(ctx, `INSERT INTO openai_weekly_quota_reset_events
		(source_account_id, source_identity_fingerprint, reset_event_id, meter_key, event_source,
		evidence_kind, status, dispatch_status, reason, official_reset_at, official_window_start, official_window_seconds,
		pre_used_percent, post_used_percent, observed_at, confirmed_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$15,
			$8,$9,$10,$11,$12,$13,$14,CASE WHEN $7='confirmed' THEN $14 ELSE NULL END,$14,$14)
		ON CONFLICT (source_account_id, source_identity_fingerprint, reset_event_id) DO UPDATE SET
		evidence_kind=CASE WHEN openai_weekly_quota_reset_events.status='confirmed' THEN openai_weekly_quota_reset_events.evidence_kind ELSE EXCLUDED.evidence_kind END,
		status=CASE WHEN openai_weekly_quota_reset_events.status='confirmed' THEN 'confirmed'
			WHEN openai_weekly_quota_reset_events.status='rejected' AND EXCLUDED.status<>'confirmed' THEN 'rejected'
			WHEN EXCLUDED.status='confirmed' THEN 'confirmed' ELSE EXCLUDED.status END,
		reason=CASE WHEN openai_weekly_quota_reset_events.status='confirmed' THEN openai_weekly_quota_reset_events.reason
			WHEN openai_weekly_quota_reset_events.status='rejected' AND EXCLUDED.status<>'confirmed' THEN openai_weekly_quota_reset_events.reason
			ELSE EXCLUDED.reason END,
		dispatch_status=CASE
			WHEN openai_weekly_quota_reset_events.status='confirmed' AND openai_weekly_quota_reset_events.dispatch_status='dispatched' AND EXCLUDED.status='confirmed' THEN 'dispatched'
			WHEN EXCLUDED.status='confirmed' THEN 'pending'
			WHEN openai_weekly_quota_reset_events.dispatch_status='dispatched' THEN 'dispatched'
			ELSE EXCLUDED.dispatch_status END,
		official_reset_at=COALESCE(EXCLUDED.official_reset_at,openai_weekly_quota_reset_events.official_reset_at),
		official_window_start=COALESCE(EXCLUDED.official_window_start,openai_weekly_quota_reset_events.official_window_start),
		official_window_seconds=COALESCE(EXCLUDED.official_window_seconds,openai_weekly_quota_reset_events.official_window_seconds),
		pre_used_percent=COALESCE(EXCLUDED.pre_used_percent,openai_weekly_quota_reset_events.pre_used_percent),
		post_used_percent=COALESCE(EXCLUDED.post_used_percent,openai_weekly_quota_reset_events.post_used_percent),
		confirmed_at=COALESCE(EXCLUDED.confirmed_at,openai_weekly_quota_reset_events.confirmed_at), updated_at=EXCLUDED.updated_at
		RETURNING id,(xmax=0)`,
		evidence.SourceAccountID, identityFingerprint, eventID, meterKey, eventSource,
		decision.EvidenceKind, status, decision.Reason, resetAt, windowStart, windowSeconds,
		preUsed, postUsed, observedAt, dispatchStatus).Scan(&storedEventID, &insertedEvent)
	if err != nil {
		return err
	}
	if !insertedEvent {
		// 事件第一次写入时即冻结目标规则集合；即使集合为空，重试也不能吸收后来新增的规则。
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO openai_weekly_quota_reset_event_rules
		(event_id,rule_id,source_account_id,target_group_id,created_at)
		SELECT $1,id,source_account_id,target_group_id,$2 FROM openai_weekly_quota_reset_rules
		WHERE source_account_id=$3 AND enabled=TRUE AND deleted_at IS NULL ON CONFLICT DO NOTHING`,
		storedEventID, observedAt, evidence.SourceAccountID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func weeklyQuotaEventShouldRemainCandidate(decision service.OpenAIWeeklyQuotaResetEvidenceDecision, evidence service.OpenAIWeeklyQuotaResetOperationEvidence) bool {
	if decision.Reason == "weekly_window_unavailable" {
		return true
	}
	return decision.Reason == "weekly_usage_unknown" && weeklyUsagePercent(evidence.Before) != nil
}

func (r *openAIWeeklyQuotaResetLinkRepository) ListResetEvents(ctx context.Context, limit int) ([]service.OpenAIWeeklyQuotaResetEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `SELECT e.id,e.source_account_id,COALESCE(a.name,''),e.event_source,
		e.evidence_kind,e.status,e.reason,e.official_reset_at,e.pre_used_percent,e.post_used_percent,
		e.observed_at,e.confirmed_at,e.dispatch_status FROM openai_weekly_quota_reset_events e
		LEFT JOIN accounts a ON a.id=e.source_account_id ORDER BY e.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]service.OpenAIWeeklyQuotaResetEvent, 0)
	for rows.Next() {
		var item service.OpenAIWeeklyQuotaResetEvent
		if err := rows.Scan(&item.ID, &item.SourceAccountID, &item.SourceAccountName, &item.EventSource,
			&item.EvidenceKind, &item.Status, &item.Reason, &item.OfficialResetAt,
			&item.PreUsedPercent, &item.PostUsedPercent, &item.ObservedAt, &item.ConfirmedAt, &item.DispatchStatus); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *openAIWeeklyQuotaResetLinkRepository) ListResetEventRules(ctx context.Context, resetEventID string, sourceAccountID int64) ([]service.OpenAIWeeklyQuotaResetRule, error) {
	rows, err := r.db.QueryContext(ctx, openAIWeeklyRuleSelect+`
		JOIN openai_weekly_quota_reset_event_rules er ON er.rule_id=r.id
		JOIN openai_weekly_quota_reset_events ev ON ev.id=er.event_id
		WHERE ev.reset_event_id=$1 AND ev.source_account_id=$2 AND r.deleted_at IS NULL AND r.enabled=TRUE
		AND r.source_account_id=er.source_account_id AND r.target_group_id=er.target_group_id
		ORDER BY r.id`, resetEventID, sourceAccountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]service.OpenAIWeeklyQuotaResetRule, 0)
	for rows.Next() {
		item, err := scanOpenAIWeeklyRule(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func (r *openAIWeeklyQuotaResetLinkRepository) ClaimResetEvents(ctx context.Context, owner string, limit int, now time.Time, lease time.Duration) ([]service.OpenAIWeeklyQuotaResetEvent, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := r.db.QueryContext(ctx, `WITH claimed AS (
		SELECT e.id FROM openai_weekly_quota_reset_events e
		WHERE e.dispatch_status<>'dispatched' AND (e.dispatch_lease_until IS NULL OR e.dispatch_lease_until<$1)
		AND NOT EXISTS (
			SELECT 1 FROM openai_weekly_quota_reset_events earlier
			WHERE earlier.source_account_id=e.source_account_id
			AND earlier.source_identity_fingerprint=e.source_identity_fingerprint
			AND earlier.meter_key=e.meter_key
			AND earlier.dispatch_status<>'dispatched'
			AND (earlier.observed_at,earlier.id) < (e.observed_at,e.id)
		)
		ORDER BY e.observed_at,e.id LIMIT $3 FOR UPDATE SKIP LOCKED
	)
	UPDATE openai_weekly_quota_reset_events e SET dispatch_lease_owner=$2,dispatch_lease_until=$4,
		dispatch_attempts=e.dispatch_attempts+1,updated_at=$1 FROM claimed WHERE e.id=claimed.id
	RETURNING e.id,e.source_account_id,COALESCE((SELECT a.name FROM accounts a WHERE a.id=e.source_account_id),''),
		e.source_identity_fingerprint,e.reset_event_id,e.meter_key,e.event_source,e.evidence_kind,e.status,e.reason,
		e.official_reset_at,e.official_window_start,e.official_window_seconds,e.pre_used_percent,e.post_used_percent,
		e.observed_at,e.confirmed_at,e.dispatch_status,e.dispatch_lease_owner`, now, owner, limit, now.Add(lease))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]service.OpenAIWeeklyQuotaResetEvent, 0)
	for rows.Next() {
		var item service.OpenAIWeeklyQuotaResetEvent
		if err := rows.Scan(&item.ID, &item.SourceAccountID, &item.SourceAccountName,
			&item.IdentityFingerprint, &item.ResetEventID, &item.MeterKey, &item.EventSource,
			&item.EvidenceKind, &item.Status, &item.Reason, &item.OfficialResetAt,
			&item.WindowStart, &item.WindowSeconds, &item.PreUsedPercent, &item.PostUsedPercent,
			&item.ObservedAt, &item.ConfirmedAt, &item.DispatchStatus, &item.LeaseOwner); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *openAIWeeklyQuotaResetLinkRepository) ConfirmResetEvent(ctx context.Context, eventID int64, owner string, snapshot service.OpenAIWeeklyQuotaSnapshot, evidenceKind string, at time.Time) error {
	result, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_events SET
		status='confirmed',dispatch_status='pending',evidence_kind=$1,reason='',
		source_identity_fingerprint=$2,meter_key=$3,official_reset_at=$4,official_window_start=$5,
		official_window_seconds=$6,post_used_percent=$7,confirmed_at=$8,
		dispatch_error_reason='',updated_at=$8 WHERE id=$9 AND dispatch_status<>'dispatched' AND dispatch_lease_owner=$10`,
		evidenceKind, snapshot.Identity.Fingerprint, snapshot.MeterKey, snapshot.ResetAt,
		snapshot.WindowStart, snapshot.WindowSeconds, snapshot.UsedPercent, at, eventID, owner)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return service.ErrOpenAIWeeklyQuotaClaimLost
	}
	return nil
}

func (r *openAIWeeklyQuotaResetLinkRepository) MarkResetEventDispatched(ctx context.Context, eventID int64, owner string, at time.Time) error {
	result, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_events SET
		dispatch_status='dispatched',dispatch_lease_owner='',dispatch_lease_until=NULL,
		dispatch_error_reason='',updated_at=$1 WHERE id=$2 AND dispatch_status<>'dispatched' AND dispatch_lease_owner=$3`, at, eventID, owner)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return service.ErrOpenAIWeeklyQuotaClaimLost
	}
	return nil
}

func (r *openAIWeeklyQuotaResetLinkRepository) RetryResetEvent(ctx context.Context, eventID int64, owner, reason string, at, retryAt time.Time) error {
	result, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_events SET
		dispatch_status='pending',dispatch_lease_owner='',dispatch_lease_until=$1,
		dispatch_error_reason=$2,updated_at=$3 WHERE id=$4 AND dispatch_status<>'dispatched' AND dispatch_lease_owner=$5`,
		retryAt, reason, at, eventID, owner)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return service.ErrOpenAIWeeklyQuotaClaimLost
	}
	return nil
}

func (r *openAIWeeklyQuotaResetLinkRepository) RejectResetEvent(ctx context.Context, eventID int64, owner, reason string, at time.Time) error {
	result, err := r.db.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_events SET
		status='rejected',reason=$1,dispatch_status='dispatched',dispatch_lease_owner='',
		dispatch_lease_until=NULL,dispatch_error_reason='',updated_at=$2 WHERE id=$3 AND dispatch_status<>'dispatched' AND dispatch_lease_owner=$4`, reason, at, eventID, owner)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return service.ErrOpenAIWeeklyQuotaClaimLost
	}
	return nil
}

func weeklyUsagePercent(usage *service.OpenAIQuotaUsage) *float64 {
	if usage == nil || usage.RateLimit == nil {
		return nil
	}
	for _, window := range []*service.OpenAIRateLimitWindow{usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow} {
		if window == nil || window.LimitWindowSeconds < int64(6*24*time.Hour/time.Second) || window.LimitWindowSeconds > int64(8*24*time.Hour/time.Second) {
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
