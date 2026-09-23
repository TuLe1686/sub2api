package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
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
	if result != nil {
		result.FailureKind = classifyScheduledTestFailureKind(result, runErr)
	}
	return ScheduledTestClassificationFailure
}

var scheduledTestHTTPStatusPattern = regexp.MustCompile(`returned (\d{3})`)

// classifyScheduledTestFailureKind 给出失败尝试的观察性子类，只写入结果供展示与统计。
// 返回值不参与连续计数、平台熔断、停用预算或保护动作，因此这里宁可保守地回退到
// unknown，也不引入任何会改变处置行为的判定。
func classifyScheduledTestFailureKind(result *ScheduledTestResult, runErr error) string {
	message := ""
	if result != nil {
		message = strings.TrimSpace(result.ErrorMessage)
	}
	if message == "" && runErr != nil {
		message = strings.TrimSpace(runErr.Error())
	}
	if message == "" {
		return ScheduledTestFailureKindUnknown
	}
	if match := scheduledTestHTTPStatusPattern.FindStringSubmatch(message); match != nil {
		if status, err := strconv.Atoi(match[1]); err == nil {
			switch {
			case status == http.StatusUnauthorized || status == http.StatusForbidden:
				return ScheduledTestFailureKindAuth
			case status == http.StatusTooManyRequests:
				return ScheduledTestFailureKindRateLimited
			case status >= http.StatusInternalServerError:
				return ScheduledTestFailureKindUpstream
			case status >= http.StatusBadRequest:
				return ScheduledTestFailureKindBusiness
			}
		}
	}
	return classifyScheduledTestFailureKindByText(strings.ToLower(message))
}

func classifyScheduledTestFailureKindByText(message string) string {
	switch {
	case scheduledTestMessageContainsAny(message,
		"invalid_api_key", "invalid api key", "authentication failed",
		"unauthorized", "forbidden", "permission denied"):
		return ScheduledTestFailureKindAuth
	case scheduledTestMessageContainsAny(message,
		"rate limit", "rate_limit", "too many requests"):
		return ScheduledTestFailureKindRateLimited
	case scheduledTestMessageContainsAny(message,
		"service temporarily unavailable", "overloaded", "bad gateway",
		"gateway timeout", "internal server error"):
		return ScheduledTestFailureKindUpstream
	case scheduledTestMessageContainsAny(message,
		"request failed:", "dial tcp", "connection refused", "no such host",
		"tls handshake", "connection reset", "broken pipe", "unexpected eof",
		"proxyconnect", "server misbehaving"):
		return ScheduledTestFailureKindNetwork
	}
	return ScheduledTestFailureKindUnknown
}

func scheduledTestMessageContainsAny(message string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
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
