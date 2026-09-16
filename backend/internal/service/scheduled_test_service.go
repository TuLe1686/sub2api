package service

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/robfig/cron/v3"
)

var scheduledTestCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ScheduledTestService provides CRUD operations for scheduled test plans and results.
type ScheduledTestService struct {
	planRepo   ScheduledTestPlanRepository
	resultRepo ScheduledTestResultRepository
	cfg        *config.Config
}

// NewScheduledTestService creates a new ScheduledTestService.
func NewScheduledTestService(
	planRepo ScheduledTestPlanRepository,
	resultRepo ScheduledTestResultRepository,
	configs ...*config.Config,
) *ScheduledTestService {
	var cfg *config.Config
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return &ScheduledTestService{
		planRepo:   planRepo,
		resultRepo: resultRepo,
		cfg:        cfg,
	}
}

// CreatePlan validates the cron expression, computes next_run_at, and persists the plan.
func (s *ScheduledTestService) CreatePlan(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	nextRun, err := computeNextRun(plan.CronExpression, scheduledTestNowInTimezone(s.cfg))
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression: %w", err)
	}
	plan.NextRunAt = &nextRun

	if plan.MaxResults <= 0 {
		plan.MaxResults = 50
	}
	if err := normalizeScheduledTestTimeoutProtection(plan); err != nil {
		return nil, err
	}

	created, err := s.planRepo.Create(ctx, plan)
	if err != nil {
		return nil, err
	}
	s.decorateEffectiveTimeoutProtection(created)
	return created, nil
}

// GetPlan retrieves a plan by ID.
func (s *ScheduledTestService) GetPlan(ctx context.Context, id int64) (*ScheduledTestPlan, error) {
	plan, err := s.planRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	s.decorateEffectiveTimeoutProtection(plan)
	return plan, nil
}

// ListPlansByAccount returns all plans for a given account.
func (s *ScheduledTestService) ListPlansByAccount(ctx context.Context, accountID int64) ([]*ScheduledTestPlan, error) {
	plans, err := s.planRepo.ListByAccountID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	for _, plan := range plans {
		s.decorateEffectiveTimeoutProtection(plan)
	}
	return plans, nil
}

// UpdatePlan validates cron and updates the plan.
func (s *ScheduledTestService) UpdatePlan(ctx context.Context, plan *ScheduledTestPlan, restoreOwnedAccount ...bool) (*ScheduledTestPlan, error) {
	nextRun, err := computeNextRun(plan.CronExpression, scheduledTestNowInTimezone(s.cfg))
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression: %w", err)
	}
	plan.NextRunAt = &nextRun
	if err := normalizeScheduledTestTimeoutProtection(plan); err != nil {
		return nil, err
	}

	updated, err := s.planRepo.Update(ctx, plan, restoreOwnedAccount...)
	if err != nil {
		return nil, err
	}
	s.decorateEffectiveTimeoutProtection(updated)
	return updated, nil
}

// DeletePlan removes a plan and its results (via CASCADE).
func (s *ScheduledTestService) DeletePlan(ctx context.Context, id int64, restoreOwnedAccount ...bool) error {
	return s.planRepo.Delete(ctx, id, restoreOwnedAccount...)
}

// ListResults returns the most recent results for a plan.
func (s *ScheduledTestService) ListResults(ctx context.Context, planID int64, limit int) ([]*ScheduledTestResult, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.resultRepo.ListByPlanID(ctx, planID, limit)
}

// SaveResult inserts a result and prunes old entries beyond maxResults.
func (s *ScheduledTestService) SaveResult(ctx context.Context, planID int64, maxResults int, result *ScheduledTestResult) error {
	result.PlanID = planID
	if _, err := s.resultRepo.Create(ctx, result); err != nil {
		return err
	}
	return s.resultRepo.PruneOldResults(ctx, planID, maxResults)
}

func scheduledTestNowInTimezone(cfg *config.Config) time.Time {
	if cfg != nil && cfg.Timezone != "" {
		if loc, err := time.LoadLocation(cfg.Timezone); err == nil {
			return time.Now().In(loc)
		}
	}
	return time.Now()
}

func computeNextRun(cronExpr string, from time.Time) (time.Time, error) {
	sched, err := scheduledTestCronParser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(from), nil
}

func (s *ScheduledTestService) decorateEffectiveTimeoutProtection(plan *ScheduledTestPlan) {
	if plan == nil {
		return
	}
	plan.EffectiveTimeoutProtectionMode, plan.TimeoutProtectionOverrideReason =
		effectiveScheduledTestTimeoutProtection(plan.TimeoutProtectionMode, s.cfg)
}

func effectiveScheduledTestTimeoutProtection(mode string, cfg *config.Config) (string, string) {
	if mode == ScheduledTestTimeoutProtectionOff {
		return ScheduledTestTimeoutProtectionOff, ""
	}
	if cfg == nil || !cfg.ScheduledTestTimeoutGuard.Enabled {
		return ScheduledTestTimeoutProtectionOff, "kill_switch"
	}
	if mode == ScheduledTestTimeoutProtectionEnforce && cfg.ScheduledTestTimeoutGuard.ForceShadow {
		return ScheduledTestTimeoutProtectionShadow, "force_shadow"
	}
	return mode, ""
}

func normalizeScheduledTestTimeoutProtection(plan *ScheduledTestPlan) error {
	if plan.TimeoutProtectionMode == "" {
		plan.TimeoutProtectionMode = ScheduledTestTimeoutProtectionOff
	}
	if !plan.TimeoutSecondsSet && plan.TimeoutSeconds == 0 {
		plan.TimeoutSeconds = 60
	}
	if !plan.ConsecutiveTimeoutThresholdSet && plan.ConsecutiveTimeoutThreshold == 0 {
		plan.ConsecutiveTimeoutThreshold = 3
	}
	if !plan.RetryDelaysSecondsSet && len(plan.RetryDelaysSeconds) == 0 {
		plan.RetryDelaysSeconds = []int{10, 20}
	}

	switch plan.TimeoutProtectionMode {
	case ScheduledTestTimeoutProtectionOff, ScheduledTestTimeoutProtectionShadow, ScheduledTestTimeoutProtectionEnforce:
	default:
		return fmt.Errorf("invalid timeout_protection_mode: %s", plan.TimeoutProtectionMode)
	}
	if plan.TimeoutProtectionMode != ScheduledTestTimeoutProtectionOff && plan.TimeoutSeconds <= 0 {
		return fmt.Errorf("timeout_seconds must be greater than 0 when timeout protection is enabled")
	}
	if plan.TimeoutSeconds < 0 || plan.TimeoutSeconds > 600 {
		return fmt.Errorf("timeout_seconds must be between 0 and 600")
	}
	if plan.TimeoutProtectionMode != ScheduledTestTimeoutProtectionOff && plan.ConsecutiveTimeoutThreshold <= 0 {
		return fmt.Errorf("consecutive_timeout_threshold must be greater than 0 when timeout protection is enabled")
	}
	if plan.ConsecutiveTimeoutThreshold < 0 || plan.ConsecutiveTimeoutThreshold > 100 {
		return fmt.Errorf("consecutive_timeout_threshold must be between 0 and 100")
	}
	if len(plan.RetryDelaysSeconds) > 5 {
		return fmt.Errorf("retry_delays_seconds supports at most 5 retries")
	}
	for _, delay := range plan.RetryDelaysSeconds {
		if delay < 0 || delay > 300 {
			return fmt.Errorf("retry_delays_seconds values must be between 0 and 300")
		}
	}
	return nil
}
