package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestStreamRecoveryTransportKeepsWaitingConnectionAlive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	transport := NewStreamRecoveryTransport(c.Writer, "chat", nil)
	transport.heartbeatInterval = 5 * time.Millisecond
	transport.BeginAttempt()
	ctx := transport.StartWaiting(context.Background(), 200*time.Millisecond)
	select {
	case <-ctx.Done():
		t.Fatal("waiting ended before its budget")
	case <-time.After(22 * time.Millisecond):
	}
	transport.StopWaiting()
	require.GreaterOrEqual(t, strings.Count(rec.Body.String(), ": stream-recovery"), 2)
	require.Less(t, transport.Waited(), 200*time.Millisecond)
	transport.Close()
}

func TestStreamRecoveryTransportWaitBudgetEndsBeforeAnyResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	transport := NewStreamRecoveryTransport(c.Writer, "chat", nil)
	ctx := transport.StartWaiting(context.Background(), 10*time.Millisecond)
	select {
	case <-ctx.Done():
		require.ErrorIs(t, context.Cause(ctx), ErrStreamRecoveryWaitBudget)
	case <-time.After(time.Second):
		t.Fatal("waiting budget did not cancel the upstream context")
	}
	transport.Close()
}

func TestStreamRecoveryTransportProgressDoesNotLimitLongAnswer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	transport := NewStreamRecoveryTransport(c.Writer, "chat", nil)
	transport.BeginAttempt()
	ctx := transport.StartWaiting(context.Background(), 35*time.Millisecond)
	_, err := transport.WriteString("data: {\"id\":\"one\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\n")
	require.NoError(t, err)
	select {
	case <-ctx.Done():
		t.Fatal("first output must stop the waiting deadline")
	case <-time.After(60 * time.Millisecond):
	}
	transport.Close()
}

func TestStreamRecoveryTransportChargesSharedLeaseAndReleasesOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager, err := requestmemory.New(1<<20, 0, 0)
	require.NoError(t, err)
	lease, err := manager.Acquire(context.Background(), 1)
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	transport := NewStreamRecoveryTransport(c.Writer, "chat", lease)
	transport.BeginAttempt()
	_, err = transport.WriteString("data: {\"id\":\"one\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\n")
	require.NoError(t, err)
	require.Greater(t, manager.Snapshot().UsedBytes, int64(1))
	transport.Close()
	lease.Release()
	require.Zero(t, manager.Snapshot().UsedBytes)
}

func TestStreamRecoveryTransportDoesNotAccumulateWireCostForLongHealthyStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager, _ := requestmemory.New(20<<20, 0, 0)
	lease, _ := manager.Acquire(context.Background(), 1)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	transport := NewStreamRecoveryTransport(c.Writer, "chat", lease)
	transport.BeginAttempt()
	chunk := strings.Repeat("x", 8192)
	for i := 0; i < 512; i++ {
		_, err := transport.WriteString(fmt.Sprintf("data: {\"id\":\"long\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", chunk))
		require.NoError(t, err, "wire chunk %d must not cumulatively allocate forever", i)
	}
	require.Less(t, manager.Snapshot().UsedBytes, int64(20<<20))
	transport.Close()
	lease.Release()
	require.Zero(t, manager.Snapshot().UsedBytes)
}

func TestStreamRecoveryTransportHeadersAreCommittedBeforeHeartbeatConcurrency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	done := make(chan struct{})
	var handlerErr error
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c := gin.CreateTestContextOnly(w, gin.New())
		c.Request = req
		transport := NewStreamRecoveryTransport(c.Writer, "chat", nil)
		c.Writer = transport
		c.Header("Content-Type", "text/event-stream")
		c.Header("X-Before", "committed")
		transport.heartbeatInterval = time.Millisecond
		transport.StartWaiting(req.Context(), time.Second)
		for i := 0; i < 500; i++ {
			c.Header("X-After", fmt.Sprint(i))
		}
		transport.StopWaiting()
		if transport.Status() != http.StatusOK {
			mu.Lock()
			handlerErr = fmt.Errorf("status=%d", transport.Status())
			mu.Unlock()
		}
		transport.Close()
		close(done)
	}))
	defer server.Close()
	resp, err := server.Client().Get(server.URL)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	<-done
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Equal(t, "committed", resp.Header.Get("X-Before"))
	require.Empty(t, resp.Header.Get("X-After"))
	mu.Lock()
	require.NoError(t, handlerErr)
	mu.Unlock()
}

func TestStreamRecoveryTransportKeepaliveAdjustedSizeIgnoresOnlyHeartbeatBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	transport := NewStreamRecoveryTransport(c.Writer, "chat", nil)
	c.Writer = transport
	transport.heartbeatInterval = time.Millisecond
	transport.StartWaiting(context.Background(), 100*time.Millisecond)
	time.Sleep(4 * time.Millisecond)
	transport.StopWaiting()
	require.Positive(t, transport.StreamKeepaliveBytes())
	require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
	transport.BeginAttempt()
	_, err := transport.WriteString("data: {\"id\":\"one\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
	require.NoError(t, err)
	require.Positive(t, OpenAICompactKeepaliveAdjustedWrittenSize(c))
	transport.Close()
}

func TestStreamRecoveryTransportChargeFailureDoesNotWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager, _ := requestmemory.New(32, 0, 0)
	lease, _ := manager.Acquire(context.Background(), 1)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	transport := NewStreamRecoveryTransport(c.Writer, "chat", lease)
	_, err := transport.WriteString("data: {\"id\":\"one\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\n")
	require.True(t, errors.Is(err, requestmemory.ErrCapacity))
	require.Empty(t, rec.Body.String())
	transport.Close()
	lease.Release()
}
