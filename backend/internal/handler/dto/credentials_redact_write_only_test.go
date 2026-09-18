package dto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestRedactWriteOnlyCredentials_StripsAPIKeyBaseURL(t *testing.T) {
	creds := map[string]any{
		"base_url":      "https://relay.example.com/v1",
		"model_mapping": map[string]any{"foo": "bar"},
	}

	out, status := RedactWriteOnlyCredentials(service.AccountTypeAPIKey, creds, nil)

	require.NotContains(t, out, "base_url")
	require.Equal(t, map[string]any{"foo": "bar"}, out["model_mapping"])
	require.True(t, status["has_base_url"])
	// 原始 map 不应被修改
	require.Equal(t, "https://relay.example.com/v1", creds["base_url"])
}

func TestRedactWriteOnlyCredentials_KeepsBaseURLForOtherAccountTypes(t *testing.T) {
	// OAuth / upstream 的 base_url 是编辑界面必读字段，不能脱敏。
	for _, accountType := range []string{service.AccountTypeOAuth, service.AccountTypeUpstream} {
		creds := map[string]any{"base_url": "https://cli-chat-proxy.grok.com/v1"}

		out, status := RedactWriteOnlyCredentials(accountType, creds, nil)

		require.Equal(t, "https://cli-chat-proxy.grok.com/v1", out["base_url"], accountType)
		require.False(t, status["has_base_url"], accountType)
	}
}

func TestRedactWriteOnlyCredentials_EmptyValueNotMarkedPresent(t *testing.T) {
	out, status := RedactWriteOnlyCredentials(service.AccountTypeAPIKey, map[string]any{"base_url": ""}, nil)

	require.NotContains(t, out, "base_url")
	require.False(t, status["has_base_url"])
}

func TestRedactWriteOnlyCredentials_NilCredentials(t *testing.T) {
	out, status := RedactWriteOnlyCredentials(service.AccountTypeAPIKey, nil, nil)

	require.Nil(t, out)
	require.Nil(t, status)
}

func TestAccountFromServiceShallow_RedactsAPIKeyUpstreamEndpoint(t *testing.T) {
	src := &service.Account{
		ID:       7,
		Name:     "relay",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       "sk-secret",
			"base_url":      "https://relay.example.com/v1",
			"model_mapping": map[string]any{"foo": "bar"},
		},
	}

	got := AccountFromServiceShallow(src)

	require.NotContains(t, got.Credentials, "base_url")
	require.NotContains(t, got.Credentials, "api_key")
	require.Equal(t, map[string]any{"foo": "bar"}, got.Credentials["model_mapping"])
	require.True(t, got.CredentialsStatus["has_base_url"])
	require.True(t, got.CredentialsStatus["has_api_key"])

	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "relay.example.com")
	require.NotContains(t, string(raw), "sk-secret")

	// 原始 service.Account 不应被改动
	require.Equal(t, "https://relay.example.com/v1", src.Credentials["base_url"])
}

func TestAccountFromServiceShallow_KeepsOAuthBaseURL(t *testing.T) {
	src := &service.Account{
		ID:       8,
		Name:     "grok-oauth",
		Platform: service.PlatformGrok,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "at-secret",
			"base_url":     "https://cli-chat-proxy.grok.com/v1",
		},
	}

	got := AccountFromServiceShallow(src)

	require.Equal(t, "https://cli-chat-proxy.grok.com/v1", got.Credentials["base_url"])
	require.False(t, got.CredentialsStatus["has_base_url"])
}
