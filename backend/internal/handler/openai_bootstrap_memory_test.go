package handler

import (
	"bytes"
	"runtime"
	"testing"
)

func TestBootstrapNoopAllocationBudget(t *testing.T) {
	body := append([]byte(`{"model":"gpt-6-astra","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"`), bytes.Repeat([]byte{'a'}, 2<<20)...)
	body = append(body, []byte(`"}]}]}`)...)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, automationChanged := normalizeCodexAutomationBootstrap(body)
	got, delegationChanged := normalizeCodexDelegationBootstrap(got)
	runtime.ReadMemStats(&after)
	if automationChanged || delegationChanged || !bytes.Equal(got, body) || &got[0] != &body[0] {
		t.Fatal("ordinary history must preserve its original backing array")
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > 256<<10 {
		t.Fatalf("no-op bootstrap materialized a large history: allocated=%d, budget=%d", allocated, 256<<10)
	}
	runtime.KeepAlive(got)
}
