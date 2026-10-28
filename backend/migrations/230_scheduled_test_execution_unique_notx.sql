-- 225: Build scheduled-test execution idempotency without blocking result writes.

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_scheduled_test_results_plan_execution_unique
    ON scheduled_test_results(plan_id, execution_id);
