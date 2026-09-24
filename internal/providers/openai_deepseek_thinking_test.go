package providers

import "testing"

// DeepSeek V4.x (deepseek-flash, deepseek-v4-pro) thinks by default. The
// agent's thinking level maps onto DeepSeek's own controls — `thinking.type`
// and `reasoning_effort` (low|high|max) — and sampling knobs that thinking
// mode does not support are not sent.
func TestDeepSeekThinkingControls(t *testing.T) {
	p := NewOpenAIProvider("deepseek", "sk", "https://api.deepseek.com", "deepseek-flash").WithProviderType("deepseek")
	build := func(level string) map[string]any {
		opts := map[string]any{OptTemperature: 0.7}
		if level != "" {
			opts[OptThinkingLevel] = level
		}
		return p.buildRequestBody("deepseek-flash", ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, Options: opts}, true)
	}
	cases := []struct {
		level, thinking, effort string
		temperature             bool
	}{
		{"", "", "", false},           // provider default: thinking on, effort high
		{"off", "disabled", "", true}, // no thinking: temperature allowed again
		{"minimal", "enabled", "low", false},
		{"low", "enabled", "low", false},
		{"medium", "enabled", "high", false},
		{"high", "enabled", "high", false},
		{"xhigh", "enabled", "max", false},
	}
	for _, c := range cases {
		body := build(c.level)
		gotThinking := ""
		if th, ok := body["thinking"].(map[string]any); ok {
			gotThinking, _ = th["type"].(string)
		}
		gotEffort, _ := body[OptReasoningEffort].(string)
		_, hasTemp := body["temperature"]
		if gotThinking != c.thinking || gotEffort != c.effort || hasTemp != c.temperature {
			t.Errorf("level %q: thinking=%q effort=%q temperature=%v; want %q %q %v",
				c.level, gotThinking, gotEffort, hasTemp, c.thinking, c.effort, c.temperature)
		}
	}
}

// The provider-level switch (settings.thinking_enabled=false) turns thinking
// off regardless of the agent's level.
func TestDeepSeekProviderThinkingDisabled(t *testing.T) {
	off := false
	p := NewOpenAIProvider("deepseek", "sk", "https://api.deepseek.com", "deepseek-flash").WithProviderType("deepseek")
	p.thinkingEnabled = &off
	body := p.buildRequestBody("deepseek-flash", ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}},
		Options: map[string]any{OptThinkingLevel: "high"}}, true)
	th, _ := body["thinking"].(map[string]any)
	if th["type"] != "disabled" || body[OptReasoningEffort] != nil {
		t.Fatalf("provider switch ignored: %v", body)
	}
}

// DeepSeek's controls are only sent to DeepSeek's own API: an aggregator
// (OpenRouter, a proxy) routing a deepseek model uses its own parameters.
func TestDeepSeekControlsOnlyForDeepSeekAPI(t *testing.T) {
	p := NewOpenAIProvider("openrouter", "sk", "https://openrouter.ai/api/v1", "deepseek/deepseek-v4.1-flash")
	body := p.buildRequestBody("deepseek/deepseek-v4.1-flash", ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}},
		Options: map[string]any{OptThinkingLevel: "off", OptTemperature: 0.5}}, true)
	if _, ok := body["thinking"]; ok {
		t.Fatalf("DeepSeek thinking param sent to a non-DeepSeek API: %v", body)
	}
	if _, ok := body["temperature"]; !ok {
		t.Fatalf("temperature must be untouched for non-DeepSeek routes")
	}
}

// Detection also works for a DeepSeek provider registered as a generic
// OpenAI-compatible one pointing at api.deepseek.com.
func TestDeepSeekRouteDetectedByAPIBase(t *testing.T) {
	p := NewOpenAIProvider("my-ds", "sk", "https://api.deepseek.com/v1", "deepseek-flash")
	body := p.buildRequestBody("deepseek-flash", ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}},
		Options: map[string]any{OptThinkingLevel: "off"}}, true)
	if th, _ := body["thinking"].(map[string]any); th["type"] != "disabled" {
		t.Fatalf("api.deepseek.com route not detected: %v", body)
	}
}
