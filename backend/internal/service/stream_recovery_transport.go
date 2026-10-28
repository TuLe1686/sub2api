package service

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
	"github.com/gin-gonic/gin"
)

var ErrStreamRecoveryWaitBudget = errors.New("stream recovery waiting budget exhausted")

type streamRecoveryUpstreamContextKey struct{}

func WithStreamRecoveryUpstreamContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, streamRecoveryUpstreamContextKey{}, true)
}

func streamRecoveryUpstreamIsAttached(ctx context.Context) bool {
	attached, _ := ctx.Value(streamRecoveryUpstreamContextKey{}).(bool)
	return attached
}

// StreamRecoveryTransport serializes all downstream writes, including heartbeats.
// Its waiting clock stops on new semantic progress, not on transport metadata.
type StreamRecoveryTransport struct {
	mu                sync.Mutex
	writer            *StreamRecoveryWriter
	lease             *requestmemory.Lease
	working           *requestmemory.WorkingSet
	retainedCharge    int64
	scratchCharge     int64
	privateHeader     http.Header
	heartbeatBytes    atomic.Int64
	waited            time.Duration
	waitingSince      time.Time
	waitTimer         *time.Timer
	waitCancel        context.CancelCauseFunc
	waitContext       context.Context
	waitDone          chan struct{}
	heartbeatInterval time.Duration
	closed            bool
}

var _ gin.ResponseWriter = (*StreamRecoveryTransport)(nil)

func NewStreamRecoveryTransport(w gin.ResponseWriter, protocol string, lease *requestmemory.Lease) *StreamRecoveryTransport {
	t := &StreamRecoveryTransport{writer: NewStreamRecoveryWriter(w, protocol), lease: lease, heartbeatInterval: 10 * time.Second, privateHeader: w.Header().Clone()}
	t.writer.header = t.privateHeader
	if lease != nil {
		working, err := lease.NewWorkingSet()
		if err != nil {
			t.writer.fail(err)
		} else {
			t.working = working
		}
		t.writer.SetRetainedBytesHook(func(delta int64) error {
			next := t.retainedCharge + delta*2
			if next < 0 {
				next = 0
			}
			if err := t.working.Resize(next + t.scratchCharge); err != nil {
				return err
			}
			t.retainedCharge = next
			return nil
		})
	}
	return t
}

func (t *StreamRecoveryTransport) ProtocolWriter() *StreamRecoveryWriter { return t.writer }
func (t *StreamRecoveryTransport) SetHeartbeatInterval(interval time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if interval > 0 {
		t.heartbeatInterval = interval
	}
}
func (t *StreamRecoveryTransport) WaitContext(fallback context.Context) context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.waitContext != nil {
		return t.waitContext
	}
	return WithStreamRecoveryUpstreamContext(fallback)
}
func (t *StreamRecoveryTransport) Header() http.Header       { return t.privateHeader }
func (t *StreamRecoveryTransport) StreamKeepaliveBytes() int { return int(t.heartbeatBytes.Load()) }
func (t *StreamRecoveryTransport) syncHeadersLocked(commit bool) {
	raw := t.writer.ResponseWriter
	if raw.Written() {
		return
	}
	dst := raw.Header()
	for key := range dst {
		delete(dst, key)
	}
	for key, values := range t.privateHeader {
		dst[key] = append([]string(nil), values...)
	}
	dst.Del("Content-Length")
	if commit {
		raw.WriteHeaderNow()
	}
}
func (t *StreamRecoveryTransport) WriteHeader(status int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writer.WriteHeader(status)
}
func (t *StreamRecoveryTransport) WriteHeaderNow() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.syncHeadersLocked(true)
}
func (t *StreamRecoveryTransport) Status() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writer.Status()
}
func (t *StreamRecoveryTransport) Size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writer.Size()
}
func (t *StreamRecoveryTransport) Written() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writer.Written()
}
func (t *StreamRecoveryTransport) CloseNotify() <-chan bool { return t.writer.CloseNotify() }
func (t *StreamRecoveryTransport) Pusher() http.Pusher      { return t.writer.Pusher() }
func (t *StreamRecoveryTransport) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("stream recovery requires HTTP SSE")
}
func (t *StreamRecoveryTransport) Flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.syncHeadersLocked(false)
	t.writer.Flush()
}
func (t *StreamRecoveryTransport) WriteString(p string) (int, error) { return t.Write([]byte(p)) }
func (t *StreamRecoveryTransport) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return 0, errors.New("stream recovery transport is closed")
	}
	if t.working != nil {
		t.scratchCharge = int64(len(p)) * 6
		if err := t.working.Resize(t.retainedCharge + t.scratchCharge); err != nil {
			t.scratchCharge = 0
			return 0, err
		}
		defer func() { t.scratchCharge = 0; _ = t.working.Resize(t.retainedCharge) }()
	}
	t.syncHeadersLocked(false)
	before := t.writer.ProgressVersion()
	n, err := t.writer.Write(p)
	if err == nil && t.writer.ProgressVersion() != before {
		t.stopWaitingLocked()
	}
	return n, err
}
func (t *StreamRecoveryTransport) BeginAttempt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writer.BeginAttempt()
}

func (t *StreamRecoveryTransport) StartWaiting(ctx context.Context, budget time.Duration) context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopWaitingLocked()
	t.syncHeadersLocked(true)
	if t.waitCancel != nil {
		t.waitCancel(context.Canceled)
	}
	waitCtx, cancel := context.WithCancelCause(WithStreamRecoveryUpstreamContext(ctx))
	t.waitCancel = cancel
	t.waitContext = waitCtx
	remaining := budget - t.waited
	if remaining <= 0 {
		cancel(ErrStreamRecoveryWaitBudget)
		return waitCtx
	}
	t.waitingSince = time.Now()
	t.waitDone = make(chan struct{})
	done := t.waitDone
	t.waitTimer = time.AfterFunc(remaining, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.waitDone == done {
			cancel(ErrStreamRecoveryWaitBudget)
			t.stopWaitingLocked()
		}
	})
	interval := t.heartbeatInterval
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-waitCtx.Done():
				t.mu.Lock()
				if t.waitDone == done {
					t.stopWaitingLocked()
				}
				t.mu.Unlock()
				return
			case <-ticker.C:
				t.mu.Lock()
				if t.waitDone == done && !t.closed {
					n, err := t.writer.ResponseWriter.WriteString(": stream-recovery\n\n")
					t.heartbeatBytes.Add(int64(n))
					if err != nil {
						cancel(err)
						t.stopWaitingLocked()
					} else {
						t.writer.ResponseWriter.Flush()
					}
				}
				t.mu.Unlock()
			}
		}
	}()
	return waitCtx
}
func (t *StreamRecoveryTransport) stopWaitingLocked() {
	if t.waitingSince.IsZero() {
		return
	}
	t.waited += time.Since(t.waitingSince)
	t.waitingSince = time.Time{}
	if t.waitTimer != nil {
		t.waitTimer.Stop()
		t.waitTimer = nil
	}
	if t.waitDone != nil {
		close(t.waitDone)
		t.waitDone = nil
	}
}
func (t *StreamRecoveryTransport) StopWaiting() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopWaitingLocked()
}
func (t *StreamRecoveryTransport) Waited() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.waited
	if !t.waitingSince.IsZero() {
		n += time.Since(t.waitingSince)
	}
	return n
}
func (t *StreamRecoveryTransport) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.stopWaitingLocked()
	if t.waitCancel != nil {
		t.waitCancel(context.Canceled)
	}
	if t.working != nil {
		t.working.Release()
	}
	t.closed = true
}
