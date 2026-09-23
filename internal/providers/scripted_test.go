package providers

import (
	"context"
	"testing"
)

func TestScriptedProviderReplaysStepsFromConversation(t *testing.T) {
	p, err := NewScriptedProvider("s", Script{Steps: []ScriptStep{
		{ToolCalls: []ScriptToolCall{{Name: "write_file", Arguments: map[string]any{"path": "a.txt", "content": "x"}}}},
		{Text: "done"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "go"}}
	r1, err := p.Chat(context.Background(), ChatRequest{Messages: msgs})
	if err != nil || len(r1.ToolCalls) != 1 || r1.ToolCalls[0].Name != "write_file" || r1.FinishReason != "tool_calls" {
		t.Fatalf("step 0: %+v %v", r1, err)
	}
	msgs = append(msgs, Message{Role: "assistant", ToolCalls: r1.ToolCalls}, Message{Role: "tool", Content: "ok", ToolCallID: r1.ToolCalls[0].ID})
	r2, _ := p.Chat(context.Background(), ChatRequest{Messages: msgs})
	if r2.Content != "done" || len(r2.ToolCalls) != 0 || r2.FinishReason != "stop" {
		t.Fatalf("step 1: %+v", r2)
	}
	msgs = append(msgs, Message{Role: "assistant", Content: "done"})
	r3, _ := p.Chat(context.Background(), ChatRequest{Messages: msgs})
	if r3.Content == "" || len(r3.ToolCalls) != 0 {
		t.Fatalf("exhausted: %+v", r3)
	}
	// A new user message restarts the script (deterministic per turn).
	msgs = append(msgs, Message{Role: "user", Content: "again"})
	if r4, _ := p.Chat(context.Background(), ChatRequest{Messages: msgs}); len(r4.ToolCalls) != 1 {
		t.Fatalf("new turn should replay step 0: %+v", r4)
	}
	if r1.Usage == nil || r1.Usage.TotalTokens <= 0 {
		t.Fatalf("usage estimate missing: %+v", r1.Usage)
	}
}

func TestScriptedProviderValidation(t *testing.T) {
	if _, err := NewScriptedProvider("s", Script{}); err == nil {
		t.Error("empty script must be rejected")
	}
	if _, err := NewScriptedProvider("s", Script{Steps: []ScriptStep{{}}}); err == nil {
		t.Error("empty step must be rejected")
	}
	if _, err := NewScriptedProvider("s", Script{Steps: []ScriptStep{{ToolCalls: []ScriptToolCall{{}}}}}); err == nil {
		t.Error("nameless tool call must be rejected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, _ := NewScriptedProvider("s", Script{Steps: []ScriptStep{{Text: "x"}}})
	if _, err := p.Chat(ctx, ChatRequest{}); err == nil {
		t.Error("cancelled context must fail")
	}
}
