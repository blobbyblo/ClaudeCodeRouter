package providers

import (
	"encoding/json"
	"testing"
)

func TestContentBlockUnmarshal_StringContent(t *testing.T) {
	// tool_result with content as a plain string (Anthropic API allows both forms)
	raw := `{"type":"tool_result","tool_use_id":"toolu_abc","content":"bash output here"}`
	var cb ContentBlock
	if err := json.Unmarshal([]byte(raw), &cb); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if cb.Type != "tool_result" {
		t.Errorf("Type = %q, want tool_result", cb.Type)
	}
	if cb.ToolUseID != "toolu_abc" {
		t.Errorf("ToolUseID = %q, want toolu_abc", cb.ToolUseID)
	}
	if len(cb.Content) != 1 || cb.Content[0].Text != "bash output here" {
		t.Errorf("Content = %+v, want [{text: bash output here}]", cb.Content)
	}
}

func TestContentBlockUnmarshal_ArrayContent(t *testing.T) {
	// tool_result with content as an array of blocks
	raw := `{"type":"tool_result","tool_use_id":"toolu_xyz","content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}`
	var cb ContentBlock
	if err := json.Unmarshal([]byte(raw), &cb); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(cb.Content) != 2 {
		t.Fatalf("Content len = %d, want 2", len(cb.Content))
	}
	if cb.Content[0].Text != "line1" || cb.Content[1].Text != "line2" {
		t.Errorf("Content texts = %q %q, want line1 line2", cb.Content[0].Text, cb.Content[1].Text)
	}
}

func TestMessageUnmarshal_ToolResultStringContent(t *testing.T) {
	// Full message as Claude Code sends after a tool call with string tool_result content
	raw := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_abc","content":"the file contents here"}]}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if msg.Role != "user" {
		t.Errorf("Role = %q, want user", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("Content len = %d, want 1", len(msg.Content))
	}
	tr := msg.Content[0]
	if tr.Type != "tool_result" {
		t.Errorf("block Type = %q, want tool_result", tr.Type)
	}
	if tr.ToolUseID != "toolu_abc" {
		t.Errorf("ToolUseID = %q, want toolu_abc", tr.ToolUseID)
	}
	if len(tr.Content) != 1 || tr.Content[0].Text != "the file contents here" {
		t.Errorf("nested Content = %+v", tr.Content)
	}
}
