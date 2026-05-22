package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// openAISSE returns a minimal OpenAI streaming response.
// promptTok / completionTok are put in the final usage chunk.
func openAISSE(promptTok, completionTok int) string {
	var sb strings.Builder

	// First chunk: role only.
	sb.WriteString(`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`)
	sb.WriteString("\n\n")

	// Text delta chunk.
	sb.WriteString(`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello from OpenAI!"},"finish_reason":null}],"usage":null}`)
	sb.WriteString("\n\n")

	// Finish chunk.
	sb.WriteString(`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`)
	sb.WriteString("\n\n")

	// Usage chunk (stream_options: include_usage).
	fmt.Fprintf(&sb, `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		promptTok, completionTok, promptTok+completionTok)
	sb.WriteString("\n\n")

	sb.WriteString("data: [DONE]\n\n")

	return sb.String()
}

func TestOpenAIProvider_Success(t *testing.T) {
	const wantInput, wantOutput = 20, 8

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify headers.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing or malformed Authorization header: %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}

		// Verify stream_options in body.
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		so, ok := body["stream_options"].(map[string]interface{})
		if !ok {
			t.Error("missing stream_options in request body")
		} else if so["include_usage"] != true {
			t.Errorf("stream_options.include_usage not true: %v", so["include_usage"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, openAISSE(wantInput, wantOutput))
	}))
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL)
	req := AnthropicRequest{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
		},
		MaxTokens: 100,
	}

	var buf bytes.Buffer
	gotInput, gotOutput, err := p.Stream(context.Background(), req, "gpt-4o", "test-key", &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotInput != wantInput {
		t.Errorf("input tokens: got %d, want %d", gotInput, wantInput)
	}
	if gotOutput != wantOutput {
		t.Errorf("output tokens: got %d, want %d", gotOutput, wantOutput)
	}

	out := buf.String()
	// Should contain converted Anthropic events.
	if !strings.Contains(out, "event: message_start") {
		t.Error("output missing message_start event")
	}
	if !strings.Contains(out, "event: content_block_delta") {
		t.Error("output missing content_block_delta event")
	}
	if !strings.Contains(out, "Hello from OpenAI!") {
		t.Error("output missing expected text content")
	}
	if !strings.Contains(out, "event: message_delta") {
		t.Error("output missing message_delta event")
	}
	if !strings.Contains(out, "event: message_stop") {
		t.Error("output missing message_stop event")
	}
}

func TestOpenAIProvider_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL)
	req := AnthropicRequest{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}

	var buf bytes.Buffer
	_, _, err := p.Stream(context.Background(), req, "gpt-4o", "test-key", &buf)
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("expected ErrRateLimit, got %v", err)
	}
}

func TestOpenAIProvider_Upstream5xx(t *testing.T) {
	for _, code := range []int{500, 502, 503} {
		code := code
		t.Run(fmt.Sprintf("HTTP%d", code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			p := NewOpenAIProvider(srv.URL)
			req := AnthropicRequest{
				Messages: []Message{
					{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
				},
			}

			var buf bytes.Buffer
			_, _, err := p.Stream(context.Background(), req, "gpt-4o", "test-key", &buf)
			if err != ErrUpstream {
				t.Fatalf("expected ErrUpstream for %d, got %v", code, err)
			}
		})
	}
}

func TestOpenAIProvider_TokenCounts(t *testing.T) {
	const wantInput, wantOutput = 77, 33

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, openAISSE(wantInput, wantOutput))
	}))
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL)
	req := AnthropicRequest{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "count tokens"}}},
		},
	}

	var buf bytes.Buffer
	gotInput, gotOutput, err := p.Stream(context.Background(), req, "gpt-4o", "key", &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotInput != wantInput {
		t.Errorf("input tokens: got %d, want %d", gotInput, wantInput)
	}
	if gotOutput != wantOutput {
		t.Errorf("output tokens: got %d, want %d", gotOutput, wantOutput)
	}
}

func TestOpenAIProvider_SystemMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		msgs, _ := body["messages"].([]interface{})
		if len(msgs) == 0 {
			t.Fatal("no messages in request")
		}
		first, _ := msgs[0].(map[string]interface{})
		if first["role"] != "system" {
			t.Errorf("first message role = %q, want %q", first["role"], "system")
		}
		if first["content"] != "Be helpful." {
			t.Errorf("system content = %q, want %q", first["content"], "Be helpful.")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, openAISSE(5, 3))
	}))
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL)
	req := AnthropicRequest{
		System: json.RawMessage(`"Be helpful."`),
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}},
		},
	}

	var buf bytes.Buffer
	if _, _, err := p.Stream(context.Background(), req, "gpt-4o", "key", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenAIProvider_ContextExceeded(t *testing.T) {
	// NIM-style 400 body for context length exceeded.
	const contextErrBody = `{"error":{"object":"error","message":"Requested token count exceeds the model's maximum context length of 262144 tokens. You requested a total of 265409 tokens: 233409 tokens from the input messages and 32000 tokens for the completion. Please reduce the number of tokens in the input messages or the completion to fit within the limit.","type":"BadRequestError","param":null,"code":400}}`

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, contextErrBody)
	}))
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL)
	req := AnthropicRequest{
		MaxTokens: 32000,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	}

	var buf bytes.Buffer
	_, _, err := p.Stream(context.Background(), req, "kimi-k2", "key", &buf)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrContextExceeded) {
		t.Fatalf("expected ErrContextExceeded, got %v", err)
	}
	var cee *ContextExceededError
	if !errors.As(err, &cee) {
		t.Fatalf("expected *ContextExceededError, got %T", err)
	}
	if cee.ContextLimit != 262144 {
		t.Errorf("ContextLimit = %d, want 262144", cee.ContextLimit)
	}
	if cee.InputTokens != 233409 {
		t.Errorf("InputTokens = %d, want 233409", cee.InputTokens)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 upstream call (no retry), got %d", callCount)
	}
}

func TestParseContextLengthError(t *testing.T) {
	body := []byte(`{"error":{"message":"Requested token count exceeds the model's maximum context length of 262144 tokens. You requested a total of 265409 tokens: 233409 tokens from the input messages and 32000 tokens for the completion.","type":"BadRequestError"}}`)
	limit, input := parseContextLengthError(body)
	if limit != 262144 {
		t.Errorf("limit = %d, want 262144", limit)
	}
	if input != 233409 {
		t.Errorf("input = %d, want 233409", input)
	}

	// Non-matching body returns zeros.
	l2, i2 := parseContextLengthError([]byte(`{"error":{"message":"something else"}}`))
	if l2 != 0 || i2 != 0 {
		t.Errorf("non-matching body should return (0,0), got (%d,%d)", l2, i2)
	}
}

func TestOpenAIProvider_FinishReasonMapping(t *testing.T) {
	cases := []struct {
		openaiReason    string
		wantStopReason  string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"content_filter", "stop_sequence"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.openaiReason, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var sb strings.Builder
				sb.WriteString(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`)
				sb.WriteString("\n\n")
				fmt.Fprintf(&sb, `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"%s"}],"usage":null}`, tc.openaiReason)
				sb.WriteString("\n\n")
				sb.WriteString(`data: {"id":"x","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				sb.WriteString("\n\n")
				sb.WriteString("data: [DONE]\n\n")

				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, sb.String())
			}))
			defer srv.Close()

			p := NewOpenAIProvider(srv.URL)
			req := AnthropicRequest{
				Messages: []Message{
					{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
				},
			}

			var buf bytes.Buffer
			if _, _, err := p.Stream(context.Background(), req, "gpt-4o", "key", &buf); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			out := buf.String()
			if !strings.Contains(out, tc.wantStopReason) {
				t.Errorf("output does not contain stop_reason %q:\n%s", tc.wantStopReason, out)
			}
		})
	}
}

func TestOpenAIProvider_ContextCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not implement Flusher")
			return
		}

		fmt.Fprint(w, `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`)
		fmt.Fprint(w, "\n\n")
		flusher.Flush()
		close(started)

		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	p := NewOpenAIProvider(srv.URL)
	req := AnthropicRequest{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}

	errCh := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		_, _, err := p.Stream(ctx, req, "gpt-4o", "key", &buf)
		errCh <- err
	}()

	<-started
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected an error after cancellation, got nil")
	}
}

func TestSanitizeParameterSchema(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string // subset: check "type" is absent from properties
		kept  bool   // whether the schema should change at all
	}{
		{
			name:  "no conflict",
			input: `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}}}`,
			kept:  false,
		},
		{
			name:  "type property removed",
			input: `{"type":"object","properties":{"pattern":{"type":"string"},"type":{"type":"string"}},"required":["pattern","type"]}`,
			kept:  true,
		},
		{
			name:  "type property removed from required",
			input: `{"type":"object","properties":{"a":{"type":"string"},"type":{"type":"string"}},"required":["a","type"]}`,
			kept:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := sanitizeParameterSchema(json.RawMessage(tc.input))

			var schema map[string]json.RawMessage
			if err := json.Unmarshal(out, &schema); err != nil {
				t.Fatalf("output not valid JSON: %v", err)
			}

			if propsRaw, ok := schema["properties"]; ok {
				var props map[string]json.RawMessage
				if err := json.Unmarshal(propsRaw, &props); err != nil {
					t.Fatalf("properties not valid JSON: %v", err)
				}
				if _, hasType := props["type"]; hasType {
					t.Error("properties still contains 'type' key after sanitize")
				}
			}

			changed := string(out) != tc.input
			if tc.kept && !changed {
				t.Error("expected schema to be modified but it was not")
			}
			if !tc.kept && changed {
				t.Errorf("expected schema unchanged but got %s", out)
			}
		})
	}
}
