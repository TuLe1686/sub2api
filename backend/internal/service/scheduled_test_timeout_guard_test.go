package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type scheduledTestAttemptStub struct {
	results       []*ScheduledTestResult
	errs          []error
	calls         int
	deadlineAfter time.Duration
}

func (s *scheduledTestAttemptStub) RunTestBackground(ctx context.Context, _ int64, _ string) (*ScheduledTestResult, error) {
	index := s.calls
	s.calls++
	if deadline, ok := ctx.Deadline(); ok {
		s.deadlineAfter = time.Until(deadline)
	}
	if index < len(s.errs) && s.errs[index] != nil {
		return nil, s.errs[index]
	}
	return s.results[index], nil
}

func TestScheduledTestServiceDefaultsTimeoutProtectionToOff(t *testing.T) {
	repo := &scheduledTestPlanRepositoryStub{}
	svc := NewScheduledTestService(repo, nil)

	created, err := svc.CreatePlan(context.Background(), &ScheduledTestPlan{
		AccountID:      7,
		CronExpression: "*/5 * * * *",
		Enabled:        true,
	})

	require.NoError(t, err)
	require.Equal(t, ScheduledTestTimeoutProtectionOff, created.TimeoutProtectionMode)
	require.Equal(t, 60, created.TimeoutSeconds)
	require.Equal(t, 3, created.ConsecutiveTimeoutThreshold)
	require.Equal(t, []int{10, 20}, created.RetryDelaysSeconds)
}

func TestScheduledTestServiceExposesEffectiveProtectionMode(t *testing.T) {
	plan := &ScheduledTestPlan{TimeoutProtectionMode: ScheduledTestTimeoutProtectionEnforce}
	svc := NewScheduledTestService(&scheduledTestPlanRepositoryStub{plan: plan}, nil, &config.Config{
		ScheduledTestTimeoutGuard: config.ScheduledTestTimeoutGuardConfig{Enabled: true, ForceShadow: true},
	})

	loaded, err := svc.GetPlan(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, ScheduledTestTimeoutProtectionShadow, loaded.EffectiveTimeoutProtectionMode)
	require.Equal(t, "force_shadow", loaded.TimeoutProtectionOverrideReason)

	svc.cfg.ScheduledTestTimeoutGuard.Enabled = false
	loaded, err = svc.GetPlan(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, ScheduledTestTimeoutProtectionOff, loaded.EffectiveTimeoutProtectionMode)
	require.Equal(t, "kill_switch", loaded.TimeoutProtectionOverrideReason)

	svc.cfg.ScheduledTestTimeoutGuard.Enabled = true
	svc.cfg.ScheduledTestTimeoutGuard.ForceShadow = false
	loaded, err = svc.GetPlan(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, ScheduledTestTimeoutProtectionEnforce, loaded.EffectiveTimeoutProtectionMode)
	require.Empty(t, loaded.TimeoutProtectionOverrideReason)
}

func TestScheduledTestServicePreservesExplicitZeroValuesWhenProtectionIsOff(t *testing.T) {
	repo := &scheduledTestPlanRepositoryStub{}
	svc := NewScheduledTestService(repo, nil)

	created, err := svc.CreatePlan(context.Background(), &ScheduledTestPlan{
		AccountID:                      7,
		CronExpression:                 "*/5 * * * *",
		Enabled:                        true,
		TimeoutProtectionMode:          ScheduledTestTimeoutProtectionOff,
		TimeoutSecondsSet:              true,
		ConsecutiveTimeoutThresholdSet: true,
		RetryDelaysSeconds:             []int{},
		RetryDelaysSecondsSet:          true,
	})

	require.NoError(t, err)
	require.Zero(t, created.TimeoutSeconds)
	require.Zero(t, created.ConsecutiveTimeoutThreshold)
	require.Empty(t, created.RetryDelaysSeconds)
}

func TestScheduledTestServiceRejectsInvalidTimeoutProtectionConfiguration(t *testing.T) {
	svc := NewScheduledTestService(&scheduledTestPlanRepositoryStub{}, nil)

	_, err := svc.CreatePlan(context.Background(), &ScheduledTestPlan{
		AccountID:                   7,
		CronExpression:              "*/5 * * * *",
		Enabled:                     true,
		TimeoutProtectionMode:       ScheduledTestTimeoutProtectionEnforce,
		TimeoutSeconds:              0,
		TimeoutSecondsSet:           true,
		ConsecutiveTimeoutThreshold: 2,
		RetryDelaysSeconds:          []int{10, 20},
	})

	require.ErrorContains(t, err, "timeout_seconds")
}

func TestRunScheduledTestAttemptsRetriesOnlyTimeouts(t *testing.T) {
	tester := &scheduledTestAttemptStub{
		results: []*ScheduledTestResult{
			nil,
			nil,
			{Status: ScheduledTestStatusSuccess, ResponseText: "ok"},
		},
		errs: []error{context.DeadlineExceeded, context.DeadlineExceeded, nil},
	}
	var waits []time.Duration

	result, err := runScheduledTestAttempts(
		context.Background(),
		tester,
		&ScheduledTestPlan{
			AccountID:             7,
			ModelID:               "gpt-5",
			TimeoutProtectionMode: ScheduledTestTimeoutProtectionEnforce,
			TimeoutSeconds:        1,
			RetryDelaysSeconds:    []int{10, 20},
		},
		func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	)

	require.NoError(t, err)
	require.Equal(t, ScheduledTestClassificationSuccess, result.Classification)
	require.Equal(t, 3, result.AttemptCount)
	require.Equal(t, []time.Duration{10 * time.Second, 20 * time.Second}, waits)
}

func TestRunScheduledTestAttemptsKeepsLegacyPlansSingleAttempt(t *testing.T) {
	tester := &scheduledTestAttemptStub{
		errs: []error{context.DeadlineExceeded},
	}

	result, err := runScheduledTestAttempts(
		context.Background(),
		tester,
		&ScheduledTestPlan{
			AccountID:             7,
			TimeoutProtectionMode: ScheduledTestTimeoutProtectionOff,
			TimeoutSeconds:        60,
			RetryDelaysSeconds:    []int{10, 20},
		},
		func(context.Context, time.Duration) error {
			t.Fatal("legacy plans must not retry")
			return nil
		},
	)

	require.NoError(t, err)
	require.Equal(t, ScheduledTestClassificationTimeout, result.Classification)
	require.Equal(t, 1, result.AttemptCount)
	require.Equal(t, 1, tester.calls)
}

func TestRunScheduledTestAttemptsDoesNotTrustTimeoutText(t *testing.T) {
	tester := &scheduledTestAttemptStub{
		results: []*ScheduledTestResult{{Status: ScheduledTestStatusFailed, ErrorMessage: "invalid timeout parameter"}},
	}

	result, err := runScheduledTestAttempts(
		context.Background(),
		tester,
		&ScheduledTestPlan{
			AccountID:             7,
			TimeoutProtectionMode: ScheduledTestTimeoutProtectionEnforce,
			TimeoutSeconds:        60,
			RetryDelaysSeconds:    []int{10, 20},
		},
		func(context.Context, time.Duration) error {
			t.Fatal("untrusted timeout text must not trigger retries")
			return nil
		},
	)

	require.NoError(t, err)
	require.Equal(t, ScheduledTestClassificationFailure, result.Classification)
	require.Equal(t, 1, result.AttemptCount)
}

func TestRunScheduledTestAttemptsReturnsCancellationWithoutBusinessResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tester := &scheduledTestAttemptStub{errs: []error{context.Canceled}}

	result, err := runScheduledTestAttempts(
		ctx,
		tester,
		&ScheduledTestPlan{AccountID: 7, TimeoutProtectionMode: ScheduledTestTimeoutProtectionOff},
		nil,
	)

	require.Nil(t, result)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunScheduledTestAttemptsStopsOnNonTimeoutFailure(t *testing.T) {
	tester := &scheduledTestAttemptStub{
		results: []*ScheduledTestResult{{Status: ScheduledTestStatusFailed, ErrorMessage: "invalid credentials"}},
		errs:    []error{errors.New("invalid credentials")},
	}

	result, err := runScheduledTestAttempts(
		context.Background(),
		tester,
		&ScheduledTestPlan{
			AccountID:             7,
			TimeoutProtectionMode: ScheduledTestTimeoutProtectionEnforce,
			TimeoutSeconds:        1,
			RetryDelaysSeconds:    []int{10, 20},
		},
		func(context.Context, time.Duration) error {
			t.Fatal("non-timeout failures must not retry")
			return nil
		},
	)

	require.NoError(t, err)
	require.Equal(t, ScheduledTestClassificationFailure, result.Classification)
	require.Equal(t, 1, result.AttemptCount)
}

func TestScheduledTestExecutionBudgetCoversAllConfiguredAttempts(t *testing.T) {
	plan := &ScheduledTestPlan{
		TimeoutProtectionMode: ScheduledTestTimeoutProtectionEnforce,
		TimeoutSeconds:        600,
		RetryDelaysSeconds:    []int{300, 300, 300, 300, 300},
	}

	require.Equal(t, 85*time.Minute, scheduledTestExecutionBudget(plan))
}

func TestRunScheduledTestAttemptsUsesEffectiveOffMode(t *testing.T) {
	tester := &scheduledTestAttemptStub{errs: []error{context.DeadlineExceeded}}
	plan := &ScheduledTestPlan{
		AccountID:                      7,
		TimeoutProtectionMode:          ScheduledTestTimeoutProtectionEnforce,
		EffectiveTimeoutProtectionMode: ScheduledTestTimeoutProtectionOff,
		TimeoutSeconds:                 1,
		RetryDelaysSeconds:             []int{10, 20},
	}

	result, err := runScheduledTestAttempts(context.Background(), tester, plan, func(context.Context, time.Duration) error {
		t.Fatal("effective off mode must not retry")
		return nil
	})

	require.NoError(t, err)
	require.Equal(t, 1, tester.calls)
	require.Equal(t, ScheduledTestClassificationTimeout, result.Classification)
}

func TestRunScheduledTestAttemptsReturnsTimeoutResultWhenRetryWaitBudgetExpires(t *testing.T) {
	tester := &scheduledTestAttemptStub{errs: []error{context.DeadlineExceeded}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result, err := runScheduledTestAttempts(ctx, tester, &ScheduledTestPlan{
		AccountID:                      7,
		TimeoutProtectionMode:          ScheduledTestTimeoutProtectionEnforce,
		EffectiveTimeoutProtectionMode: ScheduledTestTimeoutProtectionEnforce,
		TimeoutSeconds:                 1,
		RetryDelaysSeconds:             []int{10},
	}, func(context.Context, time.Duration) error {
		return context.DeadlineExceeded
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, ScheduledTestClassificationTimeout, result.Classification)
	require.Equal(t, context.DeadlineExceeded.Error(), result.ErrorMessage)
}

type scheduledTestBlockingStub struct {
	canceled chan struct{}
}

func (s *scheduledTestBlockingStub) RunTestBackground(ctx context.Context, _ int64, _ string) (*ScheduledTestResult, error) {
	<-ctx.Done()
	close(s.canceled)
	return nil, ctx.Err()
}

func TestScheduledTestRunnerPreservesExplicitZeroCircuitRatio(t *testing.T) {
	runner := NewScheduledTestRunnerService(
		&scheduledTestPlanRepositoryStub{},
		nil,
		&scheduledTestAttemptStub{},
		nil,
		&config.Config{ScheduledTestTimeoutGuard: config.ScheduledTestTimeoutGuardConfig{CircuitTimeoutRatio: 0}},
	)

	require.Zero(t, runner.timeoutGuardCircuitTimeoutRatio())
}

func TestScheduledTestRunnerRestrictsOrdinaryAutoRecoverToUnownedOffPlans(t *testing.T) {
	repo := &scheduledTestPlanRepositoryStub{hasAccountOwnership: true}
	runner := NewScheduledTestRunnerService(repo, nil, &scheduledTestAttemptStub{}, nil, nil)
	plan := &ScheduledTestPlan{
		ID:                    19,
		AccountID:             7,
		TimeoutProtectionMode: ScheduledTestTimeoutProtectionOff,
	}

	require.False(t, runner.canRunScheduledTestAutoRecover(context.Background(), plan))

	repo.hasAccountOwnership = false
	require.True(t, runner.canRunScheduledTestAutoRecover(context.Background(), plan))

	plan.TimeoutProtectionMode = ScheduledTestTimeoutProtectionEnforce
	require.False(t, runner.canRunScheduledTestAutoRecover(context.Background(), plan))
}

func TestScheduledTestRunnerCancelsExecutionBeforeUnrenewedLeaseExpires(t *testing.T) {
	expiresAt := time.Now().Add(4 * time.Second)
	repo := &scheduledTestPlanRepositoryStub{
		renewClaim: func(context.Context, int64, string, int64, time.Duration) (time.Time, bool, error) {
			return time.Time{}, false, errors.New("database unavailable")
		},
	}
	tester := &scheduledTestBlockingStub{canceled: make(chan struct{})}
	runner := NewScheduledTestRunnerService(repo, nil, tester, nil, &config.Config{
		ScheduledTestTimeoutGuard: config.ScheduledTestTimeoutGuardConfig{Enabled: true},
	})
	plan := &ScheduledTestPlan{
		ID:                    19,
		AccountID:             7,
		CronExpression:        "* * * * *",
		TimeoutProtectionMode: ScheduledTestTimeoutProtectionEnforce,
		TimeoutSeconds:        60,
		PlanVersion:           4,
		ClaimToken:            "00000000-0000-0000-0000-000000000019",
		ClaimExpiresAt:        &expiresAt,
	}

	done := make(chan struct{})
	go func() {
		runner.runOnePlan(context.Background(), plan, 4*time.Second)
		close(done)
	}()

	select {
	case <-tester.canceled:
	case <-time.After(4 * time.Second):
		t.Fatal("runner did not cancel before the unrenewed lease expired")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not return after lease cancellation")
	}
	require.Zero(t, repo.finalizeCalls)
}

type scheduledTestPlanRepositoryStub struct {
	plan                *ScheduledTestPlan
	renewClaim          func(context.Context, int64, string, int64, time.Duration) (time.Time, bool, error)
	hasAccountOwnership bool
	finalizeCalls       int
}

func (r *scheduledTestPlanRepositoryStub) Create(_ context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	copy := *plan
	r.plan = &copy
	return &copy, nil
}

func (r *scheduledTestPlanRepositoryStub) GetByID(context.Context, int64) (*ScheduledTestPlan, error) {
	return r.plan, nil
}

func (r *scheduledTestPlanRepositoryStub) ListByAccountID(context.Context, int64) ([]*ScheduledTestPlan, error) {
	return nil, nil
}

func (r *scheduledTestPlanRepositoryStub) ListDue(context.Context, time.Time) ([]*ScheduledTestPlan, error) {
	return nil, nil
}

func (r *scheduledTestPlanRepositoryStub) ClaimDue(context.Context, time.Time, time.Duration, int) ([]*ScheduledTestPlan, error) {
	return nil, nil
}

func (r *scheduledTestPlanRepositoryStub) HealExpiredClaims(context.Context, time.Time, int) (int, error) {
	return 0, nil
}

func (r *scheduledTestPlanRepositoryStub) RenewClaim(ctx context.Context, planID int64, token string, version int64, lease time.Duration) (time.Time, bool, error) {
	if r.renewClaim != nil {
		return r.renewClaim(ctx, planID, token, version, lease)
	}
	return time.Now().Add(lease), true, nil
}

func (r *scheduledTestPlanRepositoryStub) HasAccountOwnership(context.Context, int64) (bool, error) {
	return r.hasAccountOwnership, nil
}

func (r *scheduledTestPlanRepositoryStub) FinalizeClaim(_ context.Context, input ScheduledTestFinalizeInput) (*ScheduledTestResult, error) {
	r.finalizeCalls++
	return input.Result, nil
}

func (r *scheduledTestPlanRepositoryStub) Update(_ context.Context, plan *ScheduledTestPlan, _ ...bool) (*ScheduledTestPlan, error) {
	return plan, nil
}

func (r *scheduledTestPlanRepositoryStub) Delete(context.Context, int64, ...bool) error { return nil }

func (r *scheduledTestPlanRepositoryStub) UpdateAfterRun(context.Context, int64, time.Time, time.Time) error {
	return nil
}
