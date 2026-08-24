CREATE TABLE IF NOT EXISTS openai_weekly_quota_reset_rules (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    source_account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    target_group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    last_observed_reset_at TIMESTAMPTZ,
    last_observed_window_seconds BIGINT,
    last_observed_fetched_at TIMESTAMPTZ,
    last_run_at TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_openai_weekly_reset_rules_binding_active
    ON openai_weekly_quota_reset_rules(source_account_id, target_group_id)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_openai_weekly_reset_rules_enabled
    ON openai_weekly_quota_reset_rules(enabled) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS openai_weekly_quota_reset_executions (
    id BIGSERIAL PRIMARY KEY,
    rule_id BIGINT NOT NULL REFERENCES openai_weekly_quota_reset_rules(id) ON DELETE CASCADE,
    source_account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    target_group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    official_reset_at TIMESTAMPTZ NOT NULL,
    official_window_start TIMESTAMPTZ NOT NULL,
    official_window_seconds BIGINT NOT NULL,
    status VARCHAR(32) NOT NULL,
    matched_users INTEGER NOT NULL DEFAULT 0,
    reset_users INTEGER NOT NULL DEFAULT 0,
    skipped_users INTEGER NOT NULL DEFAULT 0,
    reset_user_ids BIGINT[] NOT NULL DEFAULT '{}',
    error_message TEXT NOT NULL DEFAULT '',
    detected_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_openai_weekly_reset_execution_window
    ON openai_weekly_quota_reset_executions(rule_id, official_reset_at);
CREATE INDEX IF NOT EXISTS idx_openai_weekly_reset_executions_rule_created
    ON openai_weekly_quota_reset_executions(rule_id, created_at DESC);
