// Standalone standard-library upstream; never calls a real service.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type M = map[string]any
type Case struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Lane string `json:"lane"`
}

var cases map[string]Case
var mu sync.Mutex
var serial int
var evidence *os.File
var turnRE = regexp.MustCompile(`SR9:[a-zA-Z0-9-]+:turn([123])`)

func frame(kind string, data any) string {
	b, _ := json.Marshal(data)
	if kind != "" {
		return "event: " + kind + "\ndata: " + string(b) + "\n\n"
	}
	return "data: " + string(b) + "\n\n"
}
func event(kind string, body M) string { body["type"] = kind; return frame(kind, body) }
func writeRecord(v any) {
	mu.Lock()
	defer mu.Unlock()
	b, _ := json.Marshal(v)
	_, _ = evidence.Write(append(b, '\n'))
	_ = evidence.Sync()
}
func usage(final bool) M {
	if final {
		return M{"input_tokens": 14, "output_tokens": 4, "total_tokens": 18, "input_tokens_details": M{"cached_tokens": 5}}
	}
	return M{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12, "input_tokens_details": M{"cached_tokens": 3}}
}
func message(id, text string) M {
	return M{"id": id, "type": "message", "role": "assistant", "status": "completed", "content": []any{M{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func responseWire(c Case, id, text string, fail bool) string {
	var w strings.Builder
	put := func(k string, b M) { w.WriteString(event(k, b)) }
	put("response.created", M{"sequence_number": 0, "response": M{"id": id, "object": "response", "created_at": 1, "model": "gpt-5.1", "status": "in_progress", "output": []any{}}})
	output := []any{}
	idx := 0
	if c.Kind == "reasoning" {
		r := M{"id": id + "-reason", "type": "reasoning", "summary": []any{}}
		put("response.output_item.added", M{"output_index": idx, "item": r})
		put("response.reasoning_summary_part.added", M{"output_index": idx, "item_id": r["id"], "summary_index": 0, "part": M{"type": "summary_text", "text": ""}})
		put("response.reasoning_summary_text.delta", M{"output_index": idx, "item_id": r["id"], "summary_index": 0, "delta": "private-thought"})
		r["summary"] = []any{M{"type": "summary_text", "text": "private-thought"}}
		r["encrypted_content"] = "synthetic-opaque-" + c.Lane
		r["status"] = "completed"
		put("response.output_item.done", M{"output_index": idx, "item": r})
		output = append(output, r)
		idx++
	}
	if c.Kind == "readonly" || strings.HasPrefix(c.Kind, "sideeffect") {
		r := M{"id": id + "-hosted", "type": "web_search_call", "status": "completed", "action": M{"type": "search", "query": "synthetic query"}}
		if strings.HasPrefix(c.Kind, "sideeffect") {
			r = M{"id": id + "-hosted", "type": "computer_call", "status": "completed", "call_id": "synthetic-computer", "action": M{"type": "click", "x": 1, "y": 2}}
		}
		put("response.output_item.added", M{"output_index": idx, "item": r})
		put("response.output_item.done", M{"output_index": idx, "item": r})
		output = append(output, r)
		idx++
	}
	if c.Kind == "tool_half" || c.Kind == "tool_handoff" || c.Kind == "custom" {
		t := M{"id": id + "-tool", "type": "function_call", "call_id": id + "-call", "name": "lookup", "namespace": "functions", "arguments": "", "status": "in_progress"}
		ev, arg, field := "response.function_call_arguments.delta", "{\"x\":1}", "arguments"
		if c.Kind == "custom" {
			t["type"] = "custom_tool_call"
			t["name"] = "exec"
			t["namespace"] = "terminal"
			delete(t, "arguments")
			t["input"] = ""
			ev = "response.custom_tool_call_input.delta"
			arg = "printf synthetic"
			field = "input"
		}
		put("response.output_item.added", M{"output_index": idx, "item": t})
		delta := arg
		if c.Kind == "tool_half" && fail {
			delta = "{\"x\":"
		}
		put(ev, M{"output_index": idx, "item_id": t["id"], "delta": delta})
		if !(c.Kind == "tool_half" && fail) {
			t[field] = arg
			t["status"] = "completed"
			put("response.output_item.done", M{"output_index": idx, "item": t})
			output = append(output, t)
		}
	} else {
		m := message(id+"-message", text)
		m["status"] = "in_progress"
		m["content"] = []any{}
		put("response.output_item.added", M{"output_index": idx, "item": m})
		put("response.content_part.added", M{"output_index": idx, "item_id": m["id"], "content_index": 0, "part": M{"type": "output_text", "text": "", "annotations": []any{}}})
		put("response.output_text.delta", M{"output_index": idx, "item_id": m["id"], "content_index": 0, "delta": text})
		if !fail {
			put("response.output_text.done", M{"output_index": idx, "item_id": m["id"], "content_index": 0, "text": text})
			put("response.content_part.done", M{"output_index": idx, "item_id": m["id"], "content_index": 0, "part": M{"type": "output_text", "text": text, "annotations": []any{}}})
			m = message(id+"-message", text)
			put("response.output_item.done", M{"output_index": idx, "item": m})
			output = append(output, m)
		}
	}
	if fail {
		put("response.in_progress", M{"response": M{"id": id, "status": "in_progress", "usage": usage(false)}})
	} else {
		put("response.completed", M{"response": M{"id": id, "object": "response", "created_at": 1, "status": "completed", "model": "gpt-5.1", "output": output, "usage": usage(true)}})
	}
	return w.String()
}

func chatWire(c Case, id, text string, fail bool) string {
	var w strings.Builder
	put := func(d M, finish any) {
		w.WriteString(frame("", M{"id": id, "object": "chat.completion.chunk", "created": 1, "model": "gpt-5.1", "choices": []any{M{"index": 0, "delta": d, "finish_reason": finish}}}))
	}
	put(M{"role": "assistant"}, nil)
	if c.Kind == "parallel" {
		put(M{"reasoning_content": "private-thought", "tool_calls": []any{M{"index": 0, "id": "parallel-0", "type": "function", "function": M{"name": "lookup", "namespace": "functions", "arguments": "{\"x\":"}}, M{"index": 1, "id": "parallel-1", "type": "function", "function": M{"name": "lookup", "namespace": "functions", "arguments": "{"}}}}, nil)
		put(M{"tool_calls": []any{M{"index": 0, "function": M{"arguments": "1}"}}, M{"index": 1, "function": M{"arguments": "\"x\":2}"}}}}, nil)
		put(M{}, "tool_calls")
	} else {
		put(M{"content": text}, nil)
		if !fail {
			put(M{}, "stop")
		}
	}
	u := usage(!fail)
	w.WriteString(frame("", M{"id": id, "object": "chat.completion.chunk", "model": "gpt-5.1", "choices": []any{}, "usage": M{"prompt_tokens": u["input_tokens"], "completion_tokens": u["output_tokens"], "total_tokens": u["total_tokens"], "prompt_tokens_details": u["input_tokens_details"]}}))
	if !fail {
		w.WriteString("data: [DONE]\n\n")
	}
	return w.String()
}

func handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.Header.Get("X-Api-Key")
	}
	c, ok := cases[token]
	if !ok {
		w.WriteHeader(401)
		return
	}
	if r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-5.1","object":"model"}]}`)
		return
	}
	if r.URL.Path != "/v1/responses" && r.URL.Path != "/v1/chat/completions" {
		w.WriteHeader(404)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		w.WriteHeader(400)
		return
	}
	var body M
	if json.Unmarshal(raw, &body) != nil {
		w.WriteHeader(400)
		return
	}
	// Ignore background protocol probes: all fixture prompts carry this marker.
	if !strings.Contains(string(raw), "SR9:") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"code":"synthetic_probe_only","message":"fixture marker required"}}`)
		return
	}
	turn := 1
	for _, match := range turnRE.FindAllStringSubmatch(string(raw), -1) {
		if match[1] == "2" && turn < 2 {
			turn = 2
		}
		if match[1] == "3" {
			turn = 3
		}
	}
	fail := c.Lane == "a"
	switch c.Kind {
	case "tool_handoff", "sideeffect_fail":
		fail = true
	case "custom", "parallel", "readonly", "sideeffect_pass":
		fail = false
	case "alias":
		fail = (turn == 1 || turn == 3) && c.Lane == "a" || turn == 2 && c.Lane == "b"
	}
	text := "one\ntwo"
	if !fail {
		text = "two\nthree"
	}
	if c.Kind == "alias" {
		text = fmt.Sprintf("answer-%d\ntail", turn)
		if !fail {
			text = "tail\ncomplete"
		}
	}
	if c.Kind == "readonly" || c.Kind == "sideeffect_pass" {
		text = "hosted-ok"
	}
	mu.Lock()
	serial++
	id := fmt.Sprintf("resp_sr9_%d_%s", serial, c.Lane)
	mu.Unlock()
	delay := 0
	if fail && c.Kind == "longwait" {
		delay = 12
		w.Header().Set("Retry-After", "12")
	}
	writeRecord(M{"case": c.Name, "kind": c.Kind, "lane": c.Lane, "path": r.URL.Path, "turn": turn, "response_id": id, "failure": fail, "retry_after": delay, "request": body, "time": time.Now().UTC().Format(time.RFC3339Nano), "usage": usage(!fail)})
	wire := responseWire(c, id, text, fail)
	if r.URL.Path == "/v1/chat/completions" {
		wire = chatWire(c, id, text, fail)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if fail {
		w.Header().Set("Content-Length", fmt.Sprint(len(wire)+100))
	}
	w.WriteHeader(200)
	_, _ = io.WriteString(w, wire)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// A short response to a larger Content-Length causes actual io.ErrUnexpectedEOF.
}

func main() {
	var cfg struct {
		Cases map[string]Case `json:"cases"`
	}
	raw, err := os.ReadFile(os.Getenv("STUB_CONFIG"))
	if err != nil {
		panic("stub config unreadable")
	}
	if json.Unmarshal(raw, &cfg) != nil {
		panic("stub config invalid")
	}
	cases = cfg.Cases
	evidence, err = os.OpenFile(os.Getenv("STUB_EVIDENCE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		panic("evidence path unreadable")
	}
	defer evidence.Close()
	srv := &http.Server{Addr: ":9000", Handler: http.HandlerFunc(handle), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	if err = srv.ListenAndServe(); err != nil {
		panic(err)
	}
}
