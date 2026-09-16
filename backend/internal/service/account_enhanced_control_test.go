package service

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
)

// ──────────────────────────────────────────────────────────────────────────
// 缓存率改写测试
// ──────────────────────────────────────────────────────────────────────────

func TestApplyCacheRateOverride_Off(t *testing.T) {
	cfg := CacheRateSetting{Mode: CacheRateModeOff}
	r := ApplyCacheRateOverride(cfg, 8000, 2000, 1000)
	if r.Changed {
		t.Fatalf("off mode should not change: got %+v", r)
	}
}

func TestApplyCacheRateOverride_EmptyMode(t *testing.T) {
	cfg := CacheRateSetting{}
	r := ApplyCacheRateOverride(cfg, 8000, 2000, 1000)
	if r.Changed {
		t.Fatalf("empty mode should not change: got %+v", r)
	}
}

func TestApplyCacheRateOverride_Fixed(t *testing.T) {
	// 真实: cache_read=8000, input=2000 → 缓存率 80%
	// 目标: 固定 50% → cache_read=5000, input=5000
	// cache_creation=1000 不参与、不动
	cfg := CacheRateSetting{Mode: CacheRateModeFixed, FixedValue: 50}
	r := ApplyCacheRateOverride(cfg, 8000, 2000, 1000)
	if !r.Changed {
		t.Fatal("fixed 50% should change")
	}
	if r.NewCacheRead != 5000 {
		t.Errorf("NewCacheRead = %d, want 5000", r.NewCacheRead)
	}
	if r.NewInput != 5000 {
		t.Errorf("NewInput = %d, want 5000", r.NewInput)
	}
	// 和不变
	if r.NewCacheRead+r.NewInput != 8000+2000 {
		t.Errorf("sum changed: %d vs %d", r.NewCacheRead+r.NewInput, 8000+2000)
	}
}

func TestApplyCacheRateOverride_FixedZero(t *testing.T) {
	// 固定 0% → 全部算未命中
	cfg := CacheRateSetting{Mode: CacheRateModeFixed, FixedValue: 0}
	r := ApplyCacheRateOverride(cfg, 8000, 2000, 1000)
	if !r.Changed {
		t.Fatal("fixed 0% should change")
	}
	if r.NewCacheRead != 0 {
		t.Errorf("NewCacheRead = %d, want 0", r.NewCacheRead)
	}
	if r.NewInput != 10000 {
		t.Errorf("NewInput = %d, want 10000", r.NewInput)
	}
}

func TestApplyCacheRateOverride_Fixed95(t *testing.T) {
	// 固定 95% → 95% 命中
	cfg := CacheRateSetting{Mode: CacheRateModeFixed, FixedValue: 95}
	r := ApplyCacheRateOverride(cfg, 8000, 2000, 1000)
	if !r.Changed {
		t.Fatal("fixed 95% should change")
	}
	expected := int(float64(10000) * 0.95)
	if r.NewCacheRead != expected {
		t.Errorf("NewCacheRead = %d, want %d", r.NewCacheRead, expected)
	}
}

func TestApplyCacheRateOverride_Random(t *testing.T) {
	// 随机减少 10~30%，真实 80%
	// 减少比例随机 ∈ [10%, 30%] → 目标 ∈ [56%, 72%]
	cfg := CacheRateSetting{
		Mode:            CacheRateModeRandom,
		RandomReduceMin: 10,
		RandomReduceMax: 30,
	}
	r := ApplyCacheRateOverride(cfg, 8000, 2000, 1000)
	if !r.Changed {
		t.Fatal("random should change")
	}
	// 和不变
	sum := r.NewCacheRead + r.NewInput
	if sum != 10000 {
		t.Errorf("sum = %d, want 10000", sum)
	}
	// 新缓存率 ∈ [56%, 72%]
	pct := float64(r.NewCacheRead) / float64(sum)
	if pct < 0.55 || pct > 0.73 {
		t.Errorf("cache rate %.4f out of [0.56, 0.72]", pct)
	}
}

func TestApplyCacheRateOverride_RandomBounds(t *testing.T) {
	// 多次运行确保不越界
	cfg := CacheRateSetting{
		Mode:            CacheRateModeRandom,
		RandomReduceMin: 1,
		RandomReduceMax: 30,
	}
	for i := 0; i < 1000; i++ {
		r := ApplyCacheRateOverride(cfg, 8000, 2000, 1000)
		if r.NewCacheRead < 0 || r.NewInput < 0 {
			t.Fatalf("negative tokens: %+v", r)
		}
		if r.NewCacheRead+r.NewInput != 10000 {
			t.Fatalf("sum changed: %d", r.NewCacheRead+r.NewInput)
		}
	}
}

func TestApplyCacheRateOverride_ZeroSum(t *testing.T) {
	// cache_read=0, input=0 → 不改写
	cfg := CacheRateSetting{Mode: CacheRateModeFixed, FixedValue: 50}
	r := ApplyCacheRateOverride(cfg, 0, 0, 1000)
	if r.Changed {
		t.Fatal("zero sum should not change")
	}
}

func TestApplyCacheRateOverride_FixedValueEqualsReal(t *testing.T) {
	// 真实 80%，目标也 80% → Changed=false
	cfg := CacheRateSetting{Mode: CacheRateModeFixed, FixedValue: 80}
	r := ApplyCacheRateOverride(cfg, 8000, 2000, 1000)
	if r.Changed {
		t.Fatalf("same as real should not change: %+v", r)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 首字时长改写测试
// ──────────────────────────────────────────────────────────────────────────

func TestApplyTTFTRandomOverride(t *testing.T) {
	cfg := TTFTSetting{Mode: TTFTModeRandom}
	for i := 0; i < 1000; i++ {
		ms, ok := ApplyTTFTRandomOverride(cfg)
		if !ok {
			t.Fatal("random mode should return ok=true")
		}
		if ms < 500 || ms > 3000 {
			t.Fatalf("ms %d out of [500, 3000]", ms)
		}
	}
}

func TestApplyTTFTRandomOverride_Off(t *testing.T) {
	cfg := TTFTSetting{Mode: TTFTModeOff}
	_, ok := ApplyTTFTRandomOverride(cfg)
	if ok {
		t.Fatal("off mode should return ok=false")
	}
}

func TestApplyTTFTProportionalOverride(t *testing.T) {
	cfg := TTFTSetting{Mode: TTFTModeProportional}
	totalMs := 10000 // 10 秒
	for i := 0; i < 1000; i++ {
		ms, ok := ApplyTTFTProportionalOverride(cfg, totalMs)
		if !ok {
			t.Fatal("proportional mode should return ok=true")
		}
		// base = 10000 * 0.10 = 1000, jitter ∈ [300, 1000]
		// 结果 ∈ [1300, 2000]
		if ms < 1300 || ms > 2000 {
			t.Fatalf("ms %d out of [1300, 2000]", ms)
		}
	}
}

func TestApplyTTFTProportionalOverride_Off(t *testing.T) {
	cfg := TTFTSetting{Mode: TTFTModeOff}
	_, ok := ApplyTTFTProportionalOverride(cfg, 5000)
	if ok {
		t.Fatal("off mode should return ok=false")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 配置解析测试
// ──────────────────────────────────────────────────────────────────────────

func TestParseEnhancedControl_Nil(t *testing.T) {
	cfg := ParseEnhancedControl(nil)
	if cfg.CacheRate.Mode != "" {
		t.Errorf("nil extra should give empty mode")
	}
}

func TestParseEnhancedControl_NoKey(t *testing.T) {
	extra := map[string]any{"other_key": "value"}
	cfg := ParseEnhancedControl(extra)
	if cfg.CacheRate.Mode != "" {
		t.Errorf("missing key should give empty mode")
	}
}

func TestParseEnhancedControl_Full(t *testing.T) {
	extra := map[string]any{
		"enhanced_control": map[string]any{
			"cache_rate": map[string]any{
				"mode":              "fixed",
				"random_reduce_min": float64(5),
				"random_reduce_max": float64(20),
				"fixed_value":       float64(50),
			},
			"ttft": map[string]any{
				"mode": "random",
			},
		},
	}
	cfg := ParseEnhancedControl(extra)
	if cfg.CacheRate.Mode != CacheRateModeFixed {
		t.Errorf("cache rate mode = %s, want fixed", cfg.CacheRate.Mode)
	}
	if cfg.CacheRate.FixedValue != 50 {
		t.Errorf("fixed value = %d, want 50", cfg.CacheRate.FixedValue)
	}
	if cfg.CacheRate.RandomReduceMin != 5 {
		t.Errorf("random min = %d, want 5", cfg.CacheRate.RandomReduceMin)
	}
	if cfg.CacheRate.RandomReduceMax != 20 {
		t.Errorf("random max = %d, want 20", cfg.CacheRate.RandomReduceMax)
	}
	if cfg.TTFT.Mode != TTFTModeRandom {
		t.Errorf("ttft mode = %s, want random", cfg.TTFT.Mode)
	}
}

func TestParseEnhancedControl_Clamping(t *testing.T) {
	// 超界值应被钳位
	extra := map[string]any{
		"enhanced_control": map[string]any{
			"cache_rate": map[string]any{
				"mode":              "fixed",
				"fixed_value":       float64(150), // > 95 → 95
				"random_reduce_min": float64(-5),  // < 1 → 1
				"random_reduce_max": float64(50),  // > 30 → 30
			},
		},
	}
	cfg := ParseEnhancedControl(extra)
	if cfg.CacheRate.FixedValue != 95 {
		t.Errorf("fixed value = %d, want 95 (clamped)", cfg.CacheRate.FixedValue)
	}
	if cfg.CacheRate.RandomReduceMin != 1 {
		t.Errorf("random min = %d, want 1 (clamped)", cfg.CacheRate.RandomReduceMin)
	}
	if cfg.CacheRate.RandomReduceMax != 30 {
		t.Errorf("random max = %d, want 30 (clamped)", cfg.CacheRate.RandomReduceMax)
	}
}

func TestParseEnhancedControl_MinMaxSwap(t *testing.T) {
	// min > max 应自动交换
	extra := map[string]any{
		"enhanced_control": map[string]any{
			"cache_rate": map[string]any{
				"mode":              "random",
				"random_reduce_min": float64(25),
				"random_reduce_max": float64(10),
			},
		},
	}
	cfg := ParseEnhancedControl(extra)
	if cfg.CacheRate.RandomReduceMin != 10 {
		t.Errorf("random min = %d, want 10 (swapped)", cfg.CacheRate.RandomReduceMin)
	}
	if cfg.CacheRate.RandomReduceMax != 25 {
		t.Errorf("random max = %d, want 25 (swapped)", cfg.CacheRate.RandomReduceMax)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 随机性分布均匀性测试（宽松）
// ──────────────────────────────────────────────────────────────────────────

func TestApplyTTFTRandomOverride_Distribution(t *testing.T) {
	// 10000 次采样，确认值落在区间内且分布大致均匀
	cfg := TTFTSetting{Mode: TTFTModeRandom}
	buckets := [5]int{} // [500-999, 1000-1499, 1500-1999, 2000-2499, 2500-3000]
	for i := 0; i < 10000; i++ {
		ms, _ := ApplyTTFTRandomOverride(cfg)
		var idx int
		switch {
		case ms < 1000:
			idx = 0
		case ms < 1500:
			idx = 1
		case ms < 2000:
			idx = 2
		case ms < 2500:
			idx = 3
		default:
			idx = 4
		}
		buckets[idx]++
	}
	// 每个桶至少应占 10%
	for i, count := range buckets {
		if count < 1000 {
			t.Errorf("bucket %d too low: %d", i, count)
		}
	}
	_ = math.Sqrt // silence unused import if any
}

// ──────────────────────────────────────────────────────────────────────────
// 思考强度强制（reasoning effort）测试
// ──────────────────────────────────────────────────────────────────────────

func TestNormalizeReasoningEffortValue(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"xhigh", "xhigh"},
		{"XHigh", "xhigh"},
		{"extra-high", "xhigh"},
		{"extrahigh", "xhigh"},
		{"  high  ", "high"},
		{"max", "max"},
		{"minimal", "minimal"},
		{"none", ""},
		{"bogus", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeReasoningEffortValue(tc.in); got != tc.want {
			t.Errorf("normalizeReasoningEffortValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseReasoningEffortSetting(t *testing.T) {
	set := parseReasoningEffortSetting(map[string]any{
		"mode": "Fill",
		"rules": []any{
			map[string]any{"model": "grok-4.5", "effort": "xhigh"},
			map[string]any{"model": "grok-*", "effort": "extra-high"},
			map[string]any{"model": "", "effort": "high"},       // 丢弃：模型为空
			map[string]any{"model": "gpt-5", "effort": "bogus"}, // 丢弃：强度非法
			"not-a-map", // 丢弃：元素类型不对
		},
	})
	if set.Mode != ReasoningEffortModeFill {
		t.Fatalf("mode = %q, want %q", set.Mode, ReasoningEffortModeFill)
	}
	if len(set.Rules) != 2 {
		t.Fatalf("rules = %d, want 2 (got %+v)", len(set.Rules), set.Rules)
	}
	if set.Rules[0].Model != "grok-4.5" || set.Rules[0].Effort != "xhigh" {
		t.Errorf("rule[0] = %+v, want {grok-4.5 xhigh}", set.Rules[0])
	}
	if set.Rules[1].Effort != "xhigh" {
		t.Errorf("rule[1].Effort = %q, want xhigh (extra-high 归一)", set.Rules[1].Effort)
	}
}

func TestParseReasoningEffortSetting_DefaultsToOff(t *testing.T) {
	cases := []struct {
		name string
		raw  any
	}{
		{"nil", nil},
		{"not a map", "xhigh"},
		{"missing mode", map[string]any{"rules": []any{}}},
		{"illegal mode", map[string]any{"mode": "force"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseReasoningEffortSetting(tc.raw); got.Mode != ReasoningEffortModeOff {
				t.Errorf("mode = %q, want off", got.Mode)
			}
		})
	}
}

func TestMatchReasoningEffortRule_ExactBeatsWildcard(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode: ReasoningEffortModeOverride,
		Rules: []ReasoningEffortRule{
			{Model: "grok-*", Effort: "low"},
			{Model: "grok-4.5", Effort: "xhigh"},
		},
	}
	effort, ok := MatchReasoningEffortRule(set, []string{"grok-4.5"})
	if !ok || effort != "xhigh" {
		t.Fatalf("got (%q, %v), want (xhigh, true)", effort, ok)
	}
}

func TestMatchReasoningEffortRule_Specificity(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode: ReasoningEffortModeOverride,
		Rules: []ReasoningEffortRule{
			{Model: "*", Effort: "low"},
			{Model: "grok-4*", Effort: "high"},
		},
	}
	effort, ok := MatchReasoningEffortRule(set, []string{"grok-4.5"})
	if !ok || effort != "high" {
		t.Fatalf("got (%q, %v), want (high, true) — 更具体的通配应优先", effort, ok)
	}
}

func TestMatchReasoningEffortRule_SuffixWildcard(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode:  ReasoningEffortModeOverride,
		Rules: []ReasoningEffortRule{{Model: "*-fast", Effort: "low"}},
	}
	if effort, ok := MatchReasoningEffortRule(set, []string{"grok-composer-2.5-fast"}); !ok || effort != "low" {
		t.Fatalf("got (%q, %v), want (low, true)", effort, ok)
	}
}

func TestMatchReasoningEffortRule_CaseInsensitive(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode:  ReasoningEffortModeOverride,
		Rules: []ReasoningEffortRule{{Model: "GROK-4.5", Effort: "xhigh"}},
	}
	if effort, ok := MatchReasoningEffortRule(set, []string{"grok-4.5"}); !ok || effort != "xhigh" {
		t.Fatalf("got (%q, %v), want (xhigh, true)", effort, ok)
	}
}

func TestMatchReasoningEffortRule_NoMatch(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode:  ReasoningEffortModeOverride,
		Rules: []ReasoningEffortRule{{Model: "grok-4.5", Effort: "xhigh"}},
	}
	if effort, ok := MatchReasoningEffortRule(set, []string{"gpt-5.5"}); ok {
		t.Fatalf("got (%q, true), want no match", effort)
	}
	if _, ok := MatchReasoningEffortRule(set, nil); ok {
		t.Fatal("empty candidate list must not match")
	}
}

func TestApplyReasoningEffortOverride_Override(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode:  ReasoningEffortModeOverride,
		Rules: []ReasoningEffortRule{{Model: "grok-4.5", Effort: "xhigh"}},
	}
	body := []byte(`{"model":"grok-4.5","reasoning":{"effort":"low"}}`)
	got, changed := ApplyReasoningEffortOverride(body, set, []string{"grok-4.5"}, true)
	if !changed {
		t.Fatal("override should change")
	}
	if v := gjson.GetBytes(got, "reasoning.effort").String(); v != "xhigh" {
		t.Errorf("reasoning.effort = %q, want xhigh", v)
	}
}

func TestApplyReasoningEffortOverride_OverrideCreatesField(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode:  ReasoningEffortModeOverride,
		Rules: []ReasoningEffortRule{{Model: "grok-4.5", Effort: "xhigh"}},
	}
	got, changed := ApplyReasoningEffortOverride([]byte(`{"model":"grok-4.5"}`), set, []string{"grok-4.5"}, true)
	if !changed {
		t.Fatal("override should inject when field is absent")
	}
	if v := gjson.GetBytes(got, "reasoning.effort").String(); v != "xhigh" {
		t.Errorf("reasoning.effort = %q, want xhigh", v)
	}
}

func TestApplyReasoningEffortOverride_FillOnlyWhenAbsent(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode:  ReasoningEffortModeFill,
		Rules: []ReasoningEffortRule{{Model: "grok-4.5", Effort: "xhigh"}},
	}

	// 缺省 → 注入
	got, changed := ApplyReasoningEffortOverride([]byte(`{"model":"grok-4.5"}`), set, []string{"grok-4.5"}, true)
	if !changed || gjson.GetBytes(got, "reasoning.effort").String() != "xhigh" {
		t.Fatalf("fill on absent field: changed=%v body=%s", changed, got)
	}

	// 客户端已显式传值 → 尊重客户端
	body := []byte(`{"model":"grok-4.5","reasoning":{"effort":"low"}}`)
	got, changed = ApplyReasoningEffortOverride(body, set, []string{"grok-4.5"}, true)
	if changed {
		t.Fatalf("fill must not override client value, got %s", got)
	}
}

func TestApplyReasoningEffortOverride_Off(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode:  ReasoningEffortModeOff,
		Rules: []ReasoningEffortRule{{Model: "grok-4.5", Effort: "xhigh"}},
	}
	body := []byte(`{"model":"grok-4.5"}`)
	if _, changed := ApplyReasoningEffortOverride(body, set, []string{"grok-4.5"}, true); changed {
		t.Fatal("off mode must not change body")
	}
}

func TestApplyReasoningEffortOverride_SameValueNoChange(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode:  ReasoningEffortModeOverride,
		Rules: []ReasoningEffortRule{{Model: "grok-4.5", Effort: "xhigh"}},
	}
	body := []byte(`{"model":"grok-4.5","reasoning":{"effort":"xhigh"}}`)
	if _, changed := ApplyReasoningEffortOverride(body, set, []string{"grok-4.5"}, true); changed {
		t.Fatal("already target value must not report change")
	}
}

func TestApplyReasoningEffortOverride_ChatShape(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode:  ReasoningEffortModeOverride,
		Rules: []ReasoningEffortRule{{Model: "grok-4.5", Effort: "xhigh"}},
	}
	got, changed := ApplyReasoningEffortOverride([]byte(`{"model":"grok-4.5"}`), set, []string{"grok-4.5"}, false)
	if !changed {
		t.Fatal("chat shape should inject reasoning_effort")
	}
	if v := gjson.GetBytes(got, "reasoning_effort").String(); v != "xhigh" {
		t.Errorf("reasoning_effort = %q, want xhigh", v)
	}
	if gjson.GetBytes(got, "reasoning.effort").Exists() {
		t.Error("chat shape must not write reasoning.effort")
	}
}

func TestApplyReasoningEffortOverride_EmptyBody(t *testing.T) {
	set := ReasoningEffortSetting{
		Mode:  ReasoningEffortModeOverride,
		Rules: []ReasoningEffortRule{{Model: "*", Effort: "xhigh"}},
	}
	if _, changed := ApplyReasoningEffortOverride(nil, set, []string{"grok-4.5"}, true); changed {
		t.Fatal("empty body must not change")
	}
}

func TestApplyEnhancedReasoningEffortForAccount(t *testing.T) {
	account := &Account{
		Platform:    PlatformGrok,
		Credentials: map[string]any{},
		Extra: map[string]any{
			EnhancedControlKey: map[string]any{
				"reasoning_effort": map[string]any{
					"mode": "override",
					"rules": []any{
						map[string]any{"model": "grok-4.5", "effort": "xhigh"},
					},
				},
			},
		},
	}

	got := ApplyEnhancedReasoningEffortForAccount(account, []byte(`{"model":"grok-4.5"}`), true)
	if v := gjson.GetBytes(got, "reasoning.effort").String(); v != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh (body=%s)", v, got)
	}

	// 未命中模型 → 原样
	body := []byte(`{"model":"grok-3"}`)
	if got := ApplyEnhancedReasoningEffortForAccount(account, body, true); string(got) != string(body) {
		t.Fatalf("unmatched model must pass through, got %s", got)
	}

	// nil account / 无配置账号 → 原样
	if got := ApplyEnhancedReasoningEffortForAccount(nil, body, true); string(got) != string(body) {
		t.Fatalf("nil account must pass through, got %s", got)
	}
	empty := &Account{Platform: PlatformGrok, Credentials: map[string]any{}}
	if got := ApplyEnhancedReasoningEffortForAccount(empty, body, true); string(got) != string(body) {
		t.Fatalf("account without config must pass through, got %s", got)
	}
}

func TestApplyEnhancedReasoningEffortForAccount_ProviderPrefix(t *testing.T) {
	account := &Account{
		Platform:    PlatformGrok,
		Credentials: map[string]any{},
		Extra: map[string]any{
			EnhancedControlKey: map[string]any{
				"reasoning_effort": map[string]any{
					"mode": "fill",
					"rules": []any{
						map[string]any{"model": "grok-4.5", "effort": "xhigh"},
					},
				},
			},
		},
	}
	got := ApplyEnhancedReasoningEffortForAccount(account, []byte(`{"model":"xai/grok-4.5"}`), true)
	if v := gjson.GetBytes(got, "reasoning.effort").String(); v != "xhigh" {
		t.Fatalf("provider-prefixed model should match last segment, got %q (body=%s)", v, got)
	}
}

func TestApplyEnhancedReasoningEffortForAccount_MappedModelFallback(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"my-alias": "grok-4.5"},
		},
		Extra: map[string]any{
			EnhancedControlKey: map[string]any{
				"reasoning_effort": map[string]any{
					"mode": "override",
					"rules": []any{
						map[string]any{"model": "grok-4.5", "effort": "xhigh"},
					},
				},
			},
		},
	}
	got := ApplyEnhancedReasoningEffortForAccount(account, []byte(`{"model":"my-alias"}`), true)
	if v := gjson.GetBytes(got, "reasoning.effort").String(); v != "xhigh" {
		t.Fatalf("mapped model should be used as fallback candidate, got %q (body=%s)", v, got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 阶段二：CC 入口 / Anthropic 入口 / WS 帧
// ──────────────────────────────────────────────────────────────────────────

func overrideEffortAccount(mode ReasoningEffortMode, model, effort string) *Account {
	return &Account{
		Platform:    PlatformGrok,
		Credentials: map[string]any{},
		Extra: map[string]any{
			EnhancedControlKey: map[string]any{
				"reasoning_effort": map[string]any{
					"mode":  string(mode),
					"rules": []any{map[string]any{"model": model, "effort": effort}},
				},
			},
		},
	}
}

func TestApplyEnhancedReasoningEffortForChatEntry_ShapeDetection(t *testing.T) {
	account := overrideEffortAccount(ReasoningEffortModeOverride, "*", "xhigh")

	// Chat Completions 形态（有 messages）→ reasoning_effort
	got := ApplyEnhancedReasoningEffortForChatEntry(account, []byte(`{"model":"grok-4.5","messages":[]}`))
	if v := gjson.GetBytes(got, "reasoning_effort").String(); v != "xhigh" {
		t.Errorf("chat shape: reasoning_effort = %q, want xhigh (body=%s)", v, got)
	}
	if gjson.GetBytes(got, "reasoning.effort").Exists() {
		t.Error("chat shape must not write reasoning.effort")
	}

	// Responses 形态（有 input、无 messages）→ reasoning.effort
	got = ApplyEnhancedReasoningEffortForChatEntry(account, []byte(`{"model":"grok-4.5","input":[]}`))
	if v := gjson.GetBytes(got, "reasoning.effort").String(); v != "xhigh" {
		t.Errorf("responses shape: reasoning.effort = %q, want xhigh (body=%s)", v, got)
	}
	if gjson.GetBytes(got, "reasoning_effort").Exists() {
		t.Error("responses shape must not write reasoning_effort")
	}

	// 无配置账号 → 原样
	plain := &Account{Platform: PlatformGrok, Credentials: map[string]any{}}
	body := []byte(`{"model":"grok-4.5","messages":[]}`)
	if got := ApplyEnhancedReasoningEffortForChatEntry(plain, body); string(got) != string(body) {
		t.Fatalf("account without config must pass through, got %s", got)
	}
}

func TestApplyEnhancedChatReasoningEffortAfterGrokEligibility_KeepsBridge(t *testing.T) {
	original := []byte(`{"model":"grok","messages":[{"role":"user","content":"hi"}]}`)
	if ok, reason := grokChatResponsesBridgeEligibility(original); !ok {
		t.Fatalf("control body should be bridge-eligible, reason=%s", reason)
	}

	account := overrideEffortAccount(ReasoningEffortModeOverride, "*", "xhigh")
	account.Type = AccountTypeOAuth
	injected, eligible, reason := applyEnhancedChatReasoningEffortAfterGrokEligibility(account, original)
	if !eligible {
		t.Fatalf("eligibility must be decided on the original body, reason=%s", reason)
	}
	if v := gjson.GetBytes(injected, "reasoning_effort").String(); v != "xhigh" {
		t.Fatalf("injected reasoning_effort = %q, want xhigh (body=%s)", v, injected)
	}
	if ok, gotReason := grokChatResponsesBridgeEligibility(injected); ok || gotReason != "unsupported_reasoning_effort" {
		t.Fatalf("injected body must trip eligibility (ok=%v reason=%s); callers must not re-check after inject", ok, gotReason)
	}

	var chatReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(injected, &chatReq); err != nil {
		t.Fatalf("unmarshal injected chat body: %v", err)
	}
	responsesReq, err := apicompat.ChatCompletionsToResponses(&chatReq)
	if err != nil {
		t.Fatalf("ChatCompletionsToResponses: %v", err)
	}
	if responsesReq.Reasoning == nil || responsesReq.Reasoning.Effort != "xhigh" {
		t.Fatalf("bridge converter must carry injected effort, got %+v", responsesReq.Reasoning)
	}
}

func TestApplyEnhancedChatReasoningEffortAfterGrokEligibility_OriginalEffortStaysRaw(t *testing.T) {
	original := []byte(`{"model":"grok","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	account := overrideEffortAccount(ReasoningEffortModeOverride, "*", "xhigh")
	account.Type = AccountTypeOAuth
	injected, eligible, reason := applyEnhancedChatReasoningEffortAfterGrokEligibility(account, original)
	if eligible {
		t.Fatal("client-supplied reasoning_effort must stay on raw CC")
	}
	if reason != "unsupported_reasoning_effort" {
		t.Fatalf("reason = %q, want unsupported_reasoning_effort", reason)
	}
	if v := gjson.GetBytes(injected, "reasoning_effort").String(); v != "xhigh" {
		t.Fatalf("override must still rewrite raw CC effort, got %q", v)
	}
}

func TestApplyEnhancedChatReasoningEffortAfterGrokEligibility_NonOAuthSkipsBridge(t *testing.T) {
	original := []byte(`{"model":"grok","messages":[{"role":"user","content":"hi"}]}`)
	account := overrideEffortAccount(ReasoningEffortModeOverride, "*", "xhigh")
	_, eligible, _ := applyEnhancedChatReasoningEffortAfterGrokEligibility(account, original)
	if eligible {
		t.Fatal("non-OAuth grok must not be marked bridge-eligible")
	}
}

func TestApplyEnhancedReasoningEffortForAnthropicEntry_FillIgnoresSynthesizedDefault(t *testing.T) {
	account := overrideEffortAccount(ReasoningEffortModeFill, "grok-4.5", "xhigh")

	// 桥接合成的 medium（客户端未传 output_config.effort）→ 应被覆盖
	converted := []byte(`{"model":"grok-4.5","reasoning":{"effort":"medium","summary":"auto"}}`)
	unset := []byte(`{"model":"claude-sonnet","messages":[]}`)
	got, changed := ApplyEnhancedReasoningEffortForAnthropicEntry(account, converted, unset, true)
	if !changed {
		t.Fatal("fill must override the synthetic default when client did not set effort")
	}
	if v := gjson.GetBytes(got, "reasoning.effort").String(); v != "xhigh" {
		t.Errorf("reasoning.effort = %q, want xhigh (body=%s)", v, got)
	}

	// 客户端显式传了 output_config.effort → fill 模式保留客户端值
	explicit := []byte(`{"model":"claude-sonnet","output_config":{"effort":"low"}}`)
	if _, changed := ApplyEnhancedReasoningEffortForAnthropicEntry(account, converted, explicit, true); changed {
		t.Fatal("fill must respect an explicit client effort")
	}
}

func TestApplyEnhancedReasoningEffortForAnthropicEntry_OverrideBeatsClient(t *testing.T) {
	account := overrideEffortAccount(ReasoningEffortModeOverride, "grok-4.5", "xhigh")

	converted := []byte(`{"model":"grok-4.5","reasoning":{"effort":"low"}}`)
	explicit := []byte(`{"model":"claude-sonnet","output_config":{"effort":"low"}}`)
	got, changed := ApplyEnhancedReasoningEffortForAnthropicEntry(account, converted, explicit, true)
	if !changed {
		t.Fatal("override must replace the client value")
	}
	if v := gjson.GetBytes(got, "reasoning.effort").String(); v != "xhigh" {
		t.Errorf("reasoning.effort = %q, want xhigh", v)
	}
}

func TestSyncEnhancedResponsesReasoningEffort_AllocatesWhenNil(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","reasoning":{"effort":"xhigh"}}`)
	got := syncEnhancedResponsesReasoningEffort(nil, body)
	if got == nil {
		t.Fatal("nil Reasoning must be allocated so billing can see the injected effort")
	}
	if got.Effort != "xhigh" {
		t.Fatalf("Effort = %q, want xhigh", got.Effort)
	}
}

func TestSyncEnhancedResponsesReasoningEffort_UpdatesExisting(t *testing.T) {
	existing := &apicompat.ResponsesReasoning{Effort: "medium", Summary: "auto"}
	body := []byte(`{"reasoning":{"effort":"xhigh","summary":"auto"}}`)
	got := syncEnhancedResponsesReasoningEffort(existing, body)
	if got != existing {
		t.Fatal("non-nil Reasoning must be updated in place")
	}
	if got.Effort != "xhigh" {
		t.Fatalf("Effort = %q, want xhigh", got.Effort)
	}
	if got.Summary != "auto" {
		t.Fatalf("Summary = %q, want auto (must not be cleared)", got.Summary)
	}
}

func TestSyncEnhancedResponsesReasoningEffort_EmptyEffortKeepsOriginal(t *testing.T) {
	if got := syncEnhancedResponsesReasoningEffort(nil, []byte(`{"model":"grok-4.5"}`)); got != nil {
		t.Fatalf("empty effort must not allocate, got %+v", got)
	}
	existing := &apicompat.ResponsesReasoning{Effort: "low", Summary: "auto"}
	if got := syncEnhancedResponsesReasoningEffort(existing, nil); got != existing || got.Effort != "low" {
		t.Fatalf("empty body must keep original, got %+v", got)
	}
}

func TestApplyEnhancedReasoningEffortForAnthropicEntry_ChatShape(t *testing.T) {
	account := overrideEffortAccount(ReasoningEffortModeFill, "grok-4.5", "xhigh")

	// Anthropic→CC 回退链路：形态为 Chat Completions
	chatConverted := []byte(`{"model":"grok-4.5","reasoning_effort":"medium"}`)
	unset := []byte(`{"model":"claude-sonnet"}`)
	got, changed := ApplyEnhancedReasoningEffortForAnthropicEntry(account, chatConverted, unset, false)
	if !changed {
		t.Fatal("chat-shape fallback should be injected")
	}
	if v := gjson.GetBytes(got, "reasoning_effort").String(); v != "xhigh" {
		t.Errorf("reasoning_effort = %q, want xhigh (body=%s)", v, got)
	}
	if gjson.GetBytes(got, "reasoning.effort").Exists() {
		t.Error("chat shape must not write reasoning.effort")
	}
}

func TestApplyEnhancedReasoningEffortForWSFrame(t *testing.T) {
	account := overrideEffortAccount(ReasoningEffortModeOverride, "grok-4.5", "xhigh")

	got := ApplyEnhancedReasoningEffortForWSFrame(account, []byte(`{"type":"response.create","model":"grok-4.5"}`))
	if v := gjson.GetBytes(got, "reasoning.effort").String(); v != "xhigh" {
		t.Errorf("ws frame: reasoning.effort = %q, want xhigh (body=%s)", v, got)
	}

	// fill 模式：帧里已带值则保留
	fillAccount := overrideEffortAccount(ReasoningEffortModeFill, "grok-4.5", "xhigh")
	body := []byte(`{"type":"response.create","model":"grok-4.5","reasoning":{"effort":"low"}}`)
	if got := ApplyEnhancedReasoningEffortForWSFrame(fillAccount, body); string(got) != string(body) {
		t.Fatalf("fill must respect the frame value, got %s", got)
	}

	// 无配置账号 → 原样
	plain := &Account{Platform: PlatformGrok, Credentials: map[string]any{}}
	if got := ApplyEnhancedReasoningEffortForWSFrame(plain, body); string(got) != string(body) {
		t.Fatalf("account without config must pass through, got %s", got)
	}

	// session.update 不得写入 reasoning.effort
	session := []byte(`{"type":"session.update","model":"grok-4.5"}`)
	if got := ApplyEnhancedReasoningEffortForWSFrame(account, session); string(got) != string(session) {
		t.Fatalf("session.update must pass through, got %s", got)
	}
}

func TestEnhancedEffortForAnthropicOutputConfig(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"max", "max"},
		{"xhigh", "max"},
		{"extra-high", "max"},
		{"minimal", "low"},
		{"none", ""},
	}
	for _, tc := range cases {
		if got := enhancedEffortForAnthropicOutputConfig(tc.in); got != tc.want {
			t.Errorf("enhancedEffortForAnthropicOutputConfig(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestApplyEnhancedReasoningEffortForNativeAnthropic(t *testing.T) {
	account := overrideEffortAccount(ReasoningEffortModeOverride, "*", "xhigh")
	got := ApplyEnhancedReasoningEffortForNativeAnthropic(account, []byte(`{"model":"kimi-k2","messages":[]}`))
	if v := gjson.GetBytes(got, "output_config.effort").String(); v != "max" {
		t.Fatalf("output_config.effort = %q, want max (xhigh mapped), body=%s", v, got)
	}
	if gjson.GetBytes(got, "reasoning.effort").Exists() {
		t.Fatal("native anthropic must not write reasoning.effort")
	}

	fill := overrideEffortAccount(ReasoningEffortModeFill, "*", "high")
	explicit := []byte(`{"model":"kimi-k2","output_config":{"effort":"low"}}`)
	if got := ApplyEnhancedReasoningEffortForNativeAnthropic(fill, explicit); string(got) != string(explicit) {
		t.Fatalf("fill must respect explicit output_config.effort, got %s", got)
	}
	unset := []byte(`{"model":"kimi-k2","messages":[]}`)
	got = ApplyEnhancedReasoningEffortForNativeAnthropic(fill, unset)
	if v := gjson.GetBytes(got, "output_config.effort").String(); v != "high" {
		t.Fatalf("fill should inject high, got %q body=%s", v, got)
	}
}

func TestRequestedReasoningEffortForUsageLog_ShowsForwardedWhenEnhancedOn(t *testing.T) {
	high := "high"
	xhigh := "xhigh"
	account := overrideEffortAccount(ReasoningEffortModeOverride, "grok-4.6", "xhigh")
	got := requestedReasoningEffortForUsageLog(account, &high, &xhigh)
	if got == nil || *got != "xhigh" {
		t.Fatalf("override account should persist forwarded xhigh, got %v", derefUsageEffort(got))
	}

	off := &Account{Platform: PlatformGrok, Extra: map[string]any{}}
	got = requestedReasoningEffortForUsageLog(off, &high, &xhigh)
	if got == nil || *got != "high" {
		t.Fatalf("off account should keep requested high, got %v", derefUsageEffort(got))
	}
}

func derefUsageEffort(v *string) string {
	if v == nil {
		return "<nil>"
	}
	return *v
}
