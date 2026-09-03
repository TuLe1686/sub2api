//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestScheduledTestTimeoutGuardInactivatesAndRecoversOwnedAccountAtomically(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	suffix := time.Now().UnixNano()
	account := mustCreateAccount(t, client, &service.Account{
		Name:        fmt.Sprintf("scheduled-timeout-%d", suffix),
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"api_key": "integration-only"},
		Extra:       map[string]any{},
	})
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		UPDATE accounts SET error_message = NULL WHERE id = $1 RETURNING id
	`, account.ID).Scan(&account.ID))

	planRepo := NewScheduledTestPlanRepository(integrationDB)
	plan, err := planRepo.Create(ctx, &service.ScheduledTestPlan{
		AccountID:                   account.ID,
		ModelID:                     "gpt-5",
		CronExpression:              "* * * * *",
		Enabled:                     true,
		MaxResults:                  10,
		TimeoutProtectionMode:       service.ScheduledTestTimeoutProtectionEnforce,
		TimeoutSeconds:              1,
		ConsecutiveTimeoutThreshold: 1,
		RetryDelaysSeconds:          []int{},
		NextRunAt:                   timePointer(time.Now().Add(-time.Minute)),
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduled_test_account_ownership WHERE account_id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduled_test_results WHERE plan_id = $1", plan.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduled_test_protection_events WHERE plan_id = $1", plan.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduled_test_plans WHERE id = $1", plan.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", account.ID)
	})

	claimed, err := planRepo.ClaimDue(ctx, time.Now(), time.Minute, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.NotEmpty(t, claimed[0].ExecutionID)
	require.NotEmpty(t, claimed[0].ClaimToken)

	now := time.Now()
	finalized, err := planRepo.FinalizeClaim(ctx, service.ScheduledTestFinalizeInput{
		Plan:       claimed[0],
		ClaimToken: claimed[0].ClaimToken,
		Result: &service.ScheduledTestResult{
			Status:           service.ScheduledTestStatusFailed,
			RunMode:          service.ScheduledTestRunModeNormal,
			AttemptCount:     1,
			Classification:   service.ScheduledTestClassificationTimeout,
			ProtectionAction: service.ScheduledTestProtectionActionNone,
			ErrorMessage:     context.DeadlineExceeded.Error(),
			StartedAt:        now.Add(-time.Second),
			FinishedAt:       now,
		},
		NextRunAt:     now.Add(time.Minute),
		Now:           now,
		DisableBudget: 10,
	})
	require.NoError(t, err)
	require.Equal(t, service.ScheduledTestProtectionActionInactivated, finalized.ProtectionAction)

	var status string
	var schedulable bool
	var errorMessage sql.NullString
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, schedulable, error_message FROM accounts WHERE id = $1
	`, account.ID).Scan(&status, &schedulable, &errorMessage))
	require.Equal(t, service.ScheduledTestAccountStatusInactive, status)
	require.False(t, schedulable)
	require.True(t, errorMessage.Valid)
	require.Equal(t, "scheduled test consecutive timeouts", errorMessage.String)

	var ownershipCount, outboxCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM scheduled_test_account_ownership WHERE account_id = $1 AND plan_id = $2
	`, account.ID, plan.ID).Scan(&ownershipCount))
	require.Equal(t, 1, ownershipCount)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1
	`, account.ID).Scan(&outboxCount))
	require.GreaterOrEqual(t, outboxCount, 1)

	_, err = integrationDB.ExecContext(ctx, `
		UPDATE scheduled_test_plans SET next_run_at = $2 WHERE id = $1
	`, plan.ID, time.Now().Add(-time.Minute))
	require.NoError(t, err)
	claimed, err = planRepo.ClaimDue(ctx, time.Now(), time.Minute, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.True(t, claimed[0].OwnsInactiveAccount)

	now = time.Now()
	finalized, err = planRepo.FinalizeClaim(ctx, service.ScheduledTestFinalizeInput{
		Plan:       claimed[0],
		ClaimToken: claimed[0].ClaimToken,
		Result: &service.ScheduledTestResult{
			Status:           service.ScheduledTestStatusSuccess,
			AttemptCount:     1,
			Classification:   service.ScheduledTestClassificationSuccess,
			ProtectionAction: service.ScheduledTestProtectionActionNone,
			ResponseText:     "ok",
			StartedAt:        now.Add(-time.Second),
			FinishedAt:       now,
		},
		NextRunAt: now.Add(time.Minute),
		Now:       now,
	})
	require.NoError(t, err)
	require.Equal(t, service.ScheduledTestRunModeRecovery, finalized.RunMode)
	require.Equal(t, service.ScheduledTestProtectionActionRecovered, finalized.ProtectionAction)

	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, schedulable, error_message FROM accounts WHERE id = $1
	`, account.ID).Scan(&status, &schedulable, &errorMessage))
	require.Equal(t, service.StatusActive, status)
	require.True(t, schedulable)
	require.False(t, errorMessage.Valid)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM scheduled_test_account_ownership WHERE account_id = $1
	`, account.ID).Scan(&ownershipCount))
	require.Zero(t, ownershipCount)

	var eventCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM scheduled_test_protection_events WHERE plan_id = $1
	`, plan.ID).Scan(&eventCount))
	require.Equal(t, 2, eventCount)
	require.NoError(t, planRepo.Delete(ctx, plan.ID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM scheduled_test_protection_events WHERE plan_id = $1
	`, plan.ID).Scan(&eventCount))
	require.Equal(t, 2, eventCount)
}

func timePointer(value time.Time) *time.Time {
	return &value
}
