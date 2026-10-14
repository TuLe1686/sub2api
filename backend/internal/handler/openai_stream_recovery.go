package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

type openAIStreamRecovery struct {
	c              *gin.Context
	protocol       string
	original       []byte
	baseContext    context.Context
	eligible       bool
	writer         *service.StreamRecoveryWriter
	transport      *service.StreamRecoveryTransport
	accountEnabled bool
	pending        []byte
	recoveries     int
	attempt        int
	maxRecoveries  int
	maxWait        time.Duration
	waited         time.Duration
	finished       bool
	usage          service.OpenAIUsage
	heartbeat      time.Duration
}

func newOpenAIStreamRecovery(c *gin.Context, protocol string, original []byte, eligible bool) *openAIStreamRecovery {
	if root, _ := c.Request.Context().Value(ctxkey.RequestID).(string); root == "" {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestID, uuid.NewString()))
	}
	return &openAIStreamRecovery{
		c: c, protocol: protocol, original: original, eligible: eligible,
		baseContext: c.Request.Context(), maxRecoveries: 3,
		maxWait: 180 * time.Second, heartbeat: 10 * time.Second,
	}
}

func (r *openAIStreamRecovery) configure(cfg *config.Config) {
	if cfg == nil {
		return
	}
	setting := cfg.Gateway.StreamRecovery
	if setting.MaxRecoveries > 0 {
		r.maxRecoveries = setting.MaxRecoveries
	}
	if setting.WaitBudgetSeconds > 0 {
		r.maxWait = time.Duration(setting.WaitBudgetSeconds) * time.Second
	}
	if setting.HeartbeatIntervalSeconds > 0 {
		r.heartbeat = time.Duration(setting.HeartbeatIntervalSeconds) * time.Second
	}
}

func (r *openAIStreamRecovery) begin(account *service.Account) bool {
	if r == nil || !r.eligible || r.finished {
		return false
	}
	setting := service.ResolveAccountStreamRecovery(account)
	r.accountEnabled = setting.Enabled
	if r.writer == nil {
		if !setting.Enabled {
			return false
		}
		r.transport = service.NewStreamRecoveryTransport(r.c.Writer, r.protocol, requestmemory.FromContext(r.baseContext))
		r.transport.SetHeartbeatInterval(r.heartbeat)
		r.writer = r.transport.ProtocolWriter()
		if r.protocol == "responses" {
			r.writer.SetLogicalResponseID("resp_sr_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
		}
		r.c.Writer = r.transport
	}
	r.attempt++
	r.c.Set(service.ResponseCommittedKey, false)
	r.transport.BeginAttempt()
	if r.recoveries > 0 {
		r.c.Request = r.c.Request.WithContext(r.transport.WaitContext(r.baseContext))
	} else {
		r.c.Request = r.c.Request.WithContext(service.WithStreamRecoveryUpstreamContext(r.baseContext))
	}
	return r.writer.Err() == nil
}

func (r *openAIStreamRecovery) body(fallback []byte) ([]byte, error) {
	if r == nil || r.writer == nil || r.pending == nil {
		return fallback, nil
	}
	return r.pending, nil
}

func (r *openAIStreamRecovery) afterAttempt(svc *service.OpenAIGatewayService, account *service.Account, result *service.OpenAIForwardResult, err error, failed map[int64]struct{}) (retry, terminal bool) {
	if r == nil || r.writer == nil {
		return false, false
	}
	r.transport.StopWaiting()
	r.c.Request = r.c.Request.WithContext(r.baseContext)
	r.waited = max(r.waited, r.transport.Waited())
	attemptUsage := r.writer.AttemptUsage()
	if result == nil && len(attemptUsage) > 0 {
		if usage, ok := service.ParseStreamRecoveryUsage(r.protocol, attemptUsage); ok {
			result = &service.OpenAIForwardResult{Stream: true, Usage: usage, StreamRecoveryUsageKnown: true}
		}
	}
	if result != nil {
		if !result.StreamRecoveryUsageKnown && result.Usage == (service.OpenAIUsage{}) {
			if usage, ok := service.ParseStreamRecoveryUsage(r.protocol, attemptUsage); ok {
				result.Usage = usage
				result.StreamRecoveryUsageKnown = true
			}
		}
		r.usage.InputTokens += result.Usage.InputTokens
		r.usage.OutputTokens += result.Usage.OutputTokens
		r.usage.CacheReadInputTokens += result.Usage.CacheReadInputTokens
		r.usage.CacheCreationInputTokens += result.Usage.CacheCreationInputTokens
	}
	code, message, interrupted := r.writer.Interrupted()
	var failoverErr *service.UpstreamFailoverError
	if errors.As(err, &failoverErr) && !r.writer.HasOutput() && !interrupted && r.writer.Err() == nil {
		return false, false
	}
	if err == nil && !interrupted && r.writer.Err() == nil && r.writer.TerminalSeen() {
		r.writer.SetUsage(service.StreamRecoveryUsageJSON(r.protocol, r.usage))
		if svc != nil && r.protocol == "responses" {
			output, marshalErr := json.Marshal(r.writer.OutputSnapshot())
			if marshalErr == nil {
				bindCtx, cancel := context.WithTimeout(context.WithoutCancel(r.baseContext), 2*time.Second)
				bindErr := svc.BindStreamRecoveryContext(bindCtx, r.c, r.writer.LogicalResponseID(), r.writer.UpstreamResponseID(), account, r.original, output)
				cancel()
				if bindErr != nil {
					logger.L().Warn("stream_recovery.context_bind_failed", zap.Int64("account_id", account.ID), zap.Error(bindErr))
				}
			}
		}
		if finishErr := r.writer.FinishSuccess(); finishErr == nil {
			r.finished = true
			return false, false
		} else {
			err = finishErr
		}
	}
	if err == nil && !interrupted && !r.writer.TerminalSeen() {
		err = io.ErrUnexpectedEOF
	}
	if code == "" {
		code = "upstream_stream_read_error"
	}
	if message == "" {
		message = "Upstream response stream was interrupted"
	}
	if r.baseContext.Err() != nil || (result != nil && result.ClientDisconnect) {
		r.finished = true
		return false, true
	}
	canRecover := service.IsRecoverableStreamFailure(err) || errors.Is(r.writer.Err(), service.ErrStreamRecoveryReplay)
	if interrupted {
		switch code {
		case "upstream_stream_read_error", "upstream_stream_truncated", "upstream_http2_stream_error", "upstream_stream_interrupted", "stream_timeout":
			canRecover = true
		}
	}
	if r.writer.Err() != nil && !errors.Is(r.writer.Err(), service.ErrStreamRecoveryReplay) {
		canRecover = false
	}
	if interrupted && r.accountEnabled && r.writer.HasCommittedTools() && !r.writer.UnsafeReplay() {
		r.writer.SetUsage(service.StreamRecoveryUsageJSON(r.protocol, r.usage))
		if handoffErr := r.writer.CompleteToolHandoff(); handoffErr == nil {
			r.finished = true
			return false, true
		}
	}
	if canRecover && r.accountEnabled {
		if svc != nil {
			if pauseErr := svc.PauseAccountAfterStreamFailure(r.baseContext, account); pauseErr != nil {
				logger.L().Warn("stream_recovery.account_pause_failed", zap.Int64("account_id", account.ID), zap.Error(pauseErr))
			}
		}
		if r.writer.HasCommittedTools() && !r.writer.UnsafeReplay() {
			r.writer.SetUsage(service.StreamRecoveryUsageJSON(r.protocol, r.usage))
			if handoffErr := r.writer.CompleteToolHandoff(); handoffErr == nil {
				r.finished = true
				return false, true
			}
		}
		if !r.writer.UnsafeReplay() && r.recoveries < r.maxRecoveries && r.waited < r.maxWait {
			original := r.original
			var buildErr error
			if svc != nil && r.protocol == "responses" && gjson.GetBytes(r.original, "previous_response_id").String() != "" {
				original, buildErr = svc.PrepareStreamRecoveryContext(r.baseContext, r.c, r.original, -1)
			}
			if buildErr == nil {
				if lease := requestmemory.FromContext(r.baseContext); lease != nil {
					buildErr = lease.Charge(int64(len(original))*2 + int64(len(r.writer.EmittedText()))*3 + 32768)
				}
			}
			var pending []byte
			if buildErr == nil {
				pending, buildErr = service.BuildStreamRecoveryRequest(original, r.protocol, r.writer.EmittedText())
			}
			if buildErr == nil {
				r.pending = pending
				r.recoveries++
				failed[account.ID] = struct{}{}
				r.c.Request.Header.Del("X-Codex-Turn-State")
				waitCtx := r.transport.StartWaiting(r.baseContext, r.maxWait)
				r.c.Request = r.c.Request.WithContext(waitCtx)
				if waitErr := r.waitBeforeRetry(waitCtx, result); waitErr == nil {
					logger.L().Info("stream_recovery.retry", zap.String("patch", service.StreamRecoveryBuildMarker), zap.Int64("account_id", account.ID), zap.Int("recovery", r.recoveries), zap.String("protocol", r.protocol))
					return true, false
				}
				code, message = "stream_recovery_wait_exhausted", "Upstream recovery waiting budget exhausted"
			} else {
				code, message = "stream_recovery_context_unavailable", "The upstream response could not be safely resumed"
			}
		}
	}
	_ = r.writer.FinishFailure(code, message)
	r.finished = true
	return false, true
}

func (r *openAIStreamRecovery) waitBeforeRetry(ctx context.Context, result *service.OpenAIForwardResult) error {
	delay := time.Duration(0)
	if r.recoveries > 1 {
		delay = time.Duration(1<<min(r.recoveries-2, 3)) * time.Second
	}
	if result != nil {
		value := result.UpstreamHeaders.Get("Retry-After")
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			delay = max(delay, time.Duration(n)*time.Second)
		} else if until, err := http.ParseTime(value); err == nil {
			delay = max(delay, time.Until(until))
		}
	}
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func (r *openAIStreamRecovery) finishOnReturn() {
	if r == nil || r.writer == nil {
		return
	}
	defer r.transport.Close()
	r.transport.StopWaiting()
	r.c.Request = r.c.Request.WithContext(r.baseContext)
	if r.finished || r.baseContext.Err() != nil {
		return
	}
	_ = r.writer.FinishFailure("stream_recovery_exhausted", "No compatible upstream could complete the response")
	r.finished = true
}

func (r *openAIStreamRecovery) billingContext(ctx context.Context) context.Context {
	if r == nil || r.writer == nil {
		return ctx
	}
	return service.WithStreamRecoveryBillingAttempt(ctx, r.attempt)
}

func (r *openAIStreamRecovery) result(account *service.Account, result *service.OpenAIForwardResult, model string) *service.OpenAIForwardResult {
	if r == nil || r.writer == nil {
		return result
	}
	if result == nil {
		usage, ok := service.ParseStreamRecoveryUsage(r.protocol, r.writer.AttemptUsage())
		if !ok {
			return nil
		}
		result = &service.OpenAIForwardResult{Usage: usage, Model: model, Stream: true, UpstreamModel: account.GetMappedModel(model), StreamRecoveryUsageKnown: true}
	}
	if result.Model == "" {
		result.Model = model
	}
	return result
}

func streamRecoveryRequestEligible(body []byte) bool {
	if n := gjson.GetBytes(body, "n"); n.Exists() && n.Int() != 1 {
		return false
	}
	if gjson.GetBytes(body, "compaction_trigger").Exists() {
		return false
	}
	for _, mode := range gjson.GetBytes(body, "modalities").Array() {
		if mode.String() != "text" {
			return false
		}
	}
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		t := strings.ToLower(tool.Get("type").String())
		if strings.Contains(t, "image") || strings.Contains(t, "video") || strings.Contains(t, "audio") {
			return false
		}
	}
	return true
}
