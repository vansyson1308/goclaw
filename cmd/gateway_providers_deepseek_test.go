package cmd

import (
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// A DeepSeek provider created without an API base or model (web UI, API) must
// talk to DeepSeek with its current model, not fall through to OpenAI's URL
// and an empty model. deepseek-chat/deepseek-reasoner were retired upstream.
func TestDeepSeekProviderDefaults(t *testing.T) {
	base, model := openAIProviderDefaults(store.ProviderDeepSeek, "")
	if base != "https://api.deepseek.com" || model != "deepseek-flash" {
		t.Fatalf("defaults = %q %q", base, model)
	}
	if base, _ := openAIProviderDefaults(store.ProviderDeepSeek, "https://proxy.example/v1"); base != "https://proxy.example/v1" {
		t.Fatalf("explicit api_base must be kept, got %q", base)
	}
}
