package routes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/handler"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

type memoryRouteBody struct {
	io.Reader
	reads int
}

func (r *memoryRouteBody) Read(b []byte) (int, error) { r.reads++; return r.Reader.Read(b) }
func (*memoryRouteBody) Close() error                 { return nil }

func TestGatewayMemoryGuardRejectsAliasesBeforeParsingOrModelRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterGatewayRoutes(router, &handler.Handlers{
		Gateway: &handler.GatewayHandler{}, OpenAIGateway: &handler.OpenAIGatewayHandler{},
		AsyncImage: handler.NewAsyncImageHandler(nil, nil),
	}, middleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		groupID := int64(1)
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
			GroupID: &groupID,
			Group:   allowlistGroup(service.PlatformComposite, true, "allowed-*"),
		})
		c.Next()
	}), nil, nil, nil, nil, nil, &config.Config{Gateway: config.GatewayConfig{
		MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20,
		RequestMemoryGuard: config.GatewayRequestMemoryGuardConfig{
			Enabled: true, BudgetBytes: 1024, ReadTimeoutSeconds: 1,
		},
	}})
	for _, path := range []string{
		"/v1/responses", "/responses", "/responses/compact", "/v1/responses/compact",
		"/backend-api/codex/responses", "/backend-api/codex/responses/compact",
		"/v1/messages", "/messages/count_tokens", "/v1/messages/count_tokens",
		"/chat/completions", "/v1/chat/completions", "/embeddings", "/v1/embeddings",
		"/images/generations", "/v1/images/generations", "/images/edits", "/v1/images/edits",
		"/images/generations/async", "/v1/images/generations/async",
		"/images/edits/async", "/v1/images/edits/async", "/v1/images/batches",
		"/api/v3/contents/generations/tasks", "/v3/contents/generations/tasks",
		"/v1/contents/generations/tasks", "/contents/generations/tasks",
		"/antigravity/v1/messages", "/antigravity/v1/messages/count_tokens",
	} {
		t.Run(path, func(t *testing.T) {
			body := &memoryRouteBody{Reader: strings.NewReader(strings.Repeat("x", 1000))}
			req := httptest.NewRequest(http.MethodPost, path, body)
			req.ContentLength = 1000
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable || body.reads != 0 || !strings.Contains(rec.Body.String(), "request_memory_budget_exhausted") {
				t.Fatalf("guard did not precede parser/routing: status=%d reads=%d body=%s", rec.Code, body.reads, rec.Body)
			}
		})
	}
}
