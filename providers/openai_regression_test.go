package providers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func regressionRequest() AnthropicRequest {
	return AnthropicRequest{Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}}}
}

func TestOpenAIProvider_OpenRouterReasoningAndToolStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"or-1","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"First"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"or-1","choices":[{"index":0,"delta":{"reasoning_content":" second"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"or-1","choices":[{"index":0,"delta":{"content":"Answer"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"or-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"or-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var out bytes.Buffer
	_, _, err := NewOpenAIProvider(srv.URL).Stream(context.Background(), regressionRequest(), "openrouter/free", "key", &out)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"thinking_delta", "First", "second", "text_delta", "Answer", "tool_use", "call_1", "partial_json", "stop_reason", "event: message_stop"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q:\n%s", want, got)
		}
	}
}

func TestOpenAIProvider_ResponseHeaderTimeoutIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var out bytes.Buffer
	_, _, err := NewOpenAIProvider(srv.URL, 15*time.Millisecond).Stream(context.Background(), regressionRequest(), "model", "key", &out)
	if !errors.Is(err, ErrAttemptTimeout) {
		t.Fatalf("expected retryable ErrAttemptTimeout, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("provider header timeout must not masquerade as caller deadline: %v", err)
	}
}

func TestOpenAIProvider_StreamMayOutliveHeaderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(60 * time.Millisecond)
		fmt.Fprint(w, `data: {"id":"slow","choices":[{"index":0,"delta":{"content":"still streaming"},"finish_reason":"stop"}]}`+"\n\n"+"data: [DONE]\n\n")
	}))
	defer srv.Close()

	var out bytes.Buffer
	_, _, err := NewOpenAIProvider(srv.URL, 15*time.Millisecond).Stream(context.Background(), regressionRequest(), "model", "key", &out)
	if err != nil {
		t.Fatalf("stream that started before the header timeout failed: %v", err)
	}
	if !strings.Contains(out.String(), "still streaming") {
		t.Fatalf("missing delayed stream content: %s", out.String())
	}
}

func TestOpenAIProvider_CallerDeadlineRemainsDistinct(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	_, _, err := NewOpenAIProvider(srv.URL, time.Second).Stream(ctx, regressionRequest(), "model", "key", &out)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected caller deadline, got %v", err)
	}
	if errors.Is(err, ErrAttemptTimeout) {
		t.Fatalf("caller deadline must not be retried as an attempt timeout: %v", err)
	}
}

func TestOpenAIProvider_ConnectionFailureIsDistinct(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	var out bytes.Buffer
	_, _, err = NewOpenAIProvider("http://"+address).Stream(context.Background(), regressionRequest(), "model", "key", &out)
	if !errors.Is(err, ErrConnection) {
		t.Fatalf("expected ErrConnection, got %v", err)
	}
	if errors.Is(err, ErrAttemptTimeout) || errors.Is(err, ErrUpstream) || errors.Is(err, ErrRateLimit) {
		t.Fatalf("connection failure must remain distinct: %v", err)
	}
}

func TestOpenAIProvider_GoneModelIsFallbackEligible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `{"detail":"model retired"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	_, _, err := NewOpenAIProvider(srv.URL).Stream(context.Background(), regressionRequest(), "retired-model", "key", &out)
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("expected ErrModelUnavailable, got %v", err)
	}
	if errors.Is(err, ErrUpstream) || errors.Is(err, ErrAttemptTimeout) || errors.Is(err, ErrConnection) {
		t.Fatalf("retired model error must remain distinct: %v", err)
	}
}

func TestOpenAIProvider_KeyRotationTransportUsesHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("HTTP version = %s, want HTTP/2", r.Proto)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"h2","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n"+"data: [DONE]\n\n")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL)
	p.client = &http.Client{Transport: &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, // httptest certificate
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: time.Second,
	}}
	var out bytes.Buffer
	_, _, err := p.StreamWithResponseHeaderTimeout(context.Background(), regressionRequest(), "model", "key", &out, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok") {
		t.Fatalf("missing streamed response: %s", out.String())
	}
}

func TestConvertToOpenAI_RealisticMultipleTools(t *testing.T) {
	req := regressionRequest()
	req.System = json.RawMessage(`[{"type":"text","text":"You are a coding assistant."}]`)
	req.Tools = json.RawMessage(`[
		{"name":"read_file","description":"Read a file from the workspace.","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
		{"name":"search","description":"Search source files.","input_schema":{"type":"object","properties":{"query":{"type":"string"},"glob":{"type":"string"}},"required":["query"]}},
		{"name":"run_command","description":"Run a command.","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}
	]`)
	req.ToolChoice = json.RawMessage(`{"type":"auto"}`)

	converted := convertToOpenAI(req, "openrouter/free")
	if len(converted.Messages) != 2 || converted.Messages[0].Role != "system" {
		t.Fatalf("system/user messages not preserved: %#v", converted.Messages)
	}
	var tools []struct {
		Function struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(converted.Tools, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 || tools[0].Function.Name != "read_file" || tools[2].Function.Name != "run_command" {
		t.Fatalf("tools were not preserved: %s", converted.Tools)
	}
	if string(converted.ToolChoice) != `"auto"` {
		t.Fatalf("tool_choice = %s, want auto", converted.ToolChoice)
	}
}
