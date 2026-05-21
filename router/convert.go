package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/bobbyo/ccr/providers"
)

// ---- OpenAI wire types (inbound) ----

type openAIRequest struct {
	Model         string          `json:"model"`
	Messages      []openAIMessage `json:"messages"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Stop          []string        `json:"stop,omitempty"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAITool struct {
    Type     string          `json:"type"`
    Function openAIFunction  `json:"function"`
}

type openAIFunction struct {
    Name        string          `json:"name"`
    Description string          `json:"description,omitempty"`
    Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// OpenAIToAnthropic converts an OpenAI chat completions request body to our
// internal Anthropic-format request.
func OpenAIToAnthropic(body []byte) (providers.AnthropicRequest, error) {
	var oai openAIRequest
	if err := json.Unmarshal(body, &oai); err != nil {
		return providers.AnthropicRequest{}, fmt.Errorf("convert: unmarshal openai request: %w", err)
	}

	var sysRaw json.RawMessage
	var msgs []providers.Message

	for _, m := range oai.Messages {
		if m.Role == "system" {
			b, _ := json.Marshal(m.Content)
			sysRaw = json.RawMessage(b)
			continue
		}
		msgs = append(msgs, providers.Message{
			Role: m.Role,
			Content: []providers.ContentBlock{
				{Type: "text", Text: m.Content},
			},
		})
	}

	maxTokens := oai.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}

	return providers.AnthropicRequest{
		Model:       oai.Model,
		Messages:    msgs,
		System:      sysRaw,
		MaxTokens:   maxTokens,
		Stream:      true,
		Temperature: oai.Temperature,
		TopP:        oai.TopP,
		StopSeq:     oai.Stop,
	}, nil
}

// AnthropicBodyToRequest parses a raw Anthropic request body and normalises it.
func AnthropicBodyToRequest(body []byte) (providers.AnthropicRequest, error) {
	var req providers.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return providers.AnthropicRequest{}, fmt.Errorf("convert: unmarshal anthropic request: %w", err)
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 8192
	}
	// Remember whether the client wanted a non-streaming response; we always
	// stream internally so that every upstream provider can use the same code path.
	req.NonStreaming = !req.Stream
	req.Stream = true
	req.RawBody = body
	return req, nil
}

// AnthropicToOpenAI converts an internal AnthropicRequest to an OpenAI chat
// completions request body (JSON bytes). Used by tests and tooling.
func AnthropicToOpenAI(req providers.AnthropicRequest, modelID string) ([]byte, error) {
	var msgs []openAIMessage
	if sysText := providers.SystemText(req.System); sysText != "" {
		msgs = append(msgs, openAIMessage{Role: "system", Content: sysText})
	}
	for _, m := range req.Messages {
		var parts []string
		for _, b := range m.Content {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		msgs = append(msgs, openAIMessage{
			Role:    m.Role,
			Content: strings.Join(parts, ""),
		})
	}

	// Convert Anthropic tools → OpenAI tools.
	// Anthropic: { name, description, input_schema }
	// OpenAI:    { type: "function", function: { name, description, parameters } }
	var tools []json.RawMessage
	if len(req.Tools) > 0 {
		var anthropicTools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(req.Tools, &anthropicTools); err == nil {
			for _, t := range anthropicTools {
				fn := map[string]interface{}{
					"type": "function",
					"function": map[string]interface{}{
						"name":        t.Name,
						"description": t.Description,
						"parameters":  t.InputSchema,
					},
				}
				b, err := json.Marshal(fn)
				if err != nil {
					return nil, fmt.Errorf("convert: marshal tool %q: %w", t.Name, err)
				}
				tools = append(tools, b)
			}
		}
	}

	// Convert Anthropic tool_choice → OpenAI tool_choice.
	// Anthropic: { type: "auto"|"any"|"tool", name?: "..." }
	// OpenAI:    "auto" | "required" | { type: "function", function: { name } }
	var toolChoice interface{}
	if len(req.ToolChoice) > 0 {
		var tc struct {
			Type string `json:"type"`
			Name string `json:"name,omitempty"`
		}
		if err := json.Unmarshal(req.ToolChoice, &tc); err == nil {
			switch tc.Type {
			case "auto":
				toolChoice = "auto"
			case "any":
				toolChoice = "required"
			case "tool":
				toolChoice = map[string]interface{}{
					"type":     "function",
					"function": map[string]string{"name": tc.Name},
				}
			}
		}
	}

	oai := struct {
		Model         string            `json:"model"`
		Messages      []openAIMessage   `json:"messages"`
		MaxTokens     int               `json:"max_tokens,omitempty"`
		Stream        bool              `json:"stream"`
		StreamOptions *streamOptions    `json:"stream_options,omitempty"`
		Tools         []json.RawMessage `json:"tools,omitempty"`
		ToolChoice    interface{}       `json:"tool_choice,omitempty"`
	}{
		Model:         modelID,
		Messages:      msgs,
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Tools:         tools,
		ToolChoice:    toolChoice,
	}
	return json.Marshal(oai)
}

// anthropicSSEToOpenAI reads Anthropic SSE bytes and writes OpenAI SSE to w.
func anthropicSSEToOpenAI(src []byte, w io.Writer) error {
	conv := newOpenAIStreamConverter(w)
	if _, err := conv.Write(src); err != nil {
		return err
	}
	return conv.processLine("") // flush any pending incomplete event
}

// ---- OpenAI SSE output types ----

type openAIChunkOut struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Model   string            `json:"model"`
	Choices []openAIChoiceOut `json:"choices"`
	Usage   *openAIUsageOut   `json:"usage,omitempty"`
}

type openAIChoiceOut struct {
	Index        int            `json:"index"`
	Delta        openAIDeltaOut `json:"delta"`
	FinishReason *string        `json:"finish_reason,omitempty"`
}

type openAIDeltaOut struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type openAIUsageOut struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
}

// ---- openAIStreamConverter --------------------------------------------------

// openAIStreamConverter converts Anthropic SSE written to it into OpenAI SSE
// written to dst. It implements io.Writer so it can be passed anywhere a
// streaming provider would write Anthropic events.
type openAIStreamConverter struct {
	dst         io.Writer
	remainder   []byte
	curEvent    string
	curData     []string
	msgID       string
	model       string
	emittedRole bool
}

func newOpenAIStreamConverter(dst io.Writer) *openAIStreamConverter {
	return &openAIStreamConverter{dst: dst, msgID: "chatcmpl-ccr", model: "ccr"}
}

func (c *openAIStreamConverter) Write(p []byte) (int, error) {
	buf := append(c.remainder, p...)
	c.remainder = nil

	for len(buf) > 0 {
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			c.remainder = append([]byte(nil), buf...)
			break
		}
		line := strings.TrimRight(string(buf[:idx]), "\r")
		buf = buf[idx+1:]
		if err := c.processLine(line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

func (c *openAIStreamConverter) processLine(line string) error {
	switch {
	case strings.HasPrefix(line, "event:"):
		c.curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		c.curData = append(c.curData, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	case line == "":
		if c.curEvent == "" || len(c.curData) == 0 {
			c.curEvent, c.curData = "", c.curData[:0]
			return nil
		}
		combined := strings.Join(c.curData, "\n")
		err := c.convertEvent(c.curEvent, combined)
		c.curEvent, c.curData = "", c.curData[:0]
		return err
	}
	return nil
}

func (c *openAIStreamConverter) convertEvent(eventType, data string) error {
	switch eventType {
	case "message_start":
		var ms struct {
			Message struct {
				Model string `json:"model"`
				ID    string `json:"id"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(data), &ms) == nil {
			if ms.Message.Model != "" {
				c.model = ms.Message.Model
			}
			if ms.Message.ID != "" {
				c.msgID = ms.Message.ID
			}
		}
		if !c.emittedRole {
			c.emittedRole = true
			chunk := openAIChunkOut{
				ID:      c.msgID,
				Object:  "chat.completion.chunk",
				Model:   c.model,
				Choices: []openAIChoiceOut{{Index: 0, Delta: openAIDeltaOut{Role: "assistant"}}},
			}
			b, _ := json.Marshal(chunk)
			_, err := fmt.Fprintf(c.dst, "data: %s\n\n", b)
			return err
		}

	case "content_block_delta":
		var delta struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &delta) == nil && delta.Delta.Type == "text_delta" {
			chunk := openAIChunkOut{
				ID:      c.msgID,
				Object:  "chat.completion.chunk",
				Model:   c.model,
				Choices: []openAIChoiceOut{{Index: 0, Delta: openAIDeltaOut{Content: delta.Delta.Text}}},
			}
			b, _ := json.Marshal(chunk)
			_, err := fmt.Fprintf(c.dst, "data: %s\n\n", b)
			return err
		}

	case "message_delta":
		var md struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		stopReason := "stop"
		outputTokens := 0
		if json.Unmarshal([]byte(data), &md) == nil {
			if md.Delta.StopReason == "max_tokens" {
				stopReason = "length"
			} else if md.Delta.StopReason == "tool_use" {
				stopReason = "tool_calls"
			}
			outputTokens = md.Usage.OutputTokens
		}
		finish := stopReason
		chunk := openAIChunkOut{
			ID:     c.msgID,
			Object: "chat.completion.chunk",
			Model:  c.model,
			Choices: []openAIChoiceOut{{
				Index:        0,
				Delta:        openAIDeltaOut{},
				FinishReason: &finish,
			}},
			Usage: &openAIUsageOut{CompletionTokens: outputTokens},
		}
		b, _ := json.Marshal(chunk)
		_, err := fmt.Fprintf(c.dst, "data: %s\n\n", b)
		return err

	case "message_stop":
		_, err := fmt.Fprintf(c.dst, "data: [DONE]\n\n")
		return err
	}
	return nil
}
