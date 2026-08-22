package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	scheduledTestDefaultMaxWorkers = 10
	scheduledTestScanInterval      = 10 * time.Second
)

// ScheduledTestRunnerService periodically claims and executes due test plans.
type ScheduledTestRunnerService struct {
	planRepo       ScheduledTestPlanRepository
	accountTestSvc ScheduledTestAccountTester
	rateLimitSvc   *RateLimitService
	cfg            *config.Config

	cancel    context.CancelFunc
	workers   chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// NewScheduledTestRunnerService creates a new runner.
func NewScheduledTestRunnerService(
	planRepo ScheduledTestPlanRepository,
	_ *ScheduledTestService,
	accountTestSvc ScheduledTestAccountTester,
	rateLimitSvc *RateLimitService,
	cfg *config.Config,
) *ScheduledTestRunnerService {
	maxWorkers := scheduledTestDefaultMaxWorkers
	if cfg != nil && cfg.ScheduledTestTimeoutGuard.MaxWorkers > 0 {
		maxWorkers = cfg.ScheduledTestTimeoutGuard.MaxWorkers
	}
	return &ScheduledTestRunnerService{
		planRepo:       planRepo,
		accountTestSvc: accountTestSvc,
		rateLimitSvc:   rateLimitSvc,
		cfg:            cfg,
		workers:        make(chan struct{}, maxWorkers),
	}
}

// Start begins the durable scanner.
func (s *ScheduledTestRunnerService) Start() {
	if s == nil || s.planRepo == nil || s.accountTestSvc == nil {
		return
	}
	s.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		s.wg.Add(1)
		go s.scanLoop(ctx)
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] started (scan_interval=%s)", scheduledTestScanInterval)
	})
}

// Stop gracefully shuts down the scanner and workers.
func (s *ScheduledTestRunnerService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] stop timed out")
		}
	})
}

func (s *ScheduledTestRunnerService) scanLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(scheduledTestScanInterval)
	defer ticker.Stop()
	s.runScheduled(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runScheduled(ctx)
		}
	}
}

func (s *ScheduledTestRunnerService) runScheduled(ctx context.Context) {
	lease := 5 * time.Minute
	if s.cfg != nil && s.cfg.ScheduledTestTimeoutGuard.ClaimLeaseSeconds > 0 {
		lease = time.Duration(s.cfg.ScheduledTestTimeoutGuard.ClaimLeaseSeconds) * time.Second
	}
	if healed, err := s.planRepo.HealExpiredClaims(ctx, time.Now(), 20); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] HealExpiredClaims error: %v", err)
	} else if healed > 0 {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] healed %d expired claims", healed)
	}
	available := cap(s.workers) - len(s.workers)
	if available <= 0 {
		return
	}
	plans, err := s.planRepo.ClaimDue(ctx, time.Now(), lease, available)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] ClaimDue error: %v", err)
		return
	}
	for _, plan := range plans {
		s.workers <- struct{}{}
		s.wg.Add(1)
		go func(plan *ScheduledTestPlan) {
			defer s.wg.Done()
			defer func() { <-s.workers }()
			s.runOnePlan(ctx, plan, lease)
		}(plan)
	}
}

func (s *ScheduledTestRunnerService) runOnePlan(parent context.Context, plan *ScheduledTestPlan, lease time.Duration) {
	plan.EffectiveTimeoutProtectionMode, plan.TimeoutProtectionOverrideReason =
		effectiveScheduledTestTimeoutProtection(plan.TimeoutProtectionMode, s.cfg)

	lifecycleCtx, lifecycleCancel := context.WithCancel(parent)
	executionCtx, executionCancel := context.WithTimeout(lifecycleCtx, scheduledTestExecutionBudget(plan))

	renewDone := make(chan struct{})
	go s.renewClaim(lifecycleCtx, plan, lease, lifecycleCancel, renewDone)
	defer func() {
		lifecycleCancel()
		<-renewDone
	}()

	result, err := runScheduledTestAttempts(executionCtx, s.accountTestSvc, plan, nil)
	executionCancel()
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d execution error: %v", plan.ID, err)
		return
	}

	now := scheduledTestNowInTimezone(s.cfg)
	nextRun, err := computeNextRun(plan.CronExpression, now)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d computeNextRun error: %v", plan.ID, err)
		return
	}
	plan.EffectiveTimeoutProtectionMode, plan.TimeoutProtectionOverrideReason =
		effectiveScheduledTestTimeoutProtection(plan.TimeoutProtectionMode, s.cfg)
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer finalizeCancel()
	finalized, err := s.planRepo.FinalizeClaim(finalizeCtx, ScheduledTestFinalizeInput{
		Plan:                plan,
		ClaimToken:          plan.ClaimToken,
		Result:              result,
		NextRunAt:           nextRun,
		Now:                 now,
		CircuitWindow:       s.timeoutGuardCircuitWindow(),
		CircuitMinSamples:   s.timeoutGuardCircuitMinSamples(),
		CircuitTimeoutRatio: s.timeoutGuardCircuitTimeoutRatio(),
		DisableBudget:       s.timeoutGuardDisableBudget(),
	})
	if errors.Is(err, ErrScheduledTestClaimLost) {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d claim lost before finalize", plan.ID)
		return
	}
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d finalize error: %v", plan.ID, err)
		return
	}

	if finalized != nil {
		logger.LegacyPrintf("service.scheduled_test_runner",
			"[ScheduledTestRunner] finalized plan=%d account=%d execution=%s run_mode=%s classification=%s action=%s blocked=%s claim_token=%s",
			plan.ID, plan.AccountID, finalized.ExecutionID, finalized.RunMode,
			finalized.Classification, finalized.ProtectionAction, finalized.BlockedReason, plan.ClaimToken)
	}

	if finalized.Classification == ScheduledTestClassificationSuccess && plan.AutoRecover && finalized.ProtectionAction != ScheduledTestProtectionActionRecovered {
		recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer recoverCancel()
		if s.canRunScheduledTestAutoRecover(recoverCtx, plan) {
			s.tryRecoverAccount(recoverCtx, plan.AccountID, plan.ID)
		}
	}
}

func (s *ScheduledTestRunnerService) canRunScheduledTestAutoRecover(ctx context.Context, plan *ScheduledTestPlan) bool {
	if plan == nil || plan.TimeoutProtectionMode != ScheduledTestTimeoutProtectionOff {
		return false
	}
	owned, err := s.planRepo.HasAccountOwnership(ctx, plan.AccountID)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d ownership check failed: %v", plan.ID, err)
		return false
	}
	return !owned
}

func (s *ScheduledTestRunnerService) renewClaim(
	ctx context.Context,
	plan *ScheduledTestPlan,
	lease time.Duration,
	cancel context.CancelFunc,
	done chan<- struct{},
) {
	defer close(done)
	interval := lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	retryInterval := interval / 4
	if retryInterval < time.Second {
		retryInterval = time.Second
	}
	safetyMargin := lease / 5
	if safetyMargin < 2*time.Second {
		safetyMargin = 2 * time.Second
	}
	expiresAt := plan.ClaimExpiresAt
	if expiresAt == nil || expiresAt.IsZero() {
		fallback := time.Now().Add(lease)
		expiresAt = &fallback
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			remaining := time.Until(*expiresAt) - safetyMargin
			if remaining <= 0 {
				cancel()
				return
			}
			renewTimeout := 10 * time.Second
			if remaining < renewTimeout {
				renewTimeout = remaining
			}
			renewCtx, renewCancel := context.WithTimeout(context.Background(), renewTimeout)
			newExpiresAt, ok, err := s.planRepo.RenewClaim(renewCtx, plan.ID, plan.ClaimToken, plan.PlanVersion, lease)
			renewCancel()
			if err != nil {
				logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d renew error: %v", plan.ID, err)
				timer.Reset(retryInterval)
				continue
			}
			if !ok {
				cancel()
				return
			}
			expiresAt = &newExpiresAt
			plan.ClaimExpiresAt = &newExpiresAt
			timer.Reset(interval)
		}
	}
}

func (s *ScheduledTestRunnerService) timeoutGuardCircuitWindow() time.Duration {
	if s.cfg == nil || s.cfg.ScheduledTestTimeoutGuard.CircuitWindowSeconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(s.cfg.ScheduledTestTimeoutGuard.CircuitWindowSeconds) * time.Second
}

func (s *ScheduledTestRunnerService) timeoutGuardCircuitMinSamples() int {
	if s.cfg == nil {
		return 20
	}
	return s.cfg.ScheduledTestTimeoutGuard.CircuitMinSamples
}

func (s *ScheduledTestRunnerService) timeoutGuardCircuitTimeoutRatio() float64 {
	if s.cfg == nil {
		return 0.8
	}
	return s.cfg.ScheduledTestTimeoutGuard.CircuitTimeoutRatio
}

func (s *ScheduledTestRunnerService) timeoutGuardDisableBudget() int {
	if s.cfg == nil {
		return 3
	}
	return s.cfg.ScheduledTestTimeoutGuard.DisableBudgetPerWindow
}

func (s *ScheduledTestRunnerService) tryRecoverAccount(ctx context.Context, accountID int64, planID int64) {
	if s.rateLimitSvc == nil {
		return
	}
	recovery, err := s.rateLimitSvc.RecoverAccountAfterSuccessfulTest(ctx, accountID)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover failed: %v", planID, err)
		return
	}
	if recovery == nil {
		return
	}
	if recovery.ClearedError {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d recovered from error status", planID, accountID)
	}
	if recovery.ClearedRateLimit {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d cleared rate-limit/runtime state", planID, accountID)
	}
}
