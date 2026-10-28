package middleware

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/httputil"
	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
	"github.com/MACOS-DO/sub4api/internal/pkg/requestmodel"
	"github.com/gin-gonic/gin"
)

// RequestMemoryGuard must run after authentication and before any body parser.
// One closure/manager is shared by all gateway route aliases.
func RequestMemoryGuard(cfg config.GatewayRequestMemoryGuardConfig, maxBodyBytes int64) gin.HandlerFunc {
	var m *requestmemory.Manager
	if cfg.Enabled && cfg.Validate() == nil {
		m, _ = requestmemory.New(cfg.BudgetBytes, cfg.MaxWaiting, time.Duration(cfg.WaitTimeoutMS)*time.Millisecond)
	}
	return requestMemoryGuardWithManager(cfg, maxBodyBytes, m)
}

func requestMemoryGuardWithManager(cfg config.GatewayRequestMemoryGuardConfig, maxBodyBytes int64, m *requestmemory.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		req := c.Request
		if !cfg.Enabled || req == nil || req.Method == http.MethodGet || req.Method == http.MethodHead || req.Body == nil || req.Body == http.NoBody {
			c.Next()
			return
		}
		// Preserve the existing size error even when the reservation would not fit.
		if maxBodyBytes > 0 && req.ContentLength > maxBodyBytes {
			requestMemoryGuardError(c, http.StatusRequestEntityTooLarge, "Request body is too large")
			return
		}
		if m == nil {
			requestMemoryGuardError(c, 503, "Request memory guard is unavailable")
			return
		}
		initial := int64(576)
		if req.ContentLength > 0 {
			if req.ContentLength > (math.MaxInt64-4096)/6 {
				requestMemoryGuardError(c, 503, "Request memory budget exhausted")
				return
			}
			initial = 6*req.ContentLength + 4096
		}
		lease, err := m.Acquire(req.Context(), initial)
		if err != nil {
			requestMemoryGuardError(c, 503, "Request memory budget exhausted")
			return
		}
		defer lease.Release()
		c.Request = req.WithContext(requestmemory.WithLease(req.Context(), lease))
		// Read deadline is scoped only to ingress, never to forwarding or SSE.
		controller := http.NewResponseController(c.Writer)
		receiveDeadline := time.Now().Add(time.Duration(cfg.ReadTimeoutSeconds) * time.Second)
		deadlineErr := controller.SetReadDeadline(receiveDeadline)
		if deadlineErr != nil {
			requestMemoryGuardError(c, 503, "Request body receive deadline unavailable")
			return
		}
		callbackDone := make(chan struct{})
		stop := context.AfterFunc(req.Context(), func() { defer close(callbackDone); _ = controller.SetReadDeadline(time.Now()) })
		body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
		if !stop() {
			<-callbackDone
		}
		if err == nil {
			err = req.Context().Err()
		}
		if err != nil {
			status, message := http.StatusBadRequest, "Failed to read request body"
			var maxErr *http.MaxBytesError
			var netErr net.Error
			switch {
			case errors.Is(err, requestmemory.ErrCapacity):
				status, message = 503, "Request memory budget exhausted"
			case errors.As(err, &maxErr):
				status, message = 413, "Request body is too large"
			case os.IsTimeout(err) || (errors.As(err, &netErr) && netErr.Timeout()) || !time.Now().Before(receiveDeadline):
				status, message = 408, "Request body receive timeout"
			}
			requestMemoryGuardError(c, status, message)
			return
		}
		if err := controller.SetReadDeadline(time.Time{}); err != nil {
			requestMemoryGuardError(c, http.StatusServiceUnavailable, "Request body receive deadline could not be cleared")
			return
		}
		requestmodel.ResetRequestBody(c.Request, body)
		c.Next()
	}
}

func requestMemoryGuardError(c *gin.Context, status int, message string) {
	code, errorType := "invalid_request_error", "invalid_request_error"
	switch status {
	case http.StatusServiceUnavailable:
		c.Header("Retry-After", "1")
		code, errorType = "request_memory_budget_exhausted", "server_error"
		if message != "Request memory budget exhausted" {
			code = "request_memory_guard_unavailable"
		}
	case http.StatusRequestEntityTooLarge:
		code = "request_body_too_large"
	case http.StatusRequestTimeout:
		code = "request_body_timeout"
	}
	c.Set("request_memory_guard_rejected", true)
	path := c.Request.URL.Path
	switch {
	case strings.HasPrefix(path, "/v1beta") || strings.HasPrefix(path, "/antigravity/v1beta"):
		googleStatus := "INVALID_ARGUMENT"
		switch status {
		case http.StatusServiceUnavailable:
			googleStatus = "UNAVAILABLE"
		case http.StatusRequestEntityTooLarge:
			googleStatus = "RESOURCE_EXHAUSTED"
		case http.StatusRequestTimeout:
			googleStatus = "DEADLINE_EXCEEDED"
		}
		c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
			"code": status, "message": message, "status": googleStatus,
			"details": []gin.H{{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": strings.ToUpper(code), "domain": "gateway"}},
		}})
	case strings.Contains(path, "/messages"):
		if status == http.StatusServiceUnavailable {
			errorType = "api_error"
		} else if status == http.StatusRequestEntityTooLarge {
			errorType = "request_too_large"
		}
		c.AbortWithStatusJSON(status, gin.H{"type": "error", "error": gin.H{"type": errorType, "code": code, "message": message}})
	default:
		c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"type": errorType, "code": code, "message": message}})
	}
}
