package service

// 本文件实现 Responses 上游的缓存亲和注入（prompt-cache-affinity 补丁）。
//
// 背景：部分上游中转站（实测 callai / 灵绘）的 prompt 缓存亲和只看请求体里的
// prompt_cache_key；客户端不带该字段时，同一会话的连续请求会被轮换到不同后端，
// 缓存命中率塌到 0～20%（同账号 Codex 客户端 88%+）。ZCode 3.14/3.15 的
// /v1/responses 请求体不含该字段，但会带稳定的 X-Session-Id 头（网关已把它用作
// 会话信号与 usage_logs.session_id），于是在最终上游请求体缺少 prompt_cache_key
// 时用它派生一个按 API Key 隔离的稳定键注入。
// 上游直连探针（2026-10-12，账号 2307 / callai）：同一段 7921 token 请求 4 连发，
// 不带 key 时 cached=0/0/0/0，带固定 key 时 0/7680/7680/7680。
//
// 门控：
//   - 仅 OpenAI 平台 API Key 账号。OAuth/Codex 路径由客户端自带 key 与指纹收敛
//     （applyCodexFingerprintPromptCacheKey）负责，不在此重复介入；
//   - 仅非 compact 路径。compact 请求体有独立字段白名单，
//     normalizeOpenAICompactRequestBody 会丢弃 prompt_cache_key；
//   - 请求体已有非空 key 时绝不覆盖（客户端/Codex 拥有该值）。
//
// 兼容性兜底：上游若把该字段当未知参数拒绝，openai_responses_rejected_field_retry
// 会删除后重试（与 max_output_tokens / truncation 同机制），因此对「不认识该字段」
// 的上游只会多一次重试，不会把请求打失败。

import (
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// responsesPromptCacheAffinityPrefix 标识网关派生键：与客户端自带值、Codex 收敛值
// 区分开，便于上游日志与两侧排障识别（上游只把它当路由提示，不回显）。
const responsesPromptCacheAffinityPrefix = "s2a_aff_"

// injectResponsesPromptCacheAffinity 按文件头注释的门控注入 prompt_cache_key。
// 返回注入后的 body 与是否发生改动；任何不满足门控或写入失败的情况都原样返回，
// 绝不阻断请求本身。
func (s *OpenAIGatewayService) injectResponsesPromptCacheAffinity(c *gin.Context, account *Account, body []byte) ([]byte, bool) {
	if c == nil || len(body) == 0 {
		return body, false
	}
	if account == nil || !account.IsOpenAIApiKey() {
		return body, false
	}
	if isOpenAIResponsesCompactPath(c) {
		return body, false
	}
	if strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()) != "" {
		return body, false
	}
	// 会话种子只用客户端显式会话头解析（body 无 key 已在上方确认），
	// sanitizeSessionID 拒绝控制字符与超长值：宁可不注入，也不向上游发不稳定键。
	sessionID := sanitizeSessionID(s.ExtractSessionID(c, body))
	if sessionID == "" {
		return body, false
	}
	key := responsesPromptCacheAffinityPrefix + isolateOpenAISessionID(getAPIKeyIDFromContext(c), sessionID)
	next, err := sjson.SetBytes(body, "prompt_cache_key", key)
	if err != nil {
		// 注入失败不影响请求本身：宁可少一次缓存亲和，也不让请求失败。
		logger.LegacyPrintf("service.openai_gateway",
			"[OpenAI] 跳过 responses prompt_cache_key 注入（写入失败，account: %s）: %v", account.Name, err)
		return body, false
	}
	logger.LegacyPrintf("service.openai_gateway",
		"[OpenAI] 已注入 responses prompt_cache_key（缓存亲和，account: %s, key_sha256: %s)",
		account.Name, hashSensitiveValueForLog(key))
	return next, true
}
