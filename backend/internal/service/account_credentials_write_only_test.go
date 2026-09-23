package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteOnlyCredentialKeysFor(t *testing.T) {
	require.Equal(t, []string{"base_url"}, WriteOnlyCredentialKeysFor(AccountTypeAPIKey))
	// OAuth / upstream 的 base_url 允许前端回显（含"清除自定义端点"语义），不参与只写保留。
	require.Empty(t, WriteOnlyCredentialKeysFor(AccountTypeOAuth))
	require.Empty(t, WriteOnlyCredentialKeysFor(AccountTypeUpstream))
	require.Empty(t, WriteOnlyCredentialKeysFor(""))
}

func TestMergePreservingSensitiveCreds_PreservesWriteOnlyBaseURL(t *testing.T) {
	existing := map[string]any{
		"api_key":  "sk-old",
		"base_url": "https://relay.example.com/v1",
	}
	// 前端只写不读：应答里没有 base_url，留空提交时 payload 也不会带该键。
	incoming := map[string]any{
		"model_mapping": map[string]any{"foo": "bar"},
	}

	out := MergePreservingSensitiveCreds(existing, incoming, WriteOnlyCredentialKeysFor(AccountTypeAPIKey)...)

	require.Equal(t, "https://relay.example.com/v1", out["base_url"], "incoming 没传 base_url，应保留 existing")
	require.Equal(t, "sk-old", out["api_key"])
	require.Equal(t, map[string]any{"foo": "bar"}, out["model_mapping"])
}

func TestMergePreservingSensitiveCreds_OverwritesWriteOnlyBaseURLWhenProvided(t *testing.T) {
	existing := map[string]any{"base_url": "https://old.example.com/v1"}
	incoming := map[string]any{"base_url": "https://new.example.com/v1"}

	out := MergePreservingSensitiveCreds(existing, incoming, "base_url")

	require.Equal(t, "https://new.example.com/v1", out["base_url"], "incoming 显式传入应覆盖")
}

func TestMergePreservingSensitiveCreds_WriteOnlyKeysOptIn(t *testing.T) {
	// 不传只写键的调用方语义不变：base_url 由 incoming 决定（删键即清除）。
	existing := map[string]any{"base_url": "https://old.example.com/v1"}
	incoming := map[string]any{"model_mapping": map[string]any{"foo": "bar"}}

	out := MergePreservingSensitiveCreds(existing, incoming)

	require.NotContains(t, out, "base_url")
}
