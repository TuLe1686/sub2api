package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ──────────────────────────────────────────────────────────────────────────
// 账号增强控制（account-enhanced-control 补丁）
//
// 为每个启用账号提供缓存率改写和首字时长改写两块独立配置。
// 配置存放在 Account.extra["enhanced_control"]，通过专用的 GET/PUT 端点
// 读写，只更新 extra 里的这一个 key，不影响其他运行态键。
// ──────────────────────────────────────────────────────────────────────────

// EnhancedControlRequest 是 PUT /admin/accounts/:id/enhanced-control 的请求体。
// 整体替换该账号的 enhanced_control 配置（key 级合并进 extra）。
type EnhancedControlRequest struct {
	CacheRate *service.CacheRateSetting `json:"cache_rate,omitempty"`
	TTFT      *service.TTFTSetting        `json:"ttft,omitempty"`
}

// GetEnhancedControl GET /api/v1/admin/accounts/:id/enhanced-control
// 返回指定账号的增强控制配置。未配置时返回全零值（等价于全关闭）。
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
	cfg := service.ParseEnhancedControl(account.Extra)
	response.Success(c, cfg)
}

// UpdateEnhancedControl PUT /api/v1/admin/accounts/:id/enhanced-control
// 整体替换该账号的 enhanced_control 配置。
// 只更新 extra 里的 "enhanced_control" 这一个 key，其他 extra 字段保留。
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

	// 组装 EnhancedControl 结构
	cfg := service.EnhancedControl{
		CacheRate: service.CacheRateSetting{},
		TTFT:      service.TTFTSetting{},
	}
	if req.CacheRate != nil {
		cfg.CacheRate = *req.CacheRate
	}
	if req.TTFT != nil {
		cfg.TTFT = *req.TTFT
	}

	// 通过 UpdateAccountExtra 做 key 级 JSONB 合并，
	// 只更新 extra["enhanced_control"]，不影响其他键。
	updates := map[string]any{
		service.EnhancedControlKey: cfg,
	}
	if err := h.adminService.UpdateAccountExtra(c.Request.Context(), accountID, updates); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// 读回最新 account 返回
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	cfg = service.ParseEnhancedControl(account.Extra)
	response.Success(c, cfg)
}
