package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
)

const StreamRecoveryBuildMarker = "sub2api_stream_recovery_v1"

// Only observe scalar usage objects; text, tool arguments and images are never
// buffered here. The cap applies to a usage object, not to a whole SSE event.
const streamRecoveryUsageObjectLimit = 16 << 10

type streamRecoveryUsageReader struct {
	io.ReadCloser
	mu                sync.Mutex
	protocol          string
	usage             OpenAIUsage
	known             bool
	depth             int
	inString, escaped bool
	name              string
	nameDepth         int
	pending           int
	capturing         bool
	captureDepth      int
	capture           []byte
	overflow          bool
	parent, key       string
	eventType         string
	value, typeString bool
}

func streamRecoveryAttemptEnabled(ctx context.Context, c *gin.Context) bool {
	if ctx != nil && streamRecoveryUpstreamIsAttached(ctx) {
		return true
	}
	if c == nil {
		return false
	}
	_, ok := c.Writer.(interface{ ProtocolWriter() *StreamRecoveryWriter })
	return ok
}

func observeStreamRecoveryUsage(ctx context.Context, c *gin.Context, resp *http.Response, protocol string) *streamRecoveryUsageReader {
	if !streamRecoveryAttemptEnabled(ctx, c) || resp == nil || resp.Body == nil || !isEventStreamResponse(resp.Header) {
		return nil
	}
	reader := &streamRecoveryUsageReader{ReadCloser: resp.Body, protocol: protocol}
	resp.Body = reader
	return reader
}

func (r *streamRecoveryUsageReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range p[:n] {
		if r.capturing && !r.overflow {
			if len(r.capture) >= streamRecoveryUsageObjectLimit {
				r.overflow = true
				r.capture = nil
			} else {
				r.capture = append(r.capture, b)
			}
		}
		if r.inString {
			if r.escaped {
				r.escaped = false
				r.name = "!"
				continue
			}
			if b == '\\' {
				r.escaped = true
				continue
			}
			if b == '"' {
				r.inString = false
				if r.typeString {
					r.eventType = r.name
					r.typeString = false
				} else if !r.capturing && !r.value {
					r.key = r.name
					r.pending = 1
				}
				r.value = false
			} else if len(r.name) <= 8 || (r.typeString && len(r.name) < 96) {
				r.name += string(b)
			}
			continue
		}
		if r.pending != 0 && !r.capturing {
			if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
				continue
			}
			if r.pending == 1 && b == ':' {
				r.pending = 2
				continue
			}
			if r.pending == 2 {
				r.value = b == '"'
				r.typeString = r.value && r.key == "type" && r.nameDepth == 1
				if b == '{' {
					if r.key == "usage" && (r.nameDepth == 1 || (r.nameDepth == 2 && (r.parent == "response" || r.parent == "message"))) {
						r.capturing = true
						r.captureDepth = r.depth
						r.capture = append(r.capture[:0], b)
						r.overflow = false
					}
					if r.depth == 1 {
						r.parent = r.key
					}
				}
			}
			r.pending = 0
		}
		switch b {
		case '"':
			r.inString = true
			r.name = ""
			r.nameDepth = r.depth
		case '{', '[':
			if r.depth == 0 {
				r.eventType = ""
				r.parent = ""
			}
			r.depth++
		case '}', ']':
			r.depth = max(0, r.depth-1)
			if r.capturing && r.depth == r.captureDepth {
				if !r.overflow {
					var raw map[string]any
					if json.Unmarshal(r.capture, &raw) == nil {
						if usage, ok := ParseStreamRecoveryUsage(r.protocol, raw); ok {
							r.known = true
							if r.protocol != "messages" && openAIStreamEventTypeIsTerminal(r.eventType) {
								if openAIUsageHasTokens(&usage) || !openAIUsageHasTokens(&r.usage) {
									r.usage = usage
								}
							} else {
								mergeOpenAIUsageNonZero(&r.usage, usage)
							}
						}
					}
				}
				r.capturing = false
				r.capture = r.capture[:0]
			}
		}
	}
	return n, err
}

// completeStreamRecoveryAttempt preserves one upstream attempt, never a client
// aggregate. An absent usage report stays absent; no output-based estimates.
func completeStreamRecoveryAttempt(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, observation *streamRecoveryUsageReader, result *OpenAIForwardResult) *OpenAIForwardResult {
	if observation == nil || result == nil {
		return result
	}
	observation.mu.Lock()
	known, usage := observation.known, observation.usage
	observation.mu.Unlock()
	result.StreamRecoveryUsageKnown = known
	if known && !openAIUsageHasTokens(&result.Usage) {
		result.Usage = usage
	}
	if !known {
		accountID := int64(0)
		if account != nil {
			accountID = account.ID
		}
		slog.WarnContext(ctx, "stream_recovery.usage_missing", "patch", StreamRecoveryBuildMarker, "account_id", accountID, "upstream_request_id", result.RequestID)
	}
	stampOpenAIResponsesUpstreamEndpoint(c, result)
	return result
}

type streamRecoveryAttemptKey struct{}

func WithStreamRecoveryBillingAttempt(ctx context.Context, attempt int) context.Context {
	return context.WithValue(ctx, streamRecoveryAttemptKey{}, attempt)
}

func streamRecoveryBillingRequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	attempt, _ := ctx.Value(streamRecoveryAttemptKey{}).(int)
	if attempt <= 0 {
		return ""
	}
	root, _ := ctx.Value(ctxkey.RequestID).(string)
	if strings.TrimSpace(root) == "" {
		root, _ = ctx.Value(ctxkey.ClientRequestID).(string)
	}
	if strings.TrimSpace(root) == "" {
		return ""
	}
	return fmt.Sprintf("stream_recovery:%s:%d", root, attempt)
}

func IsRecoverableStreamFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUpstreamResponseBodyTooLarge) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrOpenAIUpstreamStreamTruncated) {
		return true
	}
	var readError *openAIUpstreamStreamReadError
	if errors.As(err, &readError) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"stream read error", "connection reset", "connection lost", "unexpected eof", "stream usage incomplete: missing terminal", "stream usage incomplete: missing terminal event", "stream data interval timeout", "upstream response stream was interrupted", "upstream response stream ended before completion", "stream disconnected before completion", "stream ended before a terminal event"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) PauseAccountAfterStreamFailure(ctx context.Context, account *Account) error {
	setting := ResolveAccountStreamRecovery(account)
	if s == nil || s.accountRepo == nil || !setting.Enabled || setting.CooldownSeconds <= 0 {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return s.accountRepo.SetTempUnschedulable(writeCtx, account.ID, time.Now().Add(time.Duration(setting.CooldownSeconds)*time.Second), "[stream_recovery] upstream_stream_interrupted")
}

func ParseStreamRecoveryUsage(protocol string, raw map[string]any) (OpenAIUsage, bool) {
	if len(raw) == 0 {
		return OpenAIUsage{}, false
	}
	body, err := json.Marshal(map[string]any{"usage": raw})
	if err != nil {
		return OpenAIUsage{}, false
	}
	usage, ok := extractOpenAIUsageFromJSONBytes(body)
	if protocol == "messages" {
		usage.InputTokens += usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	}
	return usage, ok
}

func StreamRecoveryUsageJSON(protocol string, usage OpenAIUsage) map[string]any {
	switch protocol {
	case "chat":
		return map[string]any{
			"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens,
			"total_tokens":          usage.InputTokens + usage.OutputTokens,
			"prompt_tokens_details": map[string]any{"cached_tokens": usage.CacheReadInputTokens, "cache_creation_tokens": usage.CacheCreationInputTokens},
		}
	case "messages":
		return map[string]any{
			"input_tokens":            max(0, usage.InputTokens-usage.CacheCreationInputTokens-usage.CacheReadInputTokens),
			"output_tokens":           usage.OutputTokens,
			"cache_read_input_tokens": usage.CacheReadInputTokens, "cache_creation_input_tokens": usage.CacheCreationInputTokens,
		}
	default:
		return map[string]any{
			"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens, "total_tokens": usage.InputTokens + usage.OutputTokens,
			"input_tokens_details": map[string]any{"cached_tokens": usage.CacheReadInputTokens, "cache_creation_tokens": usage.CacheCreationInputTokens},
		}
	}
}
