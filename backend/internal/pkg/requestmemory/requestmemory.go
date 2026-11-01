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

// image-compress 补丁的标定（builder runs/20261013-image-compress）：
// 读取链路对单个请求的累计计费 ≈ 分块缓冲（~2×size）+ 拼装（size）+ 解析权重×size，
// 三者共用同一只预算。原 4× 权重 + 512MiB 预算把读取上限压在 ~73MiB——85MB+ 的
// 全量重发会话在被拒之前也会因读取计费失败（同一条 503）。压缩管道下大请求
// 读后即缩小，解析工作集远低于 4×；对 >40MiB 的 body 用 2× 权重，
// 使 512MiB 预算下的真实读取上限抬到 ~110MiB（准入上限由守卫公式另行封在 100MiB）。
const (
	LargeBodyThresholdBytes int64 = 40 << 20
	LargeBodyParserWeight   int64 = 2
)

// ParserWeightForBodySize 返回按体积分段的解析计费权重（≤40MiB 维持 4× 不变）。
func ParserWeightForBodySize(size int64) int64 {
	if size > LargeBodyThresholdBytes {
		return LargeBodyParserWeight
	}
	return ParserWeight
}

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
	working        int64
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
		m.used -= l.bytes + l.working
		close(m.changed)
		m.changed = make(chan struct{})
	}
}

// WorkingSet tracks live reusable output memory without accumulating every streamed byte.
// It remains an actual owner of the parent request reservation until released.
type WorkingSet struct {
	lease    *Lease
	bytes    int64
	released bool
}

func (l *Lease) NewWorkingSet() (*WorkingSet, error) {
	if l == nil {
		return &WorkingSet{}, nil
	}
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if l.refs <= 0 {
		return nil, ErrCapacity
	}
	l.refs++
	return &WorkingSet{lease: l}, nil
}

func (w *WorkingSet) Resize(bytes int64) error {
	if w == nil || w.lease == nil {
		return nil
	}
	l := w.lease
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.released || bytes < 0 {
		return ErrCapacity
	}
	delta := bytes - w.bytes
	if delta > m.budget-m.used {
		return ErrCapacity
	}
	m.used += delta
	l.working += delta
	w.bytes = bytes
	if delta < 0 {
		close(m.changed)
		m.changed = make(chan struct{})
	}
	return nil
}

func (w *WorkingSet) Release() {
	if w == nil || w.lease == nil {
		return
	}
	l := w.lease
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.released {
		return
	}
	w.released = true
	m.used -= w.bytes
	l.working -= w.bytes
	w.bytes = 0
	l.refs--
	if l.refs == 0 {
		m.used -= l.bytes + l.working
	}
	close(m.changed)
	m.changed = make(chan struct{})
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
