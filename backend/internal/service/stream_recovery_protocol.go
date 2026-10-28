package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// StreamRecoveryBufferLimit is a logical retained-output cap, not a request-body cap.
// The request owner supplies locking, heartbeats and the shared memory budget.
const StreamRecoveryBufferLimit = 8 << 20

var (
	ErrStreamRecoveryUnsupported    = errors.New("stream recovery: unsupported protocol boundary")
	ErrStreamRecoveryReplay         = errors.New("stream recovery: ambiguous or restarted answer")
	ErrStreamRecoveryLimit          = errors.New("stream recovery: 8 MiB buffer limit")
	ErrStreamRecoveryIncomplete     = errors.New("stream recovery: missing terminal or incomplete item")
	ErrStreamRecoveryCommittedTools = errors.New("stream recovery: completed tools require handoff, not replay")
)

type recoveryEvent struct {
	event string
	data  map[string]any
}
type recoveryAtomicItem struct {
	events          []recoveryEvent
	args            strings.Builder
	item            map[string]any
	kind            string
	index, bytes    int
	signed, invalid bool
}
type recoveryPart struct {
	part           map[string]any
	index          int
	done, textDone bool
}
type recoveryOutput struct {
	item      map[string]any
	index     int
	parts     map[int]*recoveryPart
	summaries map[int]*recoveryPart
	done      bool
}
type recoveryBlock struct {
	item  map[string]any
	index int
	done  bool
}

// StreamRecoveryWriter joins attempts into one client lifecycle. Native calls and
// signed Messages thinking are atomic; Responses reasoning remains protocol-native.
// Unsupported *observed output* disables replay, but does not reject normal streams.
// Like gin.ResponseWriter this object is not concurrent-safe.
type StreamRecoveryWriter struct {
	gin.ResponseWriter
	header                                               http.Header
	protocol                                             string
	frame                                                []byte
	text                                                 strings.Builder
	resumeBase, resumePending                            string
	resuming                                             bool
	resumeSend                                           func(string) error
	err, attemptErr                                      error
	interruptedCode, interruptedMessage                  string
	interrupted, terminal, finished, started, inProgress bool
	nonSSE                                               bool
	hasReasoning, unsafe, readOnlyHosted                 bool
	seq                                                  int
	id, upstreamID                                       string
	progress                                             uint64
	meta, usage, attemptUsage, final                     map[string]any
	pending                                              map[string]*recoveryAtomicItem
	pendingBytes                                         int
	committed                                            []map[string]any
	chatTools                                            map[int]*recoveryAtomicItem
	chatReasoning                                        map[string]string
	outputs                                              []*recoveryOutput
	attemptOutputs                                       map[string]*recoveryOutput
	attemptParts                                         map[string]*recoveryPart
	resumeOutput                                         *recoveryOutput
	resumePart                                           *recoveryPart
	blocks                                               []*recoveryBlock
	attemptBlocks                                        map[int]*recoveryBlock
	resumeBlock                                          *recoveryBlock
	mappedResume                                         bool
	retained                                             int64
	retainedHook                                         func(int64) error
}

var _ gin.ResponseWriter = (*StreamRecoveryWriter)(nil)

func NewStreamRecoveryWriter(w gin.ResponseWriter, protocol string) *StreamRecoveryWriter {
	s := &StreamRecoveryWriter{ResponseWriter: w, protocol: protocol, pending: make(map[string]*recoveryAtomicItem), chatTools: make(map[int]*recoveryAtomicItem), chatReasoning: make(map[string]string), attemptUsage: make(map[string]any), attemptOutputs: make(map[string]*recoveryOutput), attemptParts: make(map[string]*recoveryPart), attemptBlocks: make(map[int]*recoveryBlock)}
	if protocol != "responses" && protocol != "chat" && protocol != "messages" {
		s.err = ErrStreamRecoveryUnsupported
	}
	return s
}
func (s *StreamRecoveryWriter) Header() http.Header {
	if s.header != nil {
		return s.header
	}
	return s.ResponseWriter.Header()
}

func (s *StreamRecoveryWriter) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.attemptErr
}
func (s *StreamRecoveryWriter) EmittedText() string { return s.text.String() }
func (s *StreamRecoveryWriter) HasOutput() bool {
	return s.text.Len() > 0 || len(s.committed) > 0 || s.hasReasoning || s.readOnlyHosted
}
func (s *StreamRecoveryWriter) HasCommittedTools() bool      { return len(s.committed) > 0 }
func (s *StreamRecoveryWriter) UnsafeReplay() bool           { return s.unsafe }
func (s *StreamRecoveryWriter) HasReadOnlyHostedTools() bool { return s.readOnlyHosted }
func (s *StreamRecoveryWriter) ProgressVersion() uint64      { return s.progress }
func (s *StreamRecoveryWriter) LogicalResponseID() string    { return s.id }
func (s *StreamRecoveryWriter) UpstreamResponseID() string   { return s.upstreamID }
func (s *StreamRecoveryWriter) RetainedBytes() int64         { return s.retained }

// SetLogicalResponseID lets the request owner distinguish gateway aliases from
// ordinary upstream response IDs. Identity is immutable once output has started.
func (s *StreamRecoveryWriter) SetLogicalResponseID(id string) {
	if !s.started && len(s.outputs) == 0 && len(s.blocks) == 0 && !s.HasOutput() {
		s.id = id
	}
}

// Positive deltas reserve request memory; a negative delta allows the owner to
// release it (or retain its cumulative lease). The owner must serialize this hook.
func (s *StreamRecoveryWriter) SetRetainedBytesHook(hook func(int64) error) { s.retainedHook = hook }
func (s *StreamRecoveryWriter) Interrupted() (string, string, bool) {
	return s.interruptedCode, s.interruptedMessage, s.interrupted
}
func (s *StreamRecoveryWriter) TerminalSeen() bool           { return s.terminal }
func (s *StreamRecoveryWriter) AttemptUsage() map[string]any { return recoveryCopy(s.attemptUsage) }
func (s *StreamRecoveryWriter) SetUsage(usage map[string]any) {
	s.usage = recoveryCopy(usage)
	_ = s.accountRetained()
}

// OutputSnapshot is detached, protocol-native, and contains only actually emitted
// items/blocks. Opaque reasoning/signatures are preserved, never put in body text.
func (s *StreamRecoveryWriter) OutputSnapshot() []map[string]any {
	out := make([]map[string]any, 0)
	switch s.protocol {
	case "responses":
		for _, o := range s.outputs {
			out = append(out, s.outputItem(o))
		}
	case "messages":
		for _, b := range s.blocks {
			out = append(out, recoveryCopy(b.item))
		}
	case "chat":
		m := map[string]any{"role": "assistant", "content": s.text.String()}
		for k, v := range s.chatReasoning {
			if v != "" {
				m[k] = v
			}
		}
		if len(s.committed) > 0 {
			calls := make([]any, 0, len(s.committed))
			for _, c := range s.committed {
				calls = append(calls, recoveryCopy(c))
			}
			m["tool_calls"] = calls
		}
		if s.HasOutput() {
			out = append(out, m)
		}
	}
	return out
}
func (s *StreamRecoveryWriter) History() []map[string]any {
	out := s.OutputSnapshot()
	if s.protocol != "messages" {
		return out
	}
	if len(out) == 0 {
		return nil
	}
	content := make([]any, 0, len(out))
	for _, b := range out {
		content = append(content, b)
	}
	return []map[string]any{{"role": "assistant", "content": content}}
}
func (s *StreamRecoveryWriter) BeginAttempt() {
	release, reserveErr := s.reserveCopy(s.RetainedBytes() * 4)
	if reserveErr != nil {
		s.fail(reserveErr)
		return
	}
	defer release()
	if s.finished {
		s.fail(errors.New("stream recovery: already finished"))
		return
	}
	if s.err != nil {
		return
	}
	s.attemptErr = nil
	if s.HasCommittedTools() {
		s.fail(ErrStreamRecoveryCommittedTools)
		return
	}
	if s.UnsafeReplay() {
		s.fail(ErrStreamRecoveryUnsupported)
		return
	}
	// Exposed Responses reasoning must end before a fresh reasoning item starts.
	for _, o := range s.outputs {
		if !o.done && recoveryString(o.item["type"]) == "reasoning" {
			if err := s.closeResponseOutput(o, "incomplete"); err != nil {
				s.fail(err)
				return
			}
		}
	}
	s.frame = nil
	s.pending = make(map[string]*recoveryAtomicItem)
	s.chatTools = make(map[int]*recoveryAtomicItem)
	s.pendingBytes = 0
	s.resumeBase = s.text.String()
	s.resumePending = ""
	s.resuming = s.text.Len() > 0
	s.resumeSend = nil
	s.interruptedCode, s.interruptedMessage, s.interrupted = "", "", false
	s.terminal = false
	s.final = nil
	s.attemptUsage = make(map[string]any)
	s.upstreamID = ""
	s.attemptOutputs = make(map[string]*recoveryOutput)
	s.attemptParts = make(map[string]*recoveryPart)
	s.attemptBlocks = make(map[int]*recoveryBlock)
	s.mappedResume = false
	s.resumeOutput = nil
	s.resumePart = nil
	s.resumeBlock = nil
	for i := len(s.outputs) - 1; i >= 0; i-- {
		o := s.outputs[i]
		if recoveryString(o.item["type"]) == "message" && !o.done {
			keys := recoverySortedParts(o.parts)
			for j := len(keys) - 1; j >= 0; j-- {
				p := o.parts[keys[j]]
				if !p.done && recoveryString(p.part["type"]) == "output_text" {
					s.resumeOutput = o
					s.resumePart = p
					break
				}
			}
			if s.resumePart != nil {
				break
			}
		}
	}
	for i := len(s.blocks) - 1; i >= 0; i-- {
		if !s.blocks[i].done && recoveryString(s.blocks[i].item["type"]) == "text" {
			s.resumeBlock = s.blocks[i]
			break
		}
	}
	if err := s.accountRetained(); err != nil {
		s.fail(err)
	}
}
func (s *StreamRecoveryWriter) fail(err error) error {
	if errors.Is(err, ErrStreamRecoveryReplay) || errors.Is(err, ErrStreamRecoveryIncomplete) || errors.Is(err, ErrStreamRecoveryCommittedTools) {
		if s.attemptErr == nil {
			s.attemptErr = err
		}
		return s.attemptErr
	}
	if s.err == nil {
		s.err = err
	}
	return s.err
}
func recoveryCopy(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
func recoveryMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func recoveryString(v any) string      { x, _ := v.(string); return x }
func recoveryInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}
func recoveryMerge(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}
func recoverySortedParts(m map[int]*recoveryPart) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
func (s *StreamRecoveryWriter) captureUsage(m map[string]any) {
	if u := recoveryMap(m["usage"]); u != nil {
		recoveryMerge(s.attemptUsage, u)
	}
}

// Size traversal does not marshal/copy retained state on each frame. Shared
// request memory is reserved before input parsing by the transport; this reports
// additional logical state (including all protocol-native output channels).
func recoveryValueBytes(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case string:
		return int64(len(x))
	case map[string]any:
		n := int64(2)
		for k, v := range x {
			n += int64(len(k)+4) + recoveryValueBytes(v)
		}
		return n
	case []any:
		n := int64(2)
		for _, v := range x {
			n += 1 + recoveryValueBytes(v)
		}
		return n
	default:
		return 16
	}
}
func (s *StreamRecoveryWriter) retainedSize() int64 {
	n := int64(len(s.frame) + s.text.Len() + len(s.resumePending) + s.pendingBytes)
	for _, o := range s.outputs {
		n += recoveryValueBytes(o.item)
		for _, p := range o.parts {
			n += recoveryValueBytes(p.part)
		}
		for _, p := range o.summaries {
			n += recoveryValueBytes(p.part)
		}
	}
	for _, b := range s.blocks {
		n += recoveryValueBytes(b.item)
	}
	for _, p := range s.pending {
		n += int64(p.args.Len()) + recoveryValueBytes(p.item)
	}
	for _, p := range s.chatTools {
		n += int64(p.args.Len()) + recoveryValueBytes(p.item)
	}
	for _, v := range s.chatReasoning {
		n += int64(len(v))
	}
	for _, m := range []map[string]any{s.meta, s.final, s.usage, s.attemptUsage} {
		n += recoveryValueBytes(m)
	}
	for _, m := range s.committed {
		n += recoveryValueBytes(m)
	}
	return n
}
func (s *StreamRecoveryWriter) reserveCopy(n int64) (func(), error) {
	if s.retainedHook == nil || n == 0 {
		return func() {}, nil
	}
	if err := s.retainedHook(n); err != nil {
		return nil, err
	}
	return func() { _ = s.retainedHook(-n) }, nil
}
func (s *StreamRecoveryWriter) accountRetained() error {
	n := s.retainedSize()
	delta := n - s.retained
	if n > StreamRecoveryBufferLimit {
		return ErrStreamRecoveryLimit
	}
	if delta != 0 && s.retainedHook != nil {
		if err := s.retainedHook(delta); err != nil {
			return err
		}
	}
	s.retained = n
	return nil
}
func (s *StreamRecoveryWriter) WriteString(p string) (int, error) { return s.Write([]byte(p)) }
func (s *StreamRecoveryWriter) Write(p []byte) (int, error) {
	if !s.HasOutput() && !s.ResponseWriter.Written() && s.ResponseWriter.Status() >= 400 && strings.Contains(s.Header().Get("Content-Type"), "application/json") {
		s.nonSSE = true
		return s.ResponseWriter.Write(p)
	}
	if s.nonSSE {
		return s.ResponseWriter.Write(p)
	}
	if err := s.Err(); err != nil {
		return 0, err
	}
	if s.finished {
		return 0, s.fail(errors.New("stream recovery: write after finish"))
	}
	original := len(p)
	// Incremental frame assembly permits large batches of small SSE frames without
	// treating the whole upstream body as an 8 MiB retained buffer.
	for len(p) > 0 {
		n := bytes.IndexByte(p, '\n')
		if n < 0 {
			n = len(p) - 1
		}
		n++
		if len(s.frame)+n > StreamRecoveryBufferLimit {
			return 0, s.fail(ErrStreamRecoveryLimit)
		}
		s.frame = append(s.frame, p[:n]...)
		p = p[n:]
		trim := bytes.TrimSpace(s.frame)
		if len(trim) > 0 && (trim[0] == '{' || trim[0] == '[' || trim[0] == '<') {
			return 0, s.fail(fmt.Errorf("%w: non-SSE response", ErrStreamRecoveryUnsupported))
		}
		width := 0
		if bytes.HasSuffix(s.frame, []byte("\r\n\r\n")) {
			width = 4
		} else if bytes.HasSuffix(s.frame, []byte("\n\n")) {
			width = 2
		}
		if width > 0 {
			raw := string(s.frame[:len(s.frame)-width])
			s.frame = nil
			if err := s.processFrame(raw); err != nil {
				return 0, s.fail(err)
			}
		}
		if err := s.accountRetained(); err != nil {
			return 0, s.fail(err)
		}
	}
	return original, nil
}
func (s *StreamRecoveryWriter) Flush() {
	if s.Err() == nil {
		s.Header().Del("Content-Length")
		s.ResponseWriter.Flush()
	}
}
func (s *StreamRecoveryWriter) processFrame(raw string) error {
	var event string
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			lines = append(lines, value)
		}
	}
	if len(lines) == 0 {
		if strings.HasPrefix(strings.TrimSpace(raw), ":") {
			_, err := s.ResponseWriter.WriteString(raw + "\n\n")
			return err
		}
		return nil
	}
	payload := strings.Join(lines, "\n")
	if payload == "[DONE]" {
		if s.protocol != "chat" {
			s.unsafe = true
			_, err := s.ResponseWriter.WriteString(raw + "\n\n")
			return err
		}
		s.terminal = true
		return nil
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil || data == nil {
		return fmt.Errorf("%w: invalid SSE JSON", ErrStreamRecoveryUnsupported)
	}
	if event == "" {
		event = recoveryString(data["type"])
	}
	s.captureUsage(data)
	if r := recoveryMap(data["response"]); r != nil {
		s.captureUsage(r)
	}
	if m := recoveryMap(data["message"]); m != nil {
		s.captureUsage(m)
	}
	if event == "error" || event == "response.failed" || event == "response.incomplete" || data["error"] != nil {
		failure := recoveryMap(data["error"])
		if r := recoveryMap(data["response"]); failure == nil && r != nil {
			failure = recoveryMap(r["error"])
		}
		if failure == nil {
			failure = data
		}
		s.interruptedCode, s.interruptedMessage, s.interrupted = recoveryString(failure["code"]), recoveryString(failure["message"]), true
		if s.interruptedCode == "" {
			s.interruptedCode = "upstream_stream_interrupted"
		}
		return nil
	}
	if s.interrupted {
		return nil
	}
	if s.terminal {
		choices, _ := data["choices"].([]any)
		if s.protocol == "chat" && len(choices) == 0 && data["usage"] != nil {
			return nil
		}
		return errors.New("stream recovery: event after terminal")
	}
	switch s.protocol {
	case "responses":
		return s.responses(event, data)
	case "chat":
		return s.chat(data)
	case "messages":
		return s.messages(event, data)
	}
	return ErrStreamRecoveryUnsupported
}
func (s *StreamRecoveryWriter) emit(event string, data map[string]any) error {
	if s.protocol == "responses" {
		data["sequence_number"] = s.seq
		s.seq++
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	frame := "data: " + string(b) + "\n\n"
	if event != "" {
		frame = "event: " + event + "\n" + frame
	}
	s.Header().Del("Content-Length")
	_, err = s.ResponseWriter.WriteString(frame)
	return err
}
func (s *StreamRecoveryWriter) identity(m map[string]any) {
	if raw := recoveryString(m["id"]); raw != "" {
		s.upstreamID = raw
	}
	if s.id == "" {
		s.id = s.upstreamID
		if s.id == "" {
			s.id = fmt.Sprintf("recovery_%d", time.Now().UnixNano())
		}
	}
	m["id"] = s.id
}

// recoveryOverlap finds exact bytes; callers must also require a complete anchor
// and UTF-8 boundaries. A coincidental one-character suffix is not a seam.
func recoveryOverlap(old, next string) int {
	if next == "" {
		return 0
	}
	pi := make([]int, len(next))
	for i, j := 1, 0; i < len(next); i++ {
		for j > 0 && next[i] != next[j] {
			j = pi[j-1]
		}
		if next[i] == next[j] {
			j++
		}
		pi[i] = j
	}
	j := 0
	for i := 0; i < len(old); i++ {
		for j > 0 && (j == len(next) || old[i] != next[j]) {
			j = pi[j-1]
		}
		if j < len(next) && old[i] == next[j] {
			j++
		}
	}
	return j
}
func recoveryAnchor(text string) string {
	body := strings.TrimSuffix(text, "\n")
	if i := strings.LastIndexByte(body, '\n'); i >= 0 {
		return text[i+1:]
	}
	return text
}
func (s *StreamRecoveryWriter) align(text string, final bool) (string, error) {
	if !s.resuming {
		return text, nil
	}
	s.resumePending += text
	p := s.resumePending
	if p == "" {
		if final {
			s.resuming = false
		}
		return "", nil
	}
	anchor := recoveryAnchor(s.resumeBase)
	if len(anchor) < len(s.resumeBase) && strings.HasPrefix(p, s.resumeBase) {
		return "", ErrStreamRecoveryReplay
	}
	// Recognize a full repeated opening line, not a coincidental first byte.
	if i := strings.IndexByte(s.resumeBase, '\n'); i >= 0 && len(s.resumeBase) > len(anchor) && len(p) >= i+1 && strings.HasPrefix(p, s.resumeBase[:i+1]) {
		return "", ErrStreamRecoveryReplay
	}
	if !final && (strings.HasPrefix(anchor, p) || strings.HasPrefix(s.resumeBase, p)) {
		return "", nil
	}
	n := recoveryOverlap(s.resumeBase, p)
	if n < len(anchor) || !utf8.ValidString(p[:n]) || !utf8.ValidString(s.resumeBase[len(s.resumeBase)-n:]) {
		n = 0
	}
	if final && strings.HasPrefix(anchor, p) {
		n = len(p)
	}
	s.resuming = false
	s.resumePending = ""
	return p[n:], nil
}
func (s *StreamRecoveryWriter) emitText(text string, send func(string) error) error {
	s.resumeSend = send
	out, err := s.align(text, false)
	if err != nil {
		return err
	}
	if out == "" {
		return nil
	}
	if s.text.Len()+len(out)+s.pendingBytes > StreamRecoveryBufferLimit {
		return ErrStreamRecoveryLimit
	}
	if err = send(out); err != nil {
		return err
	}
	s.text.WriteString(out)
	s.progress++
	return nil
}
func (s *StreamRecoveryWriter) flushSeam() error {
	out, err := s.align("", true)
	if err != nil {
		return err
	}
	if out != "" {
		if s.resumeSend == nil {
			return ErrStreamRecoveryIncomplete
		}
		if err = s.resumeSend(out); err != nil {
			return err
		}
		s.text.WriteString(out)
		s.progress++
	}
	return nil
}
func (s *StreamRecoveryWriter) newOutput(item map[string]any) *recoveryOutput {
	item = recoveryCopy(item)
	id := recoveryString(item["id"])
	if id == "" {
		id = fmt.Sprintf("%s_item_%d", s.id, len(s.outputs))
	}
	base := id
	for i := 0; ; i++ {
		collision := false
		for _, o := range s.outputs {
			if o.item["id"] == id {
				collision = true
				break
			}
		}
		if !collision {
			break
		}
		id = fmt.Sprintf("%s_recovery_%d", base, i+1)
	}
	item["id"] = id
	o := &recoveryOutput{item: item, index: len(s.outputs), parts: make(map[int]*recoveryPart), summaries: make(map[int]*recoveryPart)}
	s.outputs = append(s.outputs, o)
	return o
}
func (s *StreamRecoveryWriter) outputItem(o *recoveryOutput) map[string]any {
	m := recoveryCopy(o.item)
	if len(o.parts) > 0 {
		parts := make([]any, 0, len(o.parts))
		for _, k := range recoverySortedParts(o.parts) {
			parts = append(parts, recoveryCopy(o.parts[k].part))
		}
		m["content"] = parts
	}
	if len(o.summaries) > 0 {
		parts := make([]any, 0, len(o.summaries))
		for _, k := range recoverySortedParts(o.summaries) {
			parts = append(parts, recoveryCopy(o.summaries[k].part))
		}
		m["summary"] = parts
	}
	return m
}
func (s *StreamRecoveryWriter) findOutput(m map[string]any) *recoveryOutput {
	key := recoveryString(m["item_id"])
	if key == "" {
		key = recoveryString(recoveryMap(m["item"])["id"])
	}
	if o := s.attemptOutputs[key]; o != nil {
		return o
	}
	return s.attemptOutputs[fmt.Sprintf("index:%d", recoveryInt(m["output_index"]))]
}
func recoveryPartKey(m map[string]any) string {
	return fmt.Sprintf("%s:%d", recoveryString(m["item_id"]), recoveryInt(m["content_index"]))
}
func (s *StreamRecoveryWriter) normalizeOutput(m map[string]any, o *recoveryOutput) {
	m["output_index"] = o.index
	if _, ok := m["item_id"]; ok {
		m["item_id"] = o.item["id"]
	}
	if item := recoveryMap(m["item"]); item != nil {
		item["id"] = o.item["id"]
	}
}
func (s *StreamRecoveryWriter) responses(event string, m map[string]any) error {
	switch event {
	case "response.created", "response.in_progress":
		r := recoveryMap(m["response"])
		if r == nil {
			return ErrStreamRecoveryUnsupported
		}
		s.identity(r)
		if s.meta == nil {
			s.meta = recoveryCopy(r)
		}
		if event == "response.created" {
			if s.started {
				return nil
			}
			s.started = true
		} else {
			if s.inProgress {
				return nil
			}
			s.inProgress = true
		}
		return s.emit(event, m)
	case "response.output_item.added":
		item := recoveryMap(m["item"])
		if item == nil {
			return ErrStreamRecoveryUnsupported
		}
		kind := recoveryString(item["type"])
		rawID := recoveryString(item["id"])
		idx := recoveryInt(m["output_index"])
		if kind == "function_call" || kind == "custom_tool_call" {
			if rawID == "" {
				return ErrStreamRecoveryUnsupported
			}
			p := &recoveryAtomicItem{item: recoveryCopy(item), kind: kind, index: idx}
			if kind == "function_call" {
				p.args.WriteString(recoveryString(item["arguments"]))
			} else {
				p.args.WriteString(recoveryString(item["input"]))
			}
			s.pending[rawID] = p
			return s.buffer(p, event, m)
		}
		var o *recoveryOutput
		if kind == "message" && s.resumeOutput != nil && !s.mappedResume {
			o = s.resumeOutput
			s.mappedResume = true
		} else {
			o = s.newOutput(item)
			if kind == "reasoning" {
				if _, ok := o.item["summary"]; !ok {
					o.item["summary"] = []any{}
				}
			} else if kind == "web_search_call" || kind == "file_search_call" {
				s.readOnlyHosted = true
			} else if kind != "message" {
				s.unsafe = true
			}
		}
		s.attemptOutputs[rawID] = o
		s.attemptOutputs[fmt.Sprintf("index:%d", idx)] = o
		s.normalizeOutput(m, o)
		if o == s.resumeOutput {
			return nil
		}
		return s.emit(event, m)
	case "response.content_part.added":
		o := s.findOutput(m)
		if o == nil {
			s.unsafe = true
			return s.emit(event, m)
		}
		key := recoveryPartKey(m)
		idx := recoveryInt(m["content_index"])
		part := recoveryMap(m["part"])
		if part == nil {
			return ErrStreamRecoveryUnsupported
		}
		var p *recoveryPart
		if o == s.resumeOutput && s.resumePart != nil && len(s.attemptParts) == 0 {
			p = s.resumePart
		} else {
			logical := idx
			if o == s.resumeOutput {
				logical = len(o.parts)
			}
			p = &recoveryPart{part: recoveryCopy(part), index: logical}
			o.parts[logical] = p
		}
		s.attemptParts[key] = p
		s.normalizeOutput(m, o)
		m["content_index"] = p.index
		if p == s.resumePart {
			return nil
		}
		return s.emit(event, m)
	case "response.output_text.delta", "response.refusal.delta":
		o := s.findOutput(m)
		p := s.attemptParts[recoveryPartKey(m)]
		if o == nil || p == nil {
			s.unsafe = true
			return s.emit(event, m)
		}
		s.normalizeOutput(m, o)
		m["content_index"] = p.index
		field := "text"
		if event == "response.refusal.delta" {
			field = "refusal"
		}
		return s.emitText(recoveryString(m["delta"]), func(text string) error {
			m["delta"] = text
			if err := s.emit(event, m); err != nil {
				return err
			}
			p.part[field] = recoveryString(p.part[field]) + text
			return nil
		})
	case "response.output_text.annotation.added":
		o := s.findOutput(m)
		p := s.attemptParts[recoveryPartKey(m)]
		if o == nil || p == nil {
			s.unsafe = true
			return s.emit(event, m)
		}
		s.normalizeOutput(m, o)
		m["content_index"] = p.index
		if err := s.emit(event, m); err != nil {
			return err
		}
		a, _ := p.part["annotations"].([]any)
		annotation := recoveryMap(m["annotation"])
		if annotation != nil {
			a = append(a, recoveryCopy(annotation))
			p.part["annotations"] = a
			s.progress++
		}
		return nil
	case "response.output_text.done", "response.refusal.done", "response.content_part.done":
		o := s.findOutput(m)
		p := s.attemptParts[recoveryPartKey(m)]
		if o == nil || p == nil {
			s.unsafe = true
			return s.emit(event, m)
		}
		if err := s.flushSeam(); err != nil {
			return err
		}
		s.normalizeOutput(m, o)
		m["content_index"] = p.index
		if event == "response.content_part.done" {
			if p.done {
				return nil
			}
			if part := recoveryMap(m["part"]); part != nil {
				for k, v := range part {
					if k != "text" && k != "refusal" {
						p.part[k] = v
					}
				}
			}
			m["part"] = recoveryCopy(p.part)
			p.done = true
		} else {
			if p.textDone {
				return nil
			}
			field := "text"
			if event == "response.refusal.done" {
				field = "refusal"
			}
			m[field] = p.part[field]
			p.textDone = true
		}
		return s.emit(event, m)
	case "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		p := s.pending[recoveryString(m["item_id"])]
		if p == nil {
			s.unsafe = true
			return s.emit(event, m)
		}
		if strings.HasSuffix(event, ".delta") {
			p.args.WriteString(recoveryString(m["delta"]))
		}
		return s.buffer(p, event, m)
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done":
		o := s.findOutput(m)
		if o == nil {
			s.unsafe = true
			return s.emit(event, m)
		}
		summary := strings.Contains(event, "summary")
		idx := recoveryInt(m["summary_index"])
		parts := o.summaries
		if !summary {
			idx = recoveryInt(m["content_index"])
			parts = o.parts
		}
		p := parts[idx]
		if p == nil {
			typ := "summary_text"
			if !summary {
				typ = "reasoning_text"
			}
			part := map[string]any{"type": typ, "text": ""}
			if x := recoveryMap(m["part"]); x != nil {
				part = recoveryCopy(x)
			}
			p = &recoveryPart{part: part, index: idx}
			parts[idx] = p
		}
		s.normalizeOutput(m, o)
		if err := s.emit(event, m); err != nil {
			return err
		}
		if strings.HasSuffix(event, ".delta") {
			text := recoveryString(m["delta"])
			p.part["text"] = recoveryString(p.part["text"]) + text
			if text != "" {
				s.hasReasoning = true
				s.progress++
			}
		} else if strings.HasSuffix(event, ".done") {
			if part := recoveryMap(m["part"]); part != nil {
				recoveryMerge(p.part, part)
			} else if text, ok := m["text"].(string); ok {
				p.part["text"] = text
			}
			if strings.Contains(event, "part.done") {
				p.done = true
			} else {
				p.textDone = true
			}
		}
		return nil
	case "response.output_item.done":
		item := recoveryMap(m["item"])
		if item == nil {
			return ErrStreamRecoveryUnsupported
		}
		rawID := recoveryString(item["id"])
		if p := s.pending[rawID]; p != nil {
			merged := recoveryCopy(p.item)
			recoveryMerge(merged, item)
			field := "arguments"
			if p.kind == "custom_tool_call" {
				field = "input"
			}
			args := recoveryString(merged[field])
			if args == "" {
				args = p.args.String()
				merged[field] = args
			}
			valid := recoveryString(merged["call_id"]) != "" && recoveryString(merged["name"]) != ""
			if p.kind == "function_call" {
				valid = valid && json.Valid([]byte(args))
			}
			if p.args.Len() > 0 && args != p.args.String() {
				valid = false
			}
			if !valid {
				p.invalid = true
				return nil
			}
			m["item"] = merged
			if err := s.buffer(p, event, m); err != nil {
				return err
			}
			return s.commitResponseTool(rawID, p, merged)
		}
		o := s.findOutput(m)
		if o == nil {
			s.unsafe = true
			return s.emit(event, m)
		}
		if o.done {
			return nil
		}
		if recoveryString(o.item["type"]) == "message" {
			if err := s.flushSeam(); err != nil {
				return err
			}
		}
		for k, v := range item {
			if k != "id" && k != "content" && k != "summary" {
				o.item[k] = v
			}
		}
		if recoveryString(o.item["type"]) == "reasoning" {
			// Authentic done payloads may supply encrypted/raw reasoning which has
			// no public delta. Keep both channels independently, never synthesize.
			if v, ok := item["summary"]; ok && len(o.summaries) == 0 {
				o.item["summary"] = v
			}
			if v, ok := item["content"]; ok && len(o.parts) == 0 {
				o.item["content"] = v
			}
			s.hasReasoning = true
		} else if recoveryString(o.item["type"]) == "message" {
			if content, ok := item["content"].([]any); ok {
				for i, raw := range content {
					if p := o.parts[i]; p != nil {
						for k, v := range recoveryMap(raw) {
							if k != "text" && k != "refusal" {
								p.part[k] = v
							}
						}
					}
				}
			}
		} else {
			for _, k := range []string{"content", "summary"} {
				if v, ok := item[k]; ok {
					o.item[k] = v
				}
			}
		}
		s.normalizeOutput(m, o)
		m["item"] = s.outputItem(o)
		o.done = true
		if err := s.emit(event, m); err != nil {
			return err
		}
		if recoveryString(o.item["type"]) == "reasoning" || recoveryString(o.item["type"]) == "web_search_call" || recoveryString(o.item["type"]) == "file_search_call" {
			s.progress++
		}
		return nil
	case "response.completed":
		if err := s.flushSeam(); err != nil {
			return err
		}
		s.terminal = true
		s.final = recoveryCopy(recoveryMap(m["response"]))
		if len(s.pending) > 0 {
			s.markIncomplete()
		}
		return nil
	default:
		// Extension/hosted progress events are legal during a normal turn. Only
		// known read-only search extensions can be carried across recovery.
		if strings.HasPrefix(event, "response.web_search_call.") || strings.HasPrefix(event, "response.file_search_call.") {
			s.readOnlyHosted = true
		} else if event != "response.queued" {
			s.unsafe = true
		}
		if o := s.findOutput(m); o != nil {
			s.normalizeOutput(m, o)
		}
		return s.emit(event, m)
	}
}
func (s *StreamRecoveryWriter) buffer(p *recoveryAtomicItem, event string, m map[string]any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if s.text.Len()+s.pendingBytes+len(b) > StreamRecoveryBufferLimit {
		return ErrStreamRecoveryLimit
	}
	p.bytes += len(b)
	s.pendingBytes += len(b)
	p.events = append(p.events, recoveryEvent{event, recoveryCopy(m)})
	return nil
}
func (s *StreamRecoveryWriter) commitResponseTool(key string, p *recoveryAtomicItem, item map[string]any) error {
	o := s.newOutput(item)
	o.done = true
	for _, e := range p.events {
		s.normalizeOutput(e.data, o)
		if e.event == "response.output_item.done" {
			e.data["item"] = s.outputItem(o)
		}
		if err := s.emit(e.event, e.data); err != nil {
			return err
		}
	}
	s.committed = append(s.committed, s.outputItem(o))
	s.pendingBytes -= p.bytes
	delete(s.pending, key)
	s.progress++
	return nil
}
func (s *StreamRecoveryWriter) markIncomplete() {
	s.interrupted = true
	s.interruptedCode = "upstream_incomplete_item"
	s.interruptedMessage = "upstream stopped before an atomic item completed"
}
func (s *StreamRecoveryWriter) chat(m map[string]any) error {
	s.identity(m)
	if s.meta == nil {
		s.meta = recoveryCopy(m)
		delete(s.meta, "choices")
	}
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	if len(choices) != 1 || recoveryInt(recoveryMap(choices[0])["index"]) != 0 {
		s.unsafe = true
		return s.emit("", m)
	}
	c := recoveryMap(choices[0])
	d := recoveryMap(c["delta"])
	if d == nil {
		d = map[string]any{}
		c["delta"] = d
	}
	if role := recoveryString(d["role"]); role != "" {
		if s.started {
			delete(d, "role")
		} else {
			s.started = true
		}
	}
	tools, _ := d["tool_calls"].([]any)
	delete(d, "tool_calls")
	for _, raw := range tools {
		tool := recoveryMap(raw)
		idx := recoveryInt(tool["index"])
		p := s.chatTools[idx]
		if p == nil {
			p = &recoveryAtomicItem{item: recoveryCopy(tool), kind: "function_call", index: idx}
			p.item["function"] = map[string]any{}
			s.chatTools[idx] = p
		}
		for k, v := range tool {
			if k != "function" && k != "index" {
				p.item[k] = v
			}
		}
		f := recoveryMap(tool["function"])
		old := recoveryMap(p.item["function"])
		if name := recoveryString(f["name"]); name != "" {
			if old["name"] != name {
				old["name"] = recoveryString(old["name"]) + name
			}
		}
		for k, v := range f {
			if k != "name" && k != "arguments" {
				old[k] = v
			}
		}
		p.args.WriteString(recoveryString(f["arguments"]))
		p.bytes += len(recoveryString(f["arguments"]))
		s.pendingBytes += len(recoveryString(f["arguments"]))
	}
	finish := recoveryString(c["finish_reason"])
	c["finish_reason"] = nil
	if d["function_call"] != nil {
		s.unsafe = true
	}
	// Reasoning is an independent channel. Never feed it to seam/body history.
	for _, field := range []string{"reasoning_content", "reasoning"} {
		if v, ok := d[field].(string); ok && v != "" {
			out := recoveryCopy(m)
			recoveryMap(out["choices"].([]any)[0])["delta"] = map[string]any{field: v}
			if err := s.emit("", out); err != nil {
				return err
			}
			s.chatReasoning[field] += v
			s.hasReasoning = true
			s.progress++
			delete(d, field)
		}
	}
	if text := recoveryString(d["content"]); text != "" {
		if err := s.emitText(text, func(text string) error { d["content"] = text; return s.emit("", m) }); err != nil {
			return err
		}
	} else if len(d) > 0 {
		if err := s.emit("", m); err != nil {
			return err
		}
	}
	if finish != "" {
		if err := s.flushSeam(); err != nil {
			return err
		}
		if len(s.chatTools) > 0 {
			keys := make([]int, 0, len(s.chatTools))
			for k := range s.chatTools {
				keys = append(keys, k)
			}
			sort.Ints(keys)
			calls := make([]any, 0, len(keys))
			incomplete := false
			for _, k := range keys {
				p := s.chatTools[k]
				f := recoveryMap(p.item["function"])
				if finish != "tool_calls" || !json.Valid([]byte(p.args.String())) || recoveryString(p.item["id"]) == "" || recoveryString(f["name"]) == "" {
					incomplete = true
					continue
				}
				f["arguments"] = p.args.String()
				p.item["index"] = len(s.committed) + len(calls)
				calls = append(calls, recoveryCopy(p.item))
			}
			if len(calls) > 0 {
				out := recoveryCopy(m)
				choice := recoveryMap(out["choices"].([]any)[0])
				choice["delta"] = map[string]any{"tool_calls": calls}
				choice["finish_reason"] = nil
				if err := s.emit("", out); err != nil {
					return err
				}
				for _, call := range calls {
					s.committed = append(s.committed, recoveryCopy(recoveryMap(call)))
				}
				s.progress++
			}
			if incomplete {
				s.markIncomplete()
			}
			s.chatTools = make(map[int]*recoveryAtomicItem)
			s.pendingBytes = 0
		}
		c["finish_reason"] = finish
		s.final = recoveryCopy(m)
		s.terminal = true
	}
	return nil
}
func (s *StreamRecoveryWriter) newBlock(item map[string]any) *recoveryBlock {
	b := &recoveryBlock{item: recoveryCopy(item), index: len(s.blocks)}
	s.blocks = append(s.blocks, b)
	return b
}
func (s *StreamRecoveryWriter) messages(event string, m map[string]any) error {
	idx := recoveryInt(m["index"])
	key := fmt.Sprintf("block:%d", idx)
	switch event {
	case "message_start":
		message := recoveryMap(m["message"])
		if message == nil {
			return ErrStreamRecoveryUnsupported
		}
		s.identity(message)
		if s.started {
			return nil
		}
		s.meta = recoveryCopy(message)
		s.started = true
		return s.emit(event, m)
	case "ping":
		return s.emit(event, m)
	case "content_block_start":
		block := recoveryMap(m["content_block"])
		if block == nil {
			return ErrStreamRecoveryUnsupported
		}
		kind := recoveryString(block["type"])
		if kind == "tool_use" || kind == "thinking" || kind == "redacted_thinking" {
			if s.resumeBlock != nil && !s.mappedResume {
				resumeAfterThinking := s.resuming && s.resumePending == ""
				if err := s.closeMessageText(); err != nil {
					return err
				}
				s.resuming = resumeAfterThinking
				s.resumeBlock = nil
				s.mappedResume = true
			}
			p := &recoveryAtomicItem{item: recoveryCopy(block), kind: kind, index: idx, signed: recoveryString(block["signature"]) != ""}
			s.pending[key] = p
			return s.buffer(p, event, m)
		}
		var b *recoveryBlock
		if kind == "text" && s.resumeBlock != nil && !s.mappedResume {
			b = s.resumeBlock
			s.mappedResume = true
		} else {
			if err := s.closeMessageText(); err != nil {
				return err
			}
			b = s.newBlock(block)
			if kind != "text" {
				if kind == "server_tool_use" && (block["name"] == "web_search" || block["name"] == "web_fetch") {
					s.readOnlyHosted = true
				} else if kind == "web_search_tool_result" || kind == "web_fetch_tool_result" {
					s.readOnlyHosted = true
				} else {
					s.unsafe = true
				}
			}
		}
		s.attemptBlocks[idx] = b
		m["index"] = b.index
		if b == s.resumeBlock {
			return nil
		}
		return s.emit(event, m)
	case "content_block_delta":
		d := recoveryMap(m["delta"])
		kind := recoveryString(d["type"])
		if b := s.attemptBlocks[idx]; b != nil {
			m["index"] = b.index
			if kind == "text_delta" && b.item["type"] == "text" {
				return s.emitText(recoveryString(d["text"]), func(text string) error {
					d["text"] = text
					if err := s.emit(event, m); err != nil {
						return err
					}
					b.item["text"] = recoveryString(b.item["text"]) + text
					return nil
				})
			}
			s.unsafe = true
			return s.emit(event, m)
		}
		p := s.pending[key]
		if p == nil {
			s.unsafe = true
			return s.emit(event, m)
		}
		switch kind {
		case "input_json_delta":
			if p.kind != "tool_use" {
				p.invalid = true
			}
			p.args.WriteString(recoveryString(d["partial_json"]))
		case "thinking_delta":
			if p.kind != "thinking" {
				p.invalid = true
			}
			p.item["thinking"] = recoveryString(p.item["thinking"]) + recoveryString(d["thinking"])
		case "signature_delta":
			if p.kind != "thinking" {
				p.invalid = true
			}
			sig := recoveryString(d["signature"])
			p.item["signature"] = recoveryString(p.item["signature"]) + sig
			p.signed = recoveryString(p.item["signature"]) != ""
		default:
			s.unsafe = true
			return s.buffer(p, event, m)
		}
		return s.buffer(p, event, m)
	case "content_block_stop":
		if b := s.attemptBlocks[idx]; b != nil {
			if b.done {
				return nil
			}
			if err := s.flushSeam(); err != nil {
				return err
			}
			m["index"] = b.index
			if err := s.emit(event, m); err != nil {
				return err
			}
			b.done = true
			return nil
		}
		p := s.pending[key]
		if p == nil {
			s.unsafe = true
			return s.emit(event, m)
		}
		if p.kind == "tool_use" {
			args := p.args.String()
			if args == "" {
				raw, _ := json.Marshal(p.item["input"])
				args = string(raw)
			}
			var input map[string]any
			if json.Unmarshal([]byte(args), &input) != nil || input == nil || recoveryString(p.item["id"]) == "" || recoveryString(p.item["name"]) == "" {
				p.invalid = true
			} else {
				p.item["input"] = input
			}
		}
		if p.kind == "thinking" && !p.signed {
			p.invalid = true
		}
		if p.invalid {
			s.pendingBytes -= p.bytes
			delete(s.pending, key)
			return nil
		}
		if err := s.closeMessageText(); err != nil {
			return err
		}
		b := s.newBlock(p.item)
		b.done = true
		if err := s.buffer(p, event, m); err != nil {
			return err
		}
		for _, e := range p.events {
			e.data["index"] = b.index
			if err := s.emit(e.event, e.data); err != nil {
				return err
			}
		}
		s.pendingBytes -= p.bytes
		delete(s.pending, key)
		if p.kind == "tool_use" {
			s.committed = append(s.committed, recoveryCopy(p.item))
		} else {
			s.hasReasoning = true
		}
		s.progress++
		return nil
	case "message_delta":
		s.final = recoveryCopy(m)
		return nil
	case "message_stop":
		if err := s.flushSeam(); err != nil {
			return err
		}
		s.terminal = true
		if len(s.pending) > 0 {
			s.markIncomplete()
		}
		return nil
	default:
		s.unsafe = true
		return s.emit(event, m)
	}
}
func (s *StreamRecoveryWriter) closeMessageText() error {
	for _, b := range s.blocks {
		if !b.done && b.item["type"] == "text" {
			if err := s.flushSeam(); err != nil {
				return err
			}
			if err := s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.index}); err != nil {
				return err
			}
			b.done = true
			if b == s.resumeBlock {
				s.resumeBlock = nil
			}
		}
	}
	return nil
}
func (s *StreamRecoveryWriter) closeResponseOutput(o *recoveryOutput, status string) error {
	if o.done {
		return nil
	}
	kind := recoveryString(o.item["type"])
	for _, idx := range recoverySortedParts(o.parts) {
		p := o.parts[idx]
		if p.done {
			continue
		}
		event := "response.output_text.done"
		field := "text"
		if p.part["type"] == "refusal" {
			event = "response.refusal.done"
			field = "refusal"
		} else if kind == "reasoning" {
			event = "response.reasoning_text.done"
		}
		if !p.textDone {
			if err := s.emit(event, map[string]any{"type": event, "item_id": o.item["id"], "output_index": o.index, "content_index": p.index, field: p.part[field]}); err != nil {
				return err
			}
			p.textDone = true
		}
		if kind != "reasoning" {
			if err := s.emit("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": o.item["id"], "output_index": o.index, "content_index": p.index, "part": recoveryCopy(p.part)}); err != nil {
				return err
			}
		}
		p.done = true
	}
	for _, idx := range recoverySortedParts(o.summaries) {
		p := o.summaries[idx]
		if p.done {
			continue
		}
		if !p.textDone {
			if err := s.emit("response.reasoning_summary_text.done", map[string]any{"type": "response.reasoning_summary_text.done", "item_id": o.item["id"], "output_index": o.index, "summary_index": p.index, "text": p.part["text"]}); err != nil {
				return err
			}
			p.textDone = true
		}
		if err := s.emit("response.reasoning_summary_part.done", map[string]any{"type": "response.reasoning_summary_part.done", "item_id": o.item["id"], "output_index": o.index, "summary_index": p.index, "part": recoveryCopy(p.part)}); err != nil {
			return err
		}
		p.done = true
	}
	o.item["status"] = status
	if err := s.emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": o.index, "item": s.outputItem(o)}); err != nil {
		return err
	}
	o.done = true
	return nil
}
func (s *StreamRecoveryWriter) finalUsage() map[string]any {
	if s.usage != nil {
		return recoveryCopy(s.usage)
	}
	return recoveryCopy(s.attemptUsage)
}
func (s *StreamRecoveryWriter) responseFinal(status string) map[string]any {
	r := recoveryCopy(s.meta)
	if r == nil {
		r = map[string]any{"object": "response", "created_at": time.Now().Unix()}
	}
	if s.final != nil {
		for k, v := range s.final {
			if k != "id" && k != "output" {
				r[k] = v
			}
		}
	}
	r["id"] = s.id
	if s.id == "" {
		s.identity(r)
	}
	r["status"] = status
	output := make([]any, 0, len(s.outputs))
	for _, item := range s.OutputSnapshot() {
		output = append(output, item)
	}
	r["output"] = output
	delete(r, "error")
	delete(r, "incomplete_details")
	if u := s.finalUsage(); len(u) > 0 {
		r["usage"] = u
	}
	return r
}
func (s *StreamRecoveryWriter) FinishSuccess() error {
	if s.finished {
		return s.Err()
	}
	release, reserveErr := s.reserveCopy(s.RetainedBytes() * 4)
	if reserveErr != nil {
		return s.fail(reserveErr)
	}
	defer release()
	if s.finished {
		return s.Err()
	}
	if err := s.Err(); err != nil {
		return err
	}
	if s.interrupted || !s.terminal || len(bytes.TrimSpace(s.frame)) > 0 || len(s.pending) > 0 || len(s.chatTools) > 0 {
		return s.fail(ErrStreamRecoveryIncomplete)
	}
	if err := s.flushSeam(); err != nil {
		return s.fail(err)
	}
	var err error
	switch s.protocol {
	case "responses":
		for _, o := range s.outputs {
			if err = s.closeResponseOutput(o, "completed"); err != nil {
				return s.fail(err)
			}
		}
		err = s.emit("response.completed", map[string]any{"type": "response.completed", "response": s.responseFinal("completed")})
	case "chat":
		m := recoveryCopy(s.final)
		if m == nil {
			m = recoveryCopy(s.meta)
		}
		if m == nil {
			return s.fail(ErrStreamRecoveryIncomplete)
		}
		m["id"] = s.id
		reason := "stop"
		if s.HasCommittedTools() {
			reason = "tool_calls"
		} else if choices, ok := m["choices"].([]any); ok && len(choices) == 1 {
			if r := recoveryString(recoveryMap(choices[0])["finish_reason"]); r != "" {
				reason = r
			}
		}
		m["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}}
		delete(m, "usage")
		if err = s.emit("", m); err != nil {
			return s.fail(err)
		}
		if usage := s.finalUsage(); len(usage) > 0 {
			m["choices"] = []any{}
			m["usage"] = usage
			if err = s.emit("", m); err != nil {
				return s.fail(err)
			}
		}
		_, err = s.ResponseWriter.WriteString("data: [DONE]\n\n")
	case "messages":
		if err = s.closeMessageText(); err != nil {
			return s.fail(err)
		}
		reason := "end_turn"
		if s.final != nil {
			if r := recoveryString(recoveryMap(s.final["delta"])["stop_reason"]); r != "" {
				reason = r
			}
		}
		if s.HasCommittedTools() {
			reason = "tool_use"
		}
		delta := map[string]any{"stop_reason": reason, "stop_sequence": nil}
		if s.final != nil {
			if v, ok := recoveryMap(s.final["delta"])["stop_sequence"]; ok {
				delta["stop_sequence"] = v
			}
		}
		if err = s.emit("message_delta", map[string]any{"type": "message_delta", "delta": delta, "usage": s.finalUsage()}); err != nil {
			return s.fail(err)
		}
		err = s.emit("message_stop", map[string]any{"type": "message_stop"})
	default:
		err = ErrStreamRecoveryUnsupported
	}
	if err != nil {
		return s.fail(err)
	}
	s.finished = true
	s.ResponseWriter.Flush()
	if err = s.accountRetained(); err != nil {
		return s.fail(err)
	}
	return nil
}

// CompleteToolHandoff discards unexposed incomplete siblings and ends with only
// calls already committed to the client. It never generates or retries a call.
func (s *StreamRecoveryWriter) CompleteToolHandoff() error {
	if s.finished {
		return s.Err()
	}
	if !s.HasCommittedTools() || s.unsafe {
		return s.fail(ErrStreamRecoveryIncomplete)
	}
	s.attemptErr = nil
	s.pending = make(map[string]*recoveryAtomicItem)
	s.chatTools = make(map[int]*recoveryAtomicItem)
	s.pendingBytes = 0
	s.interrupted = false
	s.terminal = true
	s.frame = nil
	return s.FinishSuccess()
}
func (s *StreamRecoveryWriter) FinishFailure(code, message string) error {
	if s.nonSSE {
		s.finished = true
		return nil
	}
	if s.finished {
		return s.Err()
	}
	s.frame = nil
	s.pending = make(map[string]*recoveryAtomicItem)
	s.chatTools = make(map[int]*recoveryAtomicItem)
	s.pendingBytes = 0
	failure := map[string]any{"code": code, "message": message, "type": "upstream_error"}
	var err error
	switch s.protocol {
	case "responses":
		for _, o := range s.outputs {
			if e := s.closeResponseOutput(o, "incomplete"); e != nil {
				return s.fail(e)
			}
		}
		r := s.responseFinal("failed")
		r["error"] = failure
		err = s.emit("response.failed", map[string]any{"type": "response.failed", "response": r})
	case "chat":
		err = s.emit("", map[string]any{"error": failure})
	case "messages":
		if err = s.closeMessageText(); err == nil {
			err = s.emit("error", map[string]any{"type": "error", "error": failure})
		}
	default:
		err = ErrStreamRecoveryUnsupported
	}
	if err != nil {
		return s.fail(err)
	}
	s.finished = true
	s.ResponseWriter.Flush()
	_ = s.accountRetained()
	return nil
}
