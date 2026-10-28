package middleware

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
	"github.com/gin-gonic/gin"
)

type guardDeadlineRecorder struct{ *httptest.ResponseRecorder }

func (*guardDeadlineRecorder) SetReadDeadline(time.Time) error { return nil }

func TestRequestMemoryGuardHTTP2SlowReadDoesNotBlockOtherStreams(t *testing.T) {
	cfg := memoryGuardTestConfig()
	m, _ := requestmemory.New(cfg.BudgetBytes, 1, time.Millisecond)
	r := gin.New()
	var forwarded atomic.Int64
	r.POST("/input", requestMemoryGuardWithManager(cfg, 1<<20, m), func(c *gin.Context) {
		forwarded.Add(1)
		c.Status(http.StatusNoContent)
	})
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	srv := httptest.NewUnstartedServer(r)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 5 * time.Second
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/input", reader)
	req.ContentLength = 100
	started := make(chan struct{})
	go func() { _, _ = io.WriteString(writer, "x"); close(started) }()
	result := make(chan *http.Response, 1)
	errs := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			errs <- err
			return
		}
		result <- resp
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request body did not start")
	}
	resp, err := client.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.ProtoMajor != 2 {
		t.Fatalf("health while blocked: %d %s", resp.StatusCode, resp.Proto)
	}
	select {
	case resp := <-result:
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestTimeout || resp.ProtoMajor != 2 {
			t.Fatalf("slow stream: %d %s", resp.StatusCode, resp.Proto)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP2 body read did not terminate")
	}
	if forwarded.Load() != 0 || m.Snapshot().UsedBytes != 0 {
		t.Fatalf("forwarded=%d reservation=%+v", forwarded.Load(), m.Snapshot())
	}
}

func TestRequestMemoryGuardReceiveDeadlineDoesNotTruncateSSE(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP1", true: "HTTP2"}[http2], func(t *testing.T) {
			cfg := memoryGuardTestConfig()
			m, _ := requestmemory.New(cfg.BudgetBytes, 1, time.Millisecond)
			r := gin.New()
			r.POST("/events", requestMemoryGuardWithManager(cfg, 1<<20, m), func(c *gin.Context) {
				c.Header("Content-Type", "text/event-stream")
				_, _ = c.Writer.WriteString("data: start\n\n")
				c.Writer.Flush()
				ctx, cancel := context.WithTimeout(c.Request.Context(), 1500*time.Millisecond)
				defer cancel()
				<-ctx.Done()
				_, _ = c.Writer.WriteString("data: done\n\n")
			})
			srv := httptest.NewUnstartedServer(r)
			srv.EnableHTTP2 = http2
			if http2 {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			client := srv.Client()
			client.Timeout = 4 * time.Second
			resp, err := client.Post(srv.URL+"/events", "application/json", bytes.NewBufferString("{}"))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || !strings.Contains(string(body), "data: done") || resp.StatusCode != http.StatusOK {
				t.Fatalf("SSE beyond ingress deadline: %q %v %d", body, err, resp.StatusCode)
			}
			if m.Snapshot().UsedBytes != 0 {
				t.Fatalf("stream completion leaked: %+v", m.Snapshot())
			}
		})
	}
}
