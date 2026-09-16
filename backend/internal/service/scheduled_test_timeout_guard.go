package service

import (
	"context"
	"errors"
	"net"
	"time"
)

type scheduledTestWaitFunc func(context.Context, time.Duration) error

func runScheduledTestAttempts(
	ctx context.Context,
	tester ScheduledTestAccountTester,
	plan *ScheduledTestPlan,
	wait scheduledTestWaitFunc,
) (*ScheduledTestResult, error) {
	if wait == nil {
		wait = waitScheduledTestRetry
	}

	mode := scheduledTestEffectiveMode(plan)
	startedAt := time.Now()
	attempts := 1
	if mode != ScheduledTestTimeoutProtectionOff {
		attempts += len(plan.RetryDelaysSeconds)
	}
	for attempt := 0; attempt < attempts; attempt++ {
		attemptCtx := ctx
		cancel := func() {}
		if mode != ScheduledTestTimeoutProtectionOff && plan.TimeoutSeconds > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, time.Duration(plan.TimeoutSeconds)*time.Second)
		}
		result, err := tester.RunTestBackground(attemptCtx, plan.AccountID, plan.ModelID)
		deadlineErr := attemptCtx.Err()
		cancel()
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}

		if result == nil {
			result = &ScheduledTestResult{}
		}
		result.AttemptCount = attempt + 1
		result.RunMode = ScheduledTestRunModeNormal
		result.Classification = classifyScheduledTestAttempt(result, err, deadlineErr)
		if result.StartedAt.IsZero() {
			result.StartedAt = startedAt
		}
		if result.FinishedAt.IsZero() {
			result.FinishedAt = time.Now()
		}

		switch result.Classification {
		case ScheduledTestClassificationSuccess:
			result.Status = ScheduledTestStatusSuccess
			return result, nil
		case ScheduledTestClassificationFailure:
			result.Status = ScheduledTestStatusFailed
			if result.ErrorMessage == "" && err != nil {
				result.ErrorMessage = err.Error()
			}
			return result, nil
		case ScheduledTestClassificationTimeout:
			result.Status = ScheduledTestStatusFailed
			if result.ErrorMessage == "" {
				if err != nil {
					result.ErrorMessage = err.Error()
				} else {
					result.ErrorMessage = context.DeadlineExceeded.Error()
				}
			}
			if attempt == attempts-1 {
				return result, nil
			}
			if err := wait(ctx, time.Duration(plan.RetryDelaysSeconds[attempt])*time.Second); err != nil {
				if isScheduledTestTimeout(err) {
					result.ErrorMessage = err.Error()
					result.FinishedAt = time.Now()
					return result, nil
				}
				return nil, err
			}
		}
	}
	return nil, context.Canceled
}

func scheduledTestEffectiveMode(plan *ScheduledTestPlan) string {
	if plan == nil {
		return ScheduledTestTimeoutProtectionOff
	}
	if plan.EffectiveTimeoutProtectionMode != "" {
		return plan.EffectiveTimeoutProtectionMode
	}
	return plan.TimeoutProtectionMode
}

func scheduledTestExecutionBudget(plan *ScheduledTestPlan) time.Duration {
	if scheduledTestEffectiveMode(plan) == ScheduledTestTimeoutProtectionOff || plan.TimeoutSeconds <= 0 {
		return 5 * time.Minute
	}
	attempts := len(plan.RetryDelaysSeconds) + 1
	budget := time.Duration(attempts*plan.TimeoutSeconds) * time.Second
	for _, delay := range plan.RetryDelaysSeconds {
		budget += time.Duration(delay) * time.Second
	}
	return budget
}

func classifyScheduledTestAttempt(result *ScheduledTestResult, runErr, deadlineErr error) string {
	if runErr == nil && deadlineErr == nil && result != nil && result.Status == ScheduledTestStatusSuccess {
		return ScheduledTestClassificationSuccess
	}
	if isScheduledTestTimeout(runErr) || isScheduledTestTimeout(deadlineErr) {
		return ScheduledTestClassificationTimeout
	}
	return ScheduledTestClassificationFailure
}

func isScheduledTestTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

func waitScheduledTestRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
