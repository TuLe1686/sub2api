package service

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestBuildStreamRecoveryRequestMigration(t *testing.T) {
	for _, body := range []string{
		`{"input":"next","previous_response_id":"resp_old"}`,
		`{"input":[{"role":"user","content":"next"}],"previous_response_id":"resp_old"}`,
		`{"previous_response_id":"resp_old"}`,
		`{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`,
		`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`,
		`{"input":[{"type":"item_reference","id":"msg_old"}]}`,
		`{"input":[{"type":"function_call_output","call_id":"orphan","output":"ok"}]}`,
		`{"input":[{"type":"function_call","call_id":"unfinished","name":"inspect","arguments":"{}"}]}`,
	} {
		for _, text := range []string{"", "半行"} {
			got, err := BuildStreamRecoveryRequest([]byte(body), "responses", text)
			if !errors.Is(err, ErrStreamRecoveryContextUnavailable) || got != nil {
				t.Fatal("unsafe account-bound history must return context sentinel, even before first text")
			}
		}
	}
	body := []byte(`{"model":"same","previous_response_id":"resp_old","conversation":"conv_old","turn_state":"old","input":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"type":"function_call","call_id":"c1","name":"inspect","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"done"},{"role":"user","content":"next"}]}`)
	got, err := BuildStreamRecoveryRequest(body, "responses", "")
	if !errors.Is(err, ErrStreamRecoveryContextUnavailable) || got != nil {
		t.Fatal("inline assistant history cannot prove account-bound context is complete")
	}
}

func TestBuildStreamRecoveryRequestProtocolShapes(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			path := "messages"
			if protocol == "responses" {
				path = "input"
			}
			body := []byte(`{"model":"same","system":[{"type":"text","text":"system"}],"reasoning":{"effort":"high"},"thinking":{"type":"enabled","budget_tokens":1024},"tools":[{"name":"inspect"}],"max_tokens":200,"` + path + `":[{"role":"user","content":[{"type":"text","text":"question"},{"type":"image","source":{"type":"base64","data":"AAA"}}]}]}`)
			got, err := BuildStreamRecoveryRequest(body, protocol, "中文\n未完  ")
			if err != nil {
				t.Fatal(err)
			}
			textPath := path + ".1.content"
			if protocol != "chat" {
				textPath += ".0.text"
			}
			if gjson.GetBytes(got, textPath).String() != "中文\n未完  " || gjson.GetBytes(got, path+".#").Int() != 3 {
				t.Fatal("lost exact public text or independent instruction")
			}
			if gjson.GetBytes(got, path+".0").Raw != gjson.GetBytes(body, path+".0").Raw {
				t.Fatal("image/history prefix changed")
			}
			for _, key := range []string{"system", "model", "reasoning", "thinking", "tools", "max_tokens"} {
				if gjson.GetBytes(got, key).Raw != gjson.GetBytes(body, key).Raw {
					t.Fatalf("changed %s", key)
				}
			}
			zero, err := BuildStreamRecoveryRequest(body, protocol, "")
			if err != nil || !bytes.Equal(zero, body) {
				t.Fatal("before-first-text request must be byte-identical when portable")
			}
		})
	}
}

func TestBuildStreamRecoveryRequestInvalidAndLimit(t *testing.T) {
	for _, body := range [][]byte{nil, []byte(`{`), []byte(`null`), []byte(`[]`), []byte(`{"input":12}`)} {
		got, err := BuildStreamRecoveryRequest(body, "responses", "text")
		if err == nil || got != nil {
			t.Fatal("invalid input accepted")
		}
	}
	if _, err := BuildStreamRecoveryRequest([]byte(`{"input":"hi"}`), "other", "text"); err == nil {
		t.Fatal("unknown protocol accepted")
	}
	if _, err := BuildStreamRecoveryRequest([]byte(`{"input":"hi"}`), "responses", strings.Repeat("中", (8<<20)/3+1)); err == nil {
		t.Fatal("byte limit must count UTF-8 bytes")
	}
}

func TestBuildStreamRecoveryRequestResponsesString(t *testing.T) {
	original := []byte(`{"model":"gpt-5.6-sol","instructions":"keep system","input":"write code","reasoning": {"effort":"high"},"tools":[{"type":"function","name":"inspect"}],"max_output_tokens":123,"stream":true,"metadata":{"keep":true}}`)
	before := bytes.Clone(original)
	text := "第一行\n未完成的半行  "
	got, err := BuildStreamRecoveryRequest(original, "responses", text)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, before) {
		t.Fatal("mutated original request")
	}
	for _, path := range []string{"model", "instructions", "reasoning", "tools", "max_output_tokens", "stream", "metadata"} {
		if gjson.GetBytes(got, path).Raw != gjson.GetBytes(original, path).Raw {
			t.Fatalf("changed original field %s", path)
		}
	}
	if gjson.GetBytes(got, "input.#").Int() != 3 || gjson.GetBytes(got, "input.0.content").String() != "write code" || gjson.GetBytes(got, "input.1.role").String() != "assistant" || gjson.GetBytes(got, "input.1.content.0.text").String() != text || gjson.GetBytes(got, "input.2.role").String() != "user" {
		t.Fatal("expected original user, exact emitted assistant text, independent continuation instruction")
	}
	instruction := gjson.GetBytes(got, "input.2.content.0.text").String()
	if !bytes.Contains([]byte(instruction), []byte("最后一行")) || !bytes.Contains([]byte(instruction), []byte("原样")) || !bytes.Contains([]byte(instruction), []byte("不要重新回答")) {
		t.Fatal("continuation instruction must specify exact last-line replay without restarting")
	}
}
