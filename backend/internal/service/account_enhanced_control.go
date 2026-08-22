// Package service
//
// 账号增强控制补丁（account-enhanced-control）
//
// 在补丁1（TTFT 首字计时口径）和补丁2（定时测试超时保护）基础上叠加，
// 为每个启用账号提供两块独立配置：
//
//  1. 缓存率改写：在 usage 写入 DB 前，按账号配置改写 cache_read_tokens 和
//     input_tokens，使上报的缓存率（cache_read/(cache_read+input)）呈现设定值。
//     cache_creation_tokens 保持真实不动（它单独计费）。
//     - 方式1 随机减少：真实缓存率 × (1 - 随机1~30%) → 反推 cache_read/input
//     - 方式2 固定值：直接设 0~95% → 反推 cache_read/input
//     - 关闭：不改写
//     约束：cache_read + input 的和不变（cache_creation 不参与，单独计费不动），
//     只在两者之间重新分配。
//
//  2. 首字时长改写：在 first_token_ms 记录完真实值（TTFT 补丁口径）之后，
//     按账号配置直接覆盖 first_token_ms 为设定值。usage_logs 存改写后值，
//     不保留真实值。
//     - 方式1 随机首字：500~3000ms 随机值（记录点可直接算）
//     - 方式2 比例首字：总耗时×10% + 随机300~1000ms（需总耗时，请求结束算）
//     - 关闭：不改写
//
// 配置存储：Account.extra JSONB，键 "enhanced_control"。
package service

import (
	"math/rand"
	"time"
)

// EnhancedControlKey 是 Account.extra 里存放增强配置的键名。
const EnhancedControlKey = "enhanced_control"

// CacheRateMode 缓存率改写模式。
type CacheRateMode string

const (
	CacheRateModeOff      CacheRateMode = "off"      // 关闭，不改写
	CacheRateModeRandom   CacheRateMode = "random"   // 随机减少 1~30%
	CacheRateModeFixed    CacheRateMode = "fixed"    // 固定值 0~95%
)

// TTFTMode 首字时长改写模式。
type TTFTMode string

const (
	TTFTModeOff       TTFTMode = "off"       // 关闭
	TTFTModeRandom    TTFTMode = "random"    // 500~3000ms 随机
	TTFTModeProportional TTFTMode = "proportional" // 总耗时×10% + 随机300~1000ms
)

// EnhancedControl 存放在 Account.extra["enhanced_control"] 的配置结构。
type EnhancedControl struct {
	CacheRate CacheRateSetting `json:"cache_rate,omitempty"`
	TTFT      TTFTSetting      `json:"ttft,omitempty"`
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

// CacheRateOverrideResult 是缓存率改写的输出。
type CacheRateOverrideResult struct {
	Changed       bool
	NewCacheRead int
	NewInput     int
}

// ParseEnhancedControl 从 Account.extra 解析增强配置。
// extra 为 nil 或无配置时返回全零值（等价于全关闭）。
func ParseEnhancedControl(extra map[string]any) EnhancedControl {
	if extra == nil {
		return EnhancedControl{}
	}
	raw, ok := extra[EnhancedControlKey]
	if !ok || raw == nil {
		return EnhancedControl{}
	}
	// 直接断言为 map[string]any（JSON 反序列化结果）
	m, ok := raw.(map[string]any)
	if !ok {
		return EnhancedControl{}
	}
	return EnhancedControl{
		CacheRate: parseCacheRateSetting(m["cache_rate"]),
		TTFT:      parseTTFTSetting(m["ttft"]),
	}
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
		Changed:       true,
		NewCacheRead: newCacheRead,
		NewInput:     newInput,
	}
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
