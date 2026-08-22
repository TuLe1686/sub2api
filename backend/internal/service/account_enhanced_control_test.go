package service

import (
	"math"
	"testing"
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
				"mode":               "fixed",
				"random_reduce_min":  float64(5),
				"random_reduce_max":  float64(20),
				"fixed_value":        float64(50),
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
				"mode":               "fixed",
				"fixed_value":        float64(150), // > 95 → 95
				"random_reduce_min":  float64(-5),  // < 1 → 1
				"random_reduce_max":  float64(50),  // > 30 → 30
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
				"mode":               "random",
				"random_reduce_min":  float64(25),
				"random_reduce_max":  float64(10),
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
