package handler

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestTransparentRecoveryCompletesTheOriginalResponseAfterPartialOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	body := []byte(`{"model":"test","stream":true,"input":"Write three lines"}`)
	account := &service.Account{ID: 1, Type: service.AccountTypeAPIKey, Platform: service.PlatformOpenAI, Extra: map[string]any{
		service.EnhancedControlKey: map[string]any{"stream_recovery": map[string]any{"enabled": true, "cooldown_seconds": 0}},
	}}
	r := newOpenAIStreamRecovery(c, "responses", body, true)
	require.True(t, r.begin(account))
	_, err := io.WriteString(c.Writer, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_first\",\"status\":\"in_progress\"}}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_first\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\nevent: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_first\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"item_id\":\"msg_first\",\"output_index\":0,\"content_index\":0,\"delta\":\"one\\ntwo\"}\n\n")
	require.NoError(t, err)
	failed := map[int64]struct{}{}
	retry, terminal := r.afterAttempt(nil, account, nil, io.ErrUnexpectedEOF, failed)
	require.True(t, retry)
	require.False(t, terminal)
	require.Contains(t, failed, int64(1))
	require.NotContains(t, rec.Body.String(), "event: error")
	retryBody, err := r.body(body)
	require.NoError(t, err)
	require.Contains(t, string(retryBody), "one\\ntwo")
	account.ID = 2
	require.True(t, r.begin(account))
	_, err = io.WriteString(c.Writer, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_second\",\"status\":\"in_progress\"}}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_second\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\nevent: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_second\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"item_id\":\"msg_second\",\"output_index\":0,\"content_index\":0,\"delta\":\"two\\nthree\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_second\",\"status\":\"completed\",\"output\":[]}}\n\n")
	require.NoError(t, err)
	retry, terminal = r.afterAttempt(nil, account, nil, nil, failed)
	require.False(t, retry)
	require.False(t, terminal)
	wire := rec.Body.String()
	require.Equal(t, 1, strings.Count(wire, "event: response.created\n"))
	require.Equal(t, 1, strings.Count(wire, "event: response.completed\n"))
	require.NotContains(t, wire, "resp_second")
	require.Equal(t, "one\ntwo\nthree", r.writer.EmittedText())
}

func TestTransparentRecoveryHandsOffCompleteToolBesideIncompleteSibling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r := newOpenAIStreamRecovery(c, "chat", []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"lookup"}]}`), true)
	account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Extra: map[string]any{service.EnhancedControlKey: map[string]any{"stream_recovery": map[string]any{"enabled": true, "cooldown_seconds": 0}}}}
	r.begin(account)
	_, err := c.Writer.WriteString("data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"done\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"x\\\":1}\"}},{\"index\":1,\"id\":\"half\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\"}}]}}]}\n\ndata: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
	require.NoError(t, err)
	retry, terminal := r.afterAttempt(nil, account, nil, nil, map[int64]struct{}{})
	require.False(t, retry)
	require.True(t, terminal)
	require.True(t, r.finished)
	require.True(t, r.writer.HasCommittedTools())
}

func TestTransparentRecoveryDoesNotRestartCancelledRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	r := newOpenAIStreamRecovery(c, "responses", []byte(`{"input":"hello"}`), true)
	account := &service.Account{ID: 1, Type: service.AccountTypeAPIKey, Platform: service.PlatformOpenAI, Extra: map[string]any{
		service.EnhancedControlKey: map[string]any{"stream_recovery": map[string]any{"enabled": true}},
	}}
	r.begin(account)
	retry, _ := r.afterAttempt(nil, account, nil, errors.New("stream read error: reset"), map[int64]struct{}{})
	require.False(t, retry)
}

func TestTransparentRecoveryHasABoundedRequestBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	r := newOpenAIStreamRecovery(c, "responses", []byte(`{"input":"hello"}`), true)
	r.maxRecoveries = 1
	r.maxWait = time.Nanosecond
	r.waited = time.Second
	account := &service.Account{ID: 1, Type: service.AccountTypeAPIKey, Platform: service.PlatformOpenAI, Extra: map[string]any{
		service.EnhancedControlKey: map[string]any{"stream_recovery": map[string]any{"enabled": true}},
	}}
	r.begin(account)
	retry, terminal := r.afterAttempt(nil, account, nil, io.ErrUnexpectedEOF, map[int64]struct{}{})
	require.False(t, retry)
	require.True(t, terminal)
}
