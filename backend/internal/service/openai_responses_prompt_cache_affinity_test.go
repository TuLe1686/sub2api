package service

// Responses 缓存亲和注入（prompt-cache-affinity 补丁）的单元与端到端测试。
// 端到端部分复用 openai_responses_rejected_field_retry_test.go 里的
// httpUpstreamRecorder / newOpenAIRejectedFieldTest* 夹具。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func affinityTestContext(t *testing.T, path string, headers map[string]string, apiKeyID int64) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	if apiKeyID > 0 {
		c.Set("api_key", &APIKey{ID: apiKeyID})
	}
	return c
}

func affinityTestAccount() *Account {
	return &Account{
		ID:          2307,
		Name:        "upstream-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
	}
}

func affinityTestBody() []byte {
	return []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"hi"}],"stream":true,"max_output_tokens":128000}`)
}

func TestInjectResponsesPromptCacheAffinity_InjectsFromSessionHeader(t *testing.T) {
	body := affinityTestBody()
	c := affinityTestContext(t, "/v1/responses", map[string]string{
		"X-Session-Id": "8a5c2a25-eee5-49b6-9174-06a8f5c8dffc",
	}, 110)

	injected, changed := (&OpenAIGatewayService{}).injectResponsesPromptCacheAffinity(c, affinityTestAccount(), body)

	require.True(t, changed)
	key := gjson.GetBytes(injected, "prompt_cache_key").String()
	require.True(t, strings.HasPrefix(key, responsesPromptCacheAffinityPrefix), "key=%q", key)
	require.True(t, json.Valid(injected), "注入后 body 必须是合法 JSON")
	// 其余字段必须原样保留。
	require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(injected, "model").String())
	require.True(t, gjson.GetBytes(injected, "stream").Bool())
	require.EqualValues(t, 128000, gjson.GetBytes(injected, "max_output_tokens").Int())
	require.Equal(t, "hi", gjson.GetBytes(injected, "input.0.content").String())
	// 原 body 不被就地改写。
	require.False(t, gjson.GetBytes(body, "prompt_cache_key").Exists())
}

func TestInjectResponsesPromptCacheAffinity_SupportedSessionHeaders(t *testing.T) {
	// ExtractSessionID 认可的显式会话头集合；ZCode 用 X-Session-Id。
	for _, header := range []string{
		"X-Session-Id",
		"session_id",
		"session-id",
		"conversation_id",
		"X-Session-Affinity",
		"X-OpenCode-Session",
		"X-Conversation-ID",
	} {
		t.Run(header, func(t *testing.T) {
			c := affinityTestContext(t, "/v1/responses", map[string]string{header: "sess-" + header}, 110)
			_, changed := (&OpenAIGatewayService{}).injectResponsesPromptCacheAffinity(c, affinityTestAccount(), affinityTestBody())
			require.True(t, changed, "会话头 %s 应触发注入", header)
		})
	}
}

func TestInjectResponsesPromptCacheAffinity_StablePerKeyAndSession(t *testing.T) {
	headers := map[string]string{"X-Session-Id": "stable-session"}
	svc := &OpenAIGatewayService{}

	first, changed := svc.injectResponsesPromptCacheAffinity(
		affinityTestContext(t, "/v1/responses", headers, 110), affinityTestAccount(), affinityTestBody())
	require.True(t, changed)
	second, changed := svc.injectResponsesPromptCacheAffinity(
		affinityTestContext(t, "/v1/responses", headers, 110), affinityTestAccount(), affinityTestBody())
	require.True(t, changed)
	require.Equal(t, gjson.GetBytes(first, "prompt_cache_key").String(),
		gjson.GetBytes(second, "prompt_cache_key").String(), "同 Key 同会话必须稳定（缓存亲和的前提）")

	// 不同 API Key 使用相同会话 ID 时必须隔离。
	otherKey, changed := svc.injectResponsesPromptCacheAffinity(
		affinityTestContext(t, "/v1/responses", headers, 111), affinityTestAccount(), affinityTestBody())
	require.True(t, changed)
	require.NotEqual(t, gjson.GetBytes(first, "prompt_cache_key").String(),
		gjson.GetBytes(otherKey, "prompt_cache_key").String())

	// 不同会话必须不同。
	otherSession, changed := svc.injectResponsesPromptCacheAffinity(
		affinityTestContext(t, "/v1/responses", map[string]string{"X-Session-Id": "other-session"}, 110),
		affinityTestAccount(), affinityTestBody())
	require.True(t, changed)
	require.NotEqual(t, gjson.GetBytes(first, "prompt_cache_key").String(),
		gjson.GetBytes(otherSession, "prompt_cache_key").String())
}

func TestInjectResponsesPromptCacheAffinity_SkipsWhenClientKeyPresent(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","prompt_cache_key":"client-owned-key","input":"hi"}`)
	c := affinityTestContext(t, "/v1/responses", map[string]string{"X-Session-Id": "sess"}, 110)

	injected, changed := (&OpenAIGatewayService{}).injectResponsesPromptCacheAffinity(c, affinityTestAccount(), body)

	require.False(t, changed, "客户端已有的 prompt_cache_key 不得被覆盖")
	require.Equal(t, "client-owned-key", gjson.GetBytes(injected, "prompt_cache_key").String())
}

func TestInjectResponsesPromptCacheAffinity_SkipsWithoutUsableSession(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
	}{
		{name: "no session header", headers: nil},
		{name: "blank session header", headers: map[string]string{"X-Session-Id": "   "}},
		{name: "control characters", headers: map[string]string{"X-Session-Id": "sess\r\nX-Injected: 1"}},
		{name: "overlong session", headers: map[string]string{"X-Session-Id": strings.Repeat("a", maxPersistedSessionIDLength+1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := affinityTestContext(t, "/v1/responses", tt.headers, 110)
			injected, changed := (&OpenAIGatewayService{}).injectResponsesPromptCacheAffinity(c, affinityTestAccount(), affinityTestBody())
			require.False(t, changed)
			require.False(t, gjson.GetBytes(injected, "prompt_cache_key").Exists())
		})
	}
}

func TestInjectResponsesPromptCacheAffinity_SkipsNonAPIKeyOrNonOpenAI(t *testing.T) {
	headers := map[string]string{"X-Session-Id": "sess"}
	oauth := affinityTestAccount()
	oauth.Type = AccountTypeOAuth
	otherPlatform := affinityTestAccount()
	otherPlatform.Platform = PlatformAnthropic

	tests := []struct {
		name    string
		account *Account
	}{
		{name: "oauth account", account: oauth},
		{name: "non-openai platform", account: otherPlatform},
		{name: "nil account", account: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := affinityTestContext(t, "/v1/responses", headers, 110)
			_, changed := (&OpenAIGatewayService{}).injectResponsesPromptCacheAffinity(c, tt.account, affinityTestBody())
			require.False(t, changed)
		})
	}
}

func TestInjectResponsesPromptCacheAffinity_SkipsCompactPathAndEmptyInputs(t *testing.T) {
	svc := &OpenAIGatewayService{}
	headers := map[string]string{"X-Session-Id": "sess"}

	compactCtx := affinityTestContext(t, "/v1/responses/compact", headers, 110)
	_, changed := svc.injectResponsesPromptCacheAffinity(compactCtx, affinityTestAccount(), affinityTestBody())
	require.False(t, changed, "compact 路径有独立字段白名单，不注入")

	_, changed = svc.injectResponsesPromptCacheAffinity(nil, affinityTestAccount(), affinityTestBody())
	require.False(t, changed, "nil gin.Context 不得 panic 或注入")

	_, changed = svc.injectResponsesPromptCacheAffinity(affinityTestContext(t, "/v1/responses", headers, 110), affinityTestAccount(), nil)
	require.False(t, changed, "空 body 不注入")
}

// ---------- 上游拒绝该字段时的兜底 ----------

func TestNormalizeOpenAIResponsesRejectedFieldRetryBodyRemovesExplicitlyRejectedPromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","prompt_cache_key":"s2a_aff_0123456789abcdef","input":"keep"}`)
	responses := [][]byte{
		[]byte(`{"error":{"code":"unsupported_parameter","message":"Unsupported parameter: 'prompt_cache_key'.","param":"prompt_cache_key"}}`),
		[]byte(`{"error":{"code":"unknown_parameter","message":"Unknown parameter: prompt_cache_key."}}`),
	}
	for _, responseBody := range responses {
		retryBody, reason, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, body, responseBody)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "prompt_cache_key parameter rejection", reason)
		require.False(t, gjson.GetBytes(retryBody, "prompt_cache_key").Exists())
		require.Equal(t, "keep", gjson.GetBytes(retryBody, "input").String())
	}
}

// ---------- 端到端（走 Forward 完整链路） ----------

func TestOpenAIGatewayService_ForwardInjectsResponsesPromptCacheAffinity(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"input":"hi"}`)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusOK, `{"output":[],"usage":{"input_tokens":1,"output_tokens":1,"input_tokens_details":{"cached_tokens":0}}}`),
	}}
	c := newOpenAIRejectedFieldTestContext(body)
	c.Request.Header.Set("X-Session-Id", "8a5c2a25-eee5-49b6-9174-06a8f5c8dffc")
	c.Set("api_key", &APIKey{ID: 110})

	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(
		context.Background(), c, newOpenAIRejectedFieldTestAccount(), body,
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotEmpty(t, upstream.bodies)
	injected := gjson.GetBytes(upstream.bodies[0], "prompt_cache_key").String()
	require.True(t, strings.HasPrefix(injected, responsesPromptCacheAffinityPrefix), "上游收到的 body 应带注入键，got %q", injected)
	require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(upstream.bodies[0], "model").String())
}

func TestOpenAIGatewayService_ForwardRetriesAfterPromptCacheKeyRejection(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"input":"keep"}`)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, `{"error":{"code":"unknown_parameter","message":"Unknown parameter: 'prompt_cache_key'.","param":"prompt_cache_key"}}`),
		newOpenAIRejectedFieldTestResponse(http.StatusOK, `{"output":[],"usage":{"input_tokens":1,"output_tokens":1,"input_tokens_details":{"cached_tokens":0}}}`),
	}}
	c := newOpenAIRejectedFieldTestContext(body)
	c.Request.Header.Set("X-Session-Id", "sess-retry-1")
	c.Set("api_key", &APIKey{ID: 110})

	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(
		context.Background(), c, newOpenAIRejectedFieldTestAccount(), body,
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.NotEmpty(t, gjson.GetBytes(upstream.bodies[0], "prompt_cache_key").String())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").Exists(),
		"上游明确拒绝该字段后，重试 body 必须删掉它")
	require.Equal(t, "keep", gjson.GetBytes(upstream.bodies[1], "input").String())
}
