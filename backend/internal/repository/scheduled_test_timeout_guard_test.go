package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRenewScheduledTestClaimRejectsLeaseExpiredAfterPlanLock(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	expiresAt := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT claim_expires_at").WithArgs(
		int64(19),
		"00000000-0000-0000-0000-000000000019",
		int64(4),
	).WillReturnRows(sqlmock.NewRows([]string{"claim_expires_at"}).AddRow(expiresAt))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT clock_timestamp()")).
		WillReturnRows(sqlmock.NewRows([]string{"clock_timestamp"}).AddRow(expiresAt.Add(time.Second)))
	mock.ExpectRollback()

	repo := NewScheduledTestPlanRepository(db)
	_, renewed, err := repo.RenewClaim(
		context.Background(),
		19,
		"00000000-0000-0000-0000-000000000019",
		4,
		time.Minute,
	)

	require.NoError(t, err)
	require.False(t, renewed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRenewScheduledTestClaimUsesLockedDatabaseTime(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	lockedAt := time.Now()
	currentExpiresAt := lockedAt.Add(time.Minute)
	newExpiresAt := lockedAt.Add(2 * time.Minute)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT claim_expires_at").WithArgs(
		int64(19),
		"00000000-0000-0000-0000-000000000019",
		int64(4),
	).WillReturnRows(sqlmock.NewRows([]string{"claim_expires_at"}).AddRow(currentExpiresAt))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT clock_timestamp()")).
		WillReturnRows(sqlmock.NewRows([]string{"clock_timestamp"}).AddRow(lockedAt))
	mock.ExpectQuery("UPDATE scheduled_test_plans").WithArgs(
		int64(19),
		"00000000-0000-0000-0000-000000000019",
		int64(4),
		lockedAt,
		float64(120),
	).WillReturnRows(sqlmock.NewRows([]string{"claim_expires_at"}).AddRow(newExpiresAt))
	mock.ExpectCommit()

	repo := NewScheduledTestPlanRepository(db)
	expiresAt, renewed, err := repo.RenewClaim(
		context.Background(),
		19,
		"00000000-0000-0000-0000-000000000019",
		4,
		2*time.Minute,
	)

	require.NoError(t, err)
	require.True(t, renewed)
	require.Equal(t, newExpiresAt, expiresAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func scheduledTestResultRows(result *service.ScheduledTestResult) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "plan_id", "execution_id", "status", "run_mode", "attempt_count",
		"classification", "protection_action", "blocked_reason", "response_text",
		"error_message", "latency_ms", "started_at", "finished_at", "created_at",
	}).AddRow(
		int64(1), result.PlanID, result.ExecutionID, result.Status, result.RunMode, result.AttemptCount,
		result.Classification, result.ProtectionAction, result.BlockedReason, result.ResponseText,
		result.ErrorMessage, result.LatencyMs, result.StartedAt, result.FinishedAt, result.FinishedAt,
	)
}

func TestFinalizeScheduledTestClaimRejectsStaleTokenBeforeWrites(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT account_id, timeout_protection_mode").
		WithArgs(int64(19), "00000000-0000-0000-0000-000000000019", int64(4)).
		WillReturnRows(sqlmock.NewRows([]string{
			"account_id", "timeout_protection_mode", "consecutive_timeout_threshold",
			"consecutive_timeout_count", "max_results", "execution_id", "claim_expires_at",
		}))

	mock.ExpectRollback()

	repo := NewScheduledTestPlanRepository(db)
	_, err = repo.FinalizeClaim(context.Background(), service.ScheduledTestFinalizeInput{
		Plan:       &service.ScheduledTestPlan{ID: 19, PlanVersion: 4},
		ClaimToken: "00000000-0000-0000-0000-000000000019",
		Result: &service.ScheduledTestResult{
			Status:         service.ScheduledTestStatusFailed,
			Classification: service.ScheduledTestClassificationTimeout,
			StartedAt:      time.Now(),
			FinishedAt:     time.Now(),
		},
		Now:       time.Now(),
		NextRunAt: time.Now().Add(time.Minute),
	})

	require.Error(t, err)
	require.True(t, errors.Is(err, service.ErrScheduledTestClaimLost))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFinalizeScheduledTestClaimRejectsLeaseExpiredAfterPlanLock(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	expiresAt := time.Now().Add(-time.Second)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT account_id, timeout_protection_mode").
		WithArgs(int64(19), "00000000-0000-0000-0000-000000000019", int64(4)).
		WillReturnRows(sqlmock.NewRows([]string{
			"account_id", "timeout_protection_mode", "consecutive_timeout_threshold",
			"consecutive_timeout_count", "max_results", "execution_id", "claim_expires_at",
		}).AddRow(int64(7), service.ScheduledTestTimeoutProtectionEnforce, 3, 2, 50, "00000000-0000-0000-0000-000000000099", expiresAt))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT clock_timestamp()")).
		WillReturnRows(sqlmock.NewRows([]string{"clock_timestamp"}).AddRow(expiresAt.Add(time.Second)))
	mock.ExpectRollback()

	repo := NewScheduledTestPlanRepository(db)
	_, err = repo.FinalizeClaim(context.Background(), service.ScheduledTestFinalizeInput{
		Plan:       &service.ScheduledTestPlan{ID: 19, PlanVersion: 4},
		ClaimToken: "00000000-0000-0000-0000-000000000019",
		Result: &service.ScheduledTestResult{
			Status:         service.ScheduledTestStatusFailed,
			Classification: service.ScheduledTestClassificationTimeout,
			StartedAt:      expiresAt.Add(-time.Second),
			FinishedAt:     expiresAt,
		},
		NextRunAt: expiresAt.Add(time.Minute),
	})

	require.ErrorIs(t, err, service.ErrScheduledTestClaimLost)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRestoreScheduledTestOwnedAccountPreservesNullErrorMessage(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec("SELECT set_config").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT previous_schedulable, previous_error_message").
		WithArgs(int64(19), int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"previous_schedulable", "previous_error_message"}).AddRow(true, nil))
	mock.ExpectExec("UPDATE accounts SET status").
		WithArgs(
			int64(7), service.StatusActive, true, sql.NullString{},
			service.ScheduledTestAccountStatusInactive, false, "scheduled test consecutive timeouts",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM scheduled_test_account_ownership").
		WithArgs(int64(19), int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO scheduler_outbox").
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	restored, err := restoreScheduledTestOwnedAccount(context.Background(), tx, 19, 7)
	require.NoError(t, tx.Rollback())

	require.NoError(t, err)
	require.True(t, restored)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRestoreScheduledTestOwnedAccountRejectsManualGuardStateChange(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec("SELECT set_config").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT previous_schedulable, previous_error_message").
		WithArgs(int64(19), int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"previous_schedulable", "previous_error_message"}).AddRow(true, "original"))
	mock.ExpectExec("UPDATE accounts SET status").
		WithArgs(
			int64(7), service.StatusActive, true, "original",
			service.ScheduledTestAccountStatusInactive, false, "scheduled test consecutive timeouts",
		).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	restored, err := restoreScheduledTestOwnedAccount(context.Background(), tx, 19, 7)
	require.NoError(t, tx.Rollback())

	require.NoError(t, err)
	require.False(t, restored)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInactivateScheduledTestAccountPreservesNullErrorMessage(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec("SELECT set_config").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT schedulable, error_message FROM accounts").
		WithArgs(int64(7), service.StatusActive).
		WillReturnRows(sqlmock.NewRows([]string{"schedulable", "error_message"}).AddRow(true, nil))
	mock.ExpectExec("INSERT INTO scheduled_test_account_ownership").
		WithArgs(int64(7), int64(19), true, sql.NullString{}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE accounts SET status").
		WithArgs(int64(7), service.ScheduledTestAccountStatusInactive, "scheduled test consecutive timeouts", service.StatusActive).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO scheduler_outbox").
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	inactivated, err := inactivateScheduledTestAccount(context.Background(), tx, 19, 7)
	require.NoError(t, tx.Rollback())

	require.NoError(t, err)
	require.True(t, inactivated)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInactivateScheduledTestAccountRollsBackOwnershipWhenAccountCASFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec("SELECT set_config").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT schedulable, error_message FROM accounts").
		WithArgs(int64(7), service.StatusActive).
		WillReturnRows(sqlmock.NewRows([]string{"schedulable", "error_message"}).AddRow(true, "original"))
	mock.ExpectExec("INSERT INTO scheduled_test_account_ownership").
		WithArgs(int64(7), int64(19), true, "original").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE accounts SET status").
		WithArgs(int64(7), service.ScheduledTestAccountStatusInactive, "scheduled test consecutive timeouts", service.StatusActive).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	inactivated, err := inactivateScheduledTestAccount(context.Background(), tx, 19, 7)
	require.NoError(t, tx.Rollback())

	require.ErrorIs(t, err, service.ErrScheduledTestAccountMutationConflict)
	require.False(t, inactivated)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestScheduledTestDisableAllowedIncludesCurrentTimeoutInCircuitSample(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT platform FROM accounts").
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"platform"}).AddRow(service.PlatformOpenAI))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WithArgs("scheduled-timeout:" + service.PlatformOpenAI).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) + 1,")).
		WithArgs(service.PlatformOpenAI, now.Add(-5*time.Minute)).
		WillReturnRows(sqlmock.NewRows([]string{"samples", "timeouts"}).AddRow(2, 2))
	mock.ExpectRollback()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	allowed, reason, err := scheduledTestDisableAllowed(context.Background(), tx, 7, service.ScheduledTestFinalizeInput{
		Now:                 now,
		CircuitWindow:       5 * time.Minute,
		CircuitMinSamples:   2,
		CircuitTimeoutRatio: 1,
		DisableBudget:       3,
	})
	require.NoError(t, tx.Rollback())

	require.NoError(t, err)
	require.False(t, allowed)
	require.Equal(t, "platform_circuit_open", reason)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestScheduledTestDisableAllowedCountsImmutableInactivationEventsForBudget(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT platform FROM accounts").
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"platform"}).AddRow(service.PlatformOpenAI))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WithArgs("scheduled-timeout:" + service.PlatformOpenAI).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("AND protection_action = 'inactivated'").
		WithArgs(service.PlatformOpenAI, now.Add(-5*time.Minute)).
		WillReturnRows(sqlmock.NewRows([]string{"disabled"}).AddRow(3))
	mock.ExpectRollback()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	allowed, reason, err := scheduledTestDisableAllowed(context.Background(), tx, 7, service.ScheduledTestFinalizeInput{
		Now:           now,
		CircuitWindow: 5 * time.Minute,
		DisableBudget: 3,
	})
	require.NoError(t, tx.Rollback())

	require.NoError(t, err)
	require.False(t, allowed)
	require.Equal(t, "disable_budget_exhausted", reason)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDeleteScheduledTestPlanRollsBackWhenOwnedAccountCannotBeRestored(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM scheduled_test_plans WHERE id = $1 FOR UPDATE")).
		WithArgs(int64(19)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(19)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT account_id FROM scheduled_test_plans WHERE id = $1")).
		WithArgs(int64(19)).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(int64(7)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM accounts WHERE id = $1 FOR UPDATE")).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	mock.ExpectQuery("SELECT plan_id FROM scheduled_test_account_ownership").
		WithArgs(int64(19)).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id"}).AddRow(int64(19)))
	mock.ExpectExec("SELECT set_config").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT previous_schedulable, previous_error_message").
		WithArgs(int64(19), int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"previous_schedulable", "previous_error_message"}).AddRow(true, "original"))
	mock.ExpectExec("UPDATE accounts SET status").
		WithArgs(
			int64(7), service.StatusActive, true, "original",
			service.ScheduledTestAccountStatusInactive, false, "scheduled test consecutive timeouts",
		).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	repo := NewScheduledTestPlanRepository(db)
	err = repo.Delete(context.Background(), 19, true)

	require.ErrorIs(t, err, service.ErrScheduledTestOwnershipReleaseFailed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFinalizeScheduledTestClaimRollsBackWhenFinalFenceUpdateLosesClaim(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now()
	lockedAt := now.Add(500 * time.Millisecond)
	claimExpiresAt := now.Add(time.Minute)
	result := &service.ScheduledTestResult{
		Status:           service.ScheduledTestStatusFailed,
		RunMode:          service.ScheduledTestRunModeNormal,
		AttemptCount:     1,
		Classification:   service.ScheduledTestClassificationFailure,
		ProtectionAction: service.ScheduledTestProtectionActionNone,
		ErrorMessage:     "invalid credentials",
		StartedAt:        now.Add(-time.Second),
		FinishedAt:       now,
	}

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT account_id, timeout_protection_mode").
		WithArgs(int64(19), "00000000-0000-0000-0000-000000000019", int64(4)).
		WillReturnRows(sqlmock.NewRows([]string{
			"account_id", "timeout_protection_mode", "consecutive_timeout_threshold",
			"consecutive_timeout_count", "max_results", "execution_id", "claim_expires_at",
		}).AddRow(int64(7), service.ScheduledTestTimeoutProtectionEnforce, 3, 2, 50, "00000000-0000-0000-0000-000000000099", claimExpiresAt))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT clock_timestamp()")).
		WillReturnRows(sqlmock.NewRows([]string{"clock_timestamp"}).AddRow(lockedAt))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM accounts WHERE id = $1 FOR UPDATE")).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	mock.ExpectQuery("SELECT plan_id FROM scheduled_test_account_ownership").
		WithArgs(int64(19)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("INSERT INTO scheduled_test_results").
		WillReturnRows(scheduledTestResultRows(&service.ScheduledTestResult{
			ID:               1,
			PlanID:           19,
			ExecutionID:      "00000000-0000-0000-0000-000000000099",
			Status:           result.Status,
			RunMode:          result.RunMode,
			AttemptCount:     result.AttemptCount,
			Classification:   result.Classification,
			ProtectionAction: result.ProtectionAction,
			ErrorMessage:     result.ErrorMessage,
			StartedAt:        result.StartedAt,
			FinishedAt:       result.FinishedAt,
		}))
	mock.ExpectExec("INSERT INTO scheduled_test_protection_events").
		WithArgs(
			int64(19), int64(7), "00000000-0000-0000-0000-000000000099",
			service.ScheduledTestClassificationFailure, service.ScheduledTestProtectionActionNone, lockedAt,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM scheduled_test_results").
		WithArgs(int64(19), 50).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("UPDATE scheduled_test_plans").
		WithArgs(int64(19), "00000000-0000-0000-0000-000000000019", 0, lockedAt, sqlmock.AnyArg(), int64(4)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	repo := NewScheduledTestPlanRepository(db)
	_, err = repo.FinalizeClaim(context.Background(), service.ScheduledTestFinalizeInput{
		Plan: &service.ScheduledTestPlan{
			ID:                             19,
			PlanVersion:                    4,
			EffectiveTimeoutProtectionMode: service.ScheduledTestTimeoutProtectionEnforce,
		},
		ClaimToken: "00000000-0000-0000-0000-000000000019",
		Result:     result,
		Now:        now,
		NextRunAt:  now.Add(time.Minute),
	})

	require.ErrorIs(t, err, service.ErrScheduledTestClaimLost)
	require.NoError(t, mock.ExpectationsWereMet())
}
