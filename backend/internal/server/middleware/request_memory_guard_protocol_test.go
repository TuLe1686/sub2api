package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestMemoryCapacityErrorIsNotAModelError(t *testing.T) {
	for _, tc := range []struct {
		path, wantType, wantCode string
	}{
		{"/v1/responses", "server_error", "request_memory_budget_exhausted"},
		{"/v1/messages", "api_error", "request_memory_budget_exhausted"},
		{"/v1beta/models/gemini:generateContent", "UNAVAILABLE", "REQUEST_MEMORY_BUDGET_EXHAUSTED"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			cfg := memoryGuardTestConfig()
			cfg.BudgetBytes = 1024
			r := ginTestRouter(tc.path, RequestMemoryGuard(cfg, 1<<20))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(strings.Repeat("x", 1000)))
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
				t.Fatalf("capacity response: %d %v", rec.Code, rec.Header())
			}
			var payload struct {
				Type  string `json:"type"`
				Error struct {
					Type    string          `json:"type"`
					Code    json.RawMessage `json:"code"`
					Status  string          `json:"status"`
					Details []struct {
						Reason string `json:"reason"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if tc.wantType == "UNAVAILABLE" {
				if payload.Error.Status != tc.wantType || len(payload.Error.Details) != 1 || payload.Error.Details[0].Reason != tc.wantCode {
					t.Fatalf("Google capacity envelope: %s", rec.Body)
				}
			} else if payload.Error.Type != tc.wantType || string(payload.Error.Code) != `"`+tc.wantCode+`"` {
				t.Fatalf("capacity must have its own code: %s", rec.Body)
			}
			if strings.Contains(rec.Body.String(), "model_not_found") {
				t.Fatal("capacity rejection was a model error")
			}
		})
	}
}

func TestRequestMemoryGuardUnsupportedDeadlineRejectsBeforeRead(t *testing.T) {
	cfg := memoryGuardTestConfig()
	body := &admissionCountingBody{Reader: strings.NewReader("hello")}
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.ContentLength = 5
	rec := httptest.NewRecorder()
	ginTestRouter("/", RequestMemoryGuard(cfg, 1<<20)).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || body.reads != 0 || !strings.Contains(rec.Body.String(), "request_memory_guard_unavailable") {
		t.Fatalf("unsupported receive deadline: code=%d reads=%d body=%s", rec.Code, body.reads, rec.Body)
	}
}

func TestRequestMemoryGuardDisabledDoesNotReadOrChangeBody(t *testing.T) {
	cfg := memoryGuardTestConfig()
	cfg.Enabled = false
	body := &admissionCountingBody{Reader: strings.NewReader("hello")}
	r := gin.New()
	r.POST("/", RequestMemoryGuard(cfg, 1<<20), func(c *gin.Context) {
		if c.Request.Body != body || body.reads != 0 {
			t.Fatal("disabled guard changed input")
		}
		b, err := io.ReadAll(c.Request.Body)
		if err != nil || string(b) != "hello" {
			t.Fatalf("legacy read: %q %v", b, err)
		}
		c.Status(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disabled status=%d", rec.Code)
	}
}

func ginTestRouter(path string, guard gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.POST(path, guard, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	return r
}
