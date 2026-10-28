package admin

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

// EnhancedControlRequest 保持前三项整体替换；缺失的新字段保留已存配置。
type EnhancedControlRequest struct {
	CacheRate       *service.CacheRateSetting       `json:"cache_rate,omitempty"`
	TTFT            *service.TTFTSetting            `json:"ttft,omitempty"`
	ReasoningEffort *service.ReasoningEffortSetting `json:"reasoning_effort,omitempty"`
	AccountStats    *service.AccountStatsSetting    `json:"account_stats,omitempty"`
	StreamRecovery  json.RawMessage                 `json:"stream_recovery"`
}

// GetEnhancedControl GET /api/v1/admin/accounts/:id/enhanced-control
func (h *AccountHandler) GetEnhancedControl(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, service.ParseEnhancedControl(account.Extra))
}

// UpdateEnhancedControl PUT /api/v1/admin/accounts/:id/enhanced-control
// 只合并 extra["enhanced_control"]，其余运行态键由 UpdateAccountExtra 保留。
func (h *AccountHandler) UpdateEnhancedControl(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req EnhancedControlRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	cfg := service.EnhancedControl{
		ReasoningEffort: service.ReasoningEffortSetting{Mode: service.ReasoningEffortModeOff},
		StreamRecovery:  service.StreamRecoverySetting{CooldownSeconds: 600},
	}
	if req.CacheRate != nil {
		cfg.CacheRate = *req.CacheRate
	}
	if req.TTFT != nil {
		cfg.TTFT = *req.TTFT
	}
	if req.ReasoningEffort != nil {
		cfg.ReasoningEffort = *req.ReasoningEffort
	}
	var stored *service.EnhancedControl
	if req.AccountStats == nil || len(req.StreamRecovery) == 0 {
		account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		existing := service.ParseEnhancedControl(account.Extra)
		stored = &existing
	}
	if req.AccountStats != nil {
		cfg.AccountStats = service.AccountStatsSetting{Mode: service.NormalizeAccountStatsMode(req.AccountStats.Mode)}
	} else {
		cfg.AccountStats = stored.AccountStats
	}

	if len(req.StreamRecovery) == 0 {
		cfg.StreamRecovery = stored.StreamRecovery
	} else {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(req.StreamRecovery, &fields); err != nil || fields == nil {
			response.BadRequest(c, "stream_recovery must be an object")
			return
		}
		if raw, provided := fields["enabled"]; provided {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &cfg.StreamRecovery.Enabled) != nil {
				response.BadRequest(c, "stream_recovery.enabled must be a boolean")
				return
			}
		}
		if raw, provided := fields["cooldown_seconds"]; provided {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &cfg.StreamRecovery.CooldownSeconds) != nil || cfg.StreamRecovery.CooldownSeconds < 0 || cfg.StreamRecovery.CooldownSeconds > 86400 {
				response.BadRequest(c, "stream_recovery.cooldown_seconds must be an integer between 0 and 86400")
				return
			}
		}
	}

	if err := h.adminService.UpdateAccountExtra(c.Request.Context(), accountID, map[string]any{service.EnhancedControlKey: cfg}); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, service.ParseEnhancedControl(account.Extra))
}
