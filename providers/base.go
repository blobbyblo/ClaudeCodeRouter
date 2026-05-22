package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUpstream is returned by providers on 5xx responses.
var ErrUpstream = errors.New("upstream error (5xx)")

// ErrRateLimit is the sentinel that callers should match with errors.Is.
// Providers return a *RateLimitError (which satisfies Is(ErrRateLimit)) so
// that the cooldown duration from the Retry-After header is also available.
var ErrRateLimit = errors.New("rate limited (429)")

// ErrContextExceeded is returned when the request body is too large for the
// model's context window (HTTP 400 context-length error).  Unlike rate limits
// or upstream errors, this is a property of the REQUEST, not the key or
// provider — retrying other keys or models will produce the same result, so
// the fallback chain stops immediately and the proxy synthesises a response
// that signals Claude Code to compact the conversation.
var ErrContextExceeded = errors.New("context window exceeded")

// ContextExceededError wraps ErrContextExceeded and carries the token counts
// parsed from the upstream error body so the proxy can include them in the
// synthesised response.
type ContextExceededError struct {
	ContextLimit int
	InputTokens  int
}

func (e *ContextExceededError) Error() string        { return ErrContextExceeded.Error() }
func (e *ContextExceededError) Is(target error) bool { return target == ErrContextExceeded }
func (e *ContextExceededError) Unwrap() error        { return ErrContextExceeded }

// RateLimitError wraps ErrRateLimit and carries the duration from the
// upstream Retry-After header (0 means "use the default cooldown").
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string        { return ErrRateLimit.Error() }
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimit }
func (e *RateLimitError) Unwrap() error        { return ErrRateLimit }

// parseRetryAfter parses a Retry-After header value (integer seconds or
// HTTP-date) and returns the corresponding duration plus a 500ms buffer.
// Returns 0 if the header is absent or unparseable — callers should fall
// back to a default cooldown.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	// Integer seconds — the form used by most REST APIs.
	var secs float64
	if _, err := fmt.Sscanf(h, "%f", &secs); err == nil && secs > 0 {
		return time.Duration(secs*float64(time.Second)) + 500*time.Millisecond
	}
	// HTTP-date (RFC 1123), e.g. "Wed, 21 May 2026 13:30:00 GMT".
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d + 500*time.Millisecond
		}
	}
	return 0
}

type ContentBlock struct {
    Type string `json:"type"`
    Text string `json:"text,omitempty"`
    // tool_use fields
    ID        string          `json:"id,omitempty"`
    Name      string          `json:"name,omitempty"`
    InputJSON json.RawMessage `json:"input,omitempty"`
    // tool_result fields
    ToolUseID string          `json:"tool_use_id,omitempty"`
    Content   []ContentBlock  `json:"content,omitempty"` // ← add this
}

// AnthropicRequest is our internal normalized request format (Anthropic Messages API shape)
type AnthropicRequest struct {
	Model       string          `json:"model"`
	Messages    []Message       `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	TopK        *int            `json:"top_k,omitempty"`
	StopSeq     []string        `json:"stop_sequences,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Thinking    json.RawMessage `json:"thinking,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`

	// RawBody holds the original request bytes. When set, the Anthropic provider
	// uses it directly (patching only model/stream) so no fields are silently dropped.
	RawBody []byte `json:"-"`
	// AnthropicBeta is forwarded verbatim as the anthropic-beta request header.
	// Claude Code sends this for extended thinking, computer use, and other beta features.
	AnthropicBeta string `json:"-"`
	// NonStreaming is true when the original client sent stream=false.
	// The proxy always streams upstream; this flag tells proxy() to buffer the
	// SSE response and synthesize a plain JSON Message for the client.
	NonStreaming bool `json:"-"`
}

// SystemText extracts a plain text string from the system field, which the
// Anthropic API accepts as either a JSON string or an array of content blocks.
func SystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// UnmarshalJSON handles tool_result blocks where the nested "content" field may be
// a plain string instead of an array of content blocks (both are valid Anthropic API).
func (c *ContentBlock) UnmarshalJSON(b []byte) error {
	// Use a defined type (no methods) to avoid infinite recursion.
	type CB ContentBlock
	var aux struct {
		CB
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	// Copy all scalar fields; aux.CB.Content is nil because the outer Content field shadows it.
	*c = ContentBlock(aux.CB)
	if len(aux.Content) == 0 {
		return nil
	}
	// Try plain string first (e.g. tool_result content).
	var s string
	if err := json.Unmarshal(aux.Content, &s); err == nil {
		c.Content = []ContentBlock{{Type: "text", Text: s}}
		return nil
	}
	// Fall back to array of content blocks.
	return json.Unmarshal(aux.Content, &c.Content)
}

// UnmarshalJSON accepts either a plain string or a content-block array for Content.
func (m *Message) UnmarshalJSON(b []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	if len(raw.Content) == 0 {
		return nil
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(raw.Content, &s); err == nil {
		m.Content = []ContentBlock{{Type: "text", Text: s}}
		return nil
	}
	// Fall back to block array.
	return json.Unmarshal(raw.Content, &m.Content)
}

// Provider streams Anthropic-format SSE to w.
// Returns (inputTokens, outputTokens, error).
// error is ErrRateLimit, ErrUpstream, or a generic error.
type Provider interface {
	Stream(ctx context.Context, req AnthropicRequest, modelID, apiKey string, w io.Writer) (int, int, error)
}
