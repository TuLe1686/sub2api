package middleware

import (
	"bytes"
	"context"
	"io"
	"math"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/httputil"
	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
	"github.com/gin-gonic/gin"
)

func memoryGuardTestConfig() config.GatewayRequestMemoryGuardConfig {
	return config.GatewayRequestMemoryGuardConfig{Enabled: true, BudgetBytes: 1 << 20, MaxWaiting: 1, WaitTimeoutMS: 20, ReadTimeoutSeconds: 1}
}

type admissionCountingBody struct {
	reads int
	io.Reader
}

func (b *admissionCountingBody) Read(p []byte) (int, error) { b.reads++; return b.Reader.Read(p) }
func (*admissionCountingBody) Close() error                 { return nil }

func TestRequestMemoryGuardRejectsBeforeFirstReadAndSideEffects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := memoryGuardTestConfig()
	cfg.BudgetBytes = 1024
	r := gin.New()
	called := false
	r.POST("/v1/responses", RequestMemoryGuard(cfg, 1<<20), func(c *gin.Context) { called = true })
	body := &admissionCountingBody{Reader: strings.NewReader(strings.Repeat("x", 1000))}
	req := httptest.NewRequest("POST", "/v1/responses", body)
	req.ContentLength = 1000
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" || body.reads != 0 || called {
		t.Fatalf("code=%d reads=%d downstream=%v body=%s", rec.Code, body.reads, called, rec.Body)
	}
}

func TestRequestMemoryGuardRetainSurvivesCancelAndResponse(t *testing.T) {
	cfg := memoryGuardTestConfig()
	m, _ := requestmemory.New(cfg.BudgetBytes, 1, time.Millisecond)
	r := gin.New()
	var release func()
	r.POST("/", requestMemoryGuardWithManager(cfg, 1<<20, m), func(c *gin.Context) {
		if _, ok := c.Request.Body.(*httputil.PrereadBody); !ok {
			t.Error("preread concrete type lost")
		}
		clone := c.Request.Clone(context.WithoutCancel(c.Request.Context()))
		release = requestmemory.FromContext(clone.Context()).Retain()
		c.Status(202)
	})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/", bytes.NewBufferString("hello")).WithContext(ctx)
	r.ServeHTTP(&guardDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}, req)
	cancel()
	if m.Snapshot().UsedBytes == 0 {
		t.Fatal("response released async owner")
	}
	release()
	if m.Snapshot().UsedBytes != 0 {
		t.Fatal("last owner leaked")
	}
}

func TestRequestMemoryGuardBodylessAndGETSkipInvalidGuard(t *testing.T) {
	r := gin.New()
	cfg := memoryGuardTestConfig()
	cfg.BudgetBytes = 0
	r.GET("/", RequestMemoryGuard(cfg, 1<<20), func(c *gin.Context) { c.Status(204) })
	r.POST("/", RequestMemoryGuard(cfg, 1<<20), func(c *gin.Context) { c.Status(204) })
	for _, method := range []string{"GET", "POST"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, "/", nil))
		if rec.Code != 204 {
			t.Fatalf("bodyless %s: %d", method, rec.Code)
		}
	}
}

func TestRequestMemoryGuardPreserves413(t *testing.T) {
	r := gin.New()
	cfg := memoryGuardTestConfig()
	cfg.BudgetBytes = 1024
	r.POST("/", RequestBodyLimit(10), RequestMemoryGuard(cfg, 10), func(c *gin.Context) { t.Fatal("downstream ran") })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 11))))
	if rec.Code != 413 {
		t.Fatalf("size limit became %d", rec.Code)
	}
}

func TestRequestMemoryGuardHTTP1SlowBodyDeadlineAndRelease(t *testing.T) {
	cfg := memoryGuardTestConfig()
	m, _ := requestmemory.New(cfg.BudgetBytes, 1, time.Millisecond)
	r := gin.New()
	called := false
	r.POST("/", requestMemoryGuardWithManager(cfg, 1<<20, m), func(c *gin.Context) { called = true })
	srv := httptest.NewServer(r)
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 100\r\n\r\nx")
	reply := make([]byte, 2048)
	n, err := conn.Read(reply)
	if err != nil || !strings.Contains(string(reply[:n]), "408") || called {
		t.Fatalf("slow read: %q %v downstream=%v", reply[:n], err, called)
	}
	if m.Snapshot().UsedBytes != 0 {
		t.Fatal("timed out reader leaked")
	}
}

// image-compress 补丁：预扣分段（≤40MiB 维持 6×CL；>40MiB 换 2×CL+192MiB）。
func TestRequestMemoryGuardInitialBytesPiecewise(t *testing.T) {
	if got, ok := requestMemoryGuardInitialBytes(0); !ok || got != 576 {
		t.Fatalf("unknown length must reserve 576: (%d,%v)", got, ok)
	}
	if got, ok := requestMemoryGuardInitialBytes(1 << 20); !ok || got != int64(6*(1<<20)+4096) {
		t.Fatalf("small body must keep legacy formula: (%d,%v)", got, ok)
	}
	boundary := int64(40 << 20)
	if got, ok := requestMemoryGuardInitialBytes(boundary); !ok || got != 6*boundary+4096 {
		t.Fatalf("boundary 40MiB must stay legacy: (%d,%v)", got, ok)
	}
	if got, ok := requestMemoryGuardInitialBytes(boundary + 1); !ok || got != 2*(boundary+1)+(312<<20) {
		t.Fatalf("40MiB+1 must switch to heavy formula: (%d,%v)", got, ok)
	}
	heavy := int64(41 << 20)
	if got, ok := requestMemoryGuardInitialBytes(heavy); !ok || got != 2*heavy+(312<<20) {
		t.Fatalf("heavy body formula mismatch: (%d,%v)", got, ok)
	}
	// 准入硬上限（rev 3，与预算解耦）：恰好 100MiB 可入，101MiB 无论预算多大都拒。
	if _, ok := requestMemoryGuardInitialBytes(int64(100 << 20)); !ok {
		t.Fatal("100MiB must still be admissible")
	}
	if _, ok := requestMemoryGuardInitialBytes(int64(101 << 20)); ok {
		t.Fatal("101MiB must be rejected by hard cap regardless of budget")
	}
	if _, ok := requestMemoryGuardInitialBytes(math.MaxInt64); ok {
		t.Fatal("overflow must be rejected")
	}
}
