package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type openAIWeeklyQuotaResetLinkRepository struct{ db *sql.DB }

const openAIWeeklyQuotaExecutionLease = 2 * time.Minute

func NewOpenAIWeeklyQuotaResetLinkRepository(db *sql.DB) service.OpenAIWeeklyQuotaResetLinkRepository {
	return &openAIWeeklyQuotaResetLinkRepository{db: db}
}

const openAIWeeklyRuleSelect = `SELECT r.id, r.name, r.description, r.enabled,
	r.source_account_id, COALESCE(a.name, ''), r.target_group_id, COALESCE(g.name, ''),
	r.last_observed_reset_at, r.last_observed_window_seconds, r.last_observed_fetched_at,
	r.last_run_at, r.last_error, r.created_at, r.updated_at
	FROM openai_weekly_quota_reset_rules r
	LEFT JOIN accounts a ON a.id = r.source_account_id
	LEFT JOIN groups g ON g.id = r.target_group_id`

type weeklyRuleRowScanner interface{ Scan(dest ...any) error }

func scanOpenAIWeeklyRule(row weeklyRuleRowScanner) (*service.OpenAIWeeklyQuotaResetRule, error) {
	var r service.OpenAIWeeklyQuotaResetRule
	err := row.Scan(&r.ID, &r.Name, &r.Description, &r.Enabled,
		&r.SourceAccountID, &r.SourceAccountName, &r.TargetGroupID, &r.TargetGroupName,
		&r.LastObservedResetAt, &r.LastObservedWindowSeconds, &r.LastObservedFetchedAt,
		&r.LastRunAt, &r.LastError, &r.CreatedAt, &r.UpdatedAt)
	return &r, err
}

func (r *openAIWeeklyQuotaResetLinkRepository) ListRules(ctx context.Context) ([]service.OpenAIWeeklyQuotaResetRule, error) {
	return r.listRules(ctx, false)
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
		e.official_reset_at, e.official_window_start, e.official_window_seconds, e.status,
		e.matched_users, e.reset_users, e.skipped_users, e.error_message, e.detected_at,
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
			&e.OfficialResetAt, &e.OfficialWindowStart, &e.OfficialWindowSeconds, &e.Status,
			&e.MatchedUsers, &e.ResetUsers, &e.SkippedUsers, &e.ErrorMessage, &e.DetectedAt,
			&e.CompletedAt, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, e)
	}
	return items, rows.Err()
}

func (r *openAIWeeklyQuotaResetLinkRepository) ApplyObservedWeeklyWindow(ctx context.Context, o service.OpenAIWeeklyQuotaObservation) (result service.OpenAIWeeklyQuotaObservationResult, err error) {
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
	err = tx.QueryRowContext(ctx, `SELECT target_group_id, last_observed_reset_at
		FROM openai_weekly_quota_reset_rules
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE`, o.RuleID).Scan(&groupID, &previous)
	if err != nil {
		return result, err
	}

	if !previous.Valid {
		_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules
			SET last_observed_reset_at=$1, last_observed_window_seconds=$2,
			last_observed_fetched_at=$3, last_error='', updated_at=$3 WHERE id=$4`,
			o.OfficialResetAt, o.OfficialWindowSeconds, o.DetectedAt, o.RuleID)
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
	if o.OfficialResetAt.Equal(previous.Time) {
		err = tx.QueryRowContext(ctx, `SELECT id, status, updated_at, matched_users, skipped_users, reset_user_ids
			FROM openai_weekly_quota_reset_executions WHERE rule_id=$1 AND official_reset_at=$2`, o.RuleID, o.OfficialResetAt).
			Scan(&executionID, &executionStatus, &executionUpdatedAt, &result.MatchedUsers, &result.SkippedUsers, pq.Array(&result.ResetUserIDs))
		if errors.Is(err, sql.ErrNoRows) || executionStatus == service.OpenAIWeeklyQuotaExecutionSucceeded || executionStatus == service.OpenAIWeeklyQuotaExecutionPermanentFailed {
			err = nil
			_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET last_observed_fetched_at=$1, updated_at=$1 WHERE id=$2`, o.DetectedAt, o.RuleID)
			if err != nil {
				return result, err
			}
			result.Outcome = service.OpenAIWeeklyQuotaObservationUnchanged
			err = tx.Commit()
			return result, err
		}
		if err != nil {
			return result, err
		}
		if (executionStatus == service.OpenAIWeeklyQuotaExecutionRunning || executionStatus == service.OpenAIWeeklyQuotaExecutionPending) &&
			o.DetectedAt.Before(executionUpdatedAt.Add(openAIWeeklyQuotaExecutionLease)) {
			_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET last_observed_fetched_at=$1, updated_at=$1 WHERE id=$2`, o.DetectedAt, o.RuleID)
			if err != nil {
				return result, err
			}
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
	} else {
		err = tx.QueryRowContext(ctx, `INSERT INTO openai_weekly_quota_reset_executions
			(rule_id, source_account_id, target_group_id, official_reset_at, official_window_start,
			official_window_seconds, status, detected_at, created_at, updated_at)
			SELECT id, source_account_id, target_group_id, $2, $3, $4, $5, $6, $6, $6
			FROM openai_weekly_quota_reset_rules WHERE id=$1 RETURNING id`,
			o.RuleID, o.OfficialResetAt, o.OfficialWindowStart, o.OfficialWindowSeconds,
			service.OpenAIWeeklyQuotaExecutionRunning, o.DetectedAt).Scan(&executionID)
		if err != nil {
			return result, err
		}
	}

	err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT u.id)
		FROM user_allowed_groups uag JOIN users u ON u.id=uag.user_id
		WHERE uag.group_id=$1 AND u.deleted_at IS NULL AND u.status='active'`, groupID).Scan(&result.MatchedUsers)
	if err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE user_platform_quotas q
		SET weekly_usage_usd=0, weekly_window_start=$1, updated_at=$2
		FROM user_allowed_groups uag, users u
		WHERE uag.group_id=$3 AND uag.user_id=u.id AND q.user_id=u.id
		AND q.platform='openai' AND q.deleted_at IS NULL
		AND u.deleted_at IS NULL AND u.status='active'
		AND (q.weekly_window_start IS NULL OR q.weekly_window_start < $1)
		RETURNING q.user_id`, o.OfficialWindowStart, o.DetectedAt, groupID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			_ = rows.Close()
			return result, scanErr
		}
		result.ResetUserIDs = append(result.ResetUserIDs, id)
	}
	err = rows.Close()
	if err != nil {
		return result, err
	}
	result.SkippedUsers = result.MatchedUsers - len(result.ResetUserIDs)
	if result.SkippedUsers < 0 {
		result.SkippedUsers = 0
	}
	_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_executions SET
		matched_users=$1, reset_users=$2, skipped_users=$3, reset_user_ids=$4, updated_at=$5 WHERE id=$6`,
		result.MatchedUsers, len(result.ResetUserIDs), result.SkippedUsers, pq.Array(result.ResetUserIDs), o.DetectedAt, executionID)
	if err != nil {
		return result, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE openai_weekly_quota_reset_rules SET
		last_observed_reset_at=$1, last_observed_window_seconds=$2,
		last_observed_fetched_at=$3, last_run_at=$3, last_error='', updated_at=$3 WHERE id=$4`,
		o.OfficialResetAt, o.OfficialWindowSeconds, o.DetectedAt, o.RuleID)
	if err != nil {
		return result, err
	}
	result.Outcome = service.OpenAIWeeklyQuotaObservationTriggered
	result.ExecutionID = executionID
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
