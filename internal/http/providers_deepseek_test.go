package http

import (
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// A DeepSeek provider created through the API/web UI while the gateway runs
// must be registered like the startup path does: provider type kept (DeepSeek
// thinking controls depend on it), DeepSeek's API base and current model when
// none is given — not OpenAI's URL with an empty model.
func TestRegisterInMemoryDeepSeekDefaultsAndType(t *testing.T) {
	reg := providers.NewRegistry(nil)
	h := NewProvidersHandler(newMockProviderStore(), newMockSecretsStore(), reg, "")
	p := &store.LLMProviderData{BaseModel: store.BaseModel{ID: uuid.New()}, TenantID: uuid.New(),
		Name: "deepseek", ProviderType: store.ProviderDeepSeek, APIKey: "sk", Enabled: true}
	h.registerInMemory(p)

	got, err := reg.GetForTenant(p.TenantID, p.Name)
	if err != nil {
		t.Fatal(err)
	}
	op, ok := got.(*providers.OpenAIProvider)
	if !ok {
		t.Fatalf("runtime provider = %T", got)
	}
	if op.ProviderType() != store.ProviderDeepSeek {
		t.Errorf("provider type = %q, want deepseek", op.ProviderType())
	}
	if op.APIBase() != store.DeepSeekDefaultAPIBase || op.DefaultModel() != store.DeepSeekDefaultModel {
		t.Errorf("defaults = %q %q", op.APIBase(), op.DefaultModel())
	}
}
