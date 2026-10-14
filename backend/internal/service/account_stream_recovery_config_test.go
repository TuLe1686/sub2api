package service

import (
	"encoding/json"
	"math"
	"testing"
)

func TestStreamRecoveryConfigParse(t *testing.T) {
	cases := []struct {
		name    string
		extra   map[string]any
		enabled bool
		seconds int
	}{
		{"missing extra", nil, false, 600},
		{"missing key", map[string]any{"other": true}, false, 600},
		{"invalid control", map[string]any{EnhancedControlKey: "bad"}, false, 600},
	}
	for _, tc := range []struct {
		name    string
		raw     any
		enabled bool
		seconds int
	}{
		{"missing setting", nil, false, 600},
		{"empty setting", map[string]any{}, false, 600},
		{"enabled default duration", map[string]any{"enabled": true}, true, 600},
		{"explicit zero", map[string]any{"enabled": true, "cooldown_seconds": float64(0)}, true, 0},
		{"maximum", map[string]any{"enabled": true, "cooldown_seconds": float64(86400)}, true, 86400},
		{"int", map[string]any{"cooldown_seconds": 42}, false, 42},
		{"int64", map[string]any{"cooldown_seconds": int64(43)}, false, 43},
		{"json number", map[string]any{"cooldown_seconds": json.Number("44")}, false, 44},
		{"fraction", map[string]any{"cooldown_seconds": 1.5}, false, 600},
		{"negative", map[string]any{"cooldown_seconds": -1}, false, 600},
		{"too large", map[string]any{"cooldown_seconds": 86401}, false, 600},
		{"string", map[string]any{"enabled": "true", "cooldown_seconds": "0"}, false, 600},
		{"nan", map[string]any{"cooldown_seconds": math.NaN()}, false, 600},
	} {
		cases = append(cases, struct {
			name    string
			extra   map[string]any
			enabled bool
			seconds int
		}{tc.name, map[string]any{EnhancedControlKey: map[string]any{"stream_recovery": tc.raw}}, tc.enabled, tc.seconds})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(ParseEnhancedControl(tc.extra))
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatal(err)
			}
			var setting map[string]any
			if err := json.Unmarshal(payload["stream_recovery"], &setting); err != nil {
				t.Fatalf("stream_recovery must always serialize: %s (%v)", data, err)
			}
			if setting["enabled"] != tc.enabled || setting["cooldown_seconds"] != float64(tc.seconds) {
				t.Fatalf("got %v, want enabled=%v cooldown=%d", setting, tc.enabled, tc.seconds)
			}
		})
	}
}
