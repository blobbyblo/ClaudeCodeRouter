package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OpenAIProvider implements Provider for the OpenAI Chat Completions API,
// converting to/from the Anthropic SSE format on the fly.
type OpenAIProvider struct {
	BaseURL string
	client  *http.Client
}

// NewOpenAIProvider constructs an OpenAIProvider pointing at baseURL.
func NewOpenAIProvider(baseURL string) *OpenAIProvider {
	return &OpenAIProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		// ResponseHeaderTimeout: how long to wait for NIM to start streaming after
		// the request is sent. Once the first byte arrives the stream runs freely.
		// Without this, a queued NIM request blocks until the caller's context
		// deadline fires (~30 s for Claude Code), burning the whole budget before
		// the fallback chain can try the next key or model.
		client: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
	}
}

// ---- OpenAI wire types -------------------------------------------------------

type openAIRequest struct {
	Model         string           `json:"model"`
	Messages      []openAIMessage  `json:"messages"`
	Stream        bool             `json:"stream"`
	StreamOptions openAIStreamOpts `json:"stream_options"`
	MaxTokens     int              `json:"max_tokens,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	Stop          []string         `json:"stop,omitempty"`
	Tools         json.RawMessage  `json:"tools,omitempty"`
	ToolChoice    json.RawMessage  `json:"tool_choice,omitempty"`
}

type openAIStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
}

// openAIChunk is the minimal shape of an OpenAI streaming chunk.
type openAIChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string           `json:"role"`
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"` // DeepSeek R1, Kimi K2, Qwen3
			ToolCalls        []openAIToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type openAIToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ---- conversion helpers ------------------------------------------------------

// nimConflictingProps lists property names that are also JSON Schema keywords.
// NIM's backend conflates parameters.properties["type"] with parameters["type"],
// causing an "unhashable type: 'dict'" 500 error when a tool property is named "type".
var nimConflictingProps = map[string]bool{"type": true}

// sanitizeParameterSchema removes properties whose names conflict with JSON Schema
// keywords (confirmed: "type") from a parameters schema before sending to NIM.
// It also removes those names from the "required" array if present.
func sanitizeParameterSchema(schema json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(schema, &obj); err != nil {
		return schema
	}
	propsRaw, ok := obj["properties"]
	if !ok {
		return schema
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal(propsRaw, &props); err != nil {
		return schema
	}
	modified := false
	for key := range props {
		if nimConflictingProps[key] {
			delete(props, key)
			modified = true
		}
	}
	if !modified {
		return schema
	}
	newProps, _ := json.Marshal(props)
	obj["properties"] = json.RawMessage(newProps)
	if reqRaw, ok2 := obj["required"]; ok2 {
		var required []string
		if json.Unmarshal(reqRaw, &required) == nil {
			filtered := required[:0]
			for _, r := range required {
				if !nimConflictingProps[r] {
					filtered = append(filtered, r)
				}
			}
			if len(filtered) != len(required) {
				if rb, err := json.Marshal(filtered); err == nil {
					obj["required"] = json.RawMessage(rb)
				}
			}
		}
	}
	result, _ := json.Marshal(obj)
	return json.RawMessage(result)
}

// convertToOpenAI converts an AnthropicRequest to an openAIRequest.
func convertToOpenAI(req AnthropicRequest, modelID string) openAIRequest {
	var msgs []openAIMessage
	if sysText := SystemText(req.System); sysText != "" {
		b, _ := json.Marshal(sysText)
		msgs = append(msgs, openAIMessage{Role: "system", Content: json.RawMessage(b)})
	}

	for _, m := range req.Messages {
		var textParts []string
		var toolCalls []map[string]interface{}
		var toolResults []openAIMessage

		for _, c := range m.Content {
			switch c.Type {
			case "text":
				textParts = append(textParts, c.Text)
			case "tool_use":
				argsStr := string(c.InputJSON)
				if argsStr == "" || argsStr == "null" {
					argsStr = "{}"
				}
				toolCalls = append(toolCalls, map[string]interface{}{
					"id":   c.ID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      c.Name,
						"arguments": argsStr,
					},
				})
			case "tool_result":
				var resultText string
				if len(c.Content) > 0 {
					var parts []string
					for _, inner := range c.Content {
						if inner.Type == "text" {
							parts = append(parts, inner.Text)
						}
					}
					resultText = strings.Join(parts, "")
				} else {
					resultText = c.Text
				}
				textContent, _ := json.Marshal(resultText)
				toolResults = append(toolResults, openAIMessage{
					Role:       "tool",
					Content:    json.RawMessage(textContent),
					ToolCallID: c.ToolUseID,
				})
			}
		}

		if m.Role == "assistant" {
			msg := openAIMessage{Role: "assistant"}
			if len(toolCalls) > 0 {
				b, _ := json.Marshal(toolCalls)
				msg.ToolCalls = json.RawMessage(b)
			}
			if len(textParts) > 0 {
				text := strings.Join(textParts, "")
				b, _ := json.Marshal(text)
				msg.Content = json.RawMessage(b)
			} else {
				msg.Content = json.RawMessage(`null`)
			}
			msgs = append(msgs, msg)
		} else {
			// user: tool results first, then text
			msgs = append(msgs, toolResults...)
			if len(textParts) > 0 {
				text := strings.Join(textParts, "")
				b, _ := json.Marshal(text)
				msgs = append(msgs, openAIMessage{
					Role:    m.Role,
					Content: json.RawMessage(b),
				})
			}
		}
	}

	oai := openAIRequest{
		Model:         modelID,
		Messages:      msgs,
		Stream:        true,
		StreamOptions: openAIStreamOpts{IncludeUsage: true},
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		Stop:          req.StopSeq,
	}

	if len(req.Tools) > 0 {
		var anthropicTools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(req.Tools, &anthropicTools); err == nil {
			type oaiFunction struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters"`
			}
			type oaiTool struct {
				Type     string      `json:"type"`
				Function oaiFunction `json:"function"`
			}
			toolCount := len(anthropicTools)
			descCap := 480
			if toolCount > 100 {
				descCap = 160
			} else if toolCount > 40 {
				descCap = 280
			}
			tools := make([]oaiTool, len(anthropicTools))
			for i, t := range anthropicTools {
				desc := t.Description
				if len(desc) > descCap {
					desc = desc[:descCap] + "..."
				}
				tools[i] = oaiTool{
					Type: "function",
					Function: oaiFunction{
						Name:        t.Name,
						Description: desc,
						Parameters:  sanitizeParameterSchema(t.InputSchema),
					},
				}
			}
			b, _ := json.Marshal(tools)
			oai.Tools = json.RawMessage(b)
		}
	}

	if len(req.ToolChoice) > 0 {
		var tc struct {
			Type string `json:"type"`
			Name string `json:"name,omitempty"`
		}
		if err := json.Unmarshal(req.ToolChoice, &tc); err == nil {
			var converted interface{}
			switch tc.Type {
			case "auto":
				converted = "auto"
			case "any":
				converted = "required"
			case "tool":
				converted = map[string]interface{}{
					"type":     "function",
					"function": map[string]string{"name": tc.Name},
				}
			default:
				converted = "auto"
			}
			b, _ := json.Marshal(converted)
			oai.ToolChoice = json.RawMessage(b)
		}
	}

	return oai
}

// ---- Anthropic SSE event builder ---------------------------------------------

func writeAnthropicEvent(w io.Writer, eventType, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
}

// ---- <think> tag streaming filter -------------------------------------------

// thinkFilter routes streaming content to thinking vs text blocks,
// handling <think>...</think> tags that may be split across chunks.
type thinkFilter struct {
	inThink    bool
	partial    string // suffix of last chunk that might be start of a tag
}

// process returns (textContent, thinkContent, remainingPartial).
// textContent goes to the text block, thinkContent to the thinking block.
func (f *thinkFilter) process(chunk string) (text, think string) {
	buf := f.partial + chunk
	f.partial = ""

	for len(buf) > 0 {
		if !f.inThink {
			if idx := strings.Index(buf, "<think>"); idx >= 0 {
				text += buf[:idx]
				f.inThink = true
				buf = buf[idx+len("<think>"):]
			} else {
				// Check if buf ends with a partial "<think>" prefix
				overlap := partialTagOverlap(buf, "<think>")
				text += buf[:len(buf)-overlap]
				f.partial = buf[len(buf)-overlap:]
				buf = ""
			}
		} else {
			if idx := strings.Index(buf, "</think>"); idx >= 0 {
				think += buf[:idx]
				f.inThink = false
				buf = buf[idx+len("</think>"):]
			} else {
				overlap := partialTagOverlap(buf, "</think>")
				think += buf[:len(buf)-overlap]
				f.partial = buf[len(buf)-overlap:]
				buf = ""
			}
		}
	}
	return text, think
}

// partialTagOverlap returns the length of the longest suffix of s that is a
// prefix of tag, so we don't emit bytes that might be the start of a tag.
func partialTagOverlap(s, tag string) int {
	max := len(tag) - 1
	if max > len(s) {
		max = len(s)
	}
	for l := max; l > 0; l-- {
		if strings.HasSuffix(s, tag[:l]) {
			return l
		}
	}
	return 0
}

// ---- Stream ------------------------------------------------------------------

// Stream sends req to the OpenAI Chat Completions API, converts the response
// to Anthropic SSE format, and writes it to w.
// On a 400 context-length error it returns *ContextExceededError so the
// router can synthesise a compact signal instead of retrying other keys.
func (p *OpenAIProvider) Stream(ctx context.Context, req AnthropicRequest, modelID, apiKey string, w io.Writer) (int, int, error) {
	oaiReq := convertToOpenAI(req, modelID)

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return 0, 0, fmt.Errorf("openai: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, 0, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return 0, 0, fmt.Errorf("openai: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, 0, &RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode >= 500 {
		b, _ := io.ReadAll(resp.Body)
		slog.Error("openai: upstream 5xx", "status", resp.StatusCode, "body", string(b))
		return 0, 0, ErrUpstream
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		// Detect context-length 400 before returning a generic error.
		if resp.StatusCode == http.StatusBadRequest {
			if limit, inputToks := parseContextLengthError(b); limit > 0 && inputToks > 0 {
				slog.Warn("openai: context window exceeded",
					"model", modelID, "context_limit", limit, "input_tokens", inputToks)
				return 0, 0, &ContextExceededError{ContextLimit: limit, InputTokens: inputToks}
			}
		}
		return 0, 0, fmt.Errorf("openai: unexpected status %d: %s", resp.StatusCode, string(b))
	}

	var inputTokens, outputTokens int
	messageID := "msg_" + modelID

	// Anthropic stream state
	streamStarted := false
	streamEnded := false   // prevents duplicate finish events
	nextBlockIdx := 0
	thinkingBlockIdx := -1 // -1=not opened, >=0=open, -2=closed
	textBlockIdx := -1     // -1=not opened, >=0=open, -2=closed

	// tool call block tracking: openAI tool_calls[].index → anthropic block index
	type tcInfo struct{ blockIdx int }
	var toolCallSlots []tcInfo

	var filter thinkFilter

	// emitThinkingDelta opens the thinking block if needed and writes a delta.
	emitThinkingDelta := func(content string) {
		if content == "" {
			return
		}
		if thinkingBlockIdx == -1 {
			thinkingBlockIdx = nextBlockIdx
			nextBlockIdx++
			d, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_start",
				"index": thinkingBlockIdx,
				"content_block": map[string]interface{}{
					"type": "thinking", "thinking": "",
				},
			})
			writeAnthropicEvent(w, "content_block_start", string(d))
		}
		if thinkingBlockIdx < 0 {
			return // already closed
		}
		d, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_delta",
			"index": thinkingBlockIdx,
			"delta": map[string]string{"type": "thinking_delta", "thinking": content},
		})
		writeAnthropicEvent(w, "content_block_delta", string(d))
	}

	closeThinkingBlock := func() {
		if thinkingBlockIdx >= 0 {
			d, _ := json.Marshal(map[string]interface{}{"type": "content_block_stop", "index": thinkingBlockIdx})
			writeAnthropicEvent(w, "content_block_stop", string(d))
			thinkingBlockIdx = -2
		}
	}

	// emitTextDelta opens the text block if needed and writes a delta.
	emitTextDelta := func(content string) {
		if content == "" {
			return
		}
		if textBlockIdx == -1 {
			closeThinkingBlock()
			textBlockIdx = nextBlockIdx
			nextBlockIdx++
			d, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_start",
				"index": textBlockIdx,
				"content_block": map[string]string{"type": "text", "text": ""},
			})
			writeAnthropicEvent(w, "content_block_start", string(d))
		}
		if textBlockIdx < 0 {
			return // already closed
		}
		d, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_delta",
			"index": textBlockIdx,
			"delta": map[string]string{"type": "text_delta", "text": content},
		})
		writeAnthropicEvent(w, "content_block_delta", string(d))
	}

	scanner := bufio.NewScanner(resp.Body)
	// Some models stream large tool arguments; increase default buffer.
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return inputTokens, outputTokens, ctx.Err()
		default:
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

		if data == "[DONE]" {
			writeAnthropicEvent(w, "message_stop", `{"type":"message_stop"}`)
			break
		}

		var chunk openAIChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if chunk.Usage != nil {
			inputTokens = chunk.Usage.PromptTokens
			outputTokens = chunk.Usage.CompletionTokens
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]

		// Emit message_start + ping on the first chunk that has choices.
		if !streamStarted {
			streamStarted = true
			if chunk.ID != "" {
				messageID = chunk.ID
			}
			d, _ := json.Marshal(map[string]interface{}{
				"type": "message_start",
				"message": map[string]interface{}{
					"id": messageID, "type": "message", "role": "assistant",
					"content": []interface{}{}, "model": modelID,
					"stop_reason": nil, "stop_sequence": nil,
					"usage": map[string]int{"input_tokens": 0, "output_tokens": 1},
				},
			})
			writeAnthropicEvent(w, "message_start", string(d))
			writeAnthropicEvent(w, "ping", `{"type":"ping"}`)
		}

		// Reasoning content (providers that use a dedicated field).
		if choice.Delta.ReasoningContent != "" {
			emitThinkingDelta(choice.Delta.ReasoningContent)
		}

		// Tool calls.
		for _, tc := range choice.Delta.ToolCalls {
			// Grow slot slice to accommodate this tool_call index.
			for len(toolCallSlots) <= tc.Index {
				toolCallSlots = append(toolCallSlots, tcInfo{blockIdx: -1})
			}
			slot := &toolCallSlots[tc.Index]
			if slot.blockIdx < 0 {
				closeThinkingBlock()
				slot.blockIdx = nextBlockIdx
				nextBlockIdx++
				d, _ := json.Marshal(map[string]interface{}{
					"type":  "content_block_start",
					"index": slot.blockIdx,
					"content_block": map[string]interface{}{
						"type": "tool_use", "id": tc.ID,
						"name": tc.Function.Name, "input": map[string]interface{}{},
					},
				})
				writeAnthropicEvent(w, "content_block_start", string(d))
			}
			if tc.Function.Arguments != "" {
				d, _ := json.Marshal(map[string]interface{}{
					"type":  "content_block_delta",
					"index": slot.blockIdx,
					"delta": map[string]string{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				})
				writeAnthropicEvent(w, "content_block_delta", string(d))
			}
		}

		// Text content — run through <think> tag filter first.
		if choice.Delta.Content != "" {
			textOut, thinkOut := filter.process(choice.Delta.Content)
			emitThinkingDelta(thinkOut)
			emitTextDelta(textOut)
		}

		// Finish (guard against duplicate finish_reason chunks).
		if choice.FinishReason != nil && !streamEnded {
			streamEnded = true

			// Flush any partial tag as text.
			if filter.partial != "" {
				if filter.inThink {
					emitThinkingDelta(filter.partial)
				} else {
					emitTextDelta(filter.partial)
				}
				filter.partial = ""
			}

			// Close all open content blocks in index order.
			closeThinkingBlock()
			for _, slot := range toolCallSlots {
				if slot.blockIdx >= 0 {
					d, _ := json.Marshal(map[string]interface{}{"type": "content_block_stop", "index": slot.blockIdx})
					writeAnthropicEvent(w, "content_block_stop", string(d))
				}
			}
			if textBlockIdx >= 0 {
				d, _ := json.Marshal(map[string]interface{}{"type": "content_block_stop", "index": textBlockIdx})
				writeAnthropicEvent(w, "content_block_stop", string(d))
				textBlockIdx = -2
			}

			stopReason := mapFinishReason(*choice.FinishReason)
			d, _ := json.Marshal(map[string]interface{}{
				"type":  "message_delta",
				"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
				"usage": map[string]int{"output_tokens": outputTokens},
			})
			writeAnthropicEvent(w, "message_delta", string(d))
		}
	}

	if err := scanner.Err(); err != nil {
		return inputTokens, outputTokens, fmt.Errorf("openai: read stream: %w", err)
	}

	return inputTokens, outputTokens, nil
}


// contextExceededRe matches OpenAI-compatible "context length exceeded" 400 bodies.
// Example: "...maximum context length of 262144 tokens. You requested ... : 233409 tokens from the input..."
var contextExceededRe = regexp.MustCompile(
	`maximum context length of (\d+)[^:]+:\s*(\d+) tokens from the input`,
)

// parseContextLengthError parses a 400 response body and returns (contextLimit,
// inputTokens) if it is a context-length-exceeded error, or (0, 0) otherwise.
func parseContextLengthError(body []byte) (contextLimit, inputTokens int) {
	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &errResp) != nil {
		return 0, 0
	}
	m := contextExceededRe.FindStringSubmatch(errResp.Error.Message)
	if m == nil {
		return 0, 0
	}
	limit, err1 := strconv.Atoi(m[1])
	input, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || limit <= 0 || input <= 0 {
		return 0, 0
	}
	return limit, input
}

// mapFinishReason converts an OpenAI finish_reason to an Anthropic stop_reason.
func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}
