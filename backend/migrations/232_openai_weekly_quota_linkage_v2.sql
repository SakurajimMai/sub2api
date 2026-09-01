-- OpenAI/Codex 周额度联动可靠性升级。
-- 该迁移只做向前兼容扩展；不得回写或修改已执行的 231 迁移。
-- 部署约束：应用节点必须先全部停止（尤其是仍写入 v1 execution/Redis schema 的旧版本），
-- 再执行本迁移并启动新版本；此迁移不承诺 v1/v2 滚动混跑安全。

ALTER TABLE user_platform_quotas
    ADD COLUMN IF NOT EXISTS weekly_quota_generation BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS weekly_reserved_generation BIGINT NOT NULL DEFAULT 0;

UPDATE user_platform_quotas
SET weekly_reserved_generation = weekly_quota_generation
WHERE weekly_reserved_generation < weekly_quota_generation;

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_weekly_generation_order,
    ADD CONSTRAINT user_platform_quotas_weekly_generation_order
        CHECK (weekly_reserved_generation >= weekly_quota_generation) NOT VALID;
ALTER TABLE user_platform_quotas
    VALIDATE CONSTRAINT user_platform_quotas_weekly_generation_order;

ALTER TABLE openai_weekly_quota_reset_rules
    ADD COLUMN IF NOT EXISTS source_identity_fingerprint TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_chatgpt_account_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_chatgpt_user_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_email TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_plan_type TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_identity_source VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_identity_verified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_snapshot_meter_key VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_snapshot_source VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_snapshot_observed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_snapshot_used_percent DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS last_snapshot_event_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_query_success_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_execution_success_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS query_status VARCHAR(32) NOT NULL DEFAULT 'never',
    ADD COLUMN IF NOT EXISTS query_error_stage VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS query_error_reason VARCHAR(96) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS query_error_message TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS query_error_request_id VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS execution_status VARCHAR(32) NOT NULL DEFAULT 'never',
    ADD COLUMN IF NOT EXISTS execution_error_stage VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS execution_error_reason VARCHAR(96) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS execution_error_message TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS execution_error_request_id VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS query_failure_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS next_query_at TIMESTAMPTZ;

ALTER TABLE openai_weekly_quota_reset_executions
    ADD COLUMN IF NOT EXISTS reset_event_id TEXT,
    ADD COLUMN IF NOT EXISTS event_source VARCHAR(32) NOT NULL DEFAULT 'poll',
    ADD COLUMN IF NOT EXISTS evidence_kind VARCHAR(48) NOT NULL DEFAULT 'weekly_window_advanced',
    ADD COLUMN IF NOT EXISTS stage VARCHAR(32) NOT NULL DEFAULT 'weekly_pending',
    ADD COLUMN IF NOT EXISTS error_reason VARCHAR(96) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS error_request_id VARCHAR(128) NOT NULL DEFAULT '';

UPDATE openai_weekly_quota_reset_executions
SET reset_event_id = 'legacy:' || id::TEXT
WHERE reset_event_id IS NULL OR reset_event_id = '';

ALTER TABLE openai_weekly_quota_reset_executions
    ALTER COLUMN reset_event_id SET DEFAULT ('legacy:' || gen_random_uuid()::TEXT),
    ALTER COLUMN reset_event_id SET NOT NULL;

DROP INDEX IF EXISTS idx_openai_weekly_reset_execution_window;
CREATE UNIQUE INDEX IF NOT EXISTS idx_openai_weekly_reset_execution_event
    ON openai_weekly_quota_reset_executions(rule_id, reset_event_id);

CREATE TABLE IF NOT EXISTS openai_weekly_quota_reset_events (
    id BIGSERIAL PRIMARY KEY,
    source_account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    source_identity_fingerprint TEXT NOT NULL,
    reset_event_id TEXT NOT NULL,
    meter_key VARCHAR(64) NOT NULL DEFAULT 'codex_weekly',
    event_source VARCHAR(32) NOT NULL,
    evidence_kind VARCHAR(48) NOT NULL,
    status VARCHAR(32) NOT NULL,
    dispatch_status VARCHAR(32) NOT NULL DEFAULT 'pending',
    dispatch_attempts INTEGER NOT NULL DEFAULT 0,
    dispatch_lease_owner VARCHAR(128) NOT NULL DEFAULT '',
    dispatch_lease_until TIMESTAMPTZ,
    dispatch_error_reason VARCHAR(96) NOT NULL DEFAULT '',
    reason VARCHAR(96) NOT NULL DEFAULT '',
    official_reset_at TIMESTAMPTZ,
    official_window_start TIMESTAMPTZ,
    official_window_seconds BIGINT,
    pre_used_percent DOUBLE PRECISION,
    post_used_percent DOUBLE PRECISION,
    observed_at TIMESTAMPTZ NOT NULL,
    confirmed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (source_account_id, source_identity_fingerprint, reset_event_id)
);

CREATE INDEX IF NOT EXISTS idx_openai_weekly_reset_events_pending
    ON openai_weekly_quota_reset_events(dispatch_status, dispatch_lease_until, observed_at)
    WHERE dispatch_status <> 'dispatched';

CREATE TABLE IF NOT EXISTS openai_weekly_quota_reset_event_rules (
    event_id BIGINT NOT NULL REFERENCES openai_weekly_quota_reset_events(id) ON DELETE CASCADE,
    rule_id BIGINT NOT NULL REFERENCES openai_weekly_quota_reset_rules(id) ON DELETE CASCADE,
    source_account_id BIGINT NOT NULL,
    target_group_id BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (event_id, rule_id)
);

CREATE TABLE IF NOT EXISTS openai_weekly_quota_reset_execution_users (
    id BIGSERIAL PRIMARY KEY,
    execution_id BIGINT NOT NULL REFERENCES openai_weekly_quota_reset_executions(id) ON DELETE CASCADE,
    reset_event_id TEXT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform VARCHAR(32) NOT NULL DEFAULT 'openai',
    previous_generation BIGINT NOT NULL,
    target_generation BIGINT NOT NULL,
    quota_window_start TIMESTAMPTZ NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'weekly_pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    lease_owner VARCHAR(128) NOT NULL DEFAULT '',
    lease_until TIMESTAMPTZ,
    cache_prepared_at TIMESTAMPTZ,
    db_applied_at TIMESTAMPTZ,
    cache_applied_at TIMESTAMPTZ,
    last_error_reason VARCHAR(96) NOT NULL DEFAULT '',
    last_error_message TEXT NOT NULL DEFAULT '',
    last_error_request_id VARCHAR(128) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (execution_id, user_id, platform),
    UNIQUE (reset_event_id, user_id, platform),
    CHECK (target_generation > previous_generation)
);

CREATE INDEX IF NOT EXISTS idx_openai_weekly_reset_execution_users_pending
    ON openai_weekly_quota_reset_execution_users(status, lease_until, id)
    WHERE status <> 'succeeded';

CREATE TABLE IF NOT EXISTS openai_weekly_quota_reset_execution_user_links (
    execution_id BIGINT NOT NULL REFERENCES openai_weekly_quota_reset_executions(id) ON DELETE CASCADE,
    target_id BIGINT NOT NULL REFERENCES openai_weekly_quota_reset_execution_users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (execution_id, target_id)
);

CREATE INDEX IF NOT EXISTS idx_openai_weekly_reset_execution_user_links_target
    ON openai_weekly_quota_reset_execution_user_links(target_id);

-- 兼容 v1 已提交 DB、尚未完成 Redis 的执行。保留 DB 中清零后产生的新消费，
-- 只补 generation 与持久 target，v2 worker 将继续 cache finalize。
WITH legacy_candidates AS (
    SELECT e.id AS execution_id, e.reset_event_id, e.detected_at, e.official_window_start,
           ids.user_id, q.weekly_quota_generation,
           GREATEST(q.weekly_quota_generation, q.weekly_reserved_generation) AS base_generation,
           ROW_NUMBER() OVER (PARTITION BY ids.user_id ORDER BY e.id) AS generation_offset
    FROM openai_weekly_quota_reset_executions e
    CROSS JOIN LATERAL UNNEST(e.reset_user_ids) AS ids(user_id)
    JOIN user_platform_quotas q ON q.user_id=ids.user_id AND q.platform='openai' AND q.deleted_at IS NULL
    WHERE e.status IN ('pending','running','retryable_failed')
), inserted_targets AS (
    INSERT INTO openai_weekly_quota_reset_execution_users
        (execution_id,reset_event_id,user_id,platform,previous_generation,target_generation,
         quota_window_start,status,cache_prepared_at,db_applied_at,created_at,updated_at)
    SELECT execution_id,reset_event_id,user_id,'openai',
           base_generation+generation_offset-1,base_generation+generation_offset,
           COALESCE(official_window_start,detected_at),'weekly_db_applied',detected_at,detected_at,detected_at,detected_at
    FROM legacy_candidates
    ON CONFLICT (reset_event_id,user_id,platform) DO NOTHING
    RETURNING user_id,target_generation
), legacy_generation AS (
    SELECT user_id,MAX(target_generation) AS target_generation FROM inserted_targets GROUP BY user_id
)
UPDATE user_platform_quotas q SET
    weekly_quota_generation=GREATEST(q.weekly_quota_generation,g.target_generation),
    weekly_reserved_generation=GREATEST(q.weekly_reserved_generation,g.target_generation),
    updated_at=NOW()
FROM legacy_generation g
WHERE q.user_id=g.user_id AND q.platform='openai' AND q.deleted_at IS NULL;

INSERT INTO openai_weekly_quota_reset_execution_user_links (execution_id,target_id)
SELECT execution_id,id FROM openai_weekly_quota_reset_execution_users
ON CONFLICT DO NOTHING;

UPDATE openai_weekly_quota_reset_executions
SET error_message='', error_reason=CASE WHEN status IN ('pending','running','retryable_failed')
    THEN 'LEGACY_CACHE_COMPENSATION_PENDING' ELSE '' END,
    stage=CASE WHEN status IN ('pending','running','retryable_failed') THEN 'redis_sync' ELSE stage END;

UPDATE openai_weekly_quota_reset_rules
SET last_error='', query_error_message='', execution_error_message='',
    execution_error_reason=CASE WHEN execution_status IN ('running','retryable_failed')
        THEN 'LEGACY_CACHE_COMPENSATION_PENDING' ELSE execution_error_reason END;
