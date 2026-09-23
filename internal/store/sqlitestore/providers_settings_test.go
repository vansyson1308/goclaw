//go:build sqlite || sqliteonly

package sqlitestore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func TestSQLiteProviderStoreReadsTextAndBlobSettings(t *testing.T) {
	db := openTestDB(t)
	if err := EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	initSqlx(db)

	providerStore := NewSQLiteProviderStore(db, "")
	ctx := store.WithTenantID(context.Background(), store.MasterTenantID)

	tests := []struct {
		name     string
		settings json.RawMessage
	}{
		{name: "text-settings", settings: json.RawMessage(`{"source":"text"}`)},
		{name: "blob-settings", settings: json.RawMessage(`{"source":"blob"}`)},
	}
	providerIDs := make([]uuid.UUID, len(tests))
	for i := range tests {
		provider := &store.LLMProviderData{
			BaseModel:    store.BaseModel{ID: uuid.New()},
			Name:         tests[i].name,
			ProviderType: store.ProviderOpenAICompat,
			Enabled:      true,
			Settings:     tests[i].settings,
		}
		if err := providerStore.CreateProvider(ctx, provider); err != nil {
			t.Fatalf("CreateProvider(%q): %v", provider.Name, err)
		}
		providerIDs[i] = provider.ID
	}

	textSettings := string(tests[0].settings)
	if _, err := db.Exec(`UPDATE llm_providers SET settings = ? WHERE id = ?`, textSettings, providerIDs[0]); err != nil {
		t.Fatalf("store settings as TEXT: %v", err)
	}

	for i, wantType := range []string{"text", "blob"} {
		var gotType string
		if err := db.QueryRow(`SELECT typeof(settings) FROM llm_providers WHERE id = ?`, providerIDs[i]).Scan(&gotType); err != nil {
			t.Fatalf("read settings storage type: %v", err)
		}
		if gotType != wantType {
			t.Fatalf("settings storage type = %q, want %q", gotType, wantType)
		}

		got, err := providerStore.GetProvider(ctx, providerIDs[i])
		if err != nil {
			t.Fatalf("GetProvider(%s): %v", wantType, err)
		}
		if string(got.Settings) != string(tests[i].settings) {
			t.Fatalf("GetProvider(%s) settings = %s, want %s", wantType, got.Settings, tests[i].settings)
		}
	}
}

// A row with NULL display_name/api_base/api_key must not fail list scans.
func TestSQLiteProviderStoreToleratesNullTextColumns(t *testing.T) {
	db := openTestDB(t)
	if err := EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	initSqlx(db)
	providerStore := NewSQLiteProviderStore(db, "")
	ctx := store.WithTenantID(context.Background(), store.MasterTenantID)

	id := uuid.New()
	if _, err := db.Exec(`INSERT INTO llm_providers (id, name, provider_type, tenant_id) VALUES (?, 'legacy-null', 'openai_compat', ?)`,
		id.String(), store.MasterTenantID.String()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	list, err := providerStore.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(list) != 1 || list[0].DisplayName != "" || list[0].APIBase != "" || list[0].APIKey != "" {
		t.Fatalf("unexpected list: %+v", list)
	}
	if _, err := providerStore.GetProviderByName(ctx, "legacy-null"); err != nil {
		t.Fatalf("GetProviderByName: %v", err)
	}
}
