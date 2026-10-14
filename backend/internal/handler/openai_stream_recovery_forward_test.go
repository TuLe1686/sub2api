package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// This adapter uses real HTTP responses: it does not fabricate a response body
// or bypass Forward's upstream reader. Both transport methods stay on loopback.
type streamRecoveryForwardHTTP struct {
	client     *http.Client
	accountIDs []int64
}

func (u *streamRecoveryForwardHTTP) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.accountIDs = append(u.accountIDs, accountID)
	return u.client.Do(req)
}

func (u *streamRecoveryForwardHTTP) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func TestOpenAIStreamRecoveryForwardTwoAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%v", passthrough), func(t *testing.T) {
			original := []byte(`{"model":"gpt-5.1","stream":true,"input":"Write three lines"}`)
			expectedRecovery, err := service.BuildStreamRecoveryRequest(original, "responses", "one\ntwo")
			require.NoError(t, err)
			requests := make(chan []byte, 2)
			serverErrors := make(chan error, 2)
			frame := func(kind, payload string) string {
				return "event: " + kind + "\ndata: " + payload + "\n\n"
			}
			prefix := func(responseID, itemID, text string) string {
				return frame("response.created", fmt.Sprintf(`{"type":"response.created","sequence_number":0,"response":{"id":%q,"object":"response","model":"gpt-5.1","status":"in_progress","output":[]}}`, responseID)) +
					frame("response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":%q,"type":"message","role":"assistant","status":"in_progress","content":[]}}`, itemID)) +
					frame("response.content_part.added", fmt.Sprintf(`{"type":"response.content_part.added","sequence_number":2,"item_id":%q,"output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`, itemID)) +
					frame("response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","sequence_number":3,"item_id":%q,"output_index":0,"content_index":0,"delta":%q}`, itemID, text))
			}
			firstWire := prefix("resp_first", "msg_first", "one\ntwo") + frame("response.in_progress", `{"type":"response.in_progress","sequence_number":4,"response":{"id":"resp_first","status":"in_progress","usage":{"input_tokens":7,"output_tokens":2,"input_tokens_details":{"cached_tokens":3}}}}`)
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, readErr := io.ReadAll(req.Body)
				if readErr != nil {
					serverErrors <- readErr
					return
				}
				requests <- body
				if req.URL.Path != "/v1/responses" {
					serverErrors <- fmt.Errorf("first path: %s", req.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				// Closing before the advertised Content-Length creates a genuine
				// net/http io.ErrUnexpectedEOF after complete SSE partial frames.
				w.Header().Set("Content-Length", fmt.Sprint(len(firstWire)+100))
				_, _ = io.WriteString(w, firstWire)
				w.(http.Flusher).Flush()
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, readErr := io.ReadAll(req.Body)
				if readErr != nil {
					serverErrors <- readErr
					return
				}
				requests <- body
				var got, want any
				if json.Unmarshal(body, &got) != nil || json.Unmarshal(expectedRecovery, &want) != nil {
					serverErrors <- fmt.Errorf("invalid recovery request JSON")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				if string(gotJSON) != string(wantJSON) || req.URL.Path != "/v1/responses" {
					serverErrors <- fmt.Errorf("second upstream did not receive BuildStreamRecoveryRequest: %s %s", req.URL.Path, gotJSON)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, prefix("resp_second", "msg_second", "two\nthree")+
					frame("response.output_text.done", `{"type":"response.output_text.done","sequence_number":4,"item_id":"msg_second","output_index":0,"content_index":0,"text":"two\nthree"}`)+
					frame("response.content_part.done", `{"type":"response.content_part.done","sequence_number":5,"item_id":"msg_second","output_index":0,"content_index":0,"part":{"type":"output_text","text":"two\nthree","annotations":[]}}`)+
					frame("response.output_item.done", `{"type":"response.output_item.done","sequence_number":6,"output_index":0,"item":{"id":"msg_second","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"two\nthree","annotations":[]}]}}`)+
					frame("response.completed", `{"type":"response.completed","sequence_number":7,"response":{"id":"resp_second","object":"response","model":"gpt-5.1","status":"completed","output":[{"id":"msg_second","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"two\nthree","annotations":[]}]}],"usage":{"input_tokens":11,"output_tokens":4,"input_tokens_details":{"cached_tokens":5}}}}`))
			}))
			defer second.Close()

			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Security.URLAllowlist.Enabled = false
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			upstream := &streamRecoveryForwardHTTP{client: &http.Client{Timeout: 5 * time.Second}}
			defer upstream.client.CloseIdleConnections()
			svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
				nil, nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			accounts := []*service.Account{
				{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-first", "base_url": first.URL}},
				{ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-second", "base_url": second.URL}},
			}
			for _, account := range accounts {
				account.Extra = map[string]any{"openai_passthrough": passthrough, service.EnhancedControlKey: map[string]any{"stream_recovery": map[string]any{"enabled": true, "cooldown_seconds": 0}}}
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(original))).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			recovery := newOpenAIStreamRecovery(c, "responses", original, true)
			defer recovery.finishOnReturn()
			failed := map[int64]struct{}{}
			for round, account := range accounts {
				require.True(t, recovery.begin(account))
				body, bodyErr := recovery.body(original)
				require.NoError(t, bodyErr)
				result, forwardErr := svc.Forward(recovery.billingContext(ctx), c, account, body)
				if round == 0 {
					require.Error(t, forwardErr, "real upstream reader must observe the truncated HTTP body")
					require.ErrorIs(t, forwardErr, io.ErrUnexpectedEOF)
				} else {
					require.NoError(t, forwardErr)
					require.NotNil(t, result)
				}
				retry, terminal := recovery.afterAttempt(svc, account, result, forwardErr, failed)
				require.False(t, terminal)
				require.Equal(t, round == 0, retry)
				if round == 0 {
					require.Contains(t, failed, int64(1))
					require.NotContains(t, recorder.Body.String(), "event: response.completed\n")
					require.NotContains(t, recorder.Body.String(), "event: error\n")
				}
			}
			require.True(t, recovery.finished)
			require.Equal(t, []int64{1, 2}, upstream.accountIDs)
			require.Len(t, requests, 2)
			require.JSONEq(t, string(original), string(<-requests))
			require.JSONEq(t, string(expectedRecovery), string(<-requests))
			close(serverErrors)
			for serverErr := range serverErrors {
				require.NoError(t, serverErr)
			}
			require.Equal(t, http.StatusOK, recorder.Code)
			wire := recorder.Body.String()
			require.Equal(t, 1, strings.Count(wire, "event: response.created\n"))
			require.Equal(t, 1, strings.Count(wire, "event: response.completed\n"))
			require.Equal(t, 1, strings.Count(wire, "event: response.output_item.added\n"))
			require.Equal(t, 1, strings.Count(wire, "event: response.content_part.added\n"))
			require.NotContains(t, wire, "resp_second")
			require.NotContains(t, wire, "msg_second")
			require.NotContains(t, wire, "event: error\n")
			var text strings.Builder
			var completed gjson.Result
			for _, line := range strings.Split(wire, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				data := strings.TrimPrefix(line, "data: ")
				require.True(t, gjson.Valid(data), "invalid downstream SSE: %s", data)
				event := gjson.Parse(data)
				require.False(t, event.Get("error").Exists())
				require.False(t, event.Get("response.error").Exists())
				if event.Get("item_id").Exists() {
					require.Equal(t, "msg_first", event.Get("item_id").String())
				}
				switch event.Get("type").String() {
				case "response.output_text.delta":
					text.WriteString(event.Get("delta").String())
				case "response.failed", "response.incomplete", "error":
					t.Fatalf("unexpected terminal: %s", data)
				case "response.completed":
					completed = event.Get("response")
				}
			}
			require.Equal(t, "one\ntwo\nthree", text.String())
			require.True(t, strings.HasPrefix(completed.Get("id").String(), "resp_sr_"))
			require.Equal(t, "completed", completed.Get("status").String())
			require.Equal(t, "msg_first", completed.Get("output.0.id").String())
			require.Equal(t, "one\ntwo\nthree", completed.Get("output.0.content.0.text").String())
			require.Equal(t, int64(18), completed.Get("usage.input_tokens").Int())
			require.Equal(t, int64(6), completed.Get("usage.output_tokens").Int())
			require.Equal(t, int64(8), completed.Get("usage.input_tokens_details.cached_tokens").Int())
		})
	}
}
