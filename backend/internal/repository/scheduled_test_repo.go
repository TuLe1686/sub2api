package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

const scheduledTestPlanColumns = `
	p.id, p.account_id, p.model_id, p.cron_expression, p.enabled, p.max_results,
	p.auto_recover, p.timeout_protection_mode, p.timeout_seconds,
	p.consecutive_timeout_threshold, p.retry_delays_seconds,
	p.consecutive_timeout_count, p.plan_version, p.execution_id::text, p.claim_token::text,
	p.claim_plan_version, p.claim_expires_at,
	EXISTS (SELECT 1 FROM scheduled_test_account_ownership o WHERE o.plan_id = p.id),
	p.last_run_at, p.next_run_at, p.created_at, p.updated_at`

const scheduledTestResultColumns = `
	id, plan_id, execution_id::text, status, run_mode, attempt_count,
	classification, protection_action, blocked_reason, response_text,
	error_message, latency_ms, started_at, finished_at, created_at`

type scheduledTestPlanRepository struct {
	db *sql.DB
}

func NewScheduledTestPlanRepository(db *sql.DB) service.ScheduledTestPlanRepository {
	return &scheduledTestPlanRepository{db: db}
}

func (r *scheduledTestPlanRepository) Create(ctx context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO scheduled_test_plans (
			account_id, model_id, cron_expression, enabled, max_results, auto_recover,
			timeout_protection_mode, timeout_seconds, consecutive_timeout_threshold,
			retry_delays_seconds, next_run_at, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW(), NOW())
		RETURNING id
	`, plan.AccountID, plan.ModelID, plan.CronExpression, plan.Enabled, plan.MaxResults,
		plan.AutoRecover, plan.TimeoutProtectionMode, plan.TimeoutSeconds,
		plan.ConsecutiveTimeoutThreshold, pq.Array(plan.RetryDelaysSeconds), plan.NextRunAt)
	var id int64
	if err := row.Scan(&id); err != nil {
		return nil, err
	}
	return r.GetByID(ctx, id)
}

func (r *scheduledTestPlanRepository) GetByID(ctx context.Context, id int64) (*service.ScheduledTestPlan, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+scheduledTestPlanColumns+` FROM scheduled_test_plans p WHERE p.id = $1`, id)
	return scanPlan(row)
}

func (r *scheduledTestPlanRepository) ListByAccountID(ctx context.Context, accountID int64) ([]*service.ScheduledTestPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+scheduledTestPlanColumns+`
		FROM scheduled_test_plans p
		WHERE p.account_id = $1
		ORDER BY p.created_at DESC
	`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPlans(rows)
}

func (r *scheduledTestPlanRepository) ListDue(ctx context.Context, now time.Time) ([]*service.ScheduledTestPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+scheduledTestPlanColumns+`
		FROM scheduled_test_plans p
		WHERE p.enabled = true AND p.next_run_at <= NOW()
			AND (p.claim_expires_at IS NULL OR p.claim_expires_at <= NOW())
		ORDER BY p.next_run_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPlans(rows)
}

func (r *scheduledTestPlanRepository) ClaimDue(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]*service.ScheduledTestPlan, error) {
	if limit <= 0 {
		limit = 10
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT p.id, p.execution_id::text, p.plan_version
		FROM scheduled_test_plans p
		WHERE p.enabled = true AND p.next_run_at <= NOW()
			AND (p.claim_expires_at IS NULL OR p.claim_expires_at <= NOW())
		ORDER BY
			CASE
				WHEN EXISTS (SELECT 1 FROM scheduled_test_account_ownership o WHERE o.plan_id = p.id) THEN 2
				WHEN p.consecutive_timeout_count > 0 THEN 1
				ELSE 0
			END DESC,
			NOW() - p.next_run_at DESC,
			p.next_run_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	type duePlan struct {
		id          int64
		executionID sql.NullString
		planVersion int64
	}
	var due []duePlan
	for rows.Next() {
		var item duePlan
		if err := rows.Scan(&item.id, &item.executionID, &item.planVersion); err != nil {
			_ = rows.Close()
			return nil, err
		}
		due = append(due, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	claimed := make([]*service.ScheduledTestPlan, 0, len(due))
	for _, item := range due {
		executionID := item.executionID.String
		if executionID == "" {
			executionID = uuid.NewString()
		}
		claimToken := uuid.NewString()
		row := tx.QueryRowContext(ctx, `
			UPDATE scheduled_test_plans AS p
			SET execution_id = $2::uuid, claim_token = $3::uuid,
				claim_plan_version = $4, claim_expires_at = NOW() + ($5 * INTERVAL '1 second'),
				updated_at = NOW()
			WHERE p.id = $1 AND p.plan_version = $4
			RETURNING `+scheduledTestPlanColumns,
			item.id, executionID, claimToken, item.planVersion, lease.Seconds())
		plan, err := scanPlan(row)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, plan)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

func (r *scheduledTestPlanRepository) HealExpiredClaims(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE scheduled_test_plans
		SET claim_token = NULL, claim_plan_version = NULL, claim_expires_at = NULL,
			next_run_at = NOW(), updated_at = NOW()
		WHERE id IN (
			SELECT id FROM scheduled_test_plans
			WHERE claim_expires_at IS NOT NULL AND claim_expires_at <= $1
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
	`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	healed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(healed), nil
}

func (r *scheduledTestPlanRepository) RenewClaim(ctx context.Context, planID int64, claimToken string, planVersion int64, lease time.Duration) (time.Time, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var currentExpiresAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT claim_expires_at
		FROM scheduled_test_plans
		WHERE id = $1 AND claim_token = $2::uuid
			AND claim_plan_version = $3 AND plan_version = $3
		FOR UPDATE
	`, planID, claimToken, planVersion).Scan(&currentExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}

	var lockedAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&lockedAt); err != nil {
		return time.Time{}, false, err
	}
	if !currentExpiresAt.After(lockedAt) {
		return time.Time{}, false, nil
	}

	var expiresAt time.Time
	err = tx.QueryRowContext(ctx, `
		UPDATE scheduled_test_plans
		SET claim_expires_at = $4 + ($5 * INTERVAL '1 second'), updated_at = $4
		WHERE id = $1 AND claim_token = $2::uuid
			AND claim_plan_version = $3 AND plan_version = $3
		RETURNING claim_expires_at
	`, planID, claimToken, planVersion, lockedAt, lease.Seconds()).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, false, err
	}
	return expiresAt, true, nil
}

func (r *scheduledTestPlanRepository) HasAccountOwnership(ctx context.Context, accountID int64) (bool, error) {
	var owned bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM scheduled_test_account_ownership WHERE account_id = $1
		)
	`, accountID).Scan(&owned)
	return owned, err
}

func (r *scheduledTestPlanRepository) FinalizeClaim(ctx context.Context, input service.ScheduledTestFinalizeInput) (*service.ScheduledTestResult, error) {
	if input.Plan == nil || input.Result == nil {
		return nil, errors.New("scheduled test finalize input is incomplete")
	}
	if input.Now.IsZero() {
		input.Now = time.Now()
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var accountID int64
	var storedMode, executionID string
	var threshold, currentCount, maxResults int
	var claimExpiresAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT account_id, timeout_protection_mode, consecutive_timeout_threshold,
			consecutive_timeout_count, max_results, execution_id::text, claim_expires_at
		FROM scheduled_test_plans
		WHERE id = $1 AND claim_token = $2::uuid
			AND claim_plan_version = $3 AND plan_version = $3
		FOR UPDATE
	`, input.Plan.ID, input.ClaimToken, input.Plan.PlanVersion).Scan(
		&accountID, &storedMode, &threshold, &currentCount, &maxResults, &executionID, &claimExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrScheduledTestClaimLost
	}
	if err != nil {
		return nil, err
	}
	var lockedAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&lockedAt); err != nil {
		return nil, err
	}
	if !claimExpiresAt.After(lockedAt) {
		return nil, service.ErrScheduledTestClaimLost
	}
	input.Now = lockedAt

	effectiveMode := input.Plan.EffectiveTimeoutProtectionMode
	if effectiveMode == "" {
		effectiveMode = storedMode
	}

	input.Result.PlanID = input.Plan.ID
	input.Result.ExecutionID = executionID
	if input.Result.RunMode == "" {
		input.Result.RunMode = service.ScheduledTestRunModeNormal
	}
	if input.Result.ProtectionAction == "" {
		input.Result.ProtectionAction = service.ScheduledTestProtectionActionNone
	}

	if err := lockScheduledTestRow(ctx, tx, "accounts", accountID); err != nil {
		return nil, err
	}
	hasOwnership, err := lockScheduledTestOwnership(ctx, tx, input.Plan.ID)
	if err != nil {
		return nil, err
	}
	if hasOwnership {
		input.Result.RunMode = service.ScheduledTestRunModeRecovery
	}

	switch input.Result.Classification {
	case service.ScheduledTestClassificationSuccess:
		currentCount = 0
		if hasOwnership {
			recovered, err := restoreScheduledTestOwnedAccount(ctx, tx, input.Plan.ID, accountID)
			if err != nil {
				return nil, err
			}
			if recovered {
				input.Result.ProtectionAction = service.ScheduledTestProtectionActionRecovered
			} else {
				input.Result.ProtectionAction = service.ScheduledTestProtectionActionBlocked
				input.Result.BlockedReason = "manual_override"
			}
		}
	case service.ScheduledTestClassificationTimeout:
		if effectiveMode != service.ScheduledTestTimeoutProtectionOff {
			currentCount++
		} else {
			currentCount = 0
		}
		if !hasOwnership && threshold > 0 && currentCount >= threshold && effectiveMode != service.ScheduledTestTimeoutProtectionOff {
			if effectiveMode == service.ScheduledTestTimeoutProtectionShadow {
				input.Result.ProtectionAction = service.ScheduledTestProtectionActionWouldInactivate
			} else {
				allowed, reason, err := scheduledTestDisableAllowed(ctx, tx, accountID, input)
				if err != nil {
					return nil, err
				}
				if !allowed {
					input.Result.ProtectionAction = service.ScheduledTestProtectionActionBlocked
					input.Result.BlockedReason = reason
				} else {
					inactivated, err := inactivateScheduledTestAccount(ctx, tx, input.Plan.ID, accountID)
					if err != nil {
						return nil, err
					}
					if inactivated {
						input.Result.ProtectionAction = service.ScheduledTestProtectionActionInactivated
					} else {
						input.Result.ProtectionAction = service.ScheduledTestProtectionActionBlocked
						input.Result.BlockedReason = "account_already_owned"
					}
				}
			}
		}
	default:
		currentCount = 0
	}

	created, err := createScheduledTestResult(ctx, tx, input.Result)
	if err != nil {
		return nil, err
	}
	if err := createScheduledTestProtectionEvent(ctx, tx, accountID, input.Result, input.Now); err != nil {
		return nil, err
	}
	if maxResults <= 0 {
		maxResults = 50
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM scheduled_test_results
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY plan_id ORDER BY created_at DESC, id DESC) AS rn
				FROM scheduled_test_results WHERE plan_id = $1
			) ranked WHERE rn > $2
		)
	`, input.Plan.ID, maxResults); err != nil {
		return nil, err
	}
	updateResult, err := tx.ExecContext(ctx, `
		UPDATE scheduled_test_plans
		SET consecutive_timeout_count = $3, last_run_at = $4, next_run_at = $5,
			execution_id = NULL, claim_token = NULL, claim_plan_version = NULL,
			claim_expires_at = NULL, updated_at = NOW()
		WHERE id = $1 AND claim_token = $2::uuid
			AND claim_plan_version = $6 AND plan_version = $6
	`, input.Plan.ID, input.ClaimToken, currentCount, input.Now, input.NextRunAt, input.Plan.PlanVersion)
	if err != nil {
		return nil, err
	}
	updatedRows, err := updateResult.RowsAffected()
	if err != nil {
		return nil, err
	}
	if updatedRows != 1 {
		return nil, service.ErrScheduledTestClaimLost
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return created, nil
}

func scheduledTestDisableAllowed(ctx context.Context, tx *sql.Tx, accountID int64, input service.ScheduledTestFinalizeInput) (bool, string, error) {
	var platform string
	if err := tx.QueryRowContext(ctx, `SELECT platform FROM accounts WHERE id = $1 FOR UPDATE`, accountID).Scan(&platform); err != nil {
		return false, "", err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "scheduled-timeout:"+platform); err != nil {
		return false, "", err
	}

	window := input.CircuitWindow
	if window <= 0 {
		window = 5 * time.Minute
	}
	windowStart := input.Now.Add(-window)
	if input.CircuitMinSamples > 0 && input.CircuitTimeoutRatio > 0 {
		var samples, timeouts int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) + 1,
				COUNT(*) FILTER (WHERE classification = 'timeout') + 1
			FROM scheduled_test_protection_events
			WHERE platform = $1 AND occurred_at >= $2
		`, platform, windowStart).Scan(&samples, &timeouts); err != nil {
			return false, "", err
		}
		if samples >= input.CircuitMinSamples && float64(timeouts)/float64(samples) >= input.CircuitTimeoutRatio {
			return false, "platform_circuit_open", nil
		}
	}
	if input.DisableBudget > 0 {
		var disabled int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM scheduled_test_protection_events
			WHERE platform = $1 AND occurred_at >= $2
				AND protection_action = 'inactivated'
		`, platform, windowStart).Scan(&disabled); err != nil {
			return false, "", err
		}
		if disabled >= input.DisableBudget {
			return false, "disable_budget_exhausted", nil
		}
	}
	return true, "", nil
}

func createScheduledTestProtectionEvent(
	ctx context.Context,
	tx *sql.Tx,
	accountID int64,
	result *service.ScheduledTestResult,
	occurredAt time.Time,
) error {
	if result == nil || result.ExecutionID == "" {
		return errors.New("scheduled test protection event is incomplete")
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO scheduled_test_protection_events (
			plan_id, account_id, execution_id, platform,
			classification, protection_action, occurred_at
		)
		SELECT $1, $2, $3::uuid, platform, $4, $5, $6
		FROM accounts
		WHERE id = $2
		ON CONFLICT (plan_id, execution_id) DO NOTHING
	`, result.PlanID, accountID, result.ExecutionID, result.Classification, result.ProtectionAction, occurredAt)
	return err
}

func inactivateScheduledTestAccount(ctx context.Context, tx *sql.Tx, planID, accountID int64) (bool, error) {
	if _, err := tx.ExecContext(ctx, `SELECT set_config('sub2api.scheduled_test_owner_mutation', '1', true)`); err != nil {
		return false, err
	}
	var schedulable bool
	var errorMessage sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT schedulable, error_message FROM accounts
		WHERE id = $1 AND status = $2
		FOR UPDATE
	`, accountID, service.StatusActive).Scan(&schedulable, &errorMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ownershipResult, err := tx.ExecContext(ctx, `
		INSERT INTO scheduled_test_account_ownership (
			account_id, plan_id, previous_schedulable, previous_error_message, acquired_at, updated_at
		) VALUES ($1, $2, $3, $4, NOW(), NOW())
		ON CONFLICT (account_id) DO NOTHING
	`, accountID, planID, schedulable, errorMessage)
	if err != nil {
		return false, err
	}
	ownershipRows, err := ownershipResult.RowsAffected()
	if err != nil {
		return false, err
	}
	if ownershipRows == 0 {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE accounts SET status = $2, schedulable = false,
			error_message = $3, updated_at = NOW()
		WHERE id = $1 AND status = $4
	`, accountID, service.ScheduledTestAccountStatusInactive, "scheduled test consecutive timeouts", service.StatusActive)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		return false, service.ErrScheduledTestAccountMutationConflict
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		VALUES ($1, $2, NULL, NULL)
	`, service.SchedulerOutboxEventAccountChanged, accountID)
	return err == nil, err
}

func restoreScheduledTestOwnedAccount(ctx context.Context, tx *sql.Tx, planID, accountID int64) (bool, error) {
	if _, err := tx.ExecContext(ctx, `SELECT set_config('sub2api.scheduled_test_owner_mutation', '1', true)`); err != nil {
		return false, err
	}
	var schedulable bool
	var errorMessage sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT previous_schedulable, previous_error_message
		FROM scheduled_test_account_ownership
		WHERE plan_id = $1 AND account_id = $2
		FOR UPDATE
	`, planID, accountID).Scan(&schedulable, &errorMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE accounts SET status = $3, schedulable = $4,
			error_message = $5, updated_at = NOW()
		WHERE id = $2 AND status = $6
			AND schedulable = $7 AND error_message = $8
	`, planID, accountID, service.StatusActive, schedulable, errorMessage,
		service.ScheduledTestAccountStatusInactive, false, "scheduled test consecutive timeouts")
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scheduled_test_account_ownership WHERE plan_id = $1 AND account_id = $2`, planID, accountID); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		VALUES ($1, $2, NULL, NULL)
	`, service.SchedulerOutboxEventAccountChanged, accountID)
	return err == nil, err
}

func lockScheduledTestRow(ctx context.Context, tx *sql.Tx, table string, id int64) error {
	query := ""
	switch table {
	case "accounts":
		query = `SELECT id FROM accounts WHERE id = $1 FOR UPDATE`
	case "scheduled_test_plans":
		query = `SELECT id FROM scheduled_test_plans WHERE id = $1 FOR UPDATE`
	default:
		return errors.New("unsupported scheduled test lock target")
	}
	var lockedID int64
	return tx.QueryRowContext(ctx, query, id).Scan(&lockedID)
}

func lockScheduledTestOwnership(ctx context.Context, tx *sql.Tx, planID int64) (bool, error) {
	var lockedPlanID int64
	err := tx.QueryRowContext(ctx, `
		SELECT plan_id FROM scheduled_test_account_ownership
		WHERE plan_id = $1
		FOR UPDATE
	`, planID).Scan(&lockedPlanID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (r *scheduledTestPlanRepository) Update(ctx context.Context, plan *service.ScheduledTestPlan, restoreOwnedAccount ...bool) (*service.ScheduledTestPlan, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockScheduledTestRow(ctx, tx, "scheduled_test_plans", plan.ID); err != nil {
		return nil, err
	}
	if !plan.Enabled {
		if err := handleScheduledTestOwnershipBeforeMutation(ctx, tx, plan.ID, len(restoreOwnedAccount) > 0 && restoreOwnedAccount[0]); err != nil {
			return nil, err
		}
	}
	row := tx.QueryRowContext(ctx, `
		UPDATE scheduled_test_plans
		SET model_id = $2, cron_expression = $3, enabled = $4, max_results = $5,
			auto_recover = $6, timeout_protection_mode = $7, timeout_seconds = $8,
			consecutive_timeout_threshold = $9, retry_delays_seconds = $10,
			next_run_at = $11, plan_version = plan_version + 1,
			execution_id = NULL, claim_token = NULL, claim_plan_version = NULL,
			claim_expires_at = NULL, updated_at = NOW()
		WHERE id = $1
		RETURNING id
	`, plan.ID, plan.ModelID, plan.CronExpression, plan.Enabled, plan.MaxResults,
		plan.AutoRecover, plan.TimeoutProtectionMode, plan.TimeoutSeconds,
		plan.ConsecutiveTimeoutThreshold, pq.Array(plan.RetryDelaysSeconds), plan.NextRunAt)
	var id int64
	if err := row.Scan(&id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r.GetByID(ctx, id)
}

func (r *scheduledTestPlanRepository) Delete(ctx context.Context, id int64, restoreOwnedAccount ...bool) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockScheduledTestRow(ctx, tx, "scheduled_test_plans", id); err != nil {
		return err
	}
	if err := handleScheduledTestOwnershipBeforeMutation(ctx, tx, id, len(restoreOwnedAccount) > 0 && restoreOwnedAccount[0]); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func handleScheduledTestOwnershipBeforeMutation(ctx context.Context, tx *sql.Tx, planID int64, restore bool) error {
	var accountID int64
	err := tx.QueryRowContext(ctx, `SELECT account_id FROM scheduled_test_plans WHERE id = $1`, planID).Scan(&accountID)
	if err != nil {
		return err
	}
	if err := lockScheduledTestRow(ctx, tx, "accounts", accountID); err != nil {
		return err
	}
	hasOwnership, err := lockScheduledTestOwnership(ctx, tx, planID)
	if err != nil || !hasOwnership {
		return err
	}
	if !restore {
		return service.ErrScheduledTestOwnershipConflict
	}
	restored, err := restoreScheduledTestOwnedAccount(ctx, tx, planID, accountID)
	if err != nil {
		return err
	}
	if !restored {
		return service.ErrScheduledTestOwnershipReleaseFailed
	}
	return nil
}

func (r *scheduledTestPlanRepository) UpdateAfterRun(ctx context.Context, id int64, lastRunAt time.Time, nextRunAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE scheduled_test_plans SET last_run_at = $2, next_run_at = $3, updated_at = NOW() WHERE id = $1
	`, id, lastRunAt, nextRunAt)
	return err
}

type scheduledTestResultRepository struct {
	db *sql.DB
}

func NewScheduledTestResultRepository(db *sql.DB) service.ScheduledTestResultRepository {
	return &scheduledTestResultRepository{db: db}
}

func (r *scheduledTestResultRepository) Create(ctx context.Context, result *service.ScheduledTestResult) (*service.ScheduledTestResult, error) {
	return createScheduledTestResult(ctx, r.db, result)
}

func createScheduledTestResult(ctx context.Context, exec interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, result *service.ScheduledTestResult) (*service.ScheduledTestResult, error) {
	row := exec.QueryRowContext(ctx, `
		INSERT INTO scheduled_test_results (
			plan_id, execution_id, status, run_mode, attempt_count, classification,
			protection_action, blocked_reason, response_text, error_message,
			latency_ms, started_at, finished_at, created_at
		)
		VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, NOW())
		ON CONFLICT (plan_id, execution_id) DO UPDATE SET
			status = EXCLUDED.status,
			run_mode = EXCLUDED.run_mode,
			attempt_count = EXCLUDED.attempt_count,
			classification = EXCLUDED.classification,
			protection_action = EXCLUDED.protection_action,
			blocked_reason = EXCLUDED.blocked_reason,
			response_text = EXCLUDED.response_text,
			error_message = EXCLUDED.error_message,
			latency_ms = EXCLUDED.latency_ms,
			started_at = EXCLUDED.started_at,
			finished_at = EXCLUDED.finished_at
		RETURNING `+scheduledTestResultColumns,
		result.PlanID, result.ExecutionID, result.Status, result.RunMode, result.AttemptCount,
		result.Classification, result.ProtectionAction, result.BlockedReason,
		result.ResponseText, result.ErrorMessage, result.LatencyMs,
		result.StartedAt, result.FinishedAt)
	return scanResult(row)
}

func (r *scheduledTestResultRepository) ListByPlanID(ctx context.Context, planID int64, limit int) ([]*service.ScheduledTestResult, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+scheduledTestResultColumns+`
		FROM scheduled_test_results
		WHERE plan_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2
	`, planID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []*service.ScheduledTestResult
	for rows.Next() {
		result, err := scanResult(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func (r *scheduledTestResultRepository) PruneOldResults(ctx context.Context, planID int64, keepCount int) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM scheduled_test_results
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY plan_id ORDER BY created_at DESC, id DESC) AS rn
				FROM scheduled_test_results WHERE plan_id = $1
			) ranked WHERE rn > $2
		)
	`, planID, keepCount)
	return err
}

type scannable interface {
	Scan(dest ...any) error
}

func scanPlan(row scannable) (*service.ScheduledTestPlan, error) {
	plan := &service.ScheduledTestPlan{}
	var executionID, claimToken sql.NullString
	var claimPlanVersion sql.NullInt64
	var retryDelays pq.Int64Array
	if err := row.Scan(
		&plan.ID, &plan.AccountID, &plan.ModelID, &plan.CronExpression, &plan.Enabled,
		&plan.MaxResults, &plan.AutoRecover, &plan.TimeoutProtectionMode,
		&plan.TimeoutSeconds, &plan.ConsecutiveTimeoutThreshold, &retryDelays,
		&plan.ConsecutiveTimeoutCount, &plan.PlanVersion, &executionID, &claimToken,
		&claimPlanVersion, &plan.ClaimExpiresAt,
		&plan.OwnsInactiveAccount, &plan.LastRunAt, &plan.NextRunAt,
		&plan.CreatedAt, &plan.UpdatedAt,
	); err != nil {
		return nil, err
	}
	plan.ExecutionID = executionID.String
	plan.ClaimToken = claimToken.String
	plan.ClaimPlanVersion = claimPlanVersion.Int64
	plan.TimeoutSecondsSet = true
	plan.ConsecutiveTimeoutThresholdSet = true
	plan.RetryDelaysSecondsSet = true
	plan.RetryDelaysSeconds = make([]int, len(retryDelays))
	for index, value := range retryDelays {
		plan.RetryDelaysSeconds[index] = int(value)
	}
	return plan, nil
}

func scanPlans(rows *sql.Rows) ([]*service.ScheduledTestPlan, error) {
	var plans []*service.ScheduledTestPlan
	for rows.Next() {
		plan, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

func scanResult(row scannable) (*service.ScheduledTestResult, error) {
	result := &service.ScheduledTestResult{}
	var executionID sql.NullString
	if err := row.Scan(
		&result.ID, &result.PlanID, &executionID, &result.Status, &result.RunMode,
		&result.AttemptCount, &result.Classification, &result.ProtectionAction,
		&result.BlockedReason, &result.ResponseText, &result.ErrorMessage,
		&result.LatencyMs, &result.StartedAt, &result.FinishedAt, &result.CreatedAt,
	); err != nil {
		return nil, err
	}
	result.ExecutionID = executionID.String
	return result, nil
}
