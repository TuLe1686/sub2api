package service

// SensitiveCredentialKeys 列出 Account.Credentials JSON map 中绝不允许返回到前端的子键。
// dto 层做响应脱敏、service 层做更新合并都引用此清单——新增凭证类型时务必同步。
var SensitiveCredentialKeys = []string{
	// OAuth
	"access_token", "refresh_token", "id_token", "agent_private_key",
	// API Key 类
	"api_key", "session_key", "cookie",
	// Grok Web SSO / password (must never persist or echo after Build OAuth)
	"password", "sso_token", "sso", "sso-rw", "clearTextPassword",
	// 云服务凭据
	"aws_secret_access_key", "aws_session_token",
	"service_account_json", "service_account", "private_key",
}

var sensitiveCredentialKeySet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(SensitiveCredentialKeys))
	for _, k := range SensitiveCredentialKeys {
		m[k] = struct{}{}
	}
	return m
}()

// IsSensitiveCredentialKey 判断指定键是否为敏感凭证子键。
func IsSensitiveCredentialKey(key string) bool {
	_, ok := sensitiveCredentialKeySet[key]
	return ok
}

// MergePreservingSensitiveCreds 把 incoming 写入 existing 之上，但敏感子键（以及调用方通过
// writeOnlyKeys 传入的"只写"子键）采用"incoming 没提供就保留 existing"的语义。返回新的 map，
// 不修改入参。
//
// 用途：前端编辑账号通常采用"全对象 PUT"模式；脱敏后前端 spread 旧 credentials 时不会带上敏感键，
// 直接覆盖会清空已有 token。此函数保证：
//   - 非敏感键：完全由 incoming 决定（用户可以编辑、删除非敏感字段）。
//   - 敏感键 / 只写键：incoming 显式提供则覆盖（用户主动旋转 token 或改写上游端点），否则保留 existing。
func MergePreservingSensitiveCreds(existing, incoming map[string]any, writeOnlyKeys ...string) map[string]any {
	out := make(map[string]any, len(incoming)+len(SensitiveCredentialKeys)+len(writeOnlyKeys))
	for k, v := range incoming {
		out[k] = v
	}
	preserveMissingKeys(out, existing, incoming, SensitiveCredentialKeys)
	preserveMissingKeys(out, existing, incoming, writeOnlyKeys)
	return out
}

// preserveMissingKeys 把 existing 中 incoming 未显式提供的 keys 补回 out。
func preserveMissingKeys(out, existing, incoming map[string]any, keys []string) {
	for _, key := range keys {
		if _, hasIncoming := incoming[key]; hasIncoming {
			continue
		}
		if existingVal, ok := existing[key]; ok {
			out[key] = existingVal
		}
	}
}

// WriteOnlyCredentialKeysFor 返回该账号类型下"只写不读"的凭据子键：响应不下发原文（只给
// has_<key> 存在性），更新时 incoming 未提供即保留原值——与 api_key 采用同一存储方式。
//
// 目前只有 API Key 账号的 base_url（上游端点：面板与接口都不再暴露转发目标）。
// OAuth / upstream 账号的 base_url 仍是普通可读编辑字段（前端回显、清除即删键），
// 不能并入此表，否则"取消自定义端点"会失效。
func WriteOnlyCredentialKeysFor(accountType string) []string {
	if accountType == AccountTypeAPIKey {
		return []string{"base_url"}
	}
	return nil
}
