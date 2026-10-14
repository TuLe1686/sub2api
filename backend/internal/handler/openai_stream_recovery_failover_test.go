package handler

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai_compat"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Scope: real loopback HTTP Forward + recovery coordinator + original failover
// gate/error-writer seams. Selection/exclusion is simulated, NOT the whole
// authenticated OpenAIGatewayHandler, Redis scheduler, or billing pipeline.
type streamRecoveryFailoverHTTP struct {
	client *http.Client
	ids    []int64
}

func (u *streamRecoveryFailoverHTTP) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.ids = append(u.ids, id)
	return u.client.Do(req)
}

func (u *streamRecoveryFailoverHTTP) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func streamRecoveryFailoverService(t *testing.T) (*service.OpenAIGatewayService, *streamRecoveryFailoverHTTP) {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return nil, fmt.Errorf("non-loopback request forbidden: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	upstream := &streamRecoveryFailoverHTTP{client: &http.Client{Transport: transport, Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		nil, nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	return svc, upstream
}

func streamRecoveryFailoverAccount(id int64, url, upstreamProtocol string, passthrough bool) *service.Account {
	mode := openai_compat.ResponsesSupportModeForceResponses
	if upstreamProtocol == "chat" {
		mode = openai_compat.ResponsesSupportModeForceChatCompletions
	}
	return &service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "synthetic-loopback-key", "base_url": url},
		Extra: map[string]any{"openai_passthrough": passthrough, openai_compat.ExtraKeyResponsesMode: string(mode),
			service.EnhancedControlKey: map[string]any{"stream_recovery": map[string]any{"enabled": true, "cooldown_seconds": 0}}}}
}

func streamRecoveryFailoverContext(t *testing.T, protocol string, ctx context.Context) (*gin.Context, *httptest.ResponseRecorder, *openAIStreamRecovery, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	path := "/v1/responses"
	body := []byte(`{"model":"gpt-5.1","stream":true,"input":"Name a color."}`)
	if protocol == "chat" {
		path = "/v1/chat/completions"
		body = []byte(`{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"Name a color."}]}`)
	} else if protocol == "messages" {
		path = "/v1/messages"
		body = []byte(`{"model":"gpt-5.1","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"Name a color."}]}`)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body))).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	r := newOpenAIStreamRecovery(c, protocol, body, true)
	t.Cleanup(r.finishOnReturn)
	return c, rec, r, body
}

func streamRecoveryFailoverForward(svc *service.OpenAIGatewayService, protocol string, c *gin.Context, r *openAIStreamRecovery, account *service.Account, body []byte) (*service.OpenAIForwardResult, error) {
	ctx := r.billingContext(c.Request.Context())
	switch protocol {
	case "chat":
		return svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
	case "messages":
		return svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
	default:
		return svc.Forward(ctx, c, account, body)
	}
}

func streamRecoveryFailoverErrorServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_loopback_failover")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"Loopback upstream temporarily unavailable"}}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func streamRecoveryFailoverCheckSingleError(t *testing.T, protocol string, rec *httptest.ResponseRecorder) {
	t.Helper()
	wire := rec.Body.String()
	require.NotEmpty(t, strings.TrimSpace(wire), "error fallback must not leave a bare empty HTTP response")
	contentType := rec.Header().Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		require.True(t, gjson.Valid(wire), "JSON fallback must remain JSON, not buffered JSON replaced with SSE: %s", wire)
		require.True(t, gjson.Get(wire, "error").IsObject())
		require.NotEmpty(t, gjson.Get(wire, "error.message").String())
		require.GreaterOrEqual(t, rec.Code, 400)
		return
	}
	require.Contains(t, contentType, "text/event-stream", "SSE error must have SSE content type")
	errorCount, successCount := 0, 0
	for _, line := range strings.Split(wire, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		require.NotEqual(t, "[DONE]", payload, "failed request must not claim successful Chat completion")
		require.True(t, gjson.Valid(payload), payload)
		event := gjson.Parse(payload)
		if event.Get("error").Exists() || event.Get("response.error").Exists() {
			errorCount++
			if protocol == "responses" {
				assert.Equal(t, "response.failed", event.Get("type").String())
				assert.Equal(t, "failed", event.Get("response.status").String())
				assert.True(t, strings.HasPrefix(event.Get("response.id").String(), "resp_sr_"))
			}
		}
		if event.Get("type").String() == "response.completed" || event.Get("type").String() == "message_stop" {
			successCount++
		}
	}
	require.Equal(t, 1, errorCount, "exactly one protocol error, including deferred finishOnReturn")
	require.Zero(t, successCount)
}

func streamRecoveryFailoverMetadata(protocol string) string {
	switch protocol {
	case "responses":
		return streamRecoveryProtocolFrame("response.created", `{"type":"response.created","sequence_number":0,"response":{"id":"resp_metadata","object":"response","model":"gpt-5.1","status":"in_progress","output":[]}}`)
	case "chat":
		return streamRecoveryProtocolFrame("", `{"id":"chatcmpl_metadata","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
	default:
		return streamRecoveryProtocolFrame("message_start", `{"type":"message_start","message":{"id":"msg_metadata","type":"message","role":"assistant","model":"gpt-5.1","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`)
	}
}

func streamRecoveryFailoverHeartbeat(t *testing.T, c *gin.Context, r *openAIStreamRecovery) {
	t.Helper()
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	r.transport.SetHeartbeatInterval(2 * time.Millisecond)
	r.transport.StartWaiting(c.Request.Context(), time.Second)
	defer r.transport.StopWaiting()
	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for r.transport.StreamKeepaliveBytes() == 0 {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("short heartbeat tick never produced an observable comment")
		}
	}
	r.transport.StopWaiting()
	require.Greater(t, r.transport.StreamKeepaliveBytes(), 0)
	require.True(t, c.Writer.Written(), "heartbeat really commits HTTP 200")
	require.False(t, r.writer.HasOutput(), "comment is not semantic progress")
	require.Empty(t, r.writer.EmittedText())
}

func streamRecoveryFailoverCheckSuccess(t *testing.T, protocol string, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	wire := rec.Body.String()
	require.NotEmpty(t, wire, "200 + SSE content type is insufficient without a legal complete body")
	var text strings.Builder
	created, completed, roles, finishes, starts, stops, blocks, blockStops, dones := 0, 0, 0, 0, 0, 0, 0, 0, 0
	logicalID := ""
	for _, line := range strings.Split(wire, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			dones++
			continue
		}
		require.True(t, gjson.Valid(payload), "malformed downstream SSE: %s", payload)
		event := gjson.Parse(payload)
		require.False(t, event.Get("error").Exists(), payload)
		require.False(t, event.Get("response.error").Exists(), payload)
		switch protocol {
		case "responses":
			switch event.Get("type").String() {
			case "response.created":
				created++
				logicalID = event.Get("response.id").String()
				require.True(t, strings.HasPrefix(logicalID, "resp_sr_"), "gateway snapshot ID, not first upstream logical ID")
			case "response.output_text.delta":
				text.WriteString(event.Get("delta").String())
			case "response.completed":
				completed++
				require.Equal(t, logicalID, event.Get("response.id").String())
				require.Equal(t, "completed", event.Get("response.status").String())
				require.Equal(t, "blue", event.Get("response.output.0.content.0.text").String())
			case "response.failed", "response.incomplete", "error":
				t.Fatalf("unexpected terminal after account failover: %s", payload)
			}
		case "chat":
			require.Equal(t, "chat.completion.chunk", event.Get("object").String())
			require.Equal(t, "gpt-5.1", event.Get("model").String())
			if logicalID == "" {
				logicalID = event.Get("id").String()
			}
			require.NotEmpty(t, logicalID)
			require.Equal(t, logicalID, event.Get("id").String())
			for _, choice := range event.Get("choices").Array() {
				require.Equal(t, int64(0), choice.Get("index").Int())
				text.WriteString(choice.Get("delta.content").String())
				if choice.Get("delta.role").Exists() {
					require.Equal(t, "assistant", choice.Get("delta.role").String())
					roles++
				}
				if choice.Get("finish_reason").String() != "" {
					require.Equal(t, "stop", choice.Get("finish_reason").String())
					finishes++
				}
			}
		case "messages":
			switch event.Get("type").String() {
			case "message_start":
				starts++
				require.NotEmpty(t, event.Get("message.id").String())
				require.Equal(t, "gpt-5.1", event.Get("message.model").String())
			case "content_block_start":
				blocks++
				require.Equal(t, "text", event.Get("content_block.type").String())
			case "content_block_delta":
				require.Equal(t, "text_delta", event.Get("delta.type").String())
				text.WriteString(event.Get("delta.text").String())
			case "content_block_stop":
				blockStops++
			case "message_delta":
				require.Equal(t, "end_turn", event.Get("delta.stop_reason").String())
				finishes++
			case "message_stop":
				stops++
			case "error":
				t.Fatalf("unexpected Messages error: %s", payload)
			}
		}
	}
	require.Equal(t, "blue", text.String())
	switch protocol {
	case "responses":
		require.Equal(t, 1, created)
		require.Equal(t, 1, completed)
		require.Zero(t, dones)
	case "chat":
		require.Equal(t, 1, roles)
		require.Equal(t, 1, finishes)
		require.Equal(t, 1, dones)
	case "messages":
		require.Equal(t, 1, starts)
		require.Equal(t, 1, stops)
		require.Equal(t, 1, blocks)
		require.Equal(t, 1, blockStops)
		require.Equal(t, 1, finishes)
		require.Zero(t, dones)
	}
}

func TestOpenAIStreamRecoveryFailoverTwoAccounts(t *testing.T) {
	for _, tc := range []struct {
		protocol, upstream string
		passthrough        bool
	}{
		{"responses", "responses", false}, {"responses", "responses", true},
		{"chat", "responses", false}, {"chat", "chat", false},
		{"messages", "responses", false}, {"messages", "chat", false},
	} {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			for _, noise := range []string{"none", "heartbeat", "metadata_heartbeat"} {
				t.Run(fmt.Sprintf("%s/upstream_%s/passthrough_%v/http_%d/%s", tc.protocol, tc.upstream, tc.passthrough, status, noise), func(t *testing.T) {
					first := streamRecoveryFailoverErrorServer(t, status)
					requests := make(chan []byte, 1)
					second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						body, err := io.ReadAll(req.Body)
						if err != nil {
							http.Error(w, "fixture read failed", http.StatusBadRequest)
							return
						}
						requests <- body
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, streamRecoveryProtocolTextWire(tc.upstream, "resp_second", "blue", true, 7, 2, 1))
					}))
					t.Cleanup(second.Close)
					svc, upstream := streamRecoveryFailoverService(t)
					c, rec, r, original := streamRecoveryFailoverContext(t, tc.protocol, context.Background())
					accounts := []*service.Account{streamRecoveryFailoverAccount(101, first.URL, tc.upstream, tc.passthrough), streamRecoveryFailoverAccount(202, second.URL, tc.upstream, tc.passthrough)}
					failed := map[int64]struct{}{}
					const maxSwitches = 1
					switchCount := 0
					for round := 0; round < 2; round++ {
						var account *service.Account
						for _, candidate := range accounts {
							if _, excluded := failed[candidate.ID]; !excluded {
								account = candidate
								break
							}
						}
						require.NotNil(t, account)
						if round == 0 {
							require.Equal(t, int64(101), account.ID)
						} else {
							require.Equal(t, int64(202), account.ID)
						}
						require.True(t, r.begin(account))
						before := c.Writer.Size()
						if tc.protocol == "responses" {
							before = service.OpenAICompactKeepaliveAdjustedWrittenSize(c)
						}
						if round == 0 && noise != "none" {
							streamRecoveryFailoverHeartbeat(t, c, r)
							require.Equal(t, -1, service.OpenAICompactKeepaliveAdjustedWrittenSize(c), "heartbeat-only bytes must be discounted")
							if noise == "metadata_heartbeat" {
								_, err := io.WriteString(c.Writer, streamRecoveryFailoverMetadata(tc.protocol))
								require.NoError(t, err)
								require.False(t, r.writer.HasOutput(), "created/role/message_start is metadata only")
								require.Empty(t, r.writer.EmittedText())
							}
						}
						body, err := r.body(original)
						require.NoError(t, err)
						require.JSONEq(t, string(original), string(body), "pre-output failover must not build a continuation request")
						result, forwardErr := streamRecoveryFailoverForward(svc, tc.protocol, c, r, account, body)
						if round == 0 {
							var failoverErr *service.UpstreamFailoverError
							require.ErrorAs(t, forwardErr, &failoverErr)
							require.Equal(t, status, failoverErr.StatusCode)
							require.True(t, failoverErr.ShouldRetryNextAccount())
							retry, terminal := r.afterAttempt(svc, account, result, forwardErr, failed)
							require.False(t, retry, "ordinary HTTP failover belongs to original handler")
							require.False(t, terminal)
							require.Zero(t, r.recoveries)
							require.Nil(t, r.pending)
							require.Empty(t, failed, "coordinator must not take over original exclusion set")
							// Actual Responses gate; Chat/Messages still have inline size
							// predicates, so those two are explicitly simulated seams.
							allowed := openAIForwardMayFailover(c, before, failoverErr)
							if tc.protocol != "responses" {
								allowed = !(c.Writer.Size() != before && !(r.writer != nil && !r.writer.HasOutput()))
							}
							require.True(t, allowed, "nonsemantic metadata/heartbeats must not block original failover gate")
							require.True(t, openAIRequestAllowsFailoverReplay(c))
							// Original handler's max-switch/exclusion decision, without
							// redundant ordinary-429 same-pool retry coverage.
							failed[account.ID] = struct{}{}
							require.Less(t, switchCount, maxSwitches)
							switchCount++
						} else {
							require.NoError(t, forwardErr)
							require.NotNil(t, result)
							retry, terminal := r.afterAttempt(svc, account, result, forwardErr, failed)
							require.False(t, retry)
							require.False(t, terminal)
						}
					}
					require.Equal(t, 1, switchCount)
					assert.Equal(t, []int64{101, 202}, upstream.ids, "account 2 must be selected through account 1 exclusion")
					select {
					case body := <-requests:
						require.Equal(t, "gpt-5.1", gjson.GetBytes(body, "model").String())
						require.True(t, gjson.GetBytes(body, "stream").Bool())
						for _, item := range gjson.GetBytes(body, "messages").Array() {
							require.NotEqual(t, "assistant", item.Get("role").String(), "metadata must not become continuation history")
						}
						for _, item := range gjson.GetBytes(body, "input").Array() {
							require.NotEqual(t, "assistant", item.Get("role").String())
						}
					default:
						t.Fatal("second loopback server was not requested")
					}
					require.True(t, r.finished)
					r.finishOnReturn()
					streamRecoveryFailoverCheckSuccess(t, tc.protocol, rec)
				})
			}
		}
	}
}

func TestOpenAIStreamRecoveryFailoverCancelled(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := streamRecoveryFailoverErrorServer(t, http.StatusServiceUnavailable)
			svc, upstream := streamRecoveryFailoverService(t)
			c, rec, r, original := streamRecoveryFailoverContext(t, protocol, ctx)
			account := streamRecoveryFailoverAccount(101, first.URL, "responses", false)
			require.True(t, r.begin(account))
			result, err := streamRecoveryFailoverForward(svc, protocol, c, r, account, original)
			var failoverErr *service.UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			cancel() // client disconnects after first HTTP failure, before failover.
			failed := map[int64]struct{}{}
			retry, _ := r.afterAttempt(svc, account, result, err, failed)
			require.False(t, retry)
			require.False(t, openAIRequestAllowsFailoverReplay(c))
			r.finishOnReturn()
			require.Empty(t, failed)
			require.Zero(t, r.recoveries)
			require.Nil(t, r.pending)
			require.Equal(t, []int64{101}, upstream.ids, "canceled client must never issue attempt 2")
			require.Empty(t, rec.Body.String(), "client cancellation is silent, not a synthetic protocol error")
		})
	}
}

func TestOpenAIStreamRecoveryFailoverExhaustedSSE(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "messages"} {
		for _, noise := range []string{"heartbeat", "metadata_heartbeat"} {
			for _, fallback := range []string{"no_candidate", "handler_error"} {
				t.Run(protocol+"/"+noise+"/"+fallback, func(t *testing.T) {
					first := streamRecoveryFailoverErrorServer(t, http.StatusServiceUnavailable)
					svc, upstream := streamRecoveryFailoverService(t)
					c, rec, r, original := streamRecoveryFailoverContext(t, protocol, context.Background())
					account := streamRecoveryFailoverAccount(101, first.URL, "responses", false)
					require.True(t, r.begin(account))
					streamRecoveryFailoverHeartbeat(t, c, r)
					if noise == "metadata_heartbeat" {
						_, err := io.WriteString(c.Writer, streamRecoveryFailoverMetadata(protocol))
						require.NoError(t, err)
					}
					require.False(t, r.writer.HasOutput())
					result, err := streamRecoveryFailoverForward(svc, protocol, c, r, account, original)
					var failoverErr *service.UpstreamFailoverError
					require.ErrorAs(t, err, &failoverErr)
					failed := map[int64]struct{}{}
					retry, terminal := r.afterAttempt(svc, account, result, err, failed)
					require.False(t, retry)
					require.False(t, terminal)
					failed[account.ID] = struct{}{}
					// Candidate scan sees only the excluded first account. Do not
					// silently repeat it or claim the real scheduler was exercised.
					var next *service.Account
					for _, candidate := range []*service.Account{account} {
						if _, excluded := failed[candidate.ID]; !excluded {
							next = candidate
							break
						}
					}
					require.Nil(t, next)
					if fallback == "handler_error" {
						h := &OpenAIGatewayHandler{}
						if protocol == "messages" {
							h.handleAnthropicFailoverExhausted(c, failoverErr, true)
						} else {
							h.handleFailoverExhausted(c, failoverErr, true)
						}
					}
					r.finishOnReturn()
					require.Equal(t, http.StatusOK, rec.Code, "heartbeat has committed 200: use a protocol error, not an empty body")
					require.Equal(t, []int64{101}, upstream.ids)
					streamRecoveryFailoverCheckSingleError(t, protocol, rec)
				})
			}
		}
	}
}

func TestOpenAIStreamRecoveryFailoverJSONFallback(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			first := streamRecoveryFailoverErrorServer(t, http.StatusServiceUnavailable)
			svc, upstream := streamRecoveryFailoverService(t)
			c, rec, r, original := streamRecoveryFailoverContext(t, protocol, context.Background())
			account := streamRecoveryFailoverAccount(101, first.URL, "responses", false)
			require.True(t, r.begin(account))
			result, err := streamRecoveryFailoverForward(svc, protocol, c, r, account, original)
			var failoverErr *service.UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr, "a genuine HTTP 503 must reach the existing failover owner")
			require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode)
			failed := map[int64]struct{}{}
			retry, terminal := r.afterAttempt(svc, account, result, err, failed)
			require.False(t, retry, "transparent recovery must not steal pre-output HTTP failover")
			require.False(t, terminal, "existing handler must own its normal HTTP failover")
			require.Zero(t, r.recoveries)
			require.Nil(t, r.pending)
			require.Empty(t, failed)
			// No second candidate / original handler switch budget exhausted.
			failed[account.ID] = struct{}{}
			h := &OpenAIGatewayHandler{}
			if protocol == "messages" {
				h.handleAnthropicFailoverExhausted(c, failoverErr, false)
			} else {
				h.handleFailoverExhausted(c, failoverErr, false)
			}
			r.finishOnReturn()
			assert.Equal(t, []int64{101}, upstream.ids)
			streamRecoveryFailoverCheckSingleError(t, protocol, rec)
		})
	}
}
