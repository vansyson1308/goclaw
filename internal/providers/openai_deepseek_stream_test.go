package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A DeepSeek thinking-mode stream (reasoning_content deltas, then a tool
// call, then the usage chunk with DeepSeek's cache fields) is parsed into
// Thinking, ToolCalls and Usage, and the request carries no temperature.
func TestDeepSeekStreamParsing(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Need to read "}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_content":"sum.go first."}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"sum.go\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":60,"total_tokens":1260,"prompt_cache_hit_tokens":1000,"prompt_cache_miss_tokens":200,"completion_tokens_details":{"reasoning_tokens":25}}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := NewOpenAIProvider("deepseek", "sk", srv.URL, "deepseek-flash").WithProviderType("deepseek")
	resp, err := p.ChatStream(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "fix sum"}},
		Tools:    []ToolDefinition{{Type: "function", Function: &ToolFunctionSchema{Name: "read_file", Parameters: map[string]any{"type": "object"}}}},
		Options:  map[string]any{OptTemperature: 0.7},
	}, func(StreamChunk) {})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := sent["temperature"]; has {
		t.Errorf("temperature sent in thinking mode: %v", sent["temperature"])
	}
	if resp.Thinking != "Need to read sum.go first." {
		t.Errorf("thinking = %q", resp.Thinking)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" || resp.ToolCalls[0].Arguments["path"] != "sum.go" {
		t.Errorf("tool calls = %+v", resp.ToolCalls)
	}
	if u := resp.Usage; u == nil || u.CacheReadTokens != 1000 || u.ThinkingTokens != 25 || !u.PromptTokensIncludeCachedSegments {
		t.Errorf("usage = %+v", resp.Usage)
	}
}
