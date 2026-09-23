package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// ScriptedProvider replays a fixed script of assistant turns. It exists for
// deterministic offline runs (mission demos, evaluation fixtures, tests) and
// never calls a network API. Every response is a canned fixture, so results
// produced with it demonstrate wiring and verification, not model quality.
//
// The step to play is derived from the conversation itself — the number of
// assistant messages after the last user message — so the provider is
// stateless, concurrency-safe and restart-safe.
type ScriptedProvider struct {
	name  string
	model string
	steps []ScriptStep
}

// ScriptStep is one assistant turn: either tool calls or final text.
type ScriptStep struct {
	Text      string           `json:"text,omitempty"`
	ToolCalls []ScriptToolCall `json:"tool_calls,omitempty"`
}

// ScriptToolCall is a tool invocation replayed verbatim.
type ScriptToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// Script is the provider settings payload: {"model": "...", "steps": [...]}.
type Script struct {
	Model string       `json:"model,omitempty"`
	Steps []ScriptStep `json:"steps"`
}

// ScriptedProviderEnvVar enables registering scripted providers. Off by
// default so a production gateway never answers with canned fixtures.
const ScriptedProviderEnvVar = "GOCLAW_ENABLE_SCRIPTED_PROVIDER"

// ScriptedProviderEnabled reports whether scripted providers may be used.
func ScriptedProviderEnabled() bool { return os.Getenv(ScriptedProviderEnvVar) == "1" }

// NewScriptedProvider validates the script and builds the provider.
func NewScriptedProvider(name string, script Script) (*ScriptedProvider, error) {
	if len(script.Steps) == 0 {
		return nil, fmt.Errorf("scripted provider %q: script has no steps", name)
	}
	for i, st := range script.Steps {
		if st.Text == "" && len(st.ToolCalls) == 0 {
			return nil, fmt.Errorf("scripted provider %q: step %d is empty", name, i)
		}
		for j, tc := range st.ToolCalls {
			if tc.Name == "" {
				return nil, fmt.Errorf("scripted provider %q: step %d call %d has no tool name", name, i, j)
			}
		}
	}
	model := script.Model
	if model == "" {
		model = "scripted"
	}
	return &ScriptedProvider{name: name, model: model, steps: script.Steps}, nil
}

// ParseScript decodes provider settings into a Script.
func ParseScript(settings json.RawMessage) (Script, error) {
	var s Script
	if len(settings) == 0 {
		return s, fmt.Errorf("empty scripted provider settings")
	}
	if err := json.Unmarshal(settings, &s); err != nil {
		return s, fmt.Errorf("parse scripted provider settings: %w", err)
	}
	return s, nil
}

func (p *ScriptedProvider) Name() string         { return p.name }
func (p *ScriptedProvider) DefaultModel() string { return p.model }

// Chat returns the next scripted turn.
func (p *ScriptedProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	idx := scriptStepIndex(req.Messages)
	resp := &ChatResponse{}
	if idx >= len(p.steps) {
		resp.Content = "[scripted provider: script exhausted]"
		resp.FinishReason = "stop"
	} else {
		st := p.steps[idx]
		resp.Content = st.Text
		resp.FinishReason = "stop"
		for i, tc := range st.ToolCalls {
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:        fmt.Sprintf("call_s%d_%d", idx, i),
				Name:      tc.Name,
				Arguments: tc.Arguments,
			})
		}
		if len(resp.ToolCalls) > 0 {
			resp.FinishReason = "tool_calls"
		}
	}
	resp.Usage = scriptedUsage(req.Messages, resp)
	return resp, nil
}

// ChatStream delivers the scripted turn as a single chunk.
func (p *ScriptedProvider) ChatStream(ctx context.Context, req ChatRequest, onChunk func(StreamChunk)) (*ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if onChunk != nil {
		if resp.Content != "" {
			onChunk(StreamChunk{Content: resp.Content})
		}
		onChunk(StreamChunk{Done: true})
	}
	return resp, nil
}

func scriptStepIndex(msgs []Message) int {
	lastUser := -1
	for i, m := range msgs {
		if m.Role == "user" {
			lastUser = i
		}
	}
	n := 0
	for _, m := range msgs[lastUser+1:] {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

// scriptedUsage reports a rough character-based estimate (4 chars ≈ 1 token)
// so budget plumbing is exercised. It is not billed usage.
func scriptedUsage(msgs []Message, resp *ChatResponse) *Usage {
	in := 0
	for _, m := range msgs {
		in += len(m.Content)
	}
	out := len(resp.Content)
	for _, tc := range resp.ToolCalls {
		b, _ := json.Marshal(tc.Arguments)
		out += len(tc.Name) + len(b)
	}
	u := &Usage{PromptTokens: in / 4, CompletionTokens: out/4 + 1}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return u
}
