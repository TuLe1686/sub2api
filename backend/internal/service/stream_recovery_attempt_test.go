package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestStreamRecoveryBillingAttemptsHaveStableSeparateKeys(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "request-root")
	first := WithStreamRecoveryBillingAttempt(ctx, 1)
	second := WithStreamRecoveryBillingAttempt(ctx, 2)
	require.Equal(t, "stream_recovery:request-root:1", resolveUsageBillingRequestID(first, "upstream-1"))
	require.Equal(t, resolveUsageBillingRequestID(first, "upstream-1"), resolveUsageBillingRequestID(first, "upstream-1"))
	require.NotEqual(t, resolveUsageBillingRequestID(first, "upstream-1"), resolveUsageBillingRequestID(second, "upstream-2"))
	require.Equal(t, "local:request-root", resolveUsageBillingRequestID(ctx, "upstream-1"))
}

func TestStreamRecoveryFaultsExcludeClientCancellation(t *testing.T) {
	require.True(t, IsRecoverableStreamFailure(io.ErrUnexpectedEOF))
	require.True(t, IsRecoverableStreamFailure(errors.New("stream read error: connection reset")))
	require.False(t, IsRecoverableStreamFailure(context.Canceled))
	require.False(t, IsRecoverableStreamFailure(context.DeadlineExceeded))
	require.False(t, IsRecoverableStreamFailure(ErrUpstreamResponseBodyTooLarge))
	require.False(t, IsRecoverableStreamFailure(errors.New("invalid model")))
}

func TestStreamRecoveryMessagesUsagePreservesCacheBuckets(t *testing.T) {
	usage, ok := ParseStreamRecoveryUsage("messages", map[string]any{
		"input_tokens": 10, "output_tokens": 3,
		"cache_read_input_tokens": 4, "cache_creation_input_tokens": 2,
	})
	require.True(t, ok)
	require.Equal(t, 16, usage.InputTokens)
	require.Equal(t, 4, usage.CacheReadInputTokens)
	require.Equal(t, 2, usage.CacheCreationInputTokens)
	wire := StreamRecoveryUsageJSON("messages", usage)
	require.Equal(t, 10, wire["input_tokens"])
}

func TestStreamRecoveryDurableBillingIDWinsOverAttempt(t *testing.T) {
	ctx := WithStreamRecoveryBillingAttempt(context.WithValue(context.Background(), ctxkey.RequestID, "root"), 2)
	for _, id := range []string{"web_search:one", "grok-video:one", "grok_audio:one", "grok_realtime:one"} {
		t.Run(id, func(t *testing.T) {
			require.Equal(t, id, resolveUsageBillingRequestID(ctx, id))
		})
	}
}

func TestStreamRecoveryResponsesUsageRoundTripPreservesWriteAndRead(t *testing.T) {
	want := OpenAIUsage{InputTokens: 19, OutputTokens: 7, CacheReadInputTokens: 5, CacheCreationInputTokens: 3}
	for _, protocol := range []string{"responses", "messages", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			got, ok := ParseStreamRecoveryUsage(protocol, StreamRecoveryUsageJSON(protocol, want))
			require.True(t, ok)
			require.Equal(t, want, got)
		})
	}
}

func TestStreamRecoveryUsageObservationDistinguishesUnknownZeroAndFinalBuckets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, wire string
		known      bool
		want       OpenAIUsage
	}{
		{name: "absent", wire: `data: {"type":"response.output_text.delta","delta":"no usage reported"}` + "\n\n"},
		{name: "zero", wire: `data: {"type":"response.in_progress","response":{"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n", known: true},
		{name: "final_clears_progressive_cache", wire: `data: {"type":"response.in_progress","response":{"usage":{"input_tokens":19,"output_tokens":2,"input_tokens_details":{"cached_tokens":8,"cache_creation_tokens":3}}}}` + "\n\n" + `data: {"type":"response.completed","response":{"usage":{"input_tokens":20,"output_tokens":7,"input_tokens_details":{"cached_tokens":0,"cache_creation_tokens":0}}}}` + "\n\n", known: true, want: OpenAIUsage{InputTokens: 20, OutputTokens: 7}},
		{name: "usage_inside_tool_is_not_usage", wire: `data: {"type":"response.output_item.added","item":{"type":"function_call","usage":{"input_tokens":999}}}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx := WithStreamRecoveryUpstreamContext(context.Background())
			resp := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.wire))}
			observation := observeStreamRecoveryUsage(ctx, c, resp, "responses")
			_, err := io.Copy(io.Discard, resp.Body)
			require.NoError(t, err)
			result := completeStreamRecoveryAttempt(ctx, c, &Account{ID: 1}, resp, observation, &OpenAIForwardResult{})
			require.Equal(t, tc.known, result.StreamRecoveryUsageKnown)
			require.Equal(t, tc.want, result.Usage)
		})
	}
}

func TestStreamRecoveryHTTPForwardPreservesKnownAttemptUsageAndMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, marked := range []bool{false, true} {
			t.Run(path+"/marked="+map[bool]string{false: "false", true: "true"}[marked], func(t *testing.T) {
				body := []byte(`{"model":"gpt-5.4","stream":true,"service_tier":"priority","reasoning":{"effort":"high"},"input":"hello"}`)
				url := "/v1/responses"
				if path == "chat" {
					url = "/v1/chat/completions"
					body = []byte(`{"model":"gpt-5.4","stream":true,"service_tier":"priority","reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`)
				} else if path == "messages" {
					url = "/v1/messages"
					body = []byte(`{"model":"gpt-5.4","max_tokens":64,"stream":true,"output_config":{"effort":"high"},"thinking":{"type":"enabled","budget_tokens":8192},"messages":[{"role":"user","content":"hello"}]}`)
				}
				sse := strings.Join([]string{
					`data: {"type":"response.created","response":{"id":"resp-attempt","model":"gpt-5.4-mapped","service_tier":"priority","usage":{"input_tokens":19,"output_tokens":7,"input_tokens_details":{"cached_tokens":5,"cache_creation_tokens":3}}}}`,
					"", `data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}`, "", "",
				}, "\n")
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"request-attempt"}},
					Body:       io.NopCloser(io.MultiReader(strings.NewReader(sse), passthroughErrReadCloser{err: io.ErrUnexpectedEOF})),
				}}
				cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
				account := &Account{ID: 37, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
					Credentials: map[string]any{"api_key": "test-key", "base_url": "http://upstream.example", "model_mapping": map[string]any{"gpt-5.4": "gpt-5.4-mapped"}},
					Extra:       map[string]any{"openai_responses_supported": true, "openai_passthrough": path == "passthrough"},
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				ctx := context.WithValue(context.Background(), ctxkey.RequestID, "root-request")
				if marked {
					ctx = WithStreamRecoveryUpstreamContext(ctx)
				}
				c.Request = httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body)).WithContext(ctx)
				var result *OpenAIForwardResult
				var err error
				switch path {
				case "chat":
					result, err = svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
				case "messages":
					result, err = svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
				default:
					result, err = svc.Forward(ctx, c, account, body)
				}
				require.Error(t, err)
				require.ErrorIs(t, err, io.ErrUnexpectedEOF, "keep the original upstream failure")
				if path == "chat" {
					var readError *openAIUpstreamStreamReadError
					require.ErrorAs(t, err, &readError, "keep the original typed Chat stream error")
				}
				if !marked {
					if path == "responses" || path == "passthrough" {
						require.Nil(t, result, "unmarked legacy path still discards result")
					} else {
						require.NotNil(t, result)
						require.Nil(t, result.ReasoningEffort)
						require.Nil(t, result.ServiceTier)
					}
					return
				}
				require.NotNil(t, result)
				require.Equal(t, OpenAIUsage{InputTokens: 19, OutputTokens: 7, CacheReadInputTokens: 5, CacheCreationInputTokens: 3}, result.Usage)
				require.Equal(t, "gpt-5.4", result.Model)
				if path != "passthrough" {
					require.Equal(t, "gpt-5.4-mapped", result.BillingModel)
					require.Equal(t, "gpt-5.4-mapped", result.UpstreamModel)
				}
				require.Equal(t, "request-attempt", result.RequestID)
				require.Equal(t, "request-attempt", result.UpstreamHeaders.Get("X-Request-Id"))
				require.Equal(t, "/v1/responses", result.UpstreamEndpoint)
				require.NotNil(t, result.ReasoningEffort)
				require.Equal(t, "high", *result.ReasoningEffort)
				if path != "messages" {
					require.NotNil(t, result.ServiceTier)
					require.Equal(t, "priority", *result.ServiceTier)
				}
				require.True(t, result.StreamRecoveryUsageKnown)
				require.GreaterOrEqual(t, result.Duration, time.Duration(0))
				require.Less(t, result.Duration, time.Minute)
				require.True(t, result.Stream)
				require.False(t, result.OpenAIWSMode)
			})
		}
	}
}
