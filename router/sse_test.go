package router

import (
	"encoding/json"
	"strings"
	"testing"
)

// buildSSE is a helper that assembles raw Anthropic SSE bytes from a slice of
// (eventType, dataJSON) pairs.
func buildSSE(events [][2]string) []byte {
	var sb strings.Builder
	for _, ev := range events {
		sb.WriteString("event: ")
		sb.WriteString(ev[0])
		sb.WriteString("\ndata: ")
		sb.WriteString(ev[1])
		sb.WriteString("\n\n")
	}
	return []byte(sb.String())
}

func TestSSEBytesToMessage_TextOnly(t *testing.T) {
	sse := buildSSE([][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_abc","type":"message","role":"assistant","model":"claude-3","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`},
		{"ping", `{"type":"ping"}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello, "}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world!"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`},
		{"message_stop", `{"type":"message_stop"}`},
	})

	out, err := sseBytesToMessage(sse)
	if err != nil {
		t.Fatalf("sseBytesToMessage error: %v", err)
	}

	var msg map[string]interface{}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	if msg["id"] != "msg_abc" {
		t.Errorf("id = %v, want msg_abc", msg["id"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", msg["role"])
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", msg["stop_reason"])
	}
	usage := msg["usage"].(map[string]interface{})
	if int(usage["input_tokens"].(float64)) != 10 {
		t.Errorf("input_tokens = %v, want 10", usage["input_tokens"])
	}
	if int(usage["output_tokens"].(float64)) != 5 {
		t.Errorf("output_tokens = %v, want 5", usage["output_tokens"])
	}

	content := msg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("content len = %d, want 1", len(content))
	}
	block := content[0].(map[string]interface{})
	if block["type"] != "text" {
		t.Errorf("block type = %v, want text", block["type"])
	}
	if block["text"] != "Hello, world!" {
		t.Errorf("block text = %v, want 'Hello, world!'", block["text"])
	}
}

func TestSSEBytesToMessage_ToolUse(t *testing.T) {
	sse := buildSSE([][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_tool","model":"m","usage":{"input_tokens":20,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_xyz","name":"bash","input":{}}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}`},
		{"message_stop", `{"type":"message_stop"}`},
	})

	out, err := sseBytesToMessage(sse)
	if err != nil {
		t.Fatalf("sseBytesToMessage error: %v", err)
	}

	var msg map[string]interface{}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", msg["stop_reason"])
	}

	content := msg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("content len = %d, want 1", len(content))
	}
	block := content[0].(map[string]interface{})
	if block["type"] != "tool_use" {
		t.Errorf("block type = %v, want tool_use", block["type"])
	}
	if block["id"] != "toolu_xyz" {
		t.Errorf("block id = %v, want toolu_xyz", block["id"])
	}
	if block["name"] != "bash" {
		t.Errorf("block name = %v, want bash", block["name"])
	}
	input := block["input"].(map[string]interface{})
	if input["cmd"] != "ls" {
		t.Errorf("input.cmd = %v, want ls", input["cmd"])
	}
}

func TestSSEBytesToMessage_ThinkingAndText(t *testing.T) {
	sse := buildSSE([][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_think","model":"m","usage":{"input_tokens":5,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think..."}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Answer"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":8}}`},
		{"message_stop", `{"type":"message_stop"}`},
	})

	out, err := sseBytesToMessage(sse)
	if err != nil {
		t.Fatalf("sseBytesToMessage error: %v", err)
	}

	var msg map[string]interface{}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	content := msg["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("content len = %d, want 2", len(content))
	}
	if content[0].(map[string]interface{})["type"] != "thinking" {
		t.Errorf("block[0] type = %v, want thinking", content[0].(map[string]interface{})["type"])
	}
	if content[0].(map[string]interface{})["thinking"] != "Let me think..." {
		t.Errorf("thinking content mismatch")
	}
	if content[1].(map[string]interface{})["type"] != "text" {
		t.Errorf("block[1] type = %v, want text", content[1].(map[string]interface{})["type"])
	}
	if content[1].(map[string]interface{})["text"] != "Answer" {
		t.Errorf("text content mismatch")
	}
}

func TestSSEBytesToMessage_Empty(t *testing.T) {
	out, err := sseBytesToMessage([]byte{})
	if err != nil {
		t.Fatalf("sseBytesToMessage error on empty input: %v", err)
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("empty-input output is not valid JSON: %v\n%s", err, out)
	}
	// content must be an array (not null) so the Anthropic SDK can iterate it.
	raw, _ := json.Marshal(msg["content"])
	if string(raw) != "[]" {
		t.Errorf("content = %s, want []", raw)
	}
}
