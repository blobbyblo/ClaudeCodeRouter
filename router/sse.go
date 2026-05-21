package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// sseBytesToMessage parses a buffered Anthropic SSE response and synthesises
// the equivalent non-streaming Anthropic Message JSON body.
//
// Handled events: message_start, content_block_start, content_block_delta
// (text_delta / thinking_delta / input_json_delta), message_delta.
// Unknown events are silently ignored.
func sseBytesToMessage(sse []byte) ([]byte, error) {
	var msgID, model string
	var inputTokens, outputTokens int
	var stopReason string
	var stopSequence interface{} // nil or string

	type blockState struct {
		blockType string
		text      strings.Builder
		thinking  strings.Builder
		toolID    string
		toolName  string
		toolJSON  strings.Builder
	}

	blocks := make(map[int]*blockState)
	var blockOrder []int

	scanner := bufio.NewScanner(bytes.NewReader(sse))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var curEvent string
	var curDataLines []string

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			curDataLines = append(curDataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if curEvent == "" || len(curDataLines) == 0 {
				curEvent = ""
				curDataLines = curDataLines[:0]
				continue
			}
			data := strings.Join(curDataLines, "\n")

			switch curEvent {
			case "message_start":
				var ms struct {
					Message struct {
						ID    string `json:"id"`
						Model string `json:"model"`
						Usage struct {
							InputTokens int `json:"input_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				if json.Unmarshal([]byte(data), &ms) == nil {
					msgID = ms.Message.ID
					model = ms.Message.Model
					inputTokens = ms.Message.Usage.InputTokens
				}

			case "content_block_start":
				var cbs struct {
					Index        int `json:"index"`
					ContentBlock struct {
						Type string `json:"type"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content_block"`
				}
				if json.Unmarshal([]byte(data), &cbs) == nil {
					bs := &blockState{
						blockType: cbs.ContentBlock.Type,
						toolID:    cbs.ContentBlock.ID,
						toolName:  cbs.ContentBlock.Name,
					}
					blocks[cbs.Index] = bs
					blockOrder = append(blockOrder, cbs.Index)
				}

			case "content_block_delta":
				var cbd struct {
					Index int `json:"index"`
					Delta struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						Thinking    string `json:"thinking"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(data), &cbd) == nil {
					if bs, ok := blocks[cbd.Index]; ok {
						switch cbd.Delta.Type {
						case "text_delta":
							bs.text.WriteString(cbd.Delta.Text)
						case "thinking_delta":
							bs.thinking.WriteString(cbd.Delta.Thinking)
						case "input_json_delta":
							bs.toolJSON.WriteString(cbd.Delta.PartialJSON)
						}
					}
				}

			case "message_delta":
				var md struct {
					Delta struct {
						StopReason   string  `json:"stop_reason"`
						StopSequence *string `json:"stop_sequence"`
					} `json:"delta"`
					Usage struct {
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				}
				if json.Unmarshal([]byte(data), &md) == nil {
					stopReason = md.Delta.StopReason
					if md.Delta.StopSequence != nil {
						stopSequence = *md.Delta.StopSequence
					}
					if md.Usage.OutputTokens > 0 {
						outputTokens = md.Usage.OutputTokens
					}
				}
			}

			curEvent = ""
			curDataLines = curDataLines[:0]
		}
	}

	// Build content blocks in ascending index order (preserve model output order).
	sort.Ints(blockOrder)

	type textBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type thinkingBlock struct {
		Type     string `json:"type"`
		Thinking string `json:"thinking"`
	}
	type toolUseBlock struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}

	contentBlocks := make([]interface{}, 0, len(blockOrder))
	for _, idx := range blockOrder {
		bs := blocks[idx]
		switch bs.blockType {
		case "text":
			contentBlocks = append(contentBlocks, textBlock{Type: "text", Text: bs.text.String()})
		case "thinking":
			contentBlocks = append(contentBlocks, thinkingBlock{Type: "thinking", Thinking: bs.thinking.String()})
		case "tool_use":
			inputRaw := json.RawMessage(bs.toolJSON.String())
			if len(inputRaw) == 0 {
				inputRaw = json.RawMessage("{}")
			}
			// Ensure the accumulated JSON is valid before sending.
			var check interface{}
			if json.Unmarshal(inputRaw, &check) != nil {
				inputRaw = json.RawMessage("{}")
			}
			contentBlocks = append(contentBlocks, toolUseBlock{
				Type:  "tool_use",
				ID:    bs.toolID,
				Name:  bs.toolName,
				Input: inputRaw,
			})
		}
	}

	response := map[string]interface{}{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       contentBlocks,
		"stop_reason":   stopReason,
		"stop_sequence": stopSequence,
		"usage": map[string]int{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}

	return json.Marshal(response)
}
