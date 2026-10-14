package service

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func recoveryTestWriter(protocol string) (*StreamRecoveryWriter, *httptest.ResponseRecorder) {
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	return NewStreamRecoveryWriter(c.Writer, protocol), r
}
func recoveryFeed(t *testing.T, w *StreamRecoveryWriter, event, data string) {
	t.Helper()
	frame := "data: " + data + "\n\n"
	if event != "" {
		frame = "event: " + event + "\n" + frame
	}
	if _, err := w.WriteString(frame); err != nil {
		t.Fatal(err)
	}
}
func recoveryFrames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
				var obj map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj); err != nil {
					t.Fatal(err)
				}
				out = append(out, obj)
			}
		}
	}
	return out
}
func TestStreamRecoverySingleLineAnchorAndThinkingResumeOrder(t *testing.T) {
	t.Run("single_line_anchor", func(t *testing.T) {
		w, _ := recoveryTestWriter("chat")
		recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{"content":"Half sentence"}}]}`)
		w.BeginAttempt()
		recoveryFeed(t, w, "", `{"id":"d","choices":[{"index":0,"delta":{"content":"Half sentence continues."}}]}`)
		recoveryFeed(t, w, "", `{"id":"d","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		if err := w.FinishSuccess(); err != nil {
			t.Fatal(err)
		}
		if w.EmittedText() != "Half sentence continues." {
			t.Fatal(w.EmittedText())
		}
	})
	t.Run("thinking_after_partial_text", func(t *testing.T) {
		w, r := recoveryTestWriter("messages")
		recoveryFeed(t, w, "message_start", `{"type":"message_start","message":{"id":"m","content":[]}}`)
		recoveryFeed(t, w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		recoveryFeed(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"one\ntwo"}}`)
		w.BeginAttempt()
		recoveryFeed(t, w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		recoveryFeed(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"resume thought"}}`)
		recoveryFeed(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`)
		recoveryFeed(t, w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		recoveryFeed(t, w, "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
		recoveryFeed(t, w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"two\nthree"}}`)
		recoveryFeed(t, w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
		recoveryFeed(t, w, "message_stop", `{"type":"message_stop"}`)
		if err := w.FinishSuccess(); err != nil {
			t.Fatal(err)
		}
		last := -1
		for _, evt := range recoveryFrames(t, r.Body.String()) {
			if evt["type"] == "content_block_start" {
				idx := recoveryInt(evt["index"])
				if idx <= last {
					t.Fatal("block indices out of order")
				}
				last = idx
			}
			if evt["type"] == "content_block_delta" && recoveryInt(evt["index"]) < last {
				t.Fatal("resumed delta was sent into an older block")
			}
		}
		if w.EmittedText() != "one\ntwo\nthree" {
			t.Fatal(w.EmittedText())
		}
	})
}

func TestStreamRecoveryMultipleOutput(t *testing.T) {
	w, r := recoveryTestWriter("responses")
	recoveryFeed(t, w, "response.created", `{"type":"response.created","response":{"id":"multi","object":"response","status":"in_progress","output":[]}}`)
	for i, text := range []string{"first", "第二"} {
		id := fmt.Sprintf("message-%d", i)
		recoveryFeed(t, w, "response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":{"id":%q,"type":"message","role":"assistant","content":[]}}`, i, id))
		for j, value := range []string{text, "!"} {
			recoveryFeed(t, w, "response.content_part.added", fmt.Sprintf(`{"type":"response.content_part.added","item_id":%q,"output_index":%d,"content_index":%d,"part":{"type":"output_text","text":"","annotations":[]}}`, id, i, j))
			recoveryFeed(t, w, "response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","item_id":%q,"output_index":%d,"content_index":%d,"delta":%q}`, id, i, j, value))
		}
	}
	recoveryFeed(t, w, "response.output_text.annotation.added", `{"type":"response.output_text.annotation.added","item_id":"message-1","output_index":1,"content_index":0,"annotation_index":0,"annotation":{"type":"url_citation","url":"https://example.test","title":"source","start_index":0,"end_index":2}}`)
	recoveryFeed(t, w, "response.completed", `{"type":"response.completed","response":{"id":"multi","status":"completed","output":[]}}`)
	if err := w.FinishSuccess(); err != nil {
		t.Fatal(err)
	}
	frames := recoveryFrames(t, r.Body.String())
	output := recoveryMap(frames[len(frames)-1]["response"])["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("lost messages: %v", output)
	}
	parts := recoveryMap(output[1])["content"].([]any)
	if len(parts) != 2 || recoveryMap(parts[0])["text"] != "第二" || len(recoveryMap(parts[0])["annotations"].([]any)) != 1 {
		t.Fatalf("lost parts/annotations: %v", parts)
	}
}

func TestStreamRecoveryMultipleMessageBlocks(t *testing.T) {
	w, r := recoveryTestWriter("messages")
	recoveryFeed(t, w, "message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[]}}`)
	for i, text := range []string{"one", "two"} {
		recoveryFeed(t, w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, i))
		recoveryFeed(t, w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, i, text))
		recoveryFeed(t, w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
	}
	recoveryFeed(t, w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
	recoveryFeed(t, w, "message_stop", `{"type":"message_stop"}`)
	if err := w.FinishSuccess(); err != nil {
		t.Fatal(err)
	}
	if w.EmittedText() != "onetwo" || strings.Count(r.Body.String(), `"type":"content_block_start"`) != 2 || strings.Count(r.Body.String(), `"type":"content_block_stop"`) != 2 {
		t.Fatal(r.Body.String())
	}
}

func TestStreamRecoveryNativeExtensions(t *testing.T) {
	t.Run("custom_namespace_parallel_handoff", func(t *testing.T) {
		w, r := recoveryTestWriter("responses")
		recoveryFeed(t, w, "response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"custom","type":"custom_tool_call","call_id":"cc","name":"exec","namespace":"terminal","input":""}}`)
		recoveryFeed(t, w, "response.custom_tool_call_input.delta", `{"type":"response.custom_tool_call_input.delta","item_id":"custom","delta":"printf hello"}`)
		if r.Body.Len() != 0 {
			t.Fatal("custom input leaked before completion")
		}
		recoveryFeed(t, w, "response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"custom","type":"custom_tool_call","call_id":"cc","name":"exec","namespace":"terminal","input":"printf hello","status":"completed"}}`)
		recoveryFeed(t, w, "response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"id":"half","type":"function_call","call_id":"hc","name":"lookup","namespace":"functions","arguments":""}}`)
		recoveryFeed(t, w, "response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"half","delta":"{\"x\":"}`)
		recoveryFeed(t, w, "response.completed", `{"type":"response.completed","response":{"id":"r","output":[]}}`)
		if err := w.CompleteToolHandoff(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(r.Body.String(), "half") || !strings.Contains(r.Body.String(), `"namespace":"terminal"`) || strings.Count(r.Body.String(), `"type":"custom_tool_call"`) != 3 {
			t.Fatal(r.Body.String())
		}
	})
	t.Run("chat_reasoning_parallel_args", func(t *testing.T) {
		w, r := recoveryTestWriter("chat")
		recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"private","content":"answer","tool_calls":[{"index":3,"id":"t3","type":"function","function":{"name":"f","namespace":"n","arguments":"{\"x\":"}},{"index":7,"id":"t7","type":"function","function":{"name":"g","arguments":"{"}}]}}]}`)
		recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":3,"function":{"arguments":"1}"}}]}}]}`)
		recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		if err := w.CompleteToolHandoff(); err != nil {
			t.Fatal(err)
		}
		if w.EmittedText() != "answer" || strings.Contains(r.Body.String(), `"id":"t7"`) || !strings.Contains(r.Body.String(), `"id":"t3"`) {
			t.Fatal(r.Body.String())
		}
		if w.History()[0]["reasoning_content"] != "private" || w.UnsafeReplay() {
			t.Fatal("reasoning channel lost/blanket replay block")
		}
	})
	t.Run("responses_reasoning_resume", func(t *testing.T) {
		w, r := recoveryTestWriter("responses")
		recoveryFeed(t, w, "response.created", `{"type":"response.created","response":{"id":"old","output":[]}}`)
		recoveryFeed(t, w, "response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"reason","type":"reasoning","summary":[],"encrypted_content":"opaque"}}`)
		recoveryFeed(t, w, "response.reasoning_summary_part.added", `{"type":"response.reasoning_summary_part.added","item_id":"reason","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
		recoveryFeed(t, w, "response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"reason","output_index":0,"summary_index":0,"delta":"thought"}`)
		w.BeginAttempt()
		if w.Err() != nil {
			t.Fatal(w.Err())
		}
		recoveryFeed(t, w, "response.created", `{"type":"response.created","response":{"id":"new","output":[]}}`)
		recoveryFeed(t, w, "response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"reason","type":"reasoning","summary":[]}}`)
		recoveryFeed(t, w, "response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"reason","type":"reasoning","summary":[{"type":"summary_text","text":"new thought"}],"content":[{"type":"reasoning_text","text":"raw"}],"encrypted_content":"newopaque","status":"completed"}}`)
		recoveryFeed(t, w, "response.completed", `{"type":"response.completed","response":{"id":"new","output":[]}}`)
		if err := w.FinishSuccess(); err != nil {
			t.Fatal(err)
		}
		out := w.OutputSnapshot()
		if len(out) != 2 || out[0]["encrypted_content"] != "opaque" || out[1]["encrypted_content"] != "newopaque" || out[0]["id"] == out[1]["id"] || w.EmittedText() != "" || w.UnsafeReplay() {
			t.Fatalf("reasoning fidelity: %v", out)
		}
		if w.LogicalResponseID() != "old" || w.UpstreamResponseID() != "new" || strings.Count(r.Body.String(), `"type":"response.created"`) != 1 {
			t.Fatal("identity/lifecycle")
		}
	})
	t.Run("signed_thinking_then_resume", func(t *testing.T) {
		w, r := recoveryTestWriter("messages")
		recoveryFeed(t, w, "message_start", `{"type":"message_start","message":{"id":"m","content":[]}}`)
		recoveryFeed(t, w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		recoveryFeed(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thought"}}`)
		if strings.Contains(r.Body.String(), "thought") {
			t.Fatal("unsigned reasoning leaked")
		}
		recoveryFeed(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`)
		recoveryFeed(t, w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		w.BeginAttempt()
		if w.Err() != nil {
			t.Fatal(w.Err())
		}
		recoveryFeed(t, w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		recoveryFeed(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"discard"}}`)
		recoveryFeed(t, w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		recoveryFeed(t, w, "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
		recoveryFeed(t, w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`)
		recoveryFeed(t, w, "message_stop", `{"type":"message_stop"}`)
		if err := w.FinishSuccess(); err != nil {
			t.Fatal(err)
		}
		if w.EmittedText() != "answer" || strings.Contains(r.Body.String(), "discard") || w.OutputSnapshot()[0]["signature"] != "sig" {
			t.Fatal("signature or channels")
		}
	})
}

func TestStreamRecoveryExactSeam(t *testing.T) {
	for _, tc := range []struct{ name, old, next, want string }{
		{"cjk_half_line", "第一行\n第二", "第二行\n第三行", "第一行\n第二行\n第三行"},
		{"last_line", "x\nabc", "abcde", "x\nabcde"},
		{"direct_same_first", "Hello\nworld", "Hope", "Hello\nworldHope"},
		{"short_coincidence", "x\nabc", "cde", "x\nabccde"},
		{"newline_anchor", "x\nabc\n", "abc\ndef", "x\nabc\ndef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := recoveryTestWriter("chat")
			recoveryFeed(t, w, "", fmt.Sprintf(`{"id":"c","choices":[{"index":0,"delta":{"content":%q}}]}`, tc.old))
			w.BeginAttempt()
			for _, r := range tc.next {
				recoveryFeed(t, w, "", fmt.Sprintf(`{"id":"d","choices":[{"index":0,"delta":{"content":%q}}]}`, string(r)))
			}
			recoveryFeed(t, w, "", `{"id":"d","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
			if err := w.FinishSuccess(); err != nil {
				t.Fatal(err)
			}
			if w.EmittedText() != tc.want {
				t.Fatalf("%q want %q", w.EmittedText(), tc.want)
			}
		})
	}
	w, _ := recoveryTestWriter("chat")
	recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{"content":"opening\ntail"}}]}`)
	w.BeginAttempt()
	if _, err := w.WriteString("data: {\"id\":\"d\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"opening\\nrestart\"}}]}\n\n"); err == nil {
		t.Fatal("restart not rejected")
	}
	w.BeginAttempt()
	recoveryFeed(t, w, "", `{"id":"e","choices":[{"index":0,"delta":{"content":"tail plus"}}]}`)
	recoveryFeed(t, w, "", `{"id":"e","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if err := w.FinishSuccess(); err != nil {
		t.Fatal("replay poisoned next attempt", err)
	}
}

func TestStreamRecoveryDonePreservesAllMetadata(t *testing.T) {
	w, _ := recoveryTestWriter("responses")
	recoveryFeed(t, w, "response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"reason","type":"reasoning","summary":[]}}`)
	recoveryFeed(t, w, "response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"reason","output_index":0,"summary_index":0,"delta":"summary"}`)
	recoveryFeed(t, w, "response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"reason","type":"reasoning","summary":[{"type":"summary_text","text":"summary"}],"content":[{"type":"reasoning_text","text":"raw"}],"encrypted_content":"cipher"}}`)
	recoveryFeed(t, w, "response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"id":"m","type":"message","role":"assistant","content":[]}}`)
	recoveryFeed(t, w, "response.content_part.added", `{"type":"response.content_part.added","output_index":1,"item_id":"m","content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`)
	recoveryFeed(t, w, "response.output_text.delta", `{"type":"response.output_text.delta","output_index":1,"item_id":"m","content_index":0,"delta":"text"}`)
	recoveryFeed(t, w, "response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":{"id":"m","type":"message","content":[{"type":"output_text","text":"text","annotations":[{"type":"url_citation","url":"https://example.test","title":"ref","start_index":0,"end_index":4}]}]}}`)
	out := w.OutputSnapshot()
	if _, ok := out[0]["content"]; !ok {
		t.Fatal("reasoning content from authentic done lost")
	}
	parts := out[1]["content"].([]any)
	if len(recoveryMap(parts[0])["annotations"].([]any)) != 1 {
		t.Fatal("done annotations lost")
	}
}

func TestStreamRecoveryProgressAndBudget(t *testing.T) {
	w, _ := recoveryTestWriter("chat")
	var charge int64
	w.SetRetainedBytesHook(func(n int64) error { charge += n; return nil })
	recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
	if w.ProgressVersion() != 0 {
		t.Fatal("metadata progressed")
	}
	recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{"content":"x\nanchor"}}]}`)
	version := w.ProgressVersion()
	w.BeginAttempt()
	recoveryFeed(t, w, "", `{"id":"d","choices":[{"index":0,"delta":{"content":"anc"}}]}`)
	if w.ProgressVersion() != version {
		t.Fatal("overlap progressed")
	}
	if _, err := w.WriteString(": heartbeat\n\n"); err != nil {
		t.Fatal(err)
	}
	if w.ProgressVersion() != version {
		t.Fatal("heartbeat progressed")
	}
	recoveryFeed(t, w, "", `{"id":"d","choices":[{"index":0,"delta":{"content":"hor!"}}]}`)
	if w.ProgressVersion() != version+1 || charge != w.RetainedBytes() || charge == 0 {
		t.Fatal("progress/budget accounting")
	}
}

func TestStreamRecoveryBoundaries(t *testing.T) {
	t.Run("reject_non_sse", func(t *testing.T) {
		w, r := recoveryTestWriter("chat")
		if _, err := w.WriteString(`{"error":{"message":"failed"}}`); err == nil || r.Body.Len() != 0 {
			t.Fatal("non SSE error must not leak")
		}
	})
	t.Run("require_terminal", func(t *testing.T) {
		w, _ := recoveryTestWriter("chat")
		recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`)
		if w.FinishSuccess() == nil {
			t.Fatal("EOF is not success")
		}
	})
	t.Run("chat_usage_after_finish", func(t *testing.T) {
		w, r := recoveryTestWriter("chat")
		recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`)
		recoveryFeed(t, w, "", `{"id":"c","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		recoveryFeed(t, w, "", `[DONE]`)
		if err := w.FinishSuccess(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.Body.String(), `"finish_reason":"length"`) {
			t.Fatal("stop reason lost")
		}
	})
	t.Run("usage_failed_response", func(t *testing.T) {
		w, _ := recoveryTestWriter("responses")
		recoveryFeed(t, w, "response.failed", `{"type":"response.failed","response":{"usage":{"input_tokens":7,"output_tokens":2},"error":{"code":"lost","message":"lost"}}}`)
		if recoveryInt(w.AttemptUsage()["input_tokens"]) != 7 {
			t.Fatal("usage lost")
		}
		w.BeginAttempt()
		if len(w.AttemptUsage()) != 0 {
			t.Fatal("attempt usage not reset")
		}
	})
	t.Run("messages_usage", func(t *testing.T) {
		w, r := recoveryTestWriter("messages")
		recoveryFeed(t, w, "message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":7,"output_tokens":0,"cache_read_input_tokens":4}}}`)
		recoveryFeed(t, w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`)
		if recoveryInt(w.AttemptUsage()["output_tokens"]) != 3 || recoveryInt(w.AttemptUsage()["input_tokens"]) != 7 {
			t.Fatal("usage not merged")
		}
		w.SetUsage(map[string]any{"input_tokens": 10, "output_tokens": 6})
		recoveryFeed(t, w, "message_stop", `{"type":"message_stop"}`)
		if err := w.FinishSuccess(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.Body.String(), `"input_tokens":10`) {
			t.Fatal("cumulative usage lost")
		}
	})
	t.Run("restart_not_appended", func(t *testing.T) {
		w, r := recoveryTestWriter("chat")
		recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{"content":"Hello\nworld"}}]}`)
		w.BeginAttempt()
		_, err := w.WriteString("data: {\"id\":\"d\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\\nworld again\"}}]}\n\n")
		if err == nil || strings.Contains(r.Body.String(), "again") {
			t.Fatal("restart appended")
		}
	})
	t.Run("bounded", func(t *testing.T) {
		w, _ := recoveryTestWriter("chat")
		if _, err := w.Write([]byte(strings.Repeat("x", StreamRecoveryBufferLimit+1))); err == nil {
			t.Fatal("unbounded buffer")
		}
	})
	t.Run("hosted_replay_blocked", func(t *testing.T) {
		w, r := recoveryTestWriter("responses")
		recoveryFeed(t, w, "response.output_item.added", `{"type":"response.output_item.added","item":{"id":"web","type":"web_search_call"}}`)
		if w.UnsafeReplay() || !w.HasReadOnlyHostedTools() || !strings.Contains(r.Body.String(), "web_search_call") {
			t.Fatal("read-only output must pass normally")
		}
		recoveryFeed(t, w, "response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"id":"computer","type":"computer_call","action":{"type":"click","x":1,"y":2}}}`)
		if !w.UnsafeReplay() || !strings.Contains(r.Body.String(), "computer_call") {
			t.Fatal("observed side-effect disables recovery, not normal output")
		}
	})
}
func TestStreamRecoveryAtomicTools(t *testing.T) {
	t.Run("responses", func(t *testing.T) {
		w, r := recoveryTestWriter("responses")
		recoveryFeed(t, w, "response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc1","type":"function_call","name":"lookup","call_id":"call1","arguments":""}}`)
		recoveryFeed(t, w, "response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc1","delta":"{\"x\":"}`)
		if r.Body.Len() != 0 {
			t.Fatal("partial tool leaked")
		}
		w.BeginAttempt()
		recoveryFeed(t, w, "response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc2","type":"function_call","name":"lookup","call_id":"call2","arguments":""}}`)
		recoveryFeed(t, w, "response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc2","delta":"{\"x\":1}"}`)
		recoveryFeed(t, w, "response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"fc2","type":"function_call","name":"lookup","call_id":"call2","arguments":"{\"x\":1}"}}`)
		if !w.HasCommittedTools() || strings.Contains(r.Body.String(), "fc1") {
			t.Fatal("discard/commit failed")
		}
		recoveryFeed(t, w, "error", `{"type":"error","error":{"code":"lost"}}`)
		if err := w.CompleteToolHandoff(); err != nil {
			t.Fatal(err)
		}
		if strings.Count(r.Body.String(), `"type":"response.completed"`) != 1 {
			t.Fatal("handoff terminal missing")
		}
	})
	t.Run("messages", func(t *testing.T) {
		w, r := recoveryTestWriter("messages")
		recoveryFeed(t, w, "message_start", `{"type":"message_start","message":{"id":"m"}}`)
		recoveryFeed(t, w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"lookup","input":{}}}`)
		recoveryFeed(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`)
		if strings.Contains(r.Body.String(), "t1") {
			t.Fatal("tool leaked early")
		}
		recoveryFeed(t, w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		if !w.HasCommittedTools() {
			t.Fatal("tool not committed")
		}
		if err := w.CompleteToolHandoff(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("chat", func(t *testing.T) {
		w, r := recoveryTestWriter("chat")
		recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"lookup","arguments":"{\"x\":"}}]}}]}`)
		if r.Body.Len() != 0 {
			t.Fatal("partial tool leaked")
		}
		recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`)
		recoveryFeed(t, w, "", `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		if !w.HasCommittedTools() {
			t.Fatal("tool not committed")
		}
		if err := w.CompleteToolHandoff(); err != nil {
			t.Fatal(err)
		}
	})
}
func TestStreamRecoveryResume(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			w, r := recoveryTestWriter(protocol)
			start := func(id string) {
				switch protocol {
				case "responses":
					recoveryFeed(t, w, "response.created", fmt.Sprintf(`{"type":"response.created","sequence_number":0,"response":{"id":%q,"object":"response","created_at":1,"status":"in_progress","output":[]}}`, id))
					recoveryFeed(t, w, "response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg-a","type":"message","role":"assistant","status":"in_progress","content":[]}}`)
					recoveryFeed(t, w, "response.content_part.added", `{"type":"response.content_part.added","item_id":"msg-a","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`)
				case "chat":
					recoveryFeed(t, w, "", fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, id))
				case "messages":
					recoveryFeed(t, w, "message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":"test","content":[],"usage":{"input_tokens":0,"output_tokens":0}}}`, id))
					recoveryFeed(t, w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
				}
			}
			delta := func(text string) {
				switch protocol {
				case "responses":
					recoveryFeed(t, w, "response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","item_id":"msg-a","output_index":0,"content_index":0,"sequence_number":3,"delta":%q}`, text))
				case "chat":
					recoveryFeed(t, w, "", fmt.Sprintf(`{"id":"attempt-id","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, text))
				case "messages":
					recoveryFeed(t, w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text))
				}
			}
			start("stable-id")
			delta("Hello\nworld")
			w.Flush()
			if !strings.Contains(r.Body.String(), `Hello\nworld`) {
				t.Fatal("first attempt must stream immediately")
			}
			recoveryFeed(t, w, "error", `{"type":"error","error":{"code":"stream_interrupted","message":"lost"}}`)
			if code, _, ok := w.Interrupted(); !ok || code != "stream_interrupted" {
				t.Fatal("interruption not retained")
			}
			if strings.Contains(r.Body.String(), "stream_interrupted") {
				t.Fatal("attempt error leaked")
			}
			w.BeginAttempt()
			start("retry-id")
			delta("wor")
			delta("ld!")
			switch protocol {
			case "responses":
				recoveryFeed(t, w, "response.output_text.done", `{"type":"response.output_text.done","item_id":"msg-a","output_index":0,"content_index":0,"text":"world!"}`)
				recoveryFeed(t, w, "response.completed", `{"type":"response.completed","response":{"id":"retry-id","object":"response","created_at":2,"status":"completed","output":[]}}`)
			case "chat":
				recoveryFeed(t, w, "", `{"id":"retry-id","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
				recoveryFeed(t, w, "", `[DONE]`)
			case "messages":
				recoveryFeed(t, w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
				recoveryFeed(t, w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}`)
				recoveryFeed(t, w, "message_stop", `{"type":"message_stop"}`)
			}
			if w.EmittedText() != "Hello\nworld!" {
				t.Fatalf("duplicated text: %q", w.EmittedText())
			}
			if err := w.FinishSuccess(); err != nil {
				t.Fatal(err)
			}
			if err := w.FinishSuccess(); err != nil {
				t.Fatal(err)
			}
			body := r.Body.String()
			if strings.Contains(body, "retry-id") || strings.Contains(body, "attempt-id") {
				t.Fatal("identity changed")
			}
			frames := recoveryFrames(t, body)
			switch protocol {
			case "responses":
				n := 0
				for i, f := range frames {
					if int(f["sequence_number"].(float64)) != i {
						t.Fatal("nonmonotonic sequence")
					}
					if f["type"] == "response.completed" {
						n++
						response := f["response"].(map[string]any)
						output := response["output"].([]any)
						if response["id"] != "stable-id" || len(output) != 1 {
							t.Fatal("invalid completed response")
						}
						text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
						if text != "Hello\nworld!" {
							t.Fatalf("done mismatch: %v", text)
						}
					}
				}
				if n != 1 || strings.Count(body, `"type":"response.created"`) != 1 {
					t.Fatal("duplicate lifecycle")
				}
			case "chat":
				if strings.Count(body, "data: [DONE]") != 1 || strings.Count(body, `"finish_reason":"stop"`) != 1 {
					t.Fatal("duplicate chat terminal")
				}
			case "messages":
				if strings.Count(body, `"type":"message_start"`) != 1 || strings.Count(body, `"type":"message_stop"`) != 1 || strings.Count(body, `"type":"content_block_start"`) != 1 || strings.Count(body, `"type":"content_block_stop"`) != 1 {
					t.Fatal("invalid message/block lifecycle")
				}
			}
		})
	}
}
