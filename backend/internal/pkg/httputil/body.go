package httputil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
	"github.com/klauspost/compress/zstd"
)

const (
	requestBodyReadInitCap    = 512
	requestBodyReadMaxInitCap = 1 << 20
	jsonUTF8BOMLen            = 3
	// maxDecompressedBodySize limits the decompressed request body to 64 MB
	// to prevent decompression bomb attacks.
	maxDecompressedBodySize = 64 << 20
)

// PrereadBody 回填已读取完成的请求体：作为 io.ReadCloser 可被再次顺序消费
// （multipart 流式解析），同时暴露 Bytes() 让 ReadRequestBodyWithPrealloc
// 直接返回原始切片，避免二次分配与复制。
//
// 注意：ReadRequestBodyWithPrealloc 对 PrereadBody 的快速路径不检查内部
// reader 是否已被（部分）消费——包装的字节完整且不可变，即使 reader 已被
// 流式消费过，Bytes() 也始终返回完整请求体。
type PrereadBody struct {
	body   []byte
	reader *bytes.Reader
}

// NewPrereadBody 包装一段已读取的请求体。
func NewPrereadBody(body []byte) *PrereadBody {
	return &PrereadBody{body: body, reader: bytes.NewReader(body)}
}

// Read 实现 io.Reader（转发给内部 bytes.Reader）。
func (p *PrereadBody) Read(b []byte) (int, error) {
	if p == nil {
		return 0, io.EOF
	}
	return p.reader.Read(b)
}

// Close 实现 io.Closer；请求体已在内存中，无需释放资源。
func (p *PrereadBody) Close() error { return nil }

// Bytes 返回完整的原始请求体切片。
func (p *PrereadBody) Bytes() []byte {
	if p == nil {
		return nil
	}
	return p.body
}

// ReadRequestBodyWithPrealloc reads request body with preallocated buffer based
// on content length, transparently decoding any Content-Encoding the upstream
// client used to compress the body (zstd, gzip, deflate).
// 已由 PrereadBody 回填的请求体直接返回其完整切片（零拷贝），不检查内部
// reader 是否已被消费——见 PrereadBody 的文档说明。
func ReadRequestBodyWithPrealloc(req *http.Request) ([]byte, error) {
	if req == nil || req.Body == nil {
		return nil, nil
	}
	if preread, ok := req.Body.(*PrereadBody); ok {
		return preread.Bytes(), nil
	}

	capHint := requestBodyReadInitCap
	if req.ContentLength > 0 {
		switch {
		case req.ContentLength < int64(requestBodyReadInitCap):
			capHint = requestBodyReadInitCap
		case req.ContentLength > int64(requestBodyReadMaxInitCap):
			capHint = requestBodyReadMaxInitCap
		default:
			capHint = int(req.ContentLength)
		}
	}

	lease := requestmemory.FromContext(req.Context())
	raw, err := readRequestBodyChunksAccounted(req.Context(), req.Body, capHint, req.ContentLength, lease, 0)
	if err != nil {
		return nil, err
	}

	enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		if err := lease.Charge(int64(len(raw)) * requestmemory.ParserWeight); err != nil {
			return nil, err
		}
		return raw, nil
	}

	decoded, err := decompressRequestBodyAccounted(req.Context(), enc, raw, lease)
	if err != nil {
		return nil, fmt.Errorf("decode Content-Encoding %q: %w", enc, err)
	}
	if err := lease.Charge(int64(len(decoded)) * requestmemory.ParserWeight); err != nil {
		return nil, err
	}

	req.Header.Del("Content-Encoding")
	req.Header.Del("Content-Length")
	req.ContentLength = int64(len(decoded))

	return decoded, nil
}

// Read bounded chunks as bytes arrive, then assemble the exact-size result.
// This avoids doubling large buffers or eagerly allocating an untrusted
// Content-Length before the corresponding bytes have arrived.
func readRequestBodyChunks(reader io.Reader, initialCapacity int, contentLength int64) ([]byte, error) {
	return readRequestBodyChunksAccounted(context.Background(), reader, initialCapacity, contentLength, nil, 0)
}

func readRequestBodyChunksAccounted(ctx context.Context, reader io.Reader, initialCapacity int, contentLength int64, lease *requestmemory.Lease, limit int64) ([]byte, error) {
	capacity := initialCapacity
	var chunks [][]byte
	total := 0
	for {
		if lease != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		chunkCapacity := capacity
		if remaining := contentLength - int64(total); remaining >= 0 && remaining < int64(chunkCapacity) {
			chunkCapacity = int(remaining) + 1
		}
		if limit > 0 && int64(chunkCapacity) > limit+1-int64(total) {
			chunkCapacity = int(limit + 1 - int64(total))
		}
		// Include slice metadata as well as payload capacity before make.
		if err := lease.Charge(int64(chunkCapacity) + 64); err != nil {
			return nil, err
		}
		chunk := make([]byte, chunkCapacity)
		n := 0
		var err error
		for n < len(chunk) && err == nil {
			if lease != nil {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			var read int
			read, err = reader.Read(chunk[n:])
			n += read
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		if limit > 0 && int64(total)+int64(n) > limit {
			return nil, &http.MaxBytesError{Limit: limit}
		}
		if n > 0 {
			chunks = append(chunks, chunk[:n])
			total += n
		}
		if err != nil {
			if len(chunks) == 0 {
				return chunk[:0], nil
			}
			if len(chunks) == 1 {
				return chunks[0], nil
			}
			if err := lease.Charge(int64(total)); err != nil {
				return nil, err
			}
			body := make([]byte, total)
			offset := 0
			for _, part := range chunks {
				offset += copy(body[offset:], part)
			}
			return body, nil
		}
		if capacity < requestBodyReadMaxInitCap {
			capacity *= 2
			if capacity > requestBodyReadMaxInitCap {
				capacity = requestBodyReadMaxInitCap
			}
		}
	}
}

// ReadLenientJSONRequestBodyWithPrealloc reads a request body and normalizes
// JSON string control bytes before strict validation.
func ReadLenientJSONRequestBodyWithPrealloc(req *http.Request, maxNormalizedBytes int64) ([]byte, error) {
	body, err := ReadRequestBodyWithPrealloc(req)
	if err != nil {
		return nil, err
	}
	return normalizeLenientJSONRequestBody(body, maxNormalizedBytes, requestmemory.FromContext(req.Context()))
}

func decompressRequestBody(encoding string, raw []byte) ([]byte, error) {
	switch encoding {
	case "zstd":
		dec, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		return io.ReadAll(io.LimitReader(dec, maxDecompressedBodySize))
	case "gzip", "x-gzip":
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer func() { _ = gr.Close() }()
		return io.ReadAll(io.LimitReader(gr, maxDecompressedBodySize))
	case "deflate":
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		return io.ReadAll(io.LimitReader(zr, maxDecompressedBodySize))
	default:
		return nil, errors.New("unsupported Content-Encoding")
	}
}

func decompressRequestBodyAccounted(ctx context.Context, encoding string, raw []byte, lease *requestmemory.Lease) ([]byte, error) {
	if lease == nil {
		return decompressRequestBody(encoding, raw)
	}
	// Decoder workspace is an input-cost estimate too. Bound zstd's own window
	// before constructing it; output is separately charged before each make.
	workspace := int64(128 << 10)
	if encoding == "zstd" {
		workspace = maxDecompressedBodySize
	}
	if err := lease.Charge(workspace); err != nil {
		return nil, err
	}
	var reader io.Reader
	switch encoding {
	case "zstd":
		dec, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maxDecompressedBodySize))
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		reader = dec
	case "gzip", "x-gzip":
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer func() { _ = gr.Close() }()
		reader = gr
	case "deflate":
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		reader = zr
	default:
		return nil, errors.New("unsupported Content-Encoding")
	}
	return readRequestBodyChunksAccounted(ctx, io.LimitReader(reader, maxDecompressedBodySize+1), requestBodyReadInitCap, -1, lease, maxDecompressedBodySize)
}

// NormalizeLenientJSONRequestBody escapes raw control bytes that broken
// OpenAI-compatible clients sometimes place inside JSON strings.
func NormalizeLenientJSONRequestBody(body []byte, maxNormalizedBytes int64) ([]byte, error) {
	return normalizeLenientJSONRequestBody(body, maxNormalizedBytes, nil)
}

func normalizeLenientJSONRequestBody(body []byte, maxNormalizedBytes int64, lease *requestmemory.Lease) ([]byte, error) {
	if maxNormalizedBytes <= 0 {
		maxNormalizedBytes = maxDecompressedBodySize
	}

	body = trimUTF8BOM(body)
	if len(body) == 0 {
		return body, nil
	}
	if int64(len(body)) > maxNormalizedBytes {
		return nil, &http.MaxBytesError{Limit: maxNormalizedBytes}
	}

	if lease != nil {
		normalizedLen, changed, err := normalizedJSONLength(body, maxNormalizedBytes)
		if err != nil {
			return nil, err
		}
		if !changed {
			return body, nil
		}
		if err := lease.Charge(normalizedLen + 64); err != nil {
			return nil, err
		}
		out := make([]byte, 0, int(normalizedLen))
		inString := false
		escaped := false
		for _, b := range body {
			if inString && isJSONControlByte(b) {
				out = appendJSONUnicodeEscape(out, b)
				escaped = false
				continue
			}
			switch {
			case escaped:
				escaped = false
			case inString && b == '\\':
				escaped = true
			case b == '"':
				inString = !inString
			}
			out = append(out, b)
		}
		return out, nil
	}

	var out []byte
	inString := false
	escaped := false
	for i, b := range body {
		if inString && isJSONControlByte(b) {
			if out == nil {
				capHint := len(body) + 6
				if int64(capHint) > maxNormalizedBytes {
					capHint = int(maxNormalizedBytes)
				}
				out = make([]byte, 0, capHint)
				out = append(out, body[:i]...)
			}
			if int64(len(out)+6) > maxNormalizedBytes {
				return nil, &http.MaxBytesError{Limit: maxNormalizedBytes}
			}
			out = appendJSONUnicodeEscape(out, b)
			escaped = false
			continue
		}

		switch {
		case escaped:
			escaped = false
		case inString && b == '\\':
			escaped = true
		case b == '"':
			inString = !inString
		}

		if out != nil {
			if int64(len(out)+1) > maxNormalizedBytes {
				return nil, &http.MaxBytesError{Limit: maxNormalizedBytes}
			}
			out = append(out, b)
		}
	}
	if out != nil {
		return out, nil
	}
	return body, nil
}

func normalizedJSONLength(body []byte, maxNormalizedBytes int64) (int64, bool, error) {
	var length int64
	inString := false
	escaped := false
	changed := false
	for _, b := range body {
		if inString && isJSONControlByte(b) {
			if length > maxNormalizedBytes-6 {
				return 0, false, &http.MaxBytesError{Limit: maxNormalizedBytes}
			}
			length += 6
			changed = true
			escaped = false
			continue
		}
		if length == maxNormalizedBytes {
			return 0, false, &http.MaxBytesError{Limit: maxNormalizedBytes}
		}
		length++
		switch {
		case escaped:
			escaped = false
		case inString && b == '\\':
			escaped = true
		case b == '"':
			inString = !inString
		}
	}
	return length, changed, nil
}

func trimUTF8BOM(body []byte) []byte {
	if len(body) >= jsonUTF8BOMLen && body[0] == 0xef && body[1] == 0xbb && body[2] == 0xbf {
		return body[jsonUTF8BOMLen:]
	}
	return body
}

func isJSONControlByte(b byte) bool {
	return b < 0x20 || b == 0x7f
}

func appendJSONUnicodeEscape(dst []byte, b byte) []byte {
	const hex = "0123456789abcdef"
	return append(dst, '\\', 'u', '0', '0', hex[b>>4], hex[b&0x0f])
}
