-- 224: Durable timeout protection for scheduled account tests.

ALTER TABLE scheduled_test_plans
    ADD COLUMN IF NOT EXISTS timeout_protection_mode VARCHAR(16) NOT NULL DEFAULT 'off',
    ADD COLUMN IF NOT EXISTS timeout_seconds INT NOT NULL DEFAULT 60,
    ADD COLUMN IF NOT EXISTS consecutive_timeout_threshold INT NOT NULL DEFAULT 3,
    ADD COLUMN IF NOT EXISTS retry_delays_seconds INT[] NOT NULL DEFAULT ARRAY[10, 20]::INT[],
    ADD COLUMN IF NOT EXISTS consecutive_timeout_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS plan_version BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS execution_id UUID,
    ADD COLUMN IF NOT EXISTS claim_token UUID,
    ADD COLUMN IF NOT EXISTS claim_plan_version BIGINT,
    ADD COLUMN IF NOT EXISTS claim_expires_at TIMESTAMPTZ;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'scheduled_test_plans_timeout_mode_check'
          AND conrelid = 'scheduled_test_plans'::regclass
    ) THEN
        ALTER TABLE scheduled_test_plans
            ADD CONSTRAINT scheduled_test_plans_timeout_mode_check
            CHECK (timeout_protection_mode IN ('off', 'shadow', 'enforce'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'scheduled_test_plans_timeout_seconds_check'
          AND conrelid = 'scheduled_test_plans'::regclass
    ) THEN
        ALTER TABLE scheduled_test_plans
            ADD CONSTRAINT scheduled_test_plans_timeout_seconds_check
            CHECK (timeout_seconds BETWEEN 0 AND 600);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'scheduled_test_plans_timeout_threshold_check'
          AND conrelid = 'scheduled_test_plans'::regclass
    ) THEN
        ALTER TABLE scheduled_test_plans
            ADD CONSTRAINT scheduled_test_plans_timeout_threshold_check
            CHECK (consecutive_timeout_threshold BETWEEN 0 AND 100);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'scheduled_test_plans_retry_delays_check'
          AND conrelid = 'scheduled_test_plans'::regclass
    ) THEN
        ALTER TABLE scheduled_test_plans
            ADD CONSTRAINT scheduled_test_plans_retry_delays_check
            CHECK (
                cardinality(retry_delays_seconds) <= 5
                AND array_position(retry_delays_seconds, NULL) IS NULL
                AND 0 <= ALL(retry_delays_seconds)
                AND 300 >= ALL(retry_delays_seconds)
            );
    END IF;
END $$;

ALTER TABLE scheduled_test_results
    ADD COLUMN IF NOT EXISTS execution_id UUID,
    ADD COLUMN IF NOT EXISTS run_mode VARCHAR(16) NOT NULL DEFAULT 'normal',
    ADD COLUMN IF NOT EXISTS attempt_count INT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS classification VARCHAR(16),
    ADD COLUMN IF NOT EXISTS protection_action VARCHAR(24) NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS blocked_reason VARCHAR(64) NOT NULL DEFAULT '';

UPDATE scheduled_test_results
SET classification = CASE WHEN status = 'success' THEN 'success' ELSE 'failure' END
WHERE classification IS NULL;

ALTER TABLE scheduled_test_results
    ALTER COLUMN classification SET DEFAULT 'failure',
    ALTER COLUMN classification SET NOT NULL;

CREATE TABLE IF NOT EXISTS scheduled_test_account_ownership (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    plan_id BIGINT NOT NULL UNIQUE REFERENCES scheduled_test_plans(id) ON DELETE CASCADE,
    previous_schedulable BOOLEAN NOT NULL,
    previous_error_message TEXT,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS scheduled_test_protection_events (
    id BIGSERIAL PRIMARY KEY,
    plan_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    execution_id UUID NOT NULL,
    platform VARCHAR(32) NOT NULL,
    classification VARCHAR(16) NOT NULL,
    protection_action VARCHAR(24) NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT scheduled_test_protection_events_execution_unique
        UNIQUE (plan_id, execution_id),
    CONSTRAINT scheduled_test_protection_events_classification_check
        CHECK (classification IN ('success', 'timeout', 'failure')),
    CONSTRAINT scheduled_test_protection_events_action_check
        CHECK (protection_action IN ('none', 'would_inactivate', 'inactivated', 'recovered', 'blocked'))
);
CREATE INDEX IF NOT EXISTS idx_scheduled_test_protection_events_platform_time
    ON scheduled_test_protection_events(platform, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_scheduled_test_protection_events_inactivated
    ON scheduled_test_protection_events(platform, occurred_at DESC)
    WHERE protection_action = 'inactivated';

CREATE OR REPLACE FUNCTION release_scheduled_test_ownership_on_manual_status()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status = 'inactive'
       AND (
           NEW.status IS DISTINCT FROM OLD.status
           OR NEW.schedulable IS DISTINCT FROM OLD.schedulable
           OR NEW.error_message IS DISTINCT FROM OLD.error_message
       )
       AND current_setting('sub2api.scheduled_test_owner_mutation', true) IS DISTINCT FROM '1'
       AND pg_trigger_depth() = 1 THEN
        DELETE FROM scheduled_test_account_ownership WHERE account_id = NEW.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_release_scheduled_test_ownership_on_manual_status ON accounts;
CREATE TRIGGER trg_release_scheduled_test_ownership_on_manual_status
AFTER UPDATE OF status, schedulable, error_message ON accounts
FOR EACH ROW
EXECUTE FUNCTION release_scheduled_test_ownership_on_manual_status();
