package handler

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/httputil"
	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
	"github.com/gin-gonic/gin"
)

func TestAsyncImageRequestMemoryOwnerSurvivesCancelUntilRunEnds(t *testing.T) {
	m, _ := requestmemory.New(4096, 0, 0)
	l, _ := m.Acquire(context.Background(), 512)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/images/generations/async", strings.NewReader("body"))
	c.Request = c.Request.WithContext(requestmemory.WithLease(c.Request.Context(), l))
	task, _, cancel := newAsyncImageContext(c, []byte("body"), time.Minute)
	l.Release()
	cancel()
	if m.Snapshot().UsedBytes != 512 {
		t.Fatal("cancellation released detached body owner")
	}
	if _, ok := task.Request.Body.(*httputil.PrereadBody); !ok {
		t.Fatal("async re-read must be zero copy")
	}
	releaseAsyncImageMemory(task)
	if m.Snapshot().UsedBytes != 0 {
		t.Fatal("async completion leaked lease")
	}
	releaseAsyncImageMemory(task)
}
