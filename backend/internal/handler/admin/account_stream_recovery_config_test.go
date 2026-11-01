package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

type streamRecoveryConfigAdminStub struct {
	stubAdminService
	account *service.Account
	writes  int
}

func (s *streamRecoveryConfigAdminStub) GetAccount(context.Context, int64) (*service.Account, error) {
	return s.account, nil
}
func (s *streamRecoveryConfigAdminStub) UpdateAccountExtra(_ context.Context, _ int64, updates map[string]any) error {
	s.writes++
	data, err := json.Marshal(updates)
	if err != nil {
		return err
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	if s.account.Extra == nil {
		s.account.Extra = map[string]any{}
	}
	for k, v := range values {
		s.account.Extra[k] = v
	}
	return nil
}
func streamRecoveryConfigRequest(t *testing.T, stub *streamRecoveryConfigAdminStub, method, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &AccountHandler{adminService: stub}
	r := gin.New()
	r.GET("/accounts/:id/enhanced-control", h.GetEnhancedControl)
	r.PUT("/accounts/:id/enhanced-control", h.UpdateEnhancedControl)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/accounts/1/enhanced-control", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	data, _ := payload["data"].(map[string]any)
	return w.Code, data
}
func TestStreamRecoveryAdminConfig(t *testing.T) {
	t.Run("GET legacy defaults", func(t *testing.T) {
		s := &streamRecoveryConfigAdminStub{account: &service.Account{ID: 1}}
		code, data := streamRecoveryConfigRequest(t, s, http.MethodGet, "")
		setting, _ := data["stream_recovery"].(map[string]any)
		if code != 200 || setting["enabled"] != false || setting["cooldown_seconds"] != float64(600) {
			t.Fatalf("code=%d data=%v", code, data)
		}
	})
	t.Run("old client preserves recovery and old replacement semantics", func(t *testing.T) {
		s := &streamRecoveryConfigAdminStub{account: &service.Account{ID: 1, Extra: map[string]any{"other": "keep", service.EnhancedControlKey: map[string]any{"stream_recovery": map[string]any{"enabled": true, "cooldown_seconds": float64(0)}, "ttft": map[string]any{"mode": "random"}}}}}
		code, data := streamRecoveryConfigRequest(t, s, http.MethodPut, `{"cache_rate":{"mode":"fixed","fixed_value":50}}`)
		setting, _ := data["stream_recovery"].(map[string]any)
		if code != 200 || setting["enabled"] != true || setting["cooldown_seconds"] != float64(0) {
			t.Fatalf("code=%d data=%v", code, data)
		}
		cfg := service.ParseEnhancedControl(s.account.Extra)
		if cfg.CacheRate.FixedValue != 50 || cfg.TTFT.Mode != "" || cfg.ReasoningEffort.Mode != service.ReasoningEffortModeOff || s.account.Extra["other"] != "keep" {
			t.Fatalf("old semantics changed: %+v extra=%v", cfg, s.account.Extra)
		}
	})
	for _, tc := range []struct {
		body    string
		seconds int
		enabled bool
	}{
		{`{"stream_recovery":{"enabled":true,"cooldown_seconds":0}}`, 0, true},
		{`{"stream_recovery":{"enabled":false,"cooldown_seconds":86400}}`, 86400, false},
		{`{"stream_recovery":{"enabled":true}}`, 600, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			s := &streamRecoveryConfigAdminStub{account: &service.Account{ID: 1}}
			code, data := streamRecoveryConfigRequest(t, s, http.MethodPut, tc.body)
			setting, _ := data["stream_recovery"].(map[string]any)
			if code != 200 || setting["enabled"] != tc.enabled || setting["cooldown_seconds"] != float64(tc.seconds) || s.writes != 1 {
				t.Fatalf("code=%d data=%v writes=%d", code, data, s.writes)
			}
		})
	}
	for _, raw := range []string{`null`, `[]`, `true`, `{"enabled":"true"}`, `{"enabled":null}`, `{"cooldown_seconds":null}`, `{"cooldown_seconds":-1}`, `{"cooldown_seconds":86401}`, `{"cooldown_seconds":1.5}`, `{"cooldown_seconds":"0"}`, `{"cooldown_seconds":true}`} {
		t.Run("reject "+raw, func(t *testing.T) {
			s := &streamRecoveryConfigAdminStub{account: &service.Account{ID: 1}}
			code, _ := streamRecoveryConfigRequest(t, s, http.MethodPut, `{"stream_recovery":`+raw+`}`)
			if code != 400 || s.writes != 0 {
				t.Fatalf("code=%d writes=%d", code, s.writes)
			}
		})
	}
}
