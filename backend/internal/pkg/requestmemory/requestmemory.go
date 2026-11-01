// Package requestmemory bounds conservative request-input working cost. Its
// reservations are not a measurement or a hard cap of the Go heap.
package requestmemory

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrCapacity = errors.New("request memory budget exhausted")

const ParserWeight int64 = 4

type Snapshot struct {
	UsedBytes   int64
	BudgetBytes int64
	Waiting     int
}
type Manager struct {
	mu                  sync.Mutex
	budget, used        int64
	maxWaiting, waiting int
	wait                time.Duration
	changed             chan struct{}
}

func New(budget int64, maxWaiting int, wait time.Duration) (*Manager, error) {
	if budget <= 0 || maxWaiting < 0 || wait < 0 {
		return nil, errors.New("invalid request memory budget configuration")
	}
	return &Manager{budget: budget, maxWaiting: maxWaiting, wait: wait, changed: make(chan struct{})}, nil
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{UsedBytes: m.used, BudgetBytes: m.budget, Waiting: m.waiting}
}

func (m *Manager) Acquire(ctx context.Context, bytes int64) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if bytes <= 0 || bytes > m.budget {
		m.mu.Unlock()
		return nil, ErrCapacity
	}
	if m.used <= m.budget-bytes {
		m.used += bytes
		m.mu.Unlock()
		return &Lease{manager: m, bytes: bytes, refs: 1}, nil
	}
	if m.wait <= 0 || m.waiting >= m.maxWaiting {
		m.mu.Unlock()
		return nil, ErrCapacity
	}
	m.waiting++
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.waiting--; m.mu.Unlock() }()
	timer := time.NewTimer(m.wait)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.mu.Lock()
		if m.used <= m.budget-bytes {
			m.used += bytes
			m.mu.Unlock()
			return &Lease{manager: m, bytes: bytes, refs: 1}, nil
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, ErrCapacity
		case <-changed:
		}
	}
}

type Lease struct {
	manager        *Manager
	bytes, charged int64
	refs           int
	releaseOnce    sync.Once
}

// Ensure only tries expansion. Waiting after body allocation can deadlock
// requests which each hold a partial reservation.
func (l *Lease) Ensure(bytes int64) error {
	if l == nil {
		return nil
	}
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	return l.ensureLocked(bytes)
}
func (l *Lease) ensureLocked(bytes int64) error {
	if l.refs <= 0 || bytes < 0 {
		return ErrCapacity
	}
	if bytes <= l.bytes {
		return nil
	}
	delta := bytes - l.bytes
	if delta > l.manager.budget-l.manager.used {
		return ErrCapacity
	}
	l.manager.used += delta
	l.bytes = bytes
	return nil
}

// Charge reserves cumulative allocation cost before allocation. Allocations
// remain charged until the final actual owner releases the lease.
func (l *Lease) Charge(bytes int64) error {
	if l == nil {
		return nil
	}
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if bytes < 0 || bytes > m.budget-l.charged {
		return ErrCapacity
	}
	next := l.charged + bytes
	if err := l.ensureLocked(next); err != nil {
		return err
	}
	l.charged = next
	return nil
}

// Retain transfers ownership to work surviving the HTTP handler. The returned
// release is idempotent; cancellation deliberately does not release memory.
func (l *Lease) Retain() func() {
	if l == nil {
		return func() {}
	}
	m := l.manager
	m.mu.Lock()
	if l.refs <= 0 {
		m.mu.Unlock()
		return func() {}
	}
	l.refs++
	m.mu.Unlock()
	var once sync.Once
	return func() { once.Do(l.release) }
}
func (l *Lease) Release() {
	if l != nil {
		l.releaseOnce.Do(l.release)
	}
}
func (l *Lease) release() {
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	l.refs--
	if l.refs == 0 {
		m.used -= l.bytes
		close(m.changed)
		m.changed = make(chan struct{})
	}
}

type contextKey struct{}

func WithLease(ctx context.Context, l *Lease) context.Context {
	return context.WithValue(ctx, contextKey{}, l)
}
func FromContext(ctx context.Context) *Lease {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(contextKey{}).(*Lease)
	return l
}
