-- 226: 增强 guard 所有权释放触发器，管理员修改账号状态时递增 plan_version 使旧 claim fencing 失效。

DROP TRIGGER IF EXISTS trg_release_scheduled_test_ownership_on_manual_status ON accounts;

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
        -- 递增受影响计划的 plan_version，使正在运行的旧 claim 因版本不匹配而 fencing 失败
        UPDATE scheduled_test_plans
        SET plan_version = plan_version + 1,
            claim_token = NULL, claim_plan_version = NULL, claim_expires_at = NULL,
            execution_id = NULL,
            updated_at = NOW()
        WHERE id IN (
            SELECT plan_id FROM scheduled_test_account_ownership WHERE account_id = NEW.id
        );
        DELETE FROM scheduled_test_account_ownership WHERE account_id = NEW.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_release_scheduled_test_ownership_on_manual_status
AFTER UPDATE OF status, schedulable, error_message ON accounts
FOR EACH ROW
EXECUTE FUNCTION release_scheduled_test_ownership_on_manual_status();
