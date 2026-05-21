package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bobbyo/ccr/config"
	"github.com/bobbyo/ccr/db"
	"github.com/bobbyo/ccr/middleware"
)

// writeConfigFile writes config TOML content to a temp file and returns the path.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/config.toml"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// anthropicSSEResponse returns a minimal valid Anthropic SSE stream.
func anthropicSSEResponse() string {
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"x\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

// withClientKey injects a db.ClientKey into the request context.
func withClientKey(r *http.Request, key *db.ClientKey) *http.Request {
	ctx := context.WithValue(r.Context(), middleware.ClientKeyContextKey, key)
	return r.WithContext(ctx)
}

func TestRouter_HandleMessages_Success(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, anthropicSSEResponse())
	}))
	defer upstream.Close()

	database, _ := db.Open(":memory:")
	defer database.Close()
	_ = db.CreateKey(database, "sk-ccr-test", "test")

	cfgPath := writeConfigFile(t, fmt.Sprintf(`
[server]
client_port = 9999
admin_port  = 9998

[providers.testprov]
  base_url   = %q
  convention = "anthropic"
  api_keys   = []

[[models]]
  alias       = "test-model"
  fallback_to = ""
  providers   = [{provider = "testprov", model_id = "test-model-id"}]
`, upstream.URL))

	mgr, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	bc := middleware.NewBroadcaster()
	rt := New(mgr, database, bc)

	// Streaming request — proxy must forward SSE.
	body := `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"max_tokens":100,"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	key, _ := db.GetKey(database, "sk-ccr-test")
	req = withClientKey(req, key)

	rr := httptest.NewRecorder()
	rt.HandleMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "message_start") {
		t.Errorf("expected Anthropic SSE in body, got: %s", rr.Body.String())
	}
}

func TestRouter_HandleMessages_NonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, anthropicSSEResponse())
	}))
	defer upstream.Close()

	database, _ := db.Open(":memory:")
	defer database.Close()
	_ = db.CreateKey(database, "sk-ccr-ns", "test")

	cfgPath := writeConfigFile(t, fmt.Sprintf(`
[server]
client_port = 9888

[providers.testprov]
  base_url   = %q
  convention = "anthropic"
  api_keys   = []

[[models]]
  alias       = "test-model"
  fallback_to = ""
  providers   = [{provider = "testprov", model_id = "test-model-id"}]
`, upstream.URL))

	mgr, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	bc := middleware.NewBroadcaster()
	rt := New(mgr, database, bc)

	// stream=false (or absent) → proxy must return a plain JSON Message.
	body := `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"max_tokens":100}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	key, _ := db.GetKey(database, "sk-ccr-ns")
	req = withClientKey(req, key)

	rr := httptest.NewRecorder()
	rt.HandleMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	bodyStr := rr.Body.String()
	if strings.Contains(bodyStr, "event:") || strings.Contains(bodyStr, "data:") {
		t.Errorf("non-streaming response must not contain SSE: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"role"`) || !strings.Contains(bodyStr, `"content"`) {
		t.Errorf("expected JSON Message fields in body, got: %s", bodyStr)
	}
}

func TestRouter_HandleChatCompletions_Success(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		stop := "stop"
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":%q}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`, stop),
		}
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	database, _ := db.Open(":memory:")
	defer database.Close()
	_ = db.CreateKey(database, "sk-ccr-oaitest", "test")

	cfgPath := writeConfigFile(t, fmt.Sprintf(`
[server]
client_port = 9997

[providers.testprov]
  base_url   = %q
  convention = "openai"
  api_keys   = []

[[models]]
  alias       = "test-model"
  fallback_to = ""
  providers   = [{provider = "testprov", model_id = "test-model-id"}]
`, upstream.URL))
	mgr, _ := config.Load(cfgPath)
	bc := middleware.NewBroadcaster()
	rt := New(mgr, database, bc)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	key, _ := db.GetKey(database, "sk-ccr-oaitest")
	req = withClientKey(req, key)

	rr := httptest.NewRecorder()
	rt.HandleChatCompletions(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "[DONE]") {
		t.Errorf("expected [DONE] in OpenAI output, got: %s", rr.Body.String())
	}
}

func TestRouter_AllProvidersExhausted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	database, _ := db.Open(":memory:")
	defer database.Close()
	_ = db.CreateKey(database, "sk-ccr-exhaust", "test")

	cfgPath := writeConfigFile(t, fmt.Sprintf(`
[server]
client_port = 9996

[providers.testprov]
  base_url   = %q
  convention = "anthropic"
  api_keys   = []

[[models]]
  alias       = "test-model"
  fallback_to = ""
  providers   = [{provider = "testprov", model_id = "test-id"}]
`, upstream.URL))
	mgr, _ := config.Load(cfgPath)
	bc := middleware.NewBroadcaster()
	rt := New(mgr, database, bc)

	body := `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"max_tokens":100}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	key, _ := db.GetKey(database, "sk-ccr-exhaust")
	req = withClientKey(req, key)

	rr := httptest.NewRecorder()
	rt.HandleMessages(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rr.Code)
	}
}

func TestRouter_HandleModels(t *testing.T) {
	database, _ := db.Open(":memory:")
	defer database.Close()

	cfgPath := writeConfigFile(t, `
[server]
client_port = 9001

[providers.x]
  base_url   = "http://localhost"
  convention = "anthropic"
  api_keys   = []

[[models]]
  alias = "model-a"
  fallback_to = ""
  providers = [{provider="x", model_id="x-model"}]

[[models]]
  alias = "model-b"
  fallback_to = ""
  providers = [{provider="x", model_id="x-model"}]
`)
	mgr, _ := config.Load(cfgPath)
	bc := middleware.NewBroadcaster()
	rt := New(mgr, database, bc)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	rt.HandleModels(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "model-a") || !strings.Contains(body, "model-b") {
		t.Errorf("expected model aliases in response, got: %s", body)
	}
}

func TestRouter_FallbackToModel(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, anthropicSSEResponse())
	}))
	defer upstream.Close()

	database, _ := db.Open(":memory:")
	defer database.Close()
	_ = db.CreateKey(database, "sk-ccr-fallback", "test")

	cfgPath := writeConfigFile(t, fmt.Sprintf(`
[server]
client_port = 9005

[providers.testprov]
  base_url   = %q
  convention = "anthropic"
  api_keys   = []

[[models]]
  alias       = "primary-model"
  fallback_to = "fallback-model"
  providers   = [{provider = "testprov", model_id = "primary-id"}]

[[models]]
  alias       = "fallback-model"
  fallback_to = ""
  providers   = [{provider = "testprov", model_id = "fallback-id"}]
`, upstream.URL))
	mgr, _ := config.Load(cfgPath)
	bc := middleware.NewBroadcaster()
	rt := New(mgr, database, bc)

	body := `{"model":"primary-model","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":100}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	key, _ := db.GetKey(database, "sk-ccr-fallback")
	req = withClientKey(req, key)

	rr := httptest.NewRecorder()
	rt.HandleMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 after fallback, got %d: %s", rr.Code, rr.Body.String())
	}

	// Brief pause for async DB write.
	time.Sleep(50 * time.Millisecond)
}

// TestRouter_FallbackCount verifies that FallbackCount accumulates across key,
// provider, and model hops.  The scenario:
//   - primary-model has one provider (testprov) with two keys.
//   - Key 0 returns 429, key 1 returns 429 → 2 key fallbacks, provider exhausted
//     (1 provider fallback), model falls back to fallback-model (1 model hop).
//   - fallback-model succeeds on the first try.
//   - Expected FallbackCount = 2 (keys) + 1 (provider) + 1 (model) = 4.
func TestRouter_FallbackCount(t *testing.T) {
	// Two upstream "keys" — server always 429s so we can test both paths.
	// In practice the key values are stored in the DB; here the server ignores them.
	allFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer allFail.Close()

	succeed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, anthropicSSEResponse())
	}))
	defer succeed.Close()

	database, _ := db.Open(":memory:")
	defer database.Close()
	_ = db.CreateKey(database, "sk-ccr-fc", "test")

	// Store two provider keys for failprov (both will 429).
	_, _ = db.AddProviderKey(database, "failprov", "key-a")
	_, _ = db.AddProviderKey(database, "failprov", "key-b")

	cfgPath := writeConfigFile(t, fmt.Sprintf(`
[server]
client_port = 9020

[providers.failprov]
  base_url   = %q
  convention = "anthropic"
  api_keys   = []

[providers.okprov]
  base_url   = %q
  convention = "anthropic"
  api_keys   = []

[[models]]
  alias       = "primary-model"
  fallback_to = "fallback-model"
  providers   = [{provider = "failprov", model_id = "primary-id"}]

[[models]]
  alias       = "fallback-model"
  fallback_to = ""
  providers   = [{provider = "okprov", model_id = "fallback-id"}]
`, allFail.URL, succeed.URL))

	mgr, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	bc := middleware.NewBroadcaster()
	rt := New(mgr, database, bc)

	body := `{"model":"primary-model","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":100,"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	key, _ := db.GetKey(database, "sk-ccr-fc")
	req = withClientKey(req, key)

	rr := httptest.NewRecorder()
	rt.HandleMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Allow the async DB write to land.
	time.Sleep(50 * time.Millisecond)

	rows, err := database.Query(`SELECT fallback_count FROM usage ORDER BY ts DESC LIMIT 1`)
	if err != nil {
		t.Fatalf("query usage: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no usage row found")
	}
	var fc int
	if err := rows.Scan(&fc); err != nil {
		t.Fatalf("scan: %v", err)
	}
	// 2 key failures + 1 provider failure + 1 model hop = 4
	if fc != 4 {
		t.Errorf("FallbackCount = %d, want 4", fc)
	}
}

func TestRouter_InvalidModel(t *testing.T) {
	database, _ := db.Open(":memory:")
	defer database.Close()
	_ = db.CreateKey(database, "sk-ccr-inv", "test")

	cfgPath := writeConfigFile(t, `
[server]
client_port = 9010

[providers.x]
  base_url   = "http://localhost"
  convention = "anthropic"
  api_keys   = []

[[models]]
  alias = "real-model"
  fallback_to = ""
  providers = [{provider="x", model_id="x"}]
`)
	mgr, _ := config.Load(cfgPath)
	bc := middleware.NewBroadcaster()
	rt := New(mgr, database, bc)

	body := `{"model":"nonexistent-model","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":100}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	key, _ := db.GetKey(database, "sk-ccr-inv")
	req = withClientKey(req, key)

	rr := httptest.NewRecorder()
	rt.HandleMessages(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for unknown model, got %d", rr.Code)
	}
}
