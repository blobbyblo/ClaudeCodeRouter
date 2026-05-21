package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// anthropicSSE returns a minimal but valid Anthropic SSE stream for testing.
// inputTok and outputTok are embedded in the message_start / message_delta events.
func anthropicSSE(inputTok, outputTok int) string {
	var sb strings.Builder

	// message_start
	fmt.Fprintf(&sb, "event: message_start\n")
	fmt.Fprintf(&sb, `data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-3-opus-20240229","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":1}}}`, inputTok)
	sb.WriteString("\n\n")

	// content_block_start
	sb.WriteString("event: content_block_start\n")
	sb.WriteString(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	sb.WriteString("\n\n")

	// ping
	sb.WriteString("event: ping\n")
	sb.WriteString(`data: {"type":"ping"}`)
	sb.WriteString("\n\n")

	// content_block_delta
	sb.WriteString("event: content_block_delta\n")
	sb.WriteString(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello, world!"}}`)
	sb.WriteString("\n\n")

	// content_block_stop
	sb.WriteString("event: content_block_stop\n")
	sb.WriteString(`data: {"type":"content_block_stop","index":0}`)
	sb.WriteString("\n\n")

	// message_delta (carries output token count)
	fmt.Fprintf(&sb, "event: message_delta\n")
	fmt.Fprintf(&sb, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":%d}}`, outputTok)
	sb.WriteString("\n\n")

	// message_stop
	sb.WriteString("event: message_stop\n")
	sb.WriteString(`data: {"type":"message_stop"}`)
	sb.WriteString("\n\n")

	return sb.String()
}

func TestAnthropicProvider_Success(t *testing.T) {
	const wantInput, wantOutput = 42, 17

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify required headers.
		if r.Header.Get("x-api-key") == "" {
			t.Error("missing x-api-key header")
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("missing anthropic-version header")
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, anthropicSSE(wantInput, wantOutput))
	}))
	defer srv.Close()

	p := NewAnthropicProvider(srv.URL)
	req := AnthropicRequest{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
		},
		MaxTokens: 100,
	}

	var buf bytes.Buffer
	gotInput, gotOutput, err := p.Stream(context.Background(), req, "claude-3-opus-20240229", "test-key", &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotInput != wantInput {
		t.Errorf("input tokens: got %d, want %d", gotInput, wantInput)
	}
	if gotOutput != wantOutput {
		t.Errorf("output tokens: got %d, want %d", gotOutput, wantOutput)
	}

	// The SSE output should contain the forwarded events.
	out := buf.String()
	if !strings.Contains(out, "event: message_start") {
		t.Error("output missing message_start event")
	}
	if !strings.Contains(out, "Hello, world!") {
		t.Error("output missing expected text delta")
	}
	if !strings.Contains(out, "event: message_stop") {
		t.Error("output missing message_stop event")
	}
}

func TestAnthropicProvider_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewAnthropicProvider(srv.URL)
	req := AnthropicRequest{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}

	var buf bytes.Buffer
	_, _, err := p.Stream(context.Background(), req, "claude-3-opus-20240229", "test-key", &buf)
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("expected ErrRateLimit, got %v", err)
	}
}

func TestAnthropicProvider_Upstream5xx(t *testing.T) {
	for _, code := range []int{500, 502, 503} {
		code := code
		t.Run(fmt.Sprintf("HTTP%d", code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			p := NewAnthropicProvider(srv.URL)
			req := AnthropicRequest{
				Messages: []Message{
					{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
				},
			}

			var buf bytes.Buffer
			_, _, err := p.Stream(context.Background(), req, "claude-3-opus-20240229", "test-key", &buf)
			if err != ErrUpstream {
				t.Fatalf("expected ErrUpstream for %d, got %v", code, err)
			}
		})
	}
}

func TestAnthropicProvider_TokenCounts(t *testing.T) {
	const wantInput, wantOutput = 123, 456

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, anthropicSSE(wantInput, wantOutput))
	}))
	defer srv.Close()

	p := NewAnthropicProvider(srv.URL)
	req := AnthropicRequest{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "count my tokens"}}},
		},
	}

	var buf bytes.Buffer
	gotInput, gotOutput, err := p.Stream(context.Background(), req, "claude-3-opus-20240229", "key", &buf)
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

func TestAnthropicProvider_ContextCancellation(t *testing.T) {
	// Server that streams slowly — we cancel before it finishes.
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not implement Flusher")
			return
		}

		// Send first event and signal the test.
		fmt.Fprint(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		flusher.Flush()
		close(started)

		// Block until the request context is done (client disconnected).
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	p := NewAnthropicProvider(srv.URL)
	req := AnthropicRequest{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}

	errCh := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		_, _, err := p.Stream(ctx, req, "claude-3-opus-20240229", "key", &buf)
		errCh <- err
	}()

	// Wait until the server has sent at least one event, then cancel.
	<-started
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected an error after cancellation, got nil")
	}
}
