package service

import (
	"encoding/json"
	"errors"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const streamRecoveryContinuationInstruction = "继续上一条 assistant 已公开的正文。先原样重写其最后一行（包括未完成的半行、空格和标点），紧接着从断点续写；不要重新回答，不要重复之前的段落，不要添加续写说明，不要重新执行历史中已经完成的工具调用。"

// ErrStreamRecoveryContextUnavailable identifies history that cannot safely move accounts.
var ErrStreamRecoveryContextUnavailable = errors.New("stream recovery context unavailable")

// BuildStreamRecoveryRequest retains the original request and appends only public text.
// With no emitted text it returns the original bytes, subject to migration validation.
func BuildStreamRecoveryRequest(original []byte, protocol string, emittedText string) ([]byte, error) {
	if protocol != "responses" && protocol != "chat" && protocol != "messages" {
		return nil, errors.New("unsupported stream recovery protocol")
	}
	if !gjson.ValidBytes(original) {
		return nil, errors.New("invalid stream recovery request JSON")
	}
	root := gjson.ParseBytes(original)
	if !root.IsObject() {
		return nil, errors.New("stream recovery request must be an object")
	}
	if len(emittedText) > (8<<20)-len(streamRecoveryContinuationInstruction) {
		return nil, errors.New("stream recovery added text exceeds 8 MiB")
	}
	path := "messages"
	if protocol == "responses" {
		path = "input"
	}
	history := root.Get(path)
	if err := streamRecoveryValidateHistory(root, history); err != nil {
		return nil, err
	}
	if !history.IsArray() && !(protocol == "responses" && history.Type == gjson.String) {
		return nil, errors.New("invalid stream recovery history shape")
	}
	payload := original
	for _, state := range []string{"previous_response_id", "conversation", "conversation_id", "turn_state", "x-codex-turn-state"} {
		if root.Get(state).Exists() {
			var err error
			payload, err = sjson.DeleteBytes(payload, state)
			if err != nil {
				return nil, errors.New("cannot clear stream recovery account state")
			}
		}
	}
	if emittedText == "" {
		return payload, nil
	}
	assistant := map[string]any{"role": "assistant", "content": emittedText}
	instruction := map[string]any{"role": "user", "content": streamRecoveryContinuationInstruction}
	if protocol != "chat" {
		textType := "text"
		if protocol == "responses" {
			assistant["type"] = "message"
			instruction["type"] = "message"
			textType = "output_text"
		}
		assistant["content"] = []any{map[string]any{"type": textType, "text": emittedText}}
		if protocol == "responses" {
			textType = "input_text"
		}
		instruction["content"] = []any{map[string]any{"type": textType, "text": streamRecoveryContinuationInstruction}}
	}
	assistantRaw, _ := json.Marshal(assistant)
	instructionRaw, _ := json.Marshal(instruction)
	var prefix []byte
	if history.Type == gjson.String {
		userRaw, _ := json.Marshal(map[string]any{"role": "user", "content": history.Str})
		prefix = append([]byte{'['}, userRaw...)
	} else {
		prefix = []byte(history.Raw[:len(history.Raw)-1])
	}
	if history.Type == gjson.String || len(history.Array()) != 0 {
		prefix = append(prefix, ',')
	}
	prefix = append(prefix, assistantRaw...)
	prefix = append(prefix, ',')
	prefix = append(prefix, instructionRaw...)
	prefix = append(prefix, ']')
	return sjson.SetRawBytes(payload, path, prefix)
}

func streamRecoveryContextError(reason string) error {
	// Reasons are static structural labels only; never include request text or IDs.
	return errors.Join(ErrStreamRecoveryContextUnavailable, errors.New(reason))
}

func streamRecoveryValidateHistory(root, history gjson.Result) error {
	bound := false
	for _, key := range []string{"previous_response_id", "conversation", "conversation_id"} {
		value := root.Get(key)
		if value.Exists() && value.Type != gjson.Null && value.Raw != `""` {
			bound = true
		}
	}
	if bound {
		return streamRecoveryContextError("account-bound history needs verified gateway provenance")
	}
	if !history.IsArray() {
		return nil
	}
	userSeen, historicalAssistant := false, false
	calls, results := map[string]bool{}, map[string]bool{}
	var invalid error
	addCall := func(id string) {
		if id == "" {
			invalid = streamRecoveryContextError("tool call missing identifier")
		} else {
			calls[id] = true
		}
	}
	addResult := func(id string) {
		if id == "" {
			invalid = streamRecoveryContextError("tool result missing identifier")
		} else {
			results[id] = true
		}
	}
	history.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			invalid = errors.New("stream recovery history item must be an object")
			return false
		}
		kind := item.Get("type").String()
		if kind == "compaction" || kind == "item_reference" || item.Get("encrypted_content").Exists() {
			invalid = streamRecoveryContextError("opaque or referenced history cannot migrate accounts")
			return false
		}
		switch kind {
		case "function_call", "tool_call", "custom_tool_call", "local_shell_call", "tool_search_call", "mcp_tool_call":
			addCall(item.Get("call_id").String())
		case "function_call_output", "custom_tool_call_output", "tool_search_output", "mcp_tool_call_output":
			addResult(item.Get("call_id").String())
		}
		role := item.Get("role").String()
		if role == "assistant" && userSeen {
			historicalAssistant = true
		}
		if role == "user" {
			userSeen = true
		}
		if role == "tool" {
			addResult(item.Get("tool_call_id").String())
		}
		item.Get("tool_calls").ForEach(func(_, call gjson.Result) bool { addCall(call.Get("id").String()); return true })
		if item.Get("function_call").Exists() {
			invalid = streamRecoveryContextError("legacy tool context cannot prove completion")
		}
		content := item.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, block gjson.Result) bool {
				switch block.Get("type").String() {
				case "tool_use":
					addCall(block.Get("id").String())
				case "tool_result":
					addResult(block.Get("tool_use_id").String())
				case "redacted_thinking", "compaction":
					invalid = streamRecoveryContextError("opaque history block cannot migrate accounts")
				}
				if block.Get("encrypted_content").Exists() || block.Get("signature").Exists() {
					invalid = streamRecoveryContextError("signed or encrypted history cannot migrate accounts")
				}
				return invalid == nil
			})
		}
		return invalid == nil
	})
	if invalid != nil {
		return invalid
	}
	if bound && (!history.IsArray() || !historicalAssistant) {
		return streamRecoveryContextError("account-bound request lacks full inline history")
	}
	for id := range calls {
		if !results[id] {
			return streamRecoveryContextError("historical tool call has no completed result")
		}
	}
	for id := range results {
		if !calls[id] {
			return streamRecoveryContextError("historical tool result has no inline call")
		}
	}
	return nil
}
