package service

import (
	"context"
	"errors"
	"time"
)

const (
	ScheduledTestTimeoutProtectionOff     = "off"
	ScheduledTestTimeoutProtectionShadow  = "shadow"
	ScheduledTestTimeoutProtectionEnforce = "enforce"

	ScheduledTestStatusSuccess = "success"
	ScheduledTestStatusFailed  = "failed"

	ScheduledTestClassificationSuccess = "success"
	ScheduledTestClassificationTimeout = "timeout"
	ScheduledTestClassificationFailure = "failure"

	ScheduledTestRunModeNormal   = "normal"
	ScheduledTestRunModeRecovery = "recovery"

	ScheduledTestProtectionActionNone            = "none"
	ScheduledTestProtectionActionWouldInactivate = "would_inactivate"
	ScheduledTestProtectionActionInactivated     = "inactivated"
	ScheduledTestProtectionActionRecovered       = "recovered"
	ScheduledTestProtectionActionBlocked         = "blocked"

	ScheduledTestAccountStatusInactive = "inactive"
)

// ScheduledTestPlan represents a scheduled test plan domain model.
type ScheduledTestPlan struct {
	ID                              int64      `json:"id"`
	AccountID                       int64      `json:"account_id"`
	ModelID                         string     `json:"model_id"`
	CronExpression                  string     `json:"cron_expression"`
	Enabled                         bool       `json:"enabled"`
	MaxResults                      int        `json:"max_results"`
	AutoRecover                     bool       `json:"auto_recover"`
	TimeoutProtectionMode           string     `json:"timeout_protection_mode"`
	EffectiveTimeoutProtectionMode  string     `json:"effective_timeout_protection_mode"`
	TimeoutProtectionOverrideReason string     `json:"timeout_protection_override_reason,omitempty"`
	TimeoutSeconds                  int        `json:"timeout_seconds"`
	TimeoutSecondsSet               bool       `json:"-"`
	ConsecutiveTimeoutThreshold     int        `json:"consecutive_timeout_threshold"`
	ConsecutiveTimeoutThresholdSet  bool       `json:"-"`
	RetryDelaysSeconds              []int      `json:"retry_delays_seconds"`
	RetryDelaysSecondsSet           bool       `json:"-"`
	ConsecutiveTimeoutCount         int        `json:"consecutive_timeout_count"`
	PlanVersion                     int64      `json:"plan_version"`
	ExecutionID                     string     `json:"execution_id,omitempty"`
	ClaimToken                      string     `json:"-"`
	ClaimPlanVersion                int64      `json:"-"`
	ClaimExpiresAt                  *time.Time `json:"claim_expires_at,omitempty"`
	OwnsInactiveAccount             bool       `json:"owns_inactive_account"`
	LastRunAt                       *time.Time `json:"last_run_at"`
	NextRunAt                       *time.Time `json:"next_run_at"`
	CreatedAt                       time.Time  `json:"created_at"`
	UpdatedAt                       time.Time  `json:"updated_at"`
}

// ScheduledTestResult represents a single test execution result.
type ScheduledTestResult struct {
	ID               int64     `json:"id"`
	PlanID           int64     `json:"plan_id"`
	ExecutionID      string    `json:"execution_id,omitempty"`
	Status           string    `json:"status"`
	RunMode          string    `json:"run_mode"`
	AttemptCount     int       `json:"attempt_count"`
	Classification   string    `json:"classification"`
	ProtectionAction string    `json:"protection_action"`
	BlockedReason    string    `json:"blocked_reason,omitempty"`
	ResponseText     string    `json:"response_text"`
	ErrorMessage     string    `json:"error_message"`
	LatencyMs        int64     `json:"latency_ms"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	CreatedAt        time.Time `json:"created_at"`
}

// ScheduledTestAccountTester is the account-test boundary used by the scheduler.
type ScheduledTestAccountTester interface {
	RunTestBackground(ctx context.Context, accountID int64, modelID string) (*ScheduledTestResult, error)
}

var (
	ErrScheduledTestClaimLost               = errors.New("scheduled test claim lost")
	ErrScheduledTestOwnershipConflict       = errors.New("scheduled test plan owns an inactive account")
	ErrScheduledTestOwnershipReleaseFailed  = errors.New("scheduled test owned account could not be restored")
	ErrScheduledTestAccountMutationConflict = errors.New("scheduled test account state changed during protection mutation")
)

// ScheduledTestFinalizeInput describes one fenced logical execution finalization.
type ScheduledTestFinalizeInput struct {
	Plan                *ScheduledTestPlan
	ClaimToken          string
	Result              *ScheduledTestResult
	NextRunAt           time.Time
	Now                 time.Time
	CircuitWindow       time.Duration
	CircuitMinSamples   int
	CircuitTimeoutRatio float64
	DisableBudget       int
}

// ScheduledTestPlanRepository defines the data access interface for test plans.
type ScheduledTestPlanRepository interface {
	Create(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error)
	GetByID(ctx context.Context, id int64) (*ScheduledTestPlan, error)
	ListByAccountID(ctx context.Context, accountID int64) ([]*ScheduledTestPlan, error)
	ListDue(ctx context.Context, now time.Time) ([]*ScheduledTestPlan, error)
	ClaimDue(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]*ScheduledTestPlan, error)
	RenewClaim(ctx context.Context, planID int64, claimToken string, planVersion int64, lease time.Duration) (time.Time, bool, error)
	HasAccountOwnership(ctx context.Context, accountID int64) (bool, error)
	FinalizeClaim(ctx context.Context, input ScheduledTestFinalizeInput) (*ScheduledTestResult, error)
	HealExpiredClaims(ctx context.Context, cutoff time.Time, limit int) (int, error)
	Update(ctx context.Context, plan *ScheduledTestPlan, restoreOwnedAccount ...bool) (*ScheduledTestPlan, error)
	Delete(ctx context.Context, id int64, restoreOwnedAccount ...bool) error
	UpdateAfterRun(ctx context.Context, id int64, lastRunAt time.Time, nextRunAt time.Time) error
}

// ScheduledTestResultRepository defines the data access interface for test results.
type ScheduledTestResultRepository interface {
	Create(ctx context.Context, result *ScheduledTestResult) (*ScheduledTestResult, error)
	ListByPlanID(ctx context.Context, planID int64, limit int) ([]*ScheduledTestResult, error)
	PruneOldResults(ctx context.Context, planID int64, keepCount int) error
}
