package admin

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ScheduledTestHandler handles admin scheduled-test-plan management.
type ScheduledTestHandler struct {
	scheduledTestSvc *service.ScheduledTestService
}

// NewScheduledTestHandler creates a new ScheduledTestHandler.
func NewScheduledTestHandler(scheduledTestSvc *service.ScheduledTestService) *ScheduledTestHandler {
	return &ScheduledTestHandler{scheduledTestSvc: scheduledTestSvc}
}

type createScheduledTestPlanRequest struct {
	AccountID                   int64  `json:"account_id" binding:"required"`
	ModelID                     string `json:"model_id"`
	CronExpression              string `json:"cron_expression" binding:"required"`
	Enabled                     *bool  `json:"enabled"`
	MaxResults                  int    `json:"max_results"`
	AutoRecover                 *bool  `json:"auto_recover"`
	TimeoutProtectionMode       string `json:"timeout_protection_mode" binding:"omitempty,oneof=off shadow enforce"`
	TimeoutSeconds              *int   `json:"timeout_seconds"`
	ConsecutiveTimeoutThreshold *int   `json:"consecutive_timeout_threshold"`
	RetryDelaysSeconds          *[]int `json:"retry_delays_seconds"`
}

type updateScheduledTestPlanRequest struct {
	ModelID                     string  `json:"model_id"`
	CronExpression              string  `json:"cron_expression"`
	Enabled                     *bool   `json:"enabled"`
	MaxResults                  int     `json:"max_results"`
	AutoRecover                 *bool   `json:"auto_recover"`
	TimeoutProtectionMode       *string `json:"timeout_protection_mode" binding:"omitempty,oneof=off shadow enforce"`
	TimeoutSeconds              *int    `json:"timeout_seconds"`
	ConsecutiveTimeoutThreshold *int    `json:"consecutive_timeout_threshold"`
	RetryDelaysSeconds          *[]int  `json:"retry_delays_seconds"`
	RestoreOwnedAccount         bool    `json:"restore_owned_account"`
}

// ListByAccount GET /admin/accounts/:id/scheduled-test-plans
func (h *ScheduledTestHandler) ListByAccount(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid account id")
		return
	}

	plans, err := h.scheduledTestSvc.ListPlansByAccount(c.Request.Context(), accountID)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, plans)
}

// Create POST /admin/scheduled-test-plans
func (h *ScheduledTestHandler) Create(c *gin.Context) {
	var req createScheduledTestPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	plan := &service.ScheduledTestPlan{
		AccountID:      req.AccountID,
		ModelID:        req.ModelID,
		CronExpression: req.CronExpression,
		Enabled:        true,
		MaxResults:     req.MaxResults,
	}
	if req.Enabled != nil {
		plan.Enabled = *req.Enabled
	}
	if req.AutoRecover != nil {
		plan.AutoRecover = *req.AutoRecover
	}
	plan.TimeoutProtectionMode = req.TimeoutProtectionMode
	if req.TimeoutSeconds != nil {
		plan.TimeoutSeconds = *req.TimeoutSeconds
		plan.TimeoutSecondsSet = true
	}
	if req.ConsecutiveTimeoutThreshold != nil {
		plan.ConsecutiveTimeoutThreshold = *req.ConsecutiveTimeoutThreshold
		plan.ConsecutiveTimeoutThresholdSet = true
	}
	if req.RetryDelaysSeconds != nil {
		plan.RetryDelaysSeconds = *req.RetryDelaysSeconds
		plan.RetryDelaysSecondsSet = true
	}

	created, err := h.scheduledTestSvc.CreatePlan(c.Request.Context(), plan)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, created)
}

// Update PUT /admin/scheduled-test-plans/:id
func (h *ScheduledTestHandler) Update(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid plan id")
		return
	}

	existing, err := h.scheduledTestSvc.GetPlan(c.Request.Context(), planID)
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}

	var req updateScheduledTestPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	if req.ModelID != "" {
		existing.ModelID = req.ModelID
	}
	if req.CronExpression != "" {
		existing.CronExpression = req.CronExpression
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.MaxResults > 0 {
		existing.MaxResults = req.MaxResults
	}
	if req.AutoRecover != nil {
		existing.AutoRecover = *req.AutoRecover
	}
	if req.TimeoutProtectionMode != nil {
		existing.TimeoutProtectionMode = *req.TimeoutProtectionMode
	}
	if req.TimeoutSeconds != nil {
		existing.TimeoutSeconds = *req.TimeoutSeconds
		existing.TimeoutSecondsSet = true
	}
	if req.ConsecutiveTimeoutThreshold != nil {
		existing.ConsecutiveTimeoutThreshold = *req.ConsecutiveTimeoutThreshold
		existing.ConsecutiveTimeoutThresholdSet = true
	}
	if req.RetryDelaysSeconds != nil {
		existing.RetryDelaysSeconds = *req.RetryDelaysSeconds
		existing.RetryDelaysSecondsSet = true
	}

	restoreOwnedAccount := req.RestoreOwnedAccount
	if queryRestore, parseErr := strconv.ParseBool(c.Query("restore_owned_account")); parseErr == nil {
		restoreOwnedAccount = restoreOwnedAccount || queryRestore
	}
	updated, err := h.scheduledTestSvc.UpdatePlan(c.Request.Context(), existing, restoreOwnedAccount)
	if errors.Is(err, service.ErrScheduledTestOwnershipConflict) ||
		errors.Is(err, service.ErrScheduledTestOwnershipReleaseFailed) {
		response.Error(c, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, updated)
}

// Delete DELETE /admin/scheduled-test-plans/:id
func (h *ScheduledTestHandler) Delete(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid plan id")
		return
	}

	restoreOwnedAccount, _ := strconv.ParseBool(c.Query("restore_owned_account"))
	if err := h.scheduledTestSvc.DeletePlan(c.Request.Context(), planID, restoreOwnedAccount); err != nil {
		if errors.Is(err, service.ErrScheduledTestOwnershipConflict) ||
			errors.Is(err, service.ErrScheduledTestOwnershipReleaseFailed) {
			response.Error(c, http.StatusConflict, err.Error())
			return
		}
		response.InternalError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// ListResults GET /admin/scheduled-test-plans/:id/results
func (h *ScheduledTestHandler) ListResults(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid plan id")
		return
	}

	limit := 50
	if l, err := strconv.Atoi(c.Query("limit")); err == nil && l > 0 {
		limit = l
	}

	results, err := h.scheduledTestSvc.ListResults(c.Request.Context(), planID, limit)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, results)
}
