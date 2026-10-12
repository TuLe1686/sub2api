package requestmemory_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
)

func TestLeaseTracksActualOwnersNotCancellation(t *testing.T) {
	m, err := requestmemory.New(100, 1, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	l, err := m.Acquire(ctx, 60)
	if err != nil {
		t.Fatal(err)
	}
	release := l.Retain()
	cancel()
	if got := m.Snapshot().UsedBytes; got != 60 {
		t.Fatalf("cancel released held bytes: %d", got)
	}
	if err := l.Ensure(80); err != nil {
		t.Fatal(err)
	}
	l.Release()
	if got := m.Snapshot().UsedBytes; got != 80 {
		t.Fatalf("retained owner lost reservation: %d", got)
	}
	release()
	release()
	if got := m.Snapshot().UsedBytes; got != 0 {
		t.Fatalf("final owner leaked %d", got)
	}
}

func TestExpansionNeverWaitsWhileBodiesAreHeld(t *testing.T) {
	m, _ := requestmemory.New(100, 1, time.Second)
	a, _ := m.Acquire(context.Background(), 50)
	b, _ := m.Acquire(context.Background(), 50)
	defer a.Release()
	defer b.Release()
	done := make(chan error, 1)
	go func() { done <- a.Ensure(51) }()
	select {
	case err := <-done:
		if !errors.Is(err, requestmemory.ErrCapacity) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expansion waited and can deadlock")
	}
}

func TestAdmissionWaitIsBoundedAndCancellationDoesNotAcquire(t *testing.T) {
	m, _ := requestmemory.New(100, 1, 30*time.Millisecond)
	held, _ := m.Acquire(context.Background(), 100)
	defer held.Release()
	if _, err := m.Acquire(context.Background(), 1); !errors.Is(err, requestmemory.ErrCapacity) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Acquire(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if s := m.Snapshot(); s.UsedBytes != 100 || s.Waiting != 0 {
		t.Fatalf("snapshot: %+v", s)
	}
}

func TestManyConcurrentOwnersReleaseExactlyOnce(t *testing.T) {
	m, _ := requestmemory.New(100, 0, 0)
	l, _ := m.Acquire(context.Background(), 100)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		release := l.Retain()
		wg.Add(1)
		go func() { defer wg.Done(); release(); release() }()
	}
	l.Release()
	l.Release()
	wg.Wait()
	if s := m.Snapshot(); s.UsedBytes != 0 || s.Waiting != 0 {
		t.Fatalf("owner leak: %+v", s)
	}
	third, err := m.Acquire(context.Background(), 100)
	if err != nil {
		t.Fatalf("finished request did not return capacity: %v", err)
	}
	third.Release()
}

func TestOversizeDoesNotWaitAndChargeOverflowDoesNotWrap(t *testing.T) {
	m, _ := requestmemory.New(100, 1, time.Second)
	l, _ := m.Acquire(context.Background(), 100)
	defer l.Release()
	if _, err := m.Acquire(context.Background(), 101); !errors.Is(err, requestmemory.ErrCapacity) {
		t.Fatalf("oversize: %v", err)
	}
	if err := l.Charge(1 << 62); !errors.Is(err, requestmemory.ErrCapacity) {
		t.Fatalf("overflow: %v", err)
	}
	if s := m.Snapshot(); s.UsedBytes != 100 || s.Waiting != 0 {
		t.Fatalf("failed charge changed reservation: %+v", s)
	}
}

// image-compress 补丁：解析计费权重按体积分段（>40MiB 用 2×，≤40MiB 维持 4×）。
func TestParserWeightForBodySize(t *testing.T) {
	if got := requestmemory.ParserWeightForBodySize(40 << 20); got != requestmemory.ParserWeight {
		t.Fatalf("boundary 40MiB must keep parser weight: %d", got)
	}
	if got := requestmemory.ParserWeightForBodySize((40 << 20) + 1); got != requestmemory.LargeBodyParserWeight {
		t.Fatalf("large body must use reduced weight: %d", got)
	}
	if got := requestmemory.ParserWeightForBodySize(1 << 10); got != requestmemory.ParserWeight {
		t.Fatalf("small body must keep parser weight: %d", got)
	}
}
