// Package service
//
// 账号增强控制补丁（account-enhanced-control）
//
// 在补丁1（TTFT 首字计时口径）和补丁2（定时测试超时保护）基础上叠加，
// 为每个启用账号提供三块独立配置：
//
//  1. 缓存率改写：在计费与 usage 写入 DB 之前，按账号配置改写 cache_read_tokens 和
//     input_tokens，使上报的缓存率（cache_read/(cache_read+input)）呈现设定值。
//     cache_creation_tokens 保持真实不动（它单独计费）。
//     - 方式1 随机减少：真实缓存率 × (1 - 随机1~30%) → 反推 cache_read/input
//     - 方式2 固定值：直接设 0~95% → 反推 cache_read/input
//     - 关闭：不改写
//     约束：cache_read + input 的和不变（cache_creation 不参与，单独计费不动），
//     只在两者之间重新分配。
//     改写后的拆分同时用于**用户扣费**与 usage_logs；账号统计成本（上游成本口径）
//     默认仍使用改写前的真实 token，避免把售价改写混进利润核算。
//
//  4. 账号统计口径：默认 raw（账号计费走渠道统计规则 / 模型定价文件，账号额度按
//     真实 token × 渠道售价还原）；billed 时账号计费与账号额度都改用本次请求的
//     total_cost（改写后 token × 渠道售价，倍率前）再乘账号倍率，两者口径一致。
//
//  2. 首字时长改写：在 first_token_ms 记录完真实值（TTFT 补丁口径）之后，
//     按账号配置直接覆盖 first_token_ms 为设定值。usage_logs 存改写后值，
//     不保留真实值。
//     - 方式1 随机首字：500~3000ms 随机值（记录点可直接算）
//     - 方式2 比例首字：总耗时×10% + 随机300~1000ms（需总耗时，请求结束算）
//     - 关闭：不改写
//
//  3. 思考强度强制（reasoning effort）：在请求体发往上游之前，按账号配置的
//     「模型 ID → 强度」规则注入或改写 reasoning.effort（Responses 形态）/
//     reasoning_effort（Chat Completions 形态）。
//     - override：无条件覆盖客户端值
//     - fill：仅当客户端未显式传 effort 时注入
//     - cap：封顶。客户端档位高于规则命中值时降为命中值，不高于则原样
//     透传；usage 的 requested_reasoning_effort 保持客户端原值
//     - off：不改写
//     规则支持 '*' 通配（如 grok-*、*），精确命中优先于通配命中，
//     多个通配命中时取更具体（非通配字符更多）的一条。
//     注入发生在分组策略之后，故账号级配置最终生效（账号级优先）。
//
// 配置存储：Account.extra JSONB，键 "enhanced_control"。
package service

import (
	"math/rand"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/apicompat"
	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EnhancedControlKey 是 Account.extra 里存放增强配置的键名。
const EnhancedControlKey = "enhanced_control"

// CacheRateMode 缓存率改写模式。
type CacheRateMode string

const (
	CacheRateModeOff    CacheRateMode = "off"    // 关闭，不改写
	CacheRateModeRandom CacheRateMode = "random" // 随机减少 1~30%
	CacheRateModeFixed  CacheRateMode = "fixed"  // 固定值 0~95%
)

// TTFTMode 首字时长改写模式。
type TTFTMode string

const (
	TTFTModeOff          TTFTMode = "off"          // 关闭
	TTFTModeRandom       TTFTMode = "random"       // 500~3000ms 随机
	TTFTModeProportional TTFTMode = "proportional" // 总耗时×10% + 随机300~1000ms
)

// AccountStatsMode 账号统计（账号计费 / 账号额度）的计价口径。
type AccountStatsMode string

const (
	AccountStatsModeRaw    AccountStatsMode = "raw"    // 默认：统计规则 / 定价文件 + 真实 token 额度
	AccountStatsModeBilled AccountStatsMode = "billed" // 改写后 token × 渠道售价，账号计费 = 账号额度
)

// AccountStatsSetting 账号统计口径配置；未写或非法值按 raw 处理。
type AccountStatsSetting struct {
	Mode AccountStatsMode `json:"mode,omitempty"`
}

// EnhancedControl 存放在 Account.extra["enhanced_control"] 的配置结构。
type EnhancedControl struct {
	CacheRate CacheRateSetting `json:"cache_rate,omitempty"`
	TTFT      TTFTSetting      `json:"ttft,omitempty"`
	// ReasoningEffort 思考强度强制配置（本补丁第三块）。
	ReasoningEffort ReasoningEffortSetting `json:"reasoning_effort,omitempty"`
	AccountStats    AccountStatsSetting    `json:"account_stats"`
}

// CacheRateSetting 缓存率改写配置。
//
// 约定：缓存率 = cache_read_tokens / (cache_read_tokens + input_tokens)，
// 其中 input_tokens 是纯未命中值（usage_logs 里已扣减 cache_read 和
// cache_creation，参见 responses_to_anthropic.go:108）。cache_creation_tokens
// 不参与缓存率，保持真实不动（单独计费）。
//
// 改写约束：cache_read + input 的和不变，只在两者间重新分配。
type CacheRateSetting struct {
	// Mode: off | random | fixed
	Mode CacheRateMode `json:"mode,omitempty"`
	// RandomReduceMin / RandomReduceMax：方式1 随机减少的区间（百分比，1~30）。
	// 每次请求在此区间随机取一个减少比例。
	RandomReduceMin int `json:"random_reduce_min,omitempty"`
	RandomReduceMax int `json:"random_reduce_max,omitempty"`
	// FixedValue：方式2 固定值（百分比，0~95）。
	FixedValue int `json:"fixed_value,omitempty"`
}

// TTFTSetting 首字时长改写配置。
//
// 改写时机：方式1（random）在 first_token_ms 记录点直接覆盖；
// 方式2（proportional）在请求结束、组装 usage 写入前覆盖（因需总耗时）。
// usage_logs 存改写后值，不保留真实值。
type TTFTSetting struct {
	// Mode: off | random | proportional
	Mode TTFTMode `json:"mode,omitempty"`
}

// ReasoningEffortMode 思考强度强制模式。
type ReasoningEffortMode string

const (
	ReasoningEffortModeOff      ReasoningEffortMode = "off"      // 关闭，不改写
	ReasoningEffortModeOverride ReasoningEffortMode = "override" // 无条件覆盖客户端值
	ReasoningEffortModeFill     ReasoningEffortMode = "fill"     // 仅当客户端未显式传 effort 时注入
	ReasoningEffortModeCap      ReasoningEffortMode = "cap"      // 封顶：仅当客户端档位高于规则命中值时降为命中值
)

// ReasoningEffortRule 一条「模型 ID → 强度」规则。
// Model 支持 '*' 通配（如 "grok-*"、"*-fast"、"*"），大小写不敏感。
type ReasoningEffortRule struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// ReasoningEffortSetting 思考强度强制配置。
//
// 匹配对象是「客户端请求的模型名」优先，其次「账号映射后的模型名」；
// 两者都会额外尝试去掉 "provider/" 前缀的末段名。
type ReasoningEffortSetting struct {
	// Mode: off | override | fill | cap；未写或非法值一律按 off 处理。
	Mode ReasoningEffortMode `json:"mode,omitempty"`
	// Rules 按书写顺序保存；精确匹配优先于通配匹配。
	// cap 模式下 Effort 语义为「该模型的上限值」。
	Rules []ReasoningEffortRule `json:"rules,omitempty"`
}

// CacheRateOverrideResult 是缓存率改写的输出。
type CacheRateOverrideResult struct {
	Changed      bool
	NewCacheRead int
	NewInput     int
}

// ParseEnhancedControl 从 Account.extra 解析增强配置。
// extra 为 nil 或无配置时前三项关闭、账号统计口径为 raw。
func ParseEnhancedControl(extra map[string]any) EnhancedControl {
	defaults := EnhancedControl{AccountStats: AccountStatsSetting{Mode: AccountStatsModeRaw}}
	if extra == nil {
		return defaults
	}
	raw, ok := extra[EnhancedControlKey]
	if !ok || raw == nil {
		return defaults
	}
	// 直接断言为 map[string]any（JSON 反序列化结果）
	m, ok := raw.(map[string]any)
	if !ok {
		return defaults
	}
	return EnhancedControl{
		CacheRate:       parseCacheRateSetting(m["cache_rate"]),
		TTFT:            parseTTFTSetting(m["ttft"]),
		ReasoningEffort: parseReasoningEffortSetting(m["reasoning_effort"]),
		AccountStats:    parseAccountStatsSetting(m["account_stats"]),
	}
}

// NormalizeAccountStatsMode 只认 billed，其余（含空值）一律回到 raw。
func NormalizeAccountStatsMode(mode AccountStatsMode) AccountStatsMode {
	if AccountStatsMode(strings.ToLower(strings.TrimSpace(string(mode)))) == AccountStatsModeBilled {
		return AccountStatsModeBilled
	}
	return AccountStatsModeRaw
}

func parseAccountStatsSetting(raw any) AccountStatsSetting {
	s := AccountStatsSetting{Mode: AccountStatsModeRaw}
	if m, ok := raw.(map[string]any); ok {
		if v, ok := m["mode"].(string); ok {
			s.Mode = NormalizeAccountStatsMode(AccountStatsMode(v))
		}
	}
	return s
}

// ResolveEnhancedAccountStatsCosts 决定账号统计成本与账号额度的计价基数（均为倍率前）。
//
// rawQuotaTotal 是缓存率改写后按上游真实拆分还原的金额；未改写时为 nil。
// 返回值：
//   - statsTotal：传给账号统计定价链的 totalCost（渠道「应用模型定价到账号统计」时使用）
//   - quotaTotal：覆盖账号额度累计金额，nil 表示沿用 cost.TotalCost
//   - fixedStatsCost：非 nil 时直接写入 usage_logs.account_stats_cost，跳过统计定价链
//
// billed 口径三者都取 cost.TotalCost（改写后 token × 渠道售价），保证
// account_stats_cost × 账号倍率 与账号额度扣减逐笔相等。
func ResolveEnhancedAccountStatsCosts(account *Account, cost *CostBreakdown, rawQuotaTotal *float64) (statsTotal float64, quotaTotal *float64, fixedStatsCost *float64) {
	if cost == nil {
		return 0, rawQuotaTotal, nil
	}
	if account != nil && ParseEnhancedControl(account.Extra).AccountStats.Mode == AccountStatsModeBilled {
		billed := cost.TotalCost
		return billed, nil, &billed
	}
	statsTotal = cost.TotalCost
	if rawQuotaTotal != nil {
		statsTotal = *rawQuotaTotal
	}
	return statsTotal, rawQuotaTotal, nil
}

func parseCacheRateSetting(raw any) CacheRateSetting {
	s := CacheRateSetting{}
	if raw == nil {
		return s
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return s
	}
	if v, ok := m["mode"].(string); ok {
		s.Mode = CacheRateMode(v)
	}
	s.RandomReduceMin = parseFloatToInt(m["random_reduce_min"])
	s.RandomReduceMax = parseFloatToInt(m["random_reduce_max"])
	s.FixedValue = parseFloatToInt(m["fixed_value"])
	// 规范化：确保区间合法
	if s.RandomReduceMin < 1 {
		s.RandomReduceMin = 1
	}
	if s.RandomReduceMax > 30 {
		s.RandomReduceMax = 30
	}
	if s.RandomReduceMin > s.RandomReduceMax {
		s.RandomReduceMin, s.RandomReduceMax = s.RandomReduceMax, s.RandomReduceMin
	}
	if s.FixedValue < 0 {
		s.FixedValue = 0
	}
	if s.FixedValue > 95 {
		s.FixedValue = 95
	}
	return s
}

func parseTTFTSetting(raw any) TTFTSetting {
	s := TTFTSetting{}
	if raw == nil {
		return s
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return s
	}
	if v, ok := m["mode"].(string); ok {
		s.Mode = TTFTMode(v)
	}
	return s
}

// parseFloatToInt 把 JSON 数字（float64）或 int 安全转成 int。
func parseFloatToInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// ApplyCacheRateOverride 按账号缓存率配置改写 cache_read_tokens 和 input_tokens。
//
// 输入是真实统计值：
//   - cacheRead: 命中缓存 token 数（要改）
//   - input: 未命中 token 数（要改，与 cacheRead 反向）
//   - cacheCreation: 写入缓存 token 数（不动，单独计费）
//
// 约束：cacheRead + input 的和不变，只在两者间重新分配。
// cache_creation 不参与缓存率公式，也不改。
//
// 返回改写后的 cacheRead 和 input；配置关闭或无效时返回 Changed=false。
func ApplyCacheRateOverride(cfg CacheRateSetting, cacheRead, input, cacheCreation int) CacheRateOverrideResult {
	if cfg.Mode == CacheRateModeOff || cfg.Mode == "" {
		return CacheRateOverrideResult{Changed: false}
	}

	// 缓存率公式：cacheRead / (cacheRead + input)
	// 约束：cacheRead + input = S（不变），cacheCreation 不动
	// 目标缓存率 P → newCacheRead = S × P, newInput = S × (1-P)
	S := cacheRead + input
	if S <= 0 {
		return CacheRateOverrideResult{Changed: false}
	}

	var targetPct float64 // 目标缓存率，0~1
	switch cfg.Mode {
	case CacheRateModeRandom:
		// 真实缓存率
		realPct := float64(cacheRead) / float64(S)
		// 随机减少比例（1~30%）
		minPct := clampInt(cfg.RandomReduceMin, 1, 30)
		maxPct := clampInt(cfg.RandomReduceMax, 1, 30)
		if minPct > maxPct {
			minPct, maxPct = maxPct, minPct
		}
		reduce := float64(minPct) + rand.Float64()*float64(maxPct-minPct)
		reduceFrac := reduce / 100.0
		targetPct = realPct * (1 - reduceFrac)
		// 不会小于 0
		if targetPct < 0 {
			targetPct = 0
		}
	case CacheRateModeFixed:
		targetPct = float64(clampInt(cfg.FixedValue, 0, 95)) / 100.0
	default:
		return CacheRateOverrideResult{Changed: false}
	}

	newCacheRead := int(float64(S) * targetPct)
	newInput := S - newCacheRead
	if newCacheRead < 0 {
		newCacheRead = 0
	}
	if newInput < 0 {
		newInput = 0
	}
	// 如果改写后与原值一致（例如真实就是目标值），标记为未改写
	if newCacheRead == cacheRead && newInput == input {
		return CacheRateOverrideResult{Changed: false}
	}
	return CacheRateOverrideResult{
		Changed:      true,
		NewCacheRead: newCacheRead,
		NewInput:     newInput,
	}
}

// ApplyEnhancedCacheRateOverride 在计费前按账号配置改写缓存拆分。
//
// 返回值是改写后的 cache_read / input 以及是否发生变化；cache_creation 不参与。
// 调用方应把返回值同时用于**费用计算**与 usage_logs，未改写时保持原值。
func ApplyEnhancedCacheRateOverride(account *Account, cacheRead, input, cacheCreation int) (newCacheRead, newInput int, changed bool) {
	if account == nil {
		return cacheRead, input, false
	}
	cfg := ParseEnhancedControl(account.Extra).CacheRate
	cr := ApplyCacheRateOverride(cfg, cacheRead, input, cacheCreation)
	if !cr.Changed {
		return cacheRead, input, false
	}
	return cr.NewCacheRead, cr.NewInput, true
}

// ApplyEnhancedCacheRateToUsage 在计费前按账号配置改写 usage 的缓存拆分。
//
// 该函数必须在计算费用之前调用：改写后的 cache_read/input 会同时决定
// 用户扣费（input 与 cache_read 单价不同）和 usage_logs 展示值；
// cache_creation 保持不变，cache_read + input 的总和也不变。
// 调用方若还需要上游真实 token（如账号统计成本），应在调用前自行快照。
//
// 返回是否发生了改写；account 为 nil 或配置关闭时返回 false。
func ApplyEnhancedCacheRateToUsage(account *Account, usage *ClaudeUsage) bool {
	if usage == nil {
		return false
	}
	newCacheRead, newInput, changed := ApplyEnhancedCacheRateOverride(
		account, usage.CacheReadInputTokens, usage.InputTokens, usage.CacheCreationInputTokens)
	if !changed {
		return false
	}
	usage.CacheReadInputTokens = newCacheRead
	usage.InputTokens = newInput
	return true
}

// AccountQuotaTotalCostForCacheSplit 返回按上游真实拆分计算的费用总额（账号额度口径）。
//
// cost 必须是用改写后拆分算出的账单；newInput/newCacheRead 是该账单使用的拆分，
// rawInput/rawCacheRead 是上游真实拆分。缓存率改写只重分配 input/cache_read 且
// 两者总和不变，因此按分项单价线性还原即可；长上下文等按输入总量判定的档位不受影响。
// 账号额度/账号统计成本使用本函数还原后的金额，避免缓存率改写放大上游消耗。
//
// cost 为 nil 时返回 0；还原结果小于 0 时按 0 处理。
func AccountQuotaTotalCostForCacheSplit(cost *CostBreakdown, newInput, newCacheRead, rawInput, rawCacheRead int) float64 {
	if cost == nil {
		return 0
	}
	total := cost.TotalCost
	if newInput > 0 {
		total -= float64(newInput-rawInput) * (cost.InputCost / float64(newInput))
	}
	if newCacheRead > 0 {
		total -= float64(newCacheRead-rawCacheRead) * (cost.CacheReadCost / float64(newCacheRead))
	}
	if total < 0 {
		return 0
	}
	return total
}

// ApplyTTFTRandomOverride 在 first_token_ms 记录点按方式1（500~3000ms 随机）覆盖。
//
// 方式1 不依赖总耗时，记录点可直接算。
// 返回改写后的首字时长（毫秒）；配置关闭或非方式1时返回 ok=false。
func ApplyTTFTRandomOverride(cfg TTFTSetting) (ms int, ok bool) {
	if cfg.Mode != TTFTModeRandom {
		return 0, false
	}
	// 500~3000ms 随机
	ms = 500 + rand.Intn(2501) // 2501 = 3000-500+1
	return ms, true
}

// ApplyTTFTProportionalOverride 在请求结束、组装 usage 前按方式2覆盖。
//
// 方式2：首字 = 总耗时(ms) × 10% + 随机300~1000ms。
// 需要总耗时，故只能在请求结束后调用。
// 返回改写后的首字时长（毫秒）；配置关闭或非方式2时返回 ok=false。
func ApplyTTFTProportionalOverride(cfg TTFTSetting, totalDurationMs int) (ms int, ok bool) {
	if cfg.Mode != TTFTModeProportional {
		return 0, false
	}
	base := float64(totalDurationMs) * 0.10
	jitter := 300 + rand.Intn(701) // 701 = 1000-300+1
	ms = int(base) + jitter
	return ms, true
}

// ApplyTTFTOverride 按配置选择首字时长改写方式；totalDurationMs 供比例模式使用。
// 返回改写后的毫秒值与是否命中方式。
func ApplyTTFTOverride(cfg TTFTSetting, totalDurationMs int) (ms int, ok bool) {
	switch cfg.Mode {
	case TTFTModeRandom:
		return ApplyTTFTRandomOverride(cfg)
	case TTFTModeProportional:
		return ApplyTTFTProportionalOverride(cfg, totalDurationMs)
	default:
		return 0, false
	}
}

// clampInt 把 v 限制在 [lo, hi] 区间。
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// init 仅确保 math/rand 可用（Go 1.20+ 的 rand 全局源已自动播种，
// 这里留个显式初始化兜底，便于老版本兼容与可读性）。
func init() {
	rand.Seed(time.Now().UnixNano())
}

// ──────────────────────────────────────────────────────────────────────────
// 第三块：思考强度强制（reasoning effort）
// ──────────────────────────────────────────────────────────────────────────

// reasoningEffortAllowedValues 是允许注入的强度值白名单。
// 与上游 reasoning effort 口径一致，额外接受 "extrahigh" 并归一为 "xhigh"。
var reasoningEffortAllowedValues = map[string]string{
	"minimal":   "minimal",
	"low":       "low",
	"medium":    "medium",
	"high":      "high",
	"xhigh":     "xhigh",
	"extrahigh": "xhigh",
	"max":       "max",
}

// normalizeReasoningEffortValue 归一化强度值；不在白名单内返回空串。
func normalizeReasoningEffortValue(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	value = strings.NewReplacer("-", "", "_", "", " ", "").Replace(value)
	if canonical, ok := reasoningEffortAllowedValues[value]; ok {
		return canonical
	}
	return ""
}

// parseReasoningEffortSetting 解析第三块配置；缺省或非法一律按关闭处理。
func parseReasoningEffortSetting(raw any) ReasoningEffortSetting {
	// Mode 始终归一到 off/override/fill 三者之一，便于调用方直接比较。
	set := ReasoningEffortSetting{Mode: ReasoningEffortModeOff}
	if raw == nil {
		return set
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return set
	}

	switch mode, _ := m["mode"].(string); ReasoningEffortMode(strings.ToLower(strings.TrimSpace(mode))) {
	case ReasoningEffortModeOverride:
		set.Mode = ReasoningEffortModeOverride
	case ReasoningEffortModeFill:
		set.Mode = ReasoningEffortModeFill
	case ReasoningEffortModeCap:
		set.Mode = ReasoningEffortModeCap
	default:
		set.Mode = ReasoningEffortModeOff
	}

	rules, _ := m["rules"].([]any)
	for _, rawRule := range rules {
		ruleMap, ok := rawRule.(map[string]any)
		if !ok {
			continue
		}
		model, _ := ruleMap["model"].(string)
		effort, _ := ruleMap["effort"].(string)
		model = strings.TrimSpace(model)
		normalizedEffort := normalizeReasoningEffortValue(effort)
		// 模型名或强度非法时丢弃该条，避免把空值注入请求体。
		if model == "" || normalizedEffort == "" {
			continue
		}
		set.Rules = append(set.Rules, ReasoningEffortRule{
			Model:  model,
			Effort: normalizedEffort,
		})
	}
	return set
}

// reasoningEffortWildcardMatch 做大小写不敏感的 '*' 通配匹配（不支持 '?'）。
func reasoningEffortWildcardMatch(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	segments := strings.Split(pattern, "*")
	if len(segments) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, segments[0]) {
		return false
	}
	value = value[len(segments[0]):]
	for _, segment := range segments[1 : len(segments)-1] {
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}
	last := segments[len(segments)-1]
	if last == "" {
		return true
	}
	return strings.HasSuffix(value, last)
}

// MatchReasoningEffortRule 按模型名候选匹配规则，返回应使用的强度。
//
// 优先顺序：
//  1. 精确命中（任一候选命中即返回，规则按书写顺序）；
//  2. 通配命中（多个命中取"更具体"的一条：非通配字符更多者优先）。
func MatchReasoningEffortRule(set ReasoningEffortSetting, models []string) (string, bool) {
	if len(set.Rules) == 0 {
		return "", false
	}

	normalizedModels := make([]string, 0, len(models))
	for _, model := range models {
		if trimmed := strings.ToLower(strings.TrimSpace(model)); trimmed != "" {
			normalizedModels = append(normalizedModels, trimmed)
		}
	}
	if len(normalizedModels) == 0 {
		return "", false
	}

	for _, model := range normalizedModels {
		for _, rule := range set.Rules {
			pattern := strings.ToLower(strings.TrimSpace(rule.Model))
			if strings.Contains(pattern, "*") {
				continue
			}
			if pattern == model {
				return rule.Effort, true
			}
		}
	}

	bestSpecificity := -1
	bestEffort := ""
	for _, model := range normalizedModels {
		for _, rule := range set.Rules {
			pattern := strings.ToLower(strings.TrimSpace(rule.Model))
			if !strings.Contains(pattern, "*") {
				continue
			}
			if !reasoningEffortWildcardMatch(pattern, model) {
				continue
			}
			specificity := len(strings.ReplaceAll(pattern, "*", ""))
			if specificity > bestSpecificity {
				bestSpecificity = specificity
				bestEffort = rule.Effort
			}
		}
	}
	if bestSpecificity >= 0 {
		return bestEffort, true
	}
	return "", false
}

// ApplyReasoningEffortOverride 按账号配置在请求体上注入或改写思考强度。
//
// responsesShape=true 写 reasoning.effort（Responses 形态），
// responsesShape=false 写 reasoning_effort（Chat Completions 形态）。
// 返回改写后的 body 与是否发生了变化；未命中规则或无需改动时原样返回。
func ApplyReasoningEffortOverride(body []byte, set ReasoningEffortSetting, models []string, responsesShape bool) ([]byte, bool) {
	path := reasoningEffortBodyPath(responsesShape)
	clientExplicit := strings.TrimSpace(gjson.GetBytes(body, path).String()) != ""
	return applyReasoningEffortCore(body, path, set, models, clientExplicit)
}

// reasoningEffortBodyPath 返回对应请求体形态下承载思考强度的字段路径。
func reasoningEffortBodyPath(responsesShape bool) string {
	if responsesShape {
		return "reasoning.effort"
	}
	return "reasoning_effort"
}

// applyReasoningEffortCore 是注入的统一实现。
//
// clientExplicit 表示「客户端是否显式指定过 effort」，仅在 fill 模式下参与判断：
//   - 普通入口：等于 body 里是否已有该字段；
//   - Anthropic 入口：必须回看客户端原始请求（桥接会合成一个默认值）；
//   - WS 帧：等于客户端帧里是否带了该字段。
func applyReasoningEffortCore(body []byte, path string, set ReasoningEffortSetting, models []string, clientExplicit bool) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	if !reasoningEffortModeActive(set.Mode) {
		return body, false
	}

	effort, matched := MatchReasoningEffortRule(set, models)
	if !matched || effort == "" {
		return body, false
	}

	current := strings.TrimSpace(gjson.GetBytes(body, path).String())
	if set.Mode == ReasoningEffortModeCap {
		// 封顶：客户端档位高于上限才降档，其余（未传/不识别/不高于）一律透传。
		currentRank, recognized := reasoningEffortRank(current)
		capRank, capValid := reasoningEffortRank(effort)
		if !recognized || !capValid || currentRank <= capRank {
			return body, false
		}
		updated, err := sjson.SetBytes(body, path, effort)
		if err != nil {
			return body, false
		}
		logger.LegacyPrintf("service.gateway", "Account enhanced effort capped: %s -> %s", current, effort)
		return updated, true
	}
	// 已经是目标值：无需改动，避免无意义重写请求体。
	if strings.EqualFold(current, effort) {
		return body, false
	}
	// 补缺省模式下客户端已显式传值：尊重客户端，不注入。
	if set.Mode == ReasoningEffortModeFill && clientExplicit {
		return body, false
	}

	updated, err := sjson.SetBytes(body, path, effort)
	if err != nil {
		return body, false
	}
	return updated, true
}

// reasoningEffortModeActive 判断模式是否会产生注入。
func reasoningEffortModeActive(mode ReasoningEffortMode) bool {
	switch mode {
	case ReasoningEffortModeOverride, ReasoningEffortModeFill, ReasoningEffortModeCap:
		return true
	default:
		return false
	}
}

// enhancedReasoningEffortModelCandidates 组装规则匹配用的模型名候选。
//
// 顺序：客户端请求的模型名 → 账号映射后的模型名；
// 每个候选都会额外尝试去掉 "provider/" 前缀后的末段名。
func enhancedReasoningEffortModelCandidates(account *Account, body []byte) []string {
	candidates := make([]string, 0, 4)
	appendCandidate := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		candidates = append(candidates, model)
		if idx := strings.LastIndex(model, "/"); idx >= 0 {
			if tail := strings.TrimSpace(model[idx+1:]); tail != "" {
				candidates = append(candidates, tail)
			}
		}
	}

	requestedModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	appendCandidate(requestedModel)
	if requestedModel != "" {
		appendCandidate(account.GetMappedModel(requestedModel))
	}
	return candidates
}

// enhancedReasoningEffortApplies 判断账号是否配置了生效中的思考强度强制。
func enhancedReasoningEffortApplies(account *Account) (ReasoningEffortSetting, bool) {
	if account == nil {
		return ReasoningEffortSetting{}, false
	}
	set := ParseEnhancedControl(account.Extra).ReasoningEffort
	if !reasoningEffortModeActive(set.Mode) {
		return set, false
	}
	if len(set.Rules) == 0 {
		return set, false
	}
	return set, true
}

// requestedReasoningEffortForUsageLog 决定写入 usage_logs.requested_reasoning_effort 的值。
//
// 账号开启 override/fill 思考强度强制时，用户用量页只展示 requested 档。为了和
// 客户端原生传 xhigh 的请求看起来一样，这里改存转发后的档位（不保留客户端原值）。
// cap（封顶）不在此列：使用记录保留客户端原值（如 max），便于从日志区分
// 「客户端要了什么」与「实际转发档位」（reasoning_effort 列）。
func requestedReasoningEffortForUsageLog(account *Account, requested, forwarded *string) *string {
	set, ok := enhancedReasoningEffortApplies(account)
	if ok && set.Mode != ReasoningEffortModeCap {
		if trimmed := optionalStringValue(forwarded); trimmed != "" {
			return &trimmed
		}
	}
	return coalesceRequestedReasoningEffort(requested, forwarded)
}

// applyEnhancedReasoningEffortCore 是各入口共用的内部实现。
//
// clientExplicitOverride 为 nil 时，「客户端是否显式传值」由 body 自身推断；
// Anthropic 入口需要传入由客户端原始请求推断出的值（桥接会合成默认值）。
func applyEnhancedReasoningEffortCore(account *Account, body []byte, responsesShape bool, clientExplicitOverride *bool) ([]byte, bool) {
	if account == nil || len(body) == 0 {
		return body, false
	}
	set, ok := enhancedReasoningEffortApplies(account)
	if !ok {
		return body, false
	}

	path := reasoningEffortBodyPath(responsesShape)
	clientExplicit := strings.TrimSpace(gjson.GetBytes(body, path).String()) != ""
	if clientExplicitOverride != nil {
		clientExplicit = *clientExplicitOverride
	}
	return applyReasoningEffortCore(body, path, set, enhancedReasoningEffortModelCandidates(account, body), clientExplicit)
}

// ApplyEnhancedReasoningEffortForAccount 是 /v1/responses 链路使用的薄封装。
func ApplyEnhancedReasoningEffortForAccount(account *Account, body []byte, responsesShape bool) []byte {
	if updated, changed := applyEnhancedReasoningEffortCore(account, body, responsesShape, nil); changed {
		return updated
	}
	return body
}

// ApplyEnhancedReasoningEffortForChatEntry 处理 /v1/chat/completions 入口。
//
// 该入口的 body 可能是 Chat Completions 形态，也可能已经是 Responses 形态
// （部分客户端把 Responses body 发到该端点），此处按形态选择写入字段。
func ApplyEnhancedReasoningEffortForChatEntry(account *Account, body []byte) []byte {
	responsesShape := !gjson.GetBytes(body, "messages").Exists() && gjson.GetBytes(body, "input").Exists()
	if updated, changed := applyEnhancedReasoningEffortCore(account, body, responsesShape, nil); changed {
		return updated
	}
	return body
}

// applyEnhancedChatReasoningEffortAfterGrokEligibility 先用原始 body 判定
// Grok OAuth 能否走 Responses 桥，再注入思考强度。
//
// 顺序不能反：注入 reasoning_effort 会让 grokChatResponsesBridgeEligibility
// 返回 unsupported_reasoning_effort，把本可走桥的请求打到 raw CC
// （工具 / 图片 / 缓存语义都会变）。桥接转换会把 reasoning_effort 映射成
// reasoning.effort；composer 不支持的值仍由后续 sanitize 剥离。
func applyEnhancedChatReasoningEffortAfterGrokEligibility(account *Account, body []byte) (injected []byte, grokBridgeEligible bool, grokBridgeReason string) {
	if account != nil && account.Platform == PlatformGrok && account.IsGrokOAuth() {
		grokBridgeEligible, grokBridgeReason = grokChatResponsesBridgeEligibility(body)
	}
	return ApplyEnhancedReasoningEffortForChatEntry(account, body), grokBridgeEligible, grokBridgeReason
}

// ApplyEnhancedReasoningEffortForAnthropicEntry 处理 Anthropic /v1/messages 入口。
//
// convertedBody 是已转换成 Responses（或 Chat Completions）形态的请求体，
// anthropicBody 是客户端原始 Anthropic 请求体。
// Anthropic 桥接会在客户端未传 effort 时合成默认值，因此 fill 模式的
// 「客户端是否显式传值」必须回看原始请求的 output_config.effort。
func ApplyEnhancedReasoningEffortForAnthropicEntry(account *Account, convertedBody, anthropicBody []byte, responsesShape bool) ([]byte, bool) {
	clientExplicit := strings.TrimSpace(gjson.GetBytes(anthropicBody, "output_config.effort").String()) != ""
	return applyEnhancedReasoningEffortCore(account, convertedBody, responsesShape, &clientExplicit)
}

// syncEnhancedResponsesReasoningEffort 把 body 里的 reasoning.effort 写回结构体。
//
// /v1/messages 出站走 JSON body，但计费从 responsesReq.Reasoning.Effort 读取。
// 若只在 Reasoning != nil 时回写，桥接没建该字段、注入又是新建时，
// usage 会丢掉刚注入的强度。nil 时分配新结构体，避免这条缝。
func syncEnhancedResponsesReasoningEffort(reasoning *apicompat.ResponsesReasoning, body []byte) *apicompat.ResponsesReasoning {
	effort := strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String())
	if effort == "" {
		return reasoning
	}
	if reasoning == nil {
		reasoning = &apicompat.ResponsesReasoning{}
	}
	reasoning.Effort = effort
	return reasoning
}

// ApplyEnhancedReasoningEffortForWSFrame 处理 WebSocket 入站帧
// （Responses 形态的 response.create payload）。
func ApplyEnhancedReasoningEffortForWSFrame(account *Account, payload []byte) []byte {
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType != "" && eventType != "response.create" {
		return payload
	}
	if updated, changed := applyEnhancedReasoningEffortCore(account, payload, true, nil); changed {
		return updated
	}
	return payload
}

// enhancedEffortForAnthropicOutputConfig 把补丁白名单档位映射成 Claude
// output_config.effort。xhigh 对应 Claude 的 max；minimal 落到 low。
//
// cap（封顶）模式不用这张映射：封顶值必须保持补丁档位语义（cap=xhigh 就是
// xhigh），否则「xhigh→max」换算会让上限变成 max，封不住。Claude 5.5 系
// 原生接受 low/medium/high/xhigh/max，国产直通按此透传。
func enhancedEffortForAnthropicOutputConfig(effort string) string {
	switch normalizeReasoningEffortValue(effort) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "max":
		return "max"
	case "xhigh":
		return "max"
	case "minimal":
		return "low"
	default:
		return ""
	}
}

// capEffortForAnthropicOutputConfig 是 cap 模式的国产 Anthropic 直通档位换算。
//
// minimal 不在 Claude effort 白名单内，封顶到 minimal 时落 low；
// 其余档位（low/medium/high/xhigh/max）原样透传。
func capEffortForAnthropicOutputConfig(effort string) string {
	switch normalizeReasoningEffortValue(effort) {
	case "low", "medium", "high", "xhigh", "max":
		return normalizeReasoningEffortValue(effort)
	case "minimal":
		return "low"
	default:
		return ""
	}
}

// ApplyEnhancedReasoningEffortForNativeAnthropic 处理国产 Anthropic 直通
// （/v1/messages 零转换）。写入 output_config.effort，fill 回看该字段。
//
// 返回值只保留 body 版本；需要知道是否发生改写时用
// ApplyEnhancedReasoningEffortForNativeAnthropicWithResult。
func ApplyEnhancedReasoningEffortForNativeAnthropic(account *Account, body []byte) []byte {
	updated, _ := ApplyEnhancedReasoningEffortForNativeAnthropicWithResult(account, body)
	return updated
}

// ApplyEnhancedReasoningEffortForNativeAnthropicWithResult 同上，额外返回是否改写。
//
// Anthropic 网关（GatewayService.Messages）也走这条实现：在请求体发往
// 上游之前按账号规则改写 output_config.effort，cap 模式只降不升。
func ApplyEnhancedReasoningEffortForNativeAnthropicWithResult(account *Account, body []byte) ([]byte, bool) {
	if account == nil || len(body) == 0 {
		return body, false
	}
	set, ok := enhancedReasoningEffortApplies(account)
	if !ok {
		return body, false
	}
	effort, matched := MatchReasoningEffortRule(set, enhancedReasoningEffortModelCandidates(account, body))
	if !matched {
		return body, false
	}
	// cap 模式保持补丁档位语义（xhigh 就是 xhigh，不做 xhigh→max 换算），
	// 否则上限会被抬到 max，封顶失效；override/fill 沿用既有映射。
	claudeEffort := enhancedEffortForAnthropicOutputConfig(effort)
	if set.Mode == ReasoningEffortModeCap {
		claudeEffort = capEffortForAnthropicOutputConfig(effort)
	}
	if claudeEffort == "" {
		return body, false
	}
	clientExplicit := strings.TrimSpace(gjson.GetBytes(body, "output_config.effort").String()) != ""
	mapped := ReasoningEffortSetting{
		Mode:  set.Mode,
		Rules: []ReasoningEffortRule{{Model: "*", Effort: claudeEffort}},
	}
	if updated, changed := applyReasoningEffortCore(body, "output_config.effort", mapped, []string{"*"}, clientExplicit); changed {
		return updated, true
	}
	return body, false
}
