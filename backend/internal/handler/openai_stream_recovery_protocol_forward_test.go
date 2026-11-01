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
	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai_compat"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// These tests exercise the real HTTP Forward methods and the recovery
// coordinator. Account selection is deliberately simulated with the coordinator's
// failed-account set, not the main handler's auth/Redis/database dependencies.
// The only repository operation implemented is the observable cooldown write.
type streamRecoveryProtocolCooldown struct {
	service.AccountRepository
	calls []streamRecoveryProtocolPause
}

type streamRecoveryProtocolPause struct {
	accountID int64
	at        time.Time
	until     time.Time
	reason    string
	deadline  time.Time
}

func (r *streamRecoveryProtocolCooldown) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	deadline, _ := ctx.Deadline()
	r.calls = append(r.calls, streamRecoveryProtocolPause{id, time.Now(), until, reason, deadline})
	return nil
}

type streamRecoveryProtocolHTTP struct {
	client     *http.Client
	accountIDs []int64
}

func (u *streamRecoveryProtocolHTTP) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.accountIDs = append(u.accountIDs, accountID)
	return u.client.Do(req)
}

func (u *streamRecoveryProtocolHTTP) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

type streamRecoveryProtocolRequest struct {
	path string
	body []byte
	err  error
}

func streamRecoveryProtocolServer(t *testing.T, wire string, truncated bool) (*httptest.Server, <-chan streamRecoveryProtocolRequest) {
	t.Helper()
	requests := make(chan streamRecoveryProtocolRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		requests <- streamRecoveryProtocolRequest{req.URL.Path, body, err}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req_loopback")
		if truncated {
			// Full legal SSE frames followed by a real net/http unexpected EOF;
			// no fake reader or synthetic recovery error is injected.
			w.Header().Set("Content-Length", fmt.Sprint(len(wire)+100))
		}
		_, _ = io.WriteString(w, wire)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func streamRecoveryProtocolReceive(t *testing.T, requests <-chan streamRecoveryProtocolRequest) streamRecoveryProtocolRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("the expected loopback upstream did not receive a request")
		return streamRecoveryProtocolRequest{}
	}
}

func streamRecoveryProtocolService(t *testing.T, repo *streamRecoveryProtocolCooldown) (*service.OpenAIGatewayService, *streamRecoveryProtocolHTTP) {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return nil, fmt.Errorf("test forbids non-loopback upstream: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	upstream := &streamRecoveryProtocolHTTP{client: &http.Client{Timeout: 5 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	svc := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		nil, nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	return svc, upstream
}

func streamRecoveryProtocolAccount(id int64, baseURL, upstreamProtocol string) *service.Account {
	mode := openai_compat.ResponsesSupportModeForceResponses
	if upstreamProtocol == "chat" {
		mode = openai_compat.ResponsesSupportModeForceChatCompletions
	}
	return &service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "loopback-test-key", "base_url": baseURL},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesMode: string(mode),
			service.EnhancedControlKey: map[string]any{"stream_recovery": map[string]any{"enabled": true, "cooldown_seconds": 60}}}}
}

func streamRecoveryProtocolFrame(kind, payload string) string {
	if kind == "" {
		return "data: " + payload + "\n\n"
	}
	return "event: " + kind + "\ndata: " + payload + "\n\n"
}

func streamRecoveryProtocolTextWire(upstreamProtocol, id, text string, complete bool, input, output, cached int) string {
	frame := streamRecoveryProtocolFrame
	if upstreamProtocol == "chat" {
		meta := fmt.Sprintf(`"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.1"`, id)
		wire := frame("", `{`+meta+`,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`) +
			frame("", fmt.Sprintf(`{%s,"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}}`, meta, text, input, output, input+output, cached))
		if complete {
			wire += frame("", `{`+meta+`,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
				frame("", fmt.Sprintf(`{%s,"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}}`, meta, input, output, input+output, cached)) + frame("", "[DONE]")
		}
		return wire
	}
	itemID := "msg_" + id
	wire := frame("response.created", fmt.Sprintf(`{"type":"response.created","sequence_number":0,"response":{"id":%q,"object":"response","created_at":1700000000,"model":"gpt-5.1","status":"in_progress","output":[]}}`, id)) +
		frame("response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":%q,"type":"message","role":"assistant","status":"in_progress","content":[]}}`, itemID)) +
		frame("response.content_part.added", fmt.Sprintf(`{"type":"response.content_part.added","sequence_number":2,"item_id":%q,"output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`, itemID)) +
		frame("response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","sequence_number":3,"item_id":%q,"output_index":0,"content_index":0,"delta":%q}`, itemID, text)) +
		frame("response.in_progress", fmt.Sprintf(`{"type":"response.in_progress","sequence_number":4,"response":{"id":%q,"status":"in_progress","usage":{"input_tokens":%d,"output_tokens":%d,"input_tokens_details":{"cached_tokens":%d}}}}`, id, input, output, cached))
	if complete {
		wire += frame("response.output_text.done", fmt.Sprintf(`{"type":"response.output_text.done","sequence_number":5,"item_id":%q,"output_index":0,"content_index":0,"text":%q}`, itemID, text)) +
			frame("response.content_part.done", fmt.Sprintf(`{"type":"response.content_part.done","sequence_number":6,"item_id":%q,"output_index":0,"content_index":0,"part":{"type":"output_text","text":%q,"annotations":[]}}`, itemID, text)) +
			frame("response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":{"id":%q,"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q,"annotations":[]}]}}`, itemID, text)) +
			frame("response.completed", fmt.Sprintf(`{"type":"response.completed","sequence_number":8,"response":{"id":%q,"object":"response","model":"gpt-5.1","status":"completed","output":[{"id":%q,"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q,"annotations":[]}]}],"usage":{"input_tokens":%d,"output_tokens":%d,"input_tokens_details":{"cached_tokens":%d}}}}`, id, itemID, text, input, output, cached))
	}
	return wire
}

func streamRecoveryProtocolForward(svc *service.OpenAIGatewayService, protocol string, ctx context.Context, c *gin.Context, account *service.Account, body []byte) (*service.OpenAIForwardResult, error) {
	if protocol == "chat" {
		return svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
	}
	return svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
}

func streamRecoveryProtocolHistoryText(item gjson.Result) string {
	content := item.Get("content")
	if content.Type == gjson.String {
		return content.String()
	}
	var text strings.Builder
	for _, part := range content.Array() {
		text.WriteString(part.Get("text").String())
	}
	return text.String()
}

func streamRecoveryProtocolCheckRequest(t *testing.T, request streamRecoveryProtocolRequest, upstreamProtocol string, resumed bool) {
	t.Helper()
	require.NoError(t, request.err)
	require.True(t, gjson.ValidBytes(request.body), "upstream request must be JSON")
	path, historyKey := "/v1/responses", "input"
	if upstreamProtocol == "chat" {
		path, historyKey = "/v1/chat/completions", "messages"
		assert.True(t, gjson.GetBytes(request.body, "stream_options.include_usage").Bool())
	}
	assert.Equal(t, path, request.path)
	assert.Equal(t, "gpt-5.1", gjson.GetBytes(request.body, "model").String())
	assert.True(t, gjson.GetBytes(request.body, "stream").Bool())
	var history []gjson.Result
	for _, item := range gjson.GetBytes(request.body, historyKey).Array() {
		// The existing Messages→Responses bridge prepends its developer todo
		// guard. Validate conversational history without treating that guard as
		// a new user or assistant turn.
		if role := item.Get("role").String(); role == "developer" || role == "system" {
			assert.NotEmpty(t, streamRecoveryProtocolHistoryText(item))
			continue
		}
		history = append(history, item)
	}
	wantLen := 1
	if resumed {
		wantLen = 3
	}
	require.Len(t, history, wantLen, "the second upstream must receive the original user turn and public continuation history")
	assert.Equal(t, "user", history[0].Get("role").String())
	assert.Equal(t, "List the colors on three lines.", streamRecoveryProtocolHistoryText(history[0]))
	if resumed {
		assert.Equal(t, "assistant", history[1].Get("role").String())
		assert.Equal(t, "red\ngreen", streamRecoveryProtocolHistoryText(history[1]))
		assert.Equal(t, "user", history[2].Get("role").String())
		assert.Contains(t, streamRecoveryProtocolHistoryText(history[2]), "最后一行")
		assert.Contains(t, streamRecoveryProtocolHistoryText(history[2]), "不要重新执行历史中已经完成的工具调用")
		for _, field := range []string{"previous_response_id", "conversation", "conversation_id", "turn_state"} {
			assert.False(t, gjson.GetBytes(request.body, field).Exists(), "account-bound state %s must not cross accounts", field)
		}
	}
}

func TestOpenAIStreamRecoveryProtocolForwardText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"chat", "messages"} {
		for _, upstreamProtocol := range []string{"responses", "chat"} {
			t.Run(protocol+"/upstream_"+upstreamProtocol, func(t *testing.T) {
				first, firstRequests := streamRecoveryProtocolServer(t, streamRecoveryProtocolTextWire(upstreamProtocol, "first", "red\ngreen", false, 7, 2, 3), true)
				second, secondRequests := streamRecoveryProtocolServer(t, streamRecoveryProtocolTextWire(upstreamProtocol, "second", "green\nblue", true, 11, 4, 5), false)
				repo := &streamRecoveryProtocolCooldown{}
				svc, upstream := streamRecoveryProtocolService(t, repo)
				accounts := []*service.Account{streamRecoveryProtocolAccount(1, first.URL, upstreamProtocol), streamRecoveryProtocolAccount(2, second.URL, upstreamProtocol)}
				original := []byte(`{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"List the colors on three lines."}]}`)
				path := "/v1/chat/completions"
				if protocol == "messages" {
					path = "/v1/messages"
					original = []byte(`{"model":"gpt-5.1","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"List the colors on three lines."}]}`)
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), ctxkey.RequestID, "req_protocol_recovery"), 10*time.Second)
				defer cancel()
				c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(original))).WithContext(ctx)
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.Header.Set("X-Codex-Turn-State", "first-account-state")
				recovery := newOpenAIStreamRecovery(c, protocol, original, true)
				defer recovery.finishOnReturn()
				failed := map[int64]struct{}{}
				for round := 0; round < 2; round++ {
					// Always scan from the first candidate: choosing account 2 is
					// caused by the exclusion set, not by a hard-coded round index.
					var account *service.Account
					for _, candidate := range accounts {
						if _, excluded := failed[candidate.ID]; !excluded {
							account = candidate
							break
						}
					}
					require.NotNil(t, account)
					require.Equal(t, int64(round+1), account.ID)
					require.True(t, recovery.begin(account))
					body, err := recovery.body(original)
					require.NoError(t, err)
					result, forwardErr := streamRecoveryProtocolForward(svc, protocol, recovery.billingContext(ctx), c, account, body)
					if round == 0 {
						assert.ErrorIs(t, forwardErr, io.ErrUnexpectedEOF, "the real HTTP reader must expose Content-Length truncation even with progressive usage")
						streamRecoveryProtocolCheckRequest(t, streamRecoveryProtocolReceive(t, firstRequests), upstreamProtocol, false)
					} else {
						require.NoError(t, forwardErr)
						streamRecoveryProtocolCheckRequest(t, streamRecoveryProtocolReceive(t, secondRequests), upstreamProtocol, true)
					}
					require.NotNil(t, result)
					assert.Equal(t, "gpt-5.1", result.Model)
					assert.Equal(t, "gpt-5.1", result.BillingModel)
					assert.Equal(t, "gpt-5.1", result.UpstreamModel)
					assert.Equal(t, "req_loopback", result.RequestID)
					assert.True(t, result.Stream)
					assert.False(t, result.ClientDisconnect)
					assert.NotNil(t, result.FirstTokenMs)
					retry, terminal := recovery.afterAttempt(svc, account, result, forwardErr, failed)
					if round == 0 {
						require.True(t, retry, "coordinator failed after real %s→%s HTTP attempt: forwardErr=%v writerErr=%v wire=%s", protocol, upstreamProtocol, forwardErr, recovery.writer.Err(), rec.Body.String())
						require.False(t, terminal)
						assert.Contains(t, failed, int64(1))
						assert.Empty(t, c.Request.Header.Get("X-Codex-Turn-State"))
						require.Len(t, repo.calls, 1)
						pause := repo.calls[0]
						assert.Equal(t, int64(1), pause.accountID)
						assert.Equal(t, "[stream_recovery] upstream_stream_interrupted", pause.reason)
						assert.InDelta(t, 60, pause.until.Sub(pause.at).Seconds(), 1)
						assert.True(t, pause.deadline.After(pause.at))
						assert.LessOrEqual(t, pause.deadline.Sub(pause.at), 2*time.Second)
						assert.NotContains(t, rec.Body.String(), "data: [DONE]")
						assert.NotContains(t, rec.Body.String(), "event: message_stop")
						assert.NotContains(t, rec.Body.String(), "event: error")
					} else {
						require.False(t, retry)
						require.False(t, terminal)
					}
				}
				require.True(t, recovery.finished)
				assert.Equal(t, []int64{1, 2}, upstream.accountIDs)
				require.Len(t, repo.calls, 1, "successful continuation must not cool down account 2")
				assert.Equal(t, 18, recovery.usage.InputTokens)
				assert.Equal(t, 6, recovery.usage.OutputTokens)
				assert.Equal(t, 8, recovery.usage.CacheReadInputTokens)
				assert.Equal(t, http.StatusOK, rec.Code)
				streamRecoveryProtocolCheckText(t, protocol, rec.Body.String())
			})
		}
	}
}

func streamRecoveryProtocolToolWire(upstreamProtocol, id, callID string, argsComplete, terminal bool) string {
	frame := streamRecoveryProtocolFrame
	if upstreamProtocol == "chat" {
		meta := fmt.Sprintf(`"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.1"`, id)
		wire := frame("", `{`+meta+`,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`) +
			frame("", fmt.Sprintf(`{%s,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`, meta, callID)) +
			frame("", `{`+meta+`,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"color\":"}}]},"finish_reason":null}]}`)
		if argsComplete {
			wire += frame("", `{`+meta+`,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"blue\"}"}}]},"finish_reason":null}]}`) +
				frame("", `{`+meta+`,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		}
		if terminal {
			wire += frame("", `{`+meta+`,"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":5}}}`) + frame("", "[DONE]")
		}
		return wire
	}
	itemID := "fc_" + id
	wire := frame("response.created", fmt.Sprintf(`{"type":"response.created","sequence_number":0,"response":{"id":%q,"object":"response","created_at":1700000000,"model":"gpt-5.1","status":"in_progress","output":[]}}`, id)) +
		frame("response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":%q,"type":"function_call","call_id":%q,"name":"lookup","arguments":"","status":"in_progress"}}`, itemID, callID)) +
		frame("response.function_call_arguments.delta", fmt.Sprintf(`{"type":"response.function_call_arguments.delta","sequence_number":2,"item_id":%q,"output_index":0,"delta":"{\"color\":"}`, itemID))
	if argsComplete {
		wire += frame("response.function_call_arguments.delta", fmt.Sprintf(`{"type":"response.function_call_arguments.delta","sequence_number":3,"item_id":%q,"output_index":0,"delta":"\"blue\"}"}`, itemID)) +
			frame("response.function_call_arguments.done", fmt.Sprintf(`{"type":"response.function_call_arguments.done","sequence_number":4,"item_id":%q,"output_index":0,"arguments":"{\"color\":\"blue\"}"}`, itemID)) +
			frame("response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":{"id":%q,"type":"function_call","call_id":%q,"name":"lookup","arguments":"{\"color\":\"blue\"}","status":"completed"}}`, itemID, callID))
	}
	if terminal {
		wire += frame("response.completed", fmt.Sprintf(`{"type":"response.completed","sequence_number":6,"response":{"id":%q,"object":"response","model":"gpt-5.1","status":"completed","output":[{"id":%q,"type":"function_call","call_id":%q,"name":"lookup","arguments":"{\"color\":\"blue\"}","status":"completed"}],"usage":{"input_tokens":11,"output_tokens":4,"input_tokens_details":{"cached_tokens":5}}}}`, id, itemID, callID))
	}
	return wire
}

func streamRecoveryProtocolToolRequest(protocol string) []byte {
	if protocol == "chat" {
		return []byte(`{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"Look up the color blue."}],"tools":[{"type":"function","function":{"name":"lookup","description":"Read a color from an in-memory sample table.","parameters":{"type":"object","properties":{"color":{"type":"string"}},"required":["color"]}}}]}`)
	}
	return []byte(`{"model":"gpt-5.1","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"Look up the color blue."}],"tools":[{"name":"lookup","description":"Read a color from an in-memory sample table.","input_schema":{"type":"object","properties":{"color":{"type":"string"}},"required":["color"]}}]}`)
}

// Tool descriptors are inert fixtures: no tool is executed, and both upstreams
// are loopback-only. Partial arguments must never become a client-side call.
func TestOpenAIStreamRecoveryProtocolForwardPartialTool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"chat", "messages"} {
		for _, upstreamProtocol := range []string{"responses", "chat"} {
			t.Run(protocol+"/upstream_"+upstreamProtocol, func(t *testing.T) {
				first, firstRequests := streamRecoveryProtocolServer(t, streamRecoveryProtocolToolWire(upstreamProtocol, "first", "call_partial", false, false), true)
				second, secondRequests := streamRecoveryProtocolServer(t, streamRecoveryProtocolToolWire(upstreamProtocol, "second", "call_final", true, true), false)
				repo := &streamRecoveryProtocolCooldown{}
				svc, upstream := streamRecoveryProtocolService(t, repo)
				accounts := []*service.Account{streamRecoveryProtocolAccount(1, first.URL, upstreamProtocol), streamRecoveryProtocolAccount(2, second.URL, upstreamProtocol)}
				original := streamRecoveryProtocolToolRequest(protocol)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				path := "/v1/messages"
				if protocol == "chat" {
					path = "/v1/chat/completions"
				}
				c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(original))).WithContext(ctx)
				c.Request.Header.Set("Content-Type", "application/json")
				r := newOpenAIStreamRecovery(c, protocol, original, true)
				defer r.finishOnReturn()
				failed := map[int64]struct{}{}
				for round, account := range accounts {
					if round == 1 {
						require.Contains(t, failed, int64(1))
						require.NotContains(t, failed, account.ID)
					}
					require.True(t, r.begin(account))
					body, err := r.body(original)
					require.NoError(t, err)
					require.JSONEq(t, string(original), string(body), "unexposed partial JSON must not be appended to recovery history")
					result, forwardErr := streamRecoveryProtocolForward(svc, protocol, r.billingContext(ctx), c, account, body)
					if round == 0 {
						require.ErrorIs(t, forwardErr, io.ErrUnexpectedEOF)
						request := streamRecoveryProtocolReceive(t, firstRequests)
						require.NoError(t, request.err)
						require.NotContains(t, rec.Body.String(), "call_partial")
						require.NotContains(t, rec.Body.String(), `"tool_calls"`)
						require.NotContains(t, rec.Body.String(), `"tool_use"`)
						require.NotContains(t, rec.Body.String(), "input_json_delta")
					} else {
						require.NoError(t, forwardErr)
						request := streamRecoveryProtocolReceive(t, secondRequests)
						require.NoError(t, request.err)
						require.NotContains(t, string(request.body), "call_partial")
						for _, item := range gjson.GetBytes(request.body, "messages").Array() {
							require.NotEqual(t, "assistant", item.Get("role").String())
						}
						for _, item := range gjson.GetBytes(request.body, "input").Array() {
							require.NotEqual(t, "assistant", item.Get("role").String())
						}
					}
					retry, terminal := r.afterAttempt(svc, account, result, forwardErr, failed)
					require.Equal(t, round == 0, retry, "forward=%v writer=%v", forwardErr, r.writer.Err())
					require.False(t, terminal)
				}
				require.True(t, r.finished)
				require.Equal(t, []int64{1, 2}, upstream.accountIDs)
				require.Len(t, repo.calls, 1)
				streamRecoveryProtocolCheckTool(t, protocol, rec.Body.String(), "call_final")
			})
		}
	}
}

// A completed call is a handoff, not permission to regenerate or execute it.
// These independent cases cover native Chat and Responses→Messages conversion.
func TestOpenAIStreamRecoveryProtocolForwardCompleteToolHandoff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct{ protocol, upstream string }{{"chat", "chat"}, {"messages", "responses"}} {
		t.Run(tc.protocol+"/upstream_"+tc.upstream, func(t *testing.T) {
			first, requests := streamRecoveryProtocolServer(t, streamRecoveryProtocolToolWire(tc.upstream, "first", "call_handoff", true, false), true)
			repo := &streamRecoveryProtocolCooldown{}
			svc, upstream := streamRecoveryProtocolService(t, repo)
			account := streamRecoveryProtocolAccount(1, first.URL, tc.upstream)
			original := streamRecoveryProtocolToolRequest(tc.protocol)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			path := "/v1/messages"
			if tc.protocol == "chat" {
				path = "/v1/chat/completions"
			}
			c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(original))).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			r := newOpenAIStreamRecovery(c, tc.protocol, original, true)
			defer r.finishOnReturn()
			require.True(t, r.begin(account))
			result, forwardErr := streamRecoveryProtocolForward(svc, tc.protocol, r.billingContext(ctx), c, account, original)
			assert.ErrorIs(t, forwardErr, io.ErrUnexpectedEOF, "a complete tool must not hide the broken HTTP stream")
			request := streamRecoveryProtocolReceive(t, requests)
			require.NoError(t, request.err)
			require.True(t, r.writer.HasCommittedTools(), "only fully validated arguments qualify for handoff")
			failed := map[int64]struct{}{}
			retry, terminal := r.afterAttempt(svc, account, result, forwardErr, failed)
			assert.False(t, retry, "committed call cannot be regenerated on another account")
			assert.True(t, terminal, "coordinator must stop selecting accounts after tool handoff")
			require.True(t, r.finished)
			assert.Equal(t, 0, r.recoveries)
			assert.Nil(t, r.pending)
			assert.Empty(t, failed)
			assert.Equal(t, []int64{1}, upstream.accountIDs)
			require.Len(t, repo.calls, 1, "broken upstream must still be cooled down")
			streamRecoveryProtocolCheckTool(t, tc.protocol, rec.Body.String(), "call_handoff")
		})
	}
}

func streamRecoveryProtocolCheckTool(t *testing.T, protocol, wire, callID string) {
	t.Helper()
	toolCount, finishCount, startCount, stopCount, blockStopCount := 0, 0, 0, 0, 0
	id := ""
	var args strings.Builder
	for _, line := range strings.Split(wire, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		require.True(t, gjson.Valid(payload), payload)
		event := gjson.Parse(payload)
		assert.False(t, event.Get("error").Exists(), payload)
		if protocol == "chat" {
			if id == "" {
				id = event.Get("id").String()
			}
			assert.NotEmpty(t, id)
			assert.Equal(t, id, event.Get("id").String())
			assert.Equal(t, "chat.completion.chunk", event.Get("object").String())
			assert.Equal(t, "gpt-5.1", event.Get("model").String())
			for _, choice := range event.Get("choices").Array() {
				assert.True(t, choice.Get("index").Exists())
				assert.Equal(t, int64(0), choice.Get("index").Int())
				assert.Empty(t, choice.Get("delta.content").String())
				for _, tool := range choice.Get("delta.tool_calls").Array() {
					toolCount++
					assert.Equal(t, callID, tool.Get("id").String())
					assert.Equal(t, "function", tool.Get("type").String())
					assert.True(t, tool.Get("index").Exists())
					assert.Equal(t, int64(0), tool.Get("index").Int())
					assert.Equal(t, "lookup", tool.Get("function.name").String())
					args.WriteString(tool.Get("function.arguments").String())
				}
				if choice.Get("finish_reason").String() != "" {
					assert.Equal(t, "tool_calls", choice.Get("finish_reason").String())
					finishCount++
				}
			}
		} else {
			switch event.Get("type").String() {
			case "message_start":
				startCount++
				assert.NotEmpty(t, event.Get("message.id").String())
				assert.Equal(t, "assistant", event.Get("message.role").String())
			case "content_block_start":
				toolCount++
				assert.True(t, event.Get("index").Exists())
				assert.Equal(t, int64(0), event.Get("index").Int())
				assert.Equal(t, "tool_use", event.Get("content_block.type").String())
				assert.Equal(t, callID, event.Get("content_block.id").String())
				assert.Equal(t, "lookup", event.Get("content_block.name").String())
			case "content_block_delta":
				assert.True(t, event.Get("index").Exists())
				assert.Equal(t, int64(0), event.Get("index").Int())
				assert.Equal(t, "input_json_delta", event.Get("delta.type").String())
				args.WriteString(event.Get("delta.partial_json").String())
			case "content_block_stop":
				assert.True(t, event.Get("index").Exists())
				assert.Equal(t, int64(0), event.Get("index").Int())
				blockStopCount++
			case "message_delta":
				assert.Equal(t, "tool_use", event.Get("delta.stop_reason").String())
				finishCount++
			case "message_stop":
				stopCount++
			default:
				t.Errorf("unexpected tool stream event: %s", payload)
			}
		}
	}
	assert.NotContains(t, wire, `"id":"second"`)
	assert.NotContains(t, wire, "call_partial")
	assert.Equal(t, 1, toolCount, "one complete lookup call, no partial or duplicate call")
	assert.Equal(t, 1, finishCount)
	require.JSONEq(t, `{"color":"blue"}`, args.String(), "arguments must be complete JSON, not concatenated with a discarded partial attempt")
	if protocol == "chat" {
		assert.Equal(t, 1, strings.Count(wire, "data: [DONE]\n"))
	} else {
		assert.Equal(t, 1, startCount)
		assert.Equal(t, 1, stopCount)
		assert.Equal(t, 1, blockStopCount)
	}
}

func streamRecoveryProtocolCheckText(t *testing.T, protocol, wire string) {
	t.Helper()
	var text strings.Builder
	var usage gjson.Result
	id := ""
	roleCount, finishCount, usageCount, startCount, stopCount, blockStarts, blockStops := 0, 0, 0, 0, 0, 0, 0
	for _, line := range strings.Split(wire, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		require.True(t, gjson.Valid(payload), "invalid downstream SSE JSON: %s", payload)
		event := gjson.Parse(payload)
		assert.False(t, event.Get("error").Exists(), payload)
		if protocol == "chat" {
			if id == "" {
				id = event.Get("id").String()
			}
			assert.NotEmpty(t, id)
			assert.Equal(t, id, event.Get("id").String(), "one logical completion across both HTTP attempts")
			assert.Equal(t, "chat.completion.chunk", event.Get("object").String())
			assert.Equal(t, "gpt-5.1", event.Get("model").String())
			assert.Greater(t, event.Get("created").Int(), int64(0))
			for _, choice := range event.Get("choices").Array() {
				assert.True(t, choice.Get("index").Exists())
				assert.Equal(t, int64(0), choice.Get("index").Int())
				assert.True(t, choice.Get("delta").IsObject())
				text.WriteString(choice.Get("delta.content").String())
				if choice.Get("delta.role").Exists() {
					assert.Equal(t, "assistant", choice.Get("delta.role").String())
					roleCount++
				}
				if choice.Get("finish_reason").String() != "" {
					assert.Equal(t, "stop", choice.Get("finish_reason").String())
					finishCount++
				}
			}
			// Compatible raw Chat upstreams can carry progressive usage alongside
			// text. Only the final usage-only chunk is the logical total.
			if event.Get("usage").IsObject() && len(event.Get("choices").Array()) == 0 {
				usage, usageCount = event.Get("usage"), usageCount+1
			}
		} else {
			switch event.Get("type").String() {
			case "message_start":
				startCount++
				id = event.Get("message.id").String()
				assert.NotEmpty(t, id)
				assert.Equal(t, "message", event.Get("message.type").String())
				assert.Equal(t, "assistant", event.Get("message.role").String())
				assert.Equal(t, "gpt-5.1", event.Get("message.model").String())
				assert.True(t, event.Get("message.content").IsArray())
				assert.Equal(t, gjson.Null, event.Get("message.stop_reason").Type)
				assert.True(t, event.Get("message.usage").IsObject())
			case "content_block_start":
				assert.Equal(t, "text", event.Get("content_block.type").String())
				assert.True(t, event.Get("index").Exists())
				assert.Equal(t, int64(0), event.Get("index").Int())
				blockStarts++
			case "content_block_delta":
				assert.Equal(t, "text_delta", event.Get("delta.type").String())
				assert.True(t, event.Get("index").Exists())
				assert.Equal(t, int64(0), event.Get("index").Int())
				text.WriteString(event.Get("delta.text").String())
			case "content_block_stop":
				assert.True(t, event.Get("index").Exists())
				assert.Equal(t, int64(0), event.Get("index").Int())
				blockStops++
			case "message_delta":
				assert.Equal(t, "end_turn", event.Get("delta.stop_reason").String())
				assert.True(t, event.Get("delta.stop_sequence").Exists())
				assert.Equal(t, gjson.Null, event.Get("delta.stop_sequence").Type)
				usage, usageCount = event.Get("usage"), usageCount+1
			case "message_stop":
				stopCount++
			default:
				t.Errorf("unexpected Messages event: %s", payload)
			}
		}
	}
	assert.NotContains(t, wire, `"id":"second"`, "the continuation upstream's identity must not leak downstream")
	assert.NotContains(t, wire, `"id":"msg_second"`)
	assert.Equal(t, "red\ngreen\nblue", text.String(), "the replayed last line must not duplicate downstream text")
	require.Equal(t, 1, usageCount, "only the final logical response exposes accumulated usage")
	if protocol == "chat" {
		assert.Equal(t, 1, roleCount)
		assert.Equal(t, 1, finishCount)
		assert.Equal(t, 1, strings.Count(wire, "data: [DONE]\n"))
		assert.Equal(t, int64(18), usage.Get("prompt_tokens").Int())
		assert.Equal(t, int64(6), usage.Get("completion_tokens").Int())
		assert.Equal(t, int64(24), usage.Get("total_tokens").Int())
		assert.Equal(t, int64(8), usage.Get("prompt_tokens_details.cached_tokens").Int())
	} else {
		assert.Equal(t, 1, startCount)
		assert.Equal(t, 1, stopCount)
		assert.Equal(t, 1, blockStarts)
		assert.Equal(t, 1, blockStops)
		assert.Equal(t, int64(10), usage.Get("input_tokens").Int(), "Anthropic uncached input = 18 total minus 8 cached")
		assert.Equal(t, int64(6), usage.Get("output_tokens").Int())
		assert.Equal(t, int64(8), usage.Get("cache_read_input_tokens").Int())
		assert.Equal(t, int64(0), usage.Get("cache_creation_input_tokens").Int())
		assert.Less(t, strings.Index(wire, "event: content_block_stop\n"), strings.Index(wire, "event: message_delta\n"))
		assert.Less(t, strings.Index(wire, "event: message_delta\n"), strings.Index(wire, "event: message_stop\n"))
	}
}
