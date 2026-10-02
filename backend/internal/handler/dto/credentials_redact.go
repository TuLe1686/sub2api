// Package dto provides data transfer objects for HTTP handlers.
package dto

import (
	"slices"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// RedactCredentials 复制一份 in，剥离 service.SensitiveCredentialKeys 列出的所有敏感子键，
// 并产出一个 has_<key> 状态 map 表示哪些敏感键存在且非零值。
//
// 输入 nil 时返回 nil, nil（避免响应里出现空对象）。
// 不修改入参；调用方拿到的 out 可安全序列化进 JSON 返回前端。
func RedactCredentials(in map[string]any) (out map[string]any, status map[string]bool) {
	if in == nil {
		return nil, nil
	}
	out = make(map[string]any, len(in))
	for k, v := range in {
		if service.IsSensitiveCredentialKey(k) {
			if isCredentialValuePresent(v) {
				if status == nil {
					status = make(map[string]bool, 4)
				}
				status["has_"+k] = true
			}
			continue
		}
		out[k] = v
	}
	return out, status
}

// RedactWriteOnlyCredentials 按账号类型剥离"只写"子键（如 API Key 账号的 base_url）。
// 语义与 SensitiveCredentialKeys 一致：原文不下发前端，存在性通过 status 的 has_<key> 暴露。
// 非该类型的账号原样返回（OAuth / upstream 的 base_url 仍要回显给编辑界面）。
func RedactWriteOnlyCredentials(accountType string, creds map[string]any, status map[string]bool) (map[string]any, map[string]bool) {
	keys := service.WriteOnlyCredentialKeysFor(accountType)
	if len(keys) == 0 || creds == nil {
		return creds, status
	}
	out := make(map[string]any, len(creds))
	for k, v := range creds {
		if !slices.Contains(keys, k) {
			out[k] = v
			continue
		}
		if isCredentialValuePresent(v) {
			if status == nil {
				status = make(map[string]bool, len(keys))
			}
			status["has_"+k] = true
		}
	}
	return out, status
}

// isCredentialValuePresent 判断值是否"存在且非零"。空字符串、nil、false 均视为未配置；
// 其余非零类型（数字、对象、字符串等）视为已配置。
func isCredentialValuePresent(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	default:
		return true
	}
}
