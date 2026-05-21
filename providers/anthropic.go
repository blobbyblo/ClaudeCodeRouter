package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// AnthropicProvider implements Provider for the Anthropic Messages API.
type AnthropicProvider struct {
	BaseURL string
	client  *http.Client
}

// NewAnthropicProvider constructs an AnthropicProvider pointing at baseURL.
func NewAnthropicProvider(baseURL string) *AnthropicProvider {
	return &AnthropicProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{},
	}
}

// anthropicMessageStart is the subset of the message_start event we care about.
type anthropicMessageStart struct {
	Type    string `json:"type"`
	Message struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// anthropicMessageDelta is the subset of the message_delta event we care about.
type anthropicMessageDelta struct {
	Type  string `json:"type"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// patchAnthropicBody updates model and stream in a raw request body without
// re-marshalling through the struct, preserving all fields (tools, cache_control, etc.).
func patchAnthropicBody(raw []byte, modelID string, defaultMaxTokens int) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	modelJSON, _ := json.Marshal(modelID)
	m["model"] = json.RawMessage(modelJSON)
	m["stream"] = json.RawMessage(`true`)
	if _, ok := m["max_tokens"]; !ok {
		m["max_tokens"] = json.RawMessage(strconv.Itoa(defaultMaxTokens))
	}
	return json.Marshal(m)
}

// Stream sends req to the Anthropic Messages API and writes SSE events to w.
// It returns the input and output token counts parsed from the stream.
func (p *AnthropicProvider) Stream(ctx context.Context, req AnthropicRequest, modelID, apiKey string, w io.Writer) (int, int, error) {
	req.Model = modelID
	req.Stream = true

	var body []byte
	if len(req.RawBody) > 0 {
		var err error
		body, err = patchAnthropicBody(req.RawBody, modelID, req.MaxTokens)
		if err != nil {
			return 0, 0, fmt.Errorf("anthropic: patch request body: %w", err)
		}
	} else {
		var err error
		body, err = json.Marshal(req)
		if err != nil {
			return 0, 0, fmt.Errorf("anthropic: marshal request: %w", err)
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return 0, 0, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("Accept", "text/event-stream")
	if req.AnthropicBeta != "" {
		httpReq.Header.Set("anthropic-beta", req.AnthropicBeta)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return 0, 0, fmt.Errorf("anthropic: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, 0, &RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode >= 500 {
		return 0, 0, ErrUpstream
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("anthropic: unexpected status %d: %s", resp.StatusCode, string(b))
	}

	var inputTokens, outputTokens int

	// eventType and dataLines accumulate across SSE field lines for one event block.
	var eventType string
	var dataLines []string

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		// Check context cancellation on every line.
		select {
		case <-ctx.Done():
			return inputTokens, outputTokens, ctx.Err()
		default:
		}

		line := scanner.Text()

		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))

		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			dataLines = append(dataLines, data)

		case line == "":
			// Empty line = end of event block; flush it.
			if len(dataLines) == 0 {
				eventType = ""
				continue
			}
			combined := strings.Join(dataLines, "\n")

			// Parse token counts before forwarding.
			switch eventType {
			case "message_start":
				var ms anthropicMessageStart
				if json.Unmarshal([]byte(combined), &ms) == nil {
					inputTokens = ms.Message.Usage.InputTokens
				}
			case "message_delta":
				var md anthropicMessageDelta
				if json.Unmarshal([]byte(combined), &md) == nil {
					outputTokens = md.Usage.OutputTokens
				}
			}

			// Forward the raw SSE event to w unchanged.
			if eventType != "" {
				fmt.Fprintf(w, "event: %s\n", eventType)
			}
			for _, d := range dataLines {
				fmt.Fprintf(w, "data: %s\n", d)
			}
			fmt.Fprintf(w, "\n")

			// Reset for next event.
			eventType = ""
			dataLines = dataLines[:0]
		}
	}

	if err := scanner.Err(); err != nil {
		return inputTokens, outputTokens, fmt.Errorf("anthropic: read stream: %w", err)
	}

	return inputTokens, outputTokens, nil
}
