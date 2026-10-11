package httputil

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
)

type memoryCountingReader struct {
	reads int
	io.Reader
}

func (r *memoryCountingReader) Read(p []byte) (int, error) { r.reads++; return r.Reader.Read(p) }

func TestBudgetChargedBeforeFirstBodyAllocationAndRead(t *testing.T) {
	m, _ := requestmemory.New(100, 0, 0)
	l, _ := m.Acquire(context.Background(), 1)
	defer l.Release()
	r := &memoryCountingReader{Reader: bytes.NewBufferString("hello")}
	req, _ := http.NewRequest("POST", "/", io.NopCloser(r))
	req.ContentLength = -1
	req = req.WithContext(requestmemory.WithLease(req.Context(), l))
	_, err := ReadRequestBodyWithPrealloc(req)
	if !errors.Is(err, requestmemory.ErrCapacity) || r.reads != 0 {
		t.Fatalf("err=%v reads=%d", err, r.reads)
	}
}

func TestUnknownLengthExpansionRejectsBeforeParserAndPrereadIsFree(t *testing.T) {
	m, _ := requestmemory.New(4096, 0, 0)
	l, _ := m.Acquire(context.Background(), 512)
	defer l.Release()
	req, _ := http.NewRequest("POST", "/", io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("a"), 4096))))
	req.ContentLength = -1
	req = req.WithContext(requestmemory.WithLease(req.Context(), l))
	if _, err := ReadRequestBodyWithPrealloc(req); !errors.Is(err, requestmemory.ErrCapacity) {
		t.Fatalf("unknown length bypass: %v", err)
	}
	body := []byte("already accounted")
	req.Body = NewPrereadBody(body)
	before := m.Snapshot().UsedBytes
	got, err := ReadRequestBodyWithPrealloc(req)
	if err != nil || &got[0] != &body[0] || m.Snapshot().UsedBytes != before {
		t.Fatalf("preread lost zero-copy: %v", err)
	}
}

func TestNormalizeExpansionChargesBeforeOutputAllocation(t *testing.T) {
	body := []byte("{\"x\":\"\x00\"}")
	m, _ := requestmemory.New(120, 0, 0)
	l, _ := m.Acquire(context.Background(), 1)
	defer l.Release()

	req := newRequestWithBody(t, body, "")
	req = req.WithContext(requestmemory.WithLease(req.Context(), l))
	if _, err := ReadLenientJSONRequestBodyWithPrealloc(req, int64(len(body)+5)); !errors.Is(err, requestmemory.ErrCapacity) {
		t.Fatalf("normalization allocation bypass: %v", err)
	}
}

func TestCompressedOutputChargedBeforeAllocation(t *testing.T) {
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	_, _ = w.Write(bytes.Repeat([]byte("a"), 1<<20))
	_ = w.Close()
	m, _ := requestmemory.New(256<<10, 0, time.Second)
	l, _ := m.Acquire(context.Background(), 512)
	defer l.Release()
	req := newRequestWithBody(t, compressed.Bytes(), "gzip")
	req = req.WithContext(requestmemory.WithLease(req.Context(), l))
	if _, err := ReadRequestBodyWithPrealloc(req); !errors.Is(err, requestmemory.ErrCapacity) {
		t.Fatalf("decompression bypass: %v", err)
	}
}

func TestGuardDecompressedNPlusOneIs413NotSilentTruncation(t *testing.T) {
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	_, _ = io.Copy(w, io.LimitReader(&repeatMemoryByteReader{}, maxDecompressedBodySize+1))
	_ = w.Close()
	m, _ := requestmemory.New(128<<20, 0, 0)
	l, _ := m.Acquire(context.Background(), 1)
	defer l.Release()
	req := newRequestWithBody(t, compressed.Bytes(), "gzip")
	req = req.WithContext(requestmemory.WithLease(req.Context(), l))
	_, err := ReadRequestBodyWithPrealloc(req)
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		t.Fatalf("oversize decoded accepted: %v", err)
	}
}

type repeatMemoryByteReader struct{}

func (*repeatMemoryByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
