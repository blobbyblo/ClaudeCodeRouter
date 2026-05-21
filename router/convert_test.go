package router

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bobbyo/ccr/providers"
)

func TestOpenAIToAnthropic_Basic(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "hello"}
		]
	}`
	req, err := OpenAIToAnthropic([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		t.Errorf("expected role=user, got %s", req.Messages[0].Role)
	}
	if req.Messages[0].Content[0].Text != "hello" {
		t.Errorf("expected text=hello, got %s", req.Messages[0].Content[0].Text)
	}
	if req.MaxTokens != 8192 {
		t.Errorf("expected default max_tokens=8192, got %d", req.MaxTokens)
	}
}

func TestOpenAIToAnthropic_SystemMessage(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "you are a helper"},
			{"role": "user", "content": "hi"}
		]
	}`
	req, err := OpenAIToAnthropic([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if providers.SystemText(req.System) != "you are a helper" {
		t.Errorf("expected system message, got %q", providers.SystemText(req.System))
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 non-system message, got %d", len(req.Messages))
	}
}

func TestAnthropicToOpenAI_Basic(t *testing.T) {
	req := providers.AnthropicRequest{
		Model: "claude-sonnet",
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hello"}}},
		},
		MaxTokens: 1024,
	}
	data, err := AnthropicToOpenAI(req, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "gpt-4o" {
		t.Errorf("expected model=gpt-4o, got %v", out["model"])
	}
	opts, ok := out["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Errorf("expected stream_options.include_usage=true")
	}
}

func TestAnthropicToOpenAI_SystemField(t *testing.T) {
	req := providers.AnthropicRequest{
		System: json.RawMessage(`"be helpful"`),
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		MaxTokens: 100,
	}
	data, err := AnthropicToOpenAI(req, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Role != "system" {
		t.Errorf("expected first message role=system, got %s", out.Messages[0].Role)
	}
	if out.Messages[0].Content != "be helpful" {
		t.Errorf("expected system content, got %s", out.Messages[0].Content)
	}
}

func TestAnthropicBodyToRequest_Defaults(t *testing.T) {
	body := `{"model":"foo","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	req, err := AnthropicBodyToRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != 8192 {
		t.Errorf("expected default max_tokens 8192, got %d", req.MaxTokens)
	}
	if !req.Stream {
		t.Error("expected stream=true")
	}
}

func TestAnthropicSSEToOpenAI(t *testing.T) {
	// Simulate Anthropic SSE output.
	src := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg1\",\"model\":\"claude\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	var buf bytes.Buffer
	if err := anthropicSSEToOpenAI([]byte(src), &buf); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if !strings.Contains(out, "data: ") {
		t.Error("expected SSE data lines in output")
	}
	if !strings.Contains(out, "[DONE]") {
		t.Error("expected [DONE] terminator in output")
	}
	if !strings.Contains(out, "Hello") {
		t.Error("expected content 'Hello' in output")
	}
}
